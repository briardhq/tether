package management

import (
	"bytes"
	"testing"

	"briard.io/tether/internal/family"
)

func areq(subsystem, id byte, payload ...byte) []byte {
	return znpFrame(typeAREQ, subsystem, id, payload)
}

func srsp(subsystem, id byte, payload ...byte) []byte {
	return znpFrame(typeSRSP, subsystem, id, payload)
}

func sreq(subsystem, id byte, payload ...byte) []byte {
	return znpFrame(typeSREQ, subsystem, id, payload)
}

// The reset indication is the count that justifies the census existing at all. INV 1 and INV 7
// claim the radio is never reset by anything tether does, and a count is what makes that claim
// continuously falsifiable rather than asserted by construction. A reset the radio reports as
// *external* means something pulled its reset line — in production, on somebody else's
// machine.
func TestResetIndicationsAreCountedByReason(t *testing.T) {
	c := NewCensus()
	c.Adopt(family.ZNP)

	c.Observe(true, areq(subsystemSYS, commandResetInd, resetExternal, 0x02, 0x00, 0x02, 0x07, 0x01))
	c.Observe(true, areq(subsystemSYS, commandResetInd, resetWatchdog, 0x02, 0x00, 0x02, 0x07, 0x01))
	c.Observe(true, areq(subsystemSYS, commandResetInd, resetPowerUp, 0x02, 0x00, 0x02, 0x07, 0x01))

	got := c.Counts()
	if got.ResetsExternal != 1 || got.ResetsWatchdog != 1 || got.ResetsPowerUp != 1 {
		t.Errorf("resets counted as external=%d watchdog=%d power-up=%d, want 1 each",
			got.ResetsExternal, got.ResetsWatchdog, got.ResetsPowerUp)
	}
	// The last reason is kept in words, because "3 resets" without a cause is the log line
	// that starts the argument this whole card exists to end.
	if got.LastResetReason == "" || got.LastResetAt.IsZero() {
		t.Errorf("a reset was counted with no reason or time recorded: %+v", got)
	}
	if got.RadioAREQ != 3 {
		t.Errorf("radio unsolicited frames = %d, want 3", got.RadioAREQ)
	}
}

// A rejection is the radio saying it could not parse what reached it. That is byte corruption,
// named by the only party that can see it — and it is what a telnet-mode ser2net eating 0xFF
// produces. Being able to show it as zero is the point.
func TestRejectionsAreCountedWithTheirCode(t *testing.T) {
	c := NewCensus()
	c.Adopt(family.ZNP)
	c.Observe(true, srsp(subsystemRPCError, commandNotRecognized, 0x04, 0x21, 0x01))

	got := c.Counts()
	if got.Rejections != 1 {
		t.Fatalf("rejections = %d, want 1", got.Rejections)
	}
	if got.LastRejectionCode != "invalid length" {
		t.Errorf("rejection code %q, want %q", got.LastRejectionCode, "invalid length")
	}
	if got.RadioSRSP != 1 {
		t.Errorf("radio answers = %d, want 1", got.RadioSRSP)
	}
}

// Requests out versus answers back, as two independent counts a human compares. No matching, no
// transaction tracking — the imbalance is the diagnostic and it costs no state.
func TestRequestsAndAnswersAreCountedSeparately(t *testing.T) {
	c := NewCensus()
	c.Adopt(family.ZNP)
	for range 3 {
		c.Observe(false, sreq(subsystemSYS, commandPing))
	}
	c.Observe(true, srsp(subsystemSYS, commandPing, 0x79, 0x01))

	got := c.Counts()
	if got.ClientSREQ != 3 || got.ClientFrames != 3 {
		t.Errorf("client requests=%d frames=%d, want 3 and 3", got.ClientSREQ, got.ClientFrames)
	}
	if got.RadioSRSP != 1 {
		t.Errorf("radio answers = %d, want 1 — the imbalance is the whole signal", got.RadioSRSP)
	}
}

// The stream arrives in whatever chunks the kernel felt like, so a frame split across reads must
// still be one frame, and two frames in one read must still be two.
func TestFramesAreFoundAcrossChunkBoundaries(t *testing.T) {
	whole := append(areq(subsystemSYS, commandResetInd, resetExternal), srsp(subsystemSYS, commandPing, 0x79, 0x01)...)

	for split := 0; split <= len(whole); split++ {
		c := NewCensus()
		c.Adopt(family.ZNP)
		c.Observe(true, whole[:split])
		c.Observe(true, whole[split:])

		got := c.Counts()
		if got.RadioFrames != 2 {
			t.Errorf("split at %d: %d frames, want 2", split, got.RadioFrames)
		}
		if got.ResetsExternal != 1 {
			t.Errorf("split at %d: %d external resets, want 1", split, got.ResetsExternal)
		}
		if got.UnframedBytes != 0 {
			t.Errorf("split at %d: %d bytes called unframed, want 0", split, got.UnframedBytes)
		}
	}
}

// The unframed count is not an error count, and this is the case that says so. Both patterns
// below were captured from a real Sonoff, and one run accounted to the byte: 260 unframed =
// 256 + 4, against exactly 4 resets.
//
// It is here because a nonzero count read as "something has corrupted bytes" sends a reader
// looking for a fault in ordinary traffic. If anyone later decides
// the scanner should swallow these quietly, this test is where the number's meaning is written
// down: the *shape* is the diagnostic, and a count climbing with no connects or resets behind
// it is the corruption the counter exists to catch.
func TestUnframedCountsWhatAHealthySessionReallyProduces(t *testing.T) {
	// zigpy's _skip_bootloader burst: 256 raw BootloaderRunMode.FORCE_RUN bytes, written when
	// its first SYS ping times out. Over a socket it has no DTR/RTS to toggle, so this in-band
	// substitution is the normal path for a networked client, not an error path.
	skip := bytes.Repeat([]byte{0xEF}, 256)
	c := NewCensus()
	c.Adopt(family.ZNP)
	c.Observe(false, skip)
	if got := c.Counts(); got.UnframedBytes != 256 || got.ClientFrames != 0 {
		t.Errorf("the bootloader-skip burst counted as %d unframed bytes and %d frames, "+
			"want 256 and 0", got.UnframedBytes, got.ClientFrames)
	}

	// And the radio's side: one 0x00 as the line settles after a reset. A pty does not
	// reproduce it, which is exactly why the emulator shows zero unframed bytes and hardware
	// does not — so this is the only place it is pinned.
	c = NewCensus()
	c.Adopt(family.ZNP)
	reset := srsp(subsystemSYS, commandPing, 0x79, 0x01)
	c.Observe(true, []byte{0x00})
	c.Observe(true, reset)
	got := c.Counts()
	if got.UnframedBytes != 1 {
		t.Errorf("a settling null counted as %d unframed bytes, want 1", got.UnframedBytes)
	}
	if got.RadioFrames != 1 {
		t.Errorf("the null swallowed the frame after it: %d frames, want 1", got.RadioFrames)
	}
}

// Joining mid-frame, payload bytes that look like a SOF, and outright noise must all resolve to
// "count what is really there, and say how much was not a frame" — never to a wrong count and
// never to a stall.
func TestTheScannerResynchronises(t *testing.T) {
	good := srsp(subsystemSYS, commandPing, 0x79, 0x01)

	for _, tc := range []struct {
		name         string
		in           []byte
		wantFrames   uint64
		wantUnframed uint64
	}{
		{name: "joined mid-frame", in: append(good[3:], good...), wantFrames: 1, wantUnframed: uint64(len(good) - 3)},
		{name: "noise before a frame", in: append([]byte{0x00, 0x11, 0x22}, good...), wantFrames: 1, wantUnframed: 3},
		{name: "a false SOF inside noise", in: append([]byte{0xFE, 0x00, 0x00}, good...), wantFrames: 1, wantUnframed: 3},
		{name: "a payload byte that looks like a SOF", in: srsp(subsystemSYS, commandPing, 0xFE, 0x01), wantFrames: 1},
		{name: "nothing but noise", in: []byte{0x01, 0x02, 0x03, 0x04}, wantUnframed: 4},
		{name: "a truncated frame is not counted", in: good[:len(good)-1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCensus()
			c.Adopt(family.ZNP)
			c.Observe(true, tc.in)
			got := c.Counts()
			if got.RadioFrames != tc.wantFrames {
				t.Errorf("%d frames, want %d", got.RadioFrames, tc.wantFrames)
			}
			if got.UnframedBytes != tc.wantUnframed {
				t.Errorf("%d unframed bytes, want %d", got.UnframedBytes, tc.wantUnframed)
			}
		})
	}
}

// The census must be total over arbitrary input: it runs on the data path's goroutine, so a
// panic there would take the radio down over a counter.
func TestTheScannerSurvivesArbitraryBytes(t *testing.T) {
	c := NewCensus()
	c.Adopt(family.ZNP)
	chunk := make([]byte, 512)
	seed := uint32(1)
	for range 200 {
		for i := range chunk {
			seed = seed*1664525 + 1013904223
			chunk[i] = byte(seed >> 24)
		}
		c.Observe(true, chunk)
		c.Observe(false, chunk)
	}
	// Nothing to assert but "it returned"; the buffer must also not have grown without bound.
	if n := len(c.radio.buf); n > 512 {
		t.Errorf("the scanner is holding %d bytes; it should never exceed one frame", n)
	}
}

// A family whose frames we cannot read counts nothing, and says so by being absent rather than
// by reporting zeroes that look like a silent radio.
func TestAnUnreadableFamilyCountsNothing(t *testing.T) {
	c := NewCensus()
	c.Adopt(family.EZSP)
	if c.Enabled() {
		t.Fatal("an EZSP census claims to be counting ZNP frames")
	}
	c.Observe(true, areq(subsystemSYS, commandResetInd, resetExternal))
	if got := c.Counts(); got != (FrameCounts{}) {
		t.Errorf("an EZSP census counted %+v", got)
	}
}

// The observer is handed the pipe's own buffer, so it must not retain or mutate it.
func TestObserveDoesNotRetainTheCallersBuffer(t *testing.T) {
	c := NewCensus()
	c.Adopt(family.ZNP)
	buf := append([]byte{}, srsp(subsystemSYS, commandPing, 0x79, 0x01)...)
	original := append([]byte{}, buf...)

	c.Observe(true, buf[:3]) // a partial frame it has to hold on to
	for i := range buf {
		buf[i] = 0xAA // the pipe reuses its buffer for the next read
	}
	if !bytes.Equal(original[:3], []byte{0xFE, 0x02, 0x61}) {
		t.Fatal("the fixture is wrong")
	}
	c.Observe(true, original[3:])

	if got := c.Counts(); got.RadioFrames != 1 {
		t.Errorf("%d frames after the caller reused its buffer, want 1", got.RadioFrames)
	}
}
