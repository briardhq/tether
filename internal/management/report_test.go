package management

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/tether/internal/family"
	"briard.io/tether/internal/pipe"
)

// The status verb over the socket a running tether actually listens on, because what is being
// asserted is that a second process can read the card — not that a struct can be marshalled.
func TestStatusSocketAnswersWithTheCard(t *testing.T) {
	radio := newFakeRadio(func([]byte) []byte {
		return []byte{0xFE, 0x02, 0x61, 0x01, 0x79, 0x01, 0x1A}
	})
	defer radio.stop()
	p := pipe.New(nil)
	p.Serve(radio)

	m := NewMonitor(p, nil)

	m.DeviceOpened(family.ZNP)
	if _, err := m.Probe(); err != nil {
		t.Fatalf("probing: %v", err)
	}

	path := filepath.Join(socketDir(t), "status.sock")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- m.ServeStatus(ctx, path) }()

	var card Card
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for {
		if card, err = ReadStatus(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing answered on the status socket: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if card.RadioType != string(family.ZNP) {
		t.Errorf("radio_type %q, want %q", card.RadioType, family.ZNP)
	}
	if card.StartedAt.IsZero() || !card.Now.After(card.StartedAt) {
		t.Errorf("uptime is not readable from started_at=%v now=%v", card.StartedAt, card.Now)
	}
	if card.Probe == nil {
		t.Fatal("the card carries no probe result after a successful probe")
	}
	if !card.Probe.OK || card.Probe.Capabilities != 0x0179 {
		t.Errorf("probe reported %+v, want a healthy 0x0179", card.Probe)
	}

	// Cancelling stops the listener without an error, which is what lets a caller treat a
	// shutdown and a broken socket differently.
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("ServeStatus returned %v on a cancelled context, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("ServeStatus did not return after its context was cancelled")
	}
}

// A radio that has gone quiet has to reach the card as a *failure*, not as an absence: those
// two render differently and mean different things to whoever is reading.
func TestASilentRadioIsRecordedAsAFailedProbe(t *testing.T) {
	radio := newFakeRadio(func([]byte) []byte { return nil })
	defer radio.stop()
	p := pipe.New(nil)
	p.Serve(radio)

	m := NewMonitor(p, nil)

	m.DeviceOpened(family.ZNP)
	if card := m.Card(); card.Probe != nil {
		t.Fatalf("a monitor that has not probed reports %+v", card.Probe)
	}

	if _, err := m.Probe(); err == nil {
		t.Fatal("a silent radio probed successfully")
	}
	card := m.Card()
	if card.Probe == nil {
		t.Fatal("a failed probe left no trace on the card")
	}
	if card.Probe.OK || card.Probe.Error == "" {
		t.Errorf("failed probe reported as %+v, want ok=false with a reason", card.Probe)
	}
	if !strings.Contains(card.Summary(), "silent") {
		t.Errorf("the one-line summary does not say the radio is silent: %s", card.Summary())
	}
}

// ErrBusy is not news about the radio, so it must not overwrite news that is. Recording it
// would replace a good answer from a minute ago with "unknown" every time a client happened to
// be attached — losing the only fact worth having, on the schedule most likely to lose it.
func TestABusyDeviceDoesNotOverwriteTheLastProbe(t *testing.T) {
	radio := newFakeRadio(func([]byte) []byte {
		return []byte{0xFE, 0x02, 0x61, 0x01, 0x79, 0x01, 0x1A}
	})
	defer radio.stop()
	p := pipe.New(nil)
	p.Serve(radio)

	m := NewMonitor(p, nil)

	m.DeviceOpened(family.ZNP)
	if _, err := m.Probe(); err != nil {
		t.Fatalf("the first probe: %v", err)
	}
	before := *m.Card().Probe

	client, server := net.Pipe()
	defer client.Close()
	p.Attach(server)

	if _, err := m.Probe(); !errors.Is(err, pipe.ErrBusy) {
		t.Fatalf("probing with a client attached returned %v, want ErrBusy", err)
	}
	after := *m.Card().Probe
	if after != before {
		t.Errorf("a busy device rewrote the last probe: %+v, was %+v", after, before)
	}
}

// A family with no probe must not look like a radio that failed one.
func TestAnUnprobedFamilyReportsNoProbeRatherThanAFailure(t *testing.T) {
	radio := newFakeRadio(func([]byte) []byte { return nil })
	defer radio.stop()
	p := pipe.New(nil)
	p.Serve(radio)

	m := NewMonitor(p, nil)

	m.DeviceOpened(family.EZSP)
	ctx, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stop()
	m.Run(ctx, 50*time.Millisecond, time.Hour)

	card := m.Card()
	if card.Probe != nil {
		t.Errorf("an EZSP radio was probed anyway: %+v", card.Probe)
	}
	if !strings.Contains(card.Summary(), "not probed") {
		t.Errorf("the summary does not distinguish unprobed from failed: %s", card.Summary())
	}
}

// The probe is tether's own traffic, and the census counts the client path only: its request
// never crosses the client side and its answer is routed into the exchange, so neither may
// reach the card. Without this an idle ZNP tether gains one radio answer a minute with no
// client request behind it — 1,440 a day — and the request/answer imbalance the census exists
// to show reads as a radio answering questions nobody asked.
func TestAProbeLeavesTheCensusUntouched(t *testing.T) {
	pong := []byte{0xFE, 0x02, 0x61, 0x01, 0x79, 0x01, 0x1A}
	radio := newFakeRadio(func([]byte) []byte { return pong })
	defer radio.stop()

	census := NewCensus()
	p := pipe.New(census.Observe)
	m := NewMonitor(p, census)
	p.Serve(radio)
	m.DeviceOpened(family.ZNP)

	if _, err := m.Probe(); err != nil {
		t.Fatalf("probing: %v", err)
	}
	// The exchange has returned, so its reply has been through the pipe; nothing arrives later
	// to make a zero here a race rather than a result.
	if got := m.Card().Frames; got == nil || *got != (FrameCounts{}) {
		t.Errorf("after a probe the census reads %+v, want nothing counted", got)
	}

	// And the same seven bytes, when a client asked for them, are counted — so the zero above
	// is the exchange being kept out, not the census being switched off.
	client, server := net.Pipe()
	defer client.Close()
	p.Attach(server)
	go func() { _, _ = client.Write(pingRequest) }()
	if _, err := io.ReadFull(client, make([]byte, len(pong))); err != nil {
		t.Fatalf("reading the client's reply: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		counts := m.Card().Frames
		if counts != nil && counts.ClientSREQ == 1 && counts.RadioSRSP == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a client's exchange never reached the card: %+v", counts)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The census through the real data path: a radio that emits a reset indication, a client that
// sends a request, and the counts arriving on the card — with the bytes reaching the other side
// byte-for-byte, which is INV 6 and the thing the observer must be incapable of breaking.
func TestTheCensusCountsWhatCrossesThePipeWithoutAlteringIt(t *testing.T) {
	resetInd := znpFrame(typeAREQ, subsystemSYS, commandResetInd, []byte{resetExternal, 0x02, 0x00, 0x02, 0x07, 0x01})
	radio := newFakeRadio(func([]byte) []byte { return resetInd })
	defer radio.stop()

	census := NewCensus()
	p := pipe.New(census.Observe)
	m := NewMonitor(p, census)

	// The real program's order, and it matters: the census learns which radio it is counting
	// for when the device opens, and the device opens before any listener exists (INV 2). A
	// client cannot therefore send a frame the census was not yet switched on for.
	p.Serve(radio)
	m.DeviceOpened(family.ZNP)

	client, server := net.Pipe()
	defer client.Close()
	p.Attach(server)

	// A client request provokes the radio's frame, and the reply must arrive unchanged.
	request := znpFrame(typeSREQ, subsystemSYS, commandPing, nil)
	go func() { _, _ = client.Write(request) }()

	got := make([]byte, len(resetInd))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("reading the radio's frame at the client: %v", err)
	}
	if !bytes.Equal(got, resetInd) {
		t.Errorf("the client received % x, want % x — the observer altered the stream", got, resetInd)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		counts := m.Card().Frames
		if counts != nil && counts.ResetsExternal == 1 && counts.ClientSREQ == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the card never showed the frames that crossed: %+v", counts)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if card := m.Card(); card.Frames.UnframedBytes != 0 {
		t.Errorf("%d bytes of a clean stream were called unframed", card.Frames.UnframedBytes)
	}
}
