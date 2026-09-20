package management

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"briard.io/tether/internal/pipe"
)

// The request is the one frame tether ever puts on the UART unasked, so it is asserted as
// literal bytes rather than as "whatever the builder produces". FE, no data, SREQ|SYS, ping,
// and the XOR of the three bytes after the SOF.
func TestPingRequestIsTheZNPPingFrame(t *testing.T) {
	want := []byte{0xFE, 0x00, 0x21, 0x01, 0x20}
	if !bytes.Equal(pingRequest, want) {
		t.Errorf("SYS ping request is % x, want % x", pingRequest, want)
	}
}

func TestPingResponse(t *testing.T) {
	// A well-formed response carrying capabilities 0x0179.
	good := []byte{0xFE, 0x02, 0x61, 0x01, 0x79, 0x01, 0x1A}
	// An unsolicited AREQ the radio can emit at any time: ZDO (0x05) end-device announce,
	// type AREQ (2) → cmd0 0x45. Its content does not matter, only that it is stepped over.
	areq := znpFrame(2, 0x05, 0xC1, []byte{0xAA, 0xBB})

	for _, tc := range []struct {
		name string
		in   []byte
		want []byte
	}{
		{name: "the response on its own", in: good, want: []byte{0x79, 0x01}},
		{
			// The reason this scans instead of parsing from byte zero.
			name: "an unsolicited AREQ arrives first",
			in:   append(append([]byte{}, areq...), good...),
			want: []byte{0x79, 0x01},
		},
		{name: "leading junk before the SOF", in: append([]byte{0x00, 0x11}, good...), want: []byte{0x79, 0x01}},
		{name: "nothing yet", in: nil},
		{name: "a header but no payload yet", in: good[:4]},
		{name: "one byte short", in: good[:len(good)-1]},
		{name: "a bad checksum is not a frame", in: []byte{0xFE, 0x02, 0x61, 0x01, 0x79, 0x01, 0x00}},
		{
			// A complete, valid frame that answers a different question must not be mistaken
			// for this one — SYS Version (id 0x02) rather than Ping.
			name: "a different SRSP",
			in:   znpFrame(typeSRSP, subsystemSYS, 0x02, []byte{0x79, 0x01}),
		},
		{
			// AREQ ping-shaped: right subsystem and id, wrong type.
			name: "the right command as the wrong type",
			in:   znpFrame(2, subsystemSYS, commandPing, []byte{0x79, 0x01}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pingResponse(tc.in)
			if tc.want == nil {
				if ok {
					t.Fatalf("read a ping response out of % x: % x", tc.in, got)
				}
				return
			}
			if !ok {
				t.Fatalf("found no ping response in % x", tc.in)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("capabilities % x, want % x", got, tc.want)
			}
		})
	}
}

// The probe end to end over a real pipe and a radio that answers the way one does: the request
// goes through the device layer's write path and the answer comes back through the same
// long-lived reader a client's bytes would.
//
// **The radio takes its time on purpose, and that is what makes the RTT assertion real.**
// A real coordinator answers a ping in milliseconds; an in-memory radio answers in microseconds,
// and asserting that *that* is measurable assumes a clock finer than the platform's. Measured
// on Windows 11: `time.Now()`'s smallest non-zero step there is 70 µs and 199,995 of 200,000
// back-to-back readings report no step at all, against 29 ns and none on Linux — so a
// microsecond round trip measures exactly 0 and the assertion fails on a true reading. So the
// fake radio is asked for a delay a coordinator would really take, which keeps the assertion
// able to fail for the reason it was written: an RTT nobody measured.
func TestPingReachesTheRadioAndReadsTheAnswer(t *testing.T) {
	radio := newFakeRadio(func(req []byte) []byte {
		if !bytes.Equal(req, pingRequest) {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
		return []byte{0xFE, 0x02, 0x61, 0x01, 0x79, 0x01, 0x1A}
	})
	defer radio.stop()

	p := pipe.New(nil)
	p.Serve(radio)

	result, err := NewMonitor(p, nil).Probe()
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if result.Capabilities != 0x0179 {
		t.Errorf("capabilities 0x%04x, want 0x0179", result.Capabilities)
	}
	if result.RTT <= 0 {
		t.Errorf("round trip %v, want something measurable", result.RTT)
	}
}

// A radio that has stopped answering must be reported as such, and within the probe's own
// deadline rather than by hanging — the failure this exists to catch is a wedged coordinator,
// which is exactly the one that goes quiet instead of erroring.
func TestPingReportsASilentRadio(t *testing.T) {
	radio := newFakeRadio(func([]byte) []byte { return nil })
	defer radio.stop()

	p := pipe.New(nil)
	p.Serve(radio)

	start := time.Now()
	_, err := NewMonitor(p, nil).Probe()
	if err == nil {
		t.Fatal("a radio that said nothing was reported as healthy")
	}
	if elapsed := time.Since(start); elapsed > 2*probeTimeout {
		t.Errorf("took %v to give up, want about %v", elapsed, probeTimeout)
	}
}

// Bytes that are not an answer say something different from silence, and must not be read as
// success — this is what a wrong baud rate or the wrong radio family looks like.
func TestPingRejectsNoise(t *testing.T) {
	radio := newFakeRadio(func([]byte) []byte { return []byte{0x00, 0xFF, 0x10, 0x42} })
	defer radio.stop()

	p := pipe.New(nil)
	p.Serve(radio)

	if _, err := NewMonitor(p, nil).Probe(); err == nil {
		t.Fatal("noise from the radio was read as a healthy answer")
	}
}

// The probe is issued only when no client is connected. A client on the device is
// proving it alive already, and a probe frame injected into its session would be a byte it
// never sent — INV 6 seen from the other side.
func TestPingRefusesWhileAClientIsAttached(t *testing.T) {
	radio := newFakeRadio(func([]byte) []byte {
		return []byte{0xFE, 0x02, 0x61, 0x01, 0x79, 0x01, 0x1A}
	})
	defer radio.stop()

	p := pipe.New(nil)
	p.Serve(radio)

	client, server := net.Pipe()
	defer client.Close()
	p.Attach(server)

	if _, err := NewMonitor(p, nil).Probe(); !errors.Is(err, pipe.ErrBusy) {
		t.Fatalf("Ping returned %v while a client held the device, want ErrBusy", err)
	}
}

// fakeRadio is a coordinator that answers what reply says, on the same io.ReadWriter the
// device layer presents.
type fakeRadio struct {
	reply func([]byte) []byte
	out   chan []byte

	mu      sync.Mutex
	pending []byte
	stopped bool
}

func newFakeRadio(reply func([]byte) []byte) *fakeRadio {
	return &fakeRadio{reply: reply, out: make(chan []byte, 8)}
}

func (r *fakeRadio) Write(b []byte) (int, error) {
	if answer := r.reply(b); answer != nil {
		r.out <- answer
	}
	return len(b), nil
}

func (r *fakeRadio) Read(b []byte) (int, error) {
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		answer, ok := <-r.out
		if !ok {
			return 0, io.EOF
		}
		r.mu.Lock()
		r.pending = answer
	}
	n := copy(b, r.pending)
	r.pending = r.pending[n:]
	r.mu.Unlock()
	return n, nil
}

func (r *fakeRadio) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.stopped {
		r.stopped = true
		close(r.out)
	}
}
