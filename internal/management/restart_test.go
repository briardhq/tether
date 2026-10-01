package management

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"briard.io/tether/internal/family"
	"briard.io/tether/internal/pipe"
)

// What the measured stick says as DTR rises: one stray zero as the UART comes up, then SYS
// ResetInd with reason power-up — read off the reference ZBDongle-P, not built.
var bootAnnouncement = []byte{0x00, 0xFE, 0x06, 0x41, 0x80, 0x00, 0x02, 0x01, 0x02, 0x07, 0x01, 0xC0}

// answersPings is a coordinator that is up: it answers a SYS ping and nothing else.
func answersPings(req []byte) []byte {
	if bytes.Equal(req, pingRequest) {
		return []byte{0xFE, 0x02, 0x61, 0x01, 0x79, 0x01, 0x1A}
	}
	return nil
}

// restartRig is a monitor over a fake radio, and a restart that counts how often it was pulled.
// announce says whether the radio boots visibly when it is.
func restartRig(t *testing.T, radio family.Radio, announce bool) (*Monitor, *pipe.Pipe, *atomic.Int32) {
	t.Helper()
	fake := newFakeRadio(answersPings)
	t.Cleanup(fake.stop)
	p := pipe.New(nil)
	p.Serve(fake)

	pulled := new(atomic.Int32)
	m := NewMonitor(p, nil)
	m.DeviceOpened(radio, nil, func() error {
		pulled.Add(1)
		if announce {
			fake.out <- bootAnnouncement
		}
		return nil
	})
	return m, p, pulled
}

func TestARestartIsConfirmedByTheRadioAnnouncingItsBoot(t *testing.T) {
	m, _, pulled := restartRig(t, family.ZNP, true)

	got := m.Restart()
	if !got.OK {
		t.Fatalf("Restart failed: %s", got.Error)
	}
	if got.ResetReason != "power-up" {
		t.Errorf("reset reason %q, want the power-up the radio announced", got.ResetReason)
	}
	if n := pulled.Load(); n != 1 {
		t.Errorf("the reset line was pulled %d times, want once", n)
	}
	// The ping after it is what puts a fresh answer on the card.
	if probe := m.Card().Probe; probe == nil || !probe.OK {
		t.Errorf("the card's probe after a restart is %+v, want a fresh answer", probe)
	}
}

// A pulse the radio does not answer is not a restart. It looks exactly like a reset line
// wired somewhere else, or a radio wedged past what a reset cures, and reporting it as success
// would send whoever asked away with a broken coordinator.
func TestARestartTheRadioDoesNotAnnounceIsAFailure(t *testing.T) {
	m, _, _ := restartRig(t, family.ZNP, false)

	start := time.Now()
	got := m.Restart()
	if got.OK {
		t.Fatal("a pulse with no announcement after it was reported as a restart")
	}
	if !strings.Contains(got.Error, "did not announce") {
		t.Errorf("error %q does not say the radio stayed quiet", got.Error)
	}
	if took := time.Since(start); took > restartTimeout+2*time.Second {
		t.Errorf("took %v to give up, want about %v", took, restartTimeout)
	}
}

// A client on the radio is closed, not refused — and closed before the line moves, so it is
// never left talking to a radio that has gone quiet under it. To the client that close is an
// unplug, which it already recovers from.
func TestARestartClosesTheClientBeforeTouchingTheLine(t *testing.T) {
	fake := newFakeRadio(answersPings)
	t.Cleanup(fake.stop)
	p := pipe.New(nil)
	p.Serve(fake)

	client, server := net.Pipe()
	defer client.Close()
	p.Attach(server)

	var openWhenPulled bool
	m := NewMonitor(p, nil)
	m.DeviceOpened(family.ZNP, nil, func() error {
		openWhenPulled = p.Stats().Client != ""
		fake.out <- bootAnnouncement
		return nil
	})

	if got := m.Restart(); !got.OK {
		t.Fatalf("Restart with a client attached failed: %s", got.Error)
	}
	if openWhenPulled {
		t.Error("the reset line moved while the client was still attached")
	}
	// The client's end is closed: its next read ends rather than waiting.
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the client was not closed: read returned %v", err)
	}
	if s := p.Stats(); s.Disconnects != 1 || !strings.Contains(s.LastEvent, "radio restart") {
		t.Errorf("the close was recorded as %d disconnects, last %q; want one, saying why",
			s.Disconnects, s.LastEvent)
	}
}

func TestARestartRefusesWhereNoneIsKnown(t *testing.T) {
	t.Run("no restart for the adapter", func(t *testing.T) {
		m := NewMonitor(pipe.New(nil), nil)
		m.DeviceOpened(family.ZNP, nil, nil)
		if got := m.Restart(); got.OK || !strings.Contains(got.Error, "no reset line") {
			t.Errorf("Restart = %+v, want a refusal", got)
		}
	})
	t.Run("not a ZNP radio", func(t *testing.T) {
		m, _, pulled := restartRig(t, family.EZSP, true)
		if got := m.Restart(); got.OK {
			t.Errorf("Restart on an EZSP radio = %+v, want a refusal — nothing confirms it", got)
		}
		if n := pulled.Load(); n != 0 {
			t.Errorf("the reset line was pulled %d times with no way to confirm it", n)
		}
	})
	t.Run("no adapter", func(t *testing.T) {
		m, _, pulled := restartRig(t, family.ZNP, true)
		m.NoDevice("unplugged")
		if got := m.Restart(); got.OK || !strings.Contains(got.Error, "no adapter") {
			t.Errorf("Restart with no adapter = %+v, want a refusal", got)
		}
		if n := pulled.Load(); n != 0 {
			t.Errorf("a restart from the last generation was pulled %d times", n)
		}
	})
}

// The verb end to end over the socket, and the property that keeps it safe to share one
// socket with status: reading the card never restarts anything.
func TestTheSocketRestartsOnlyWhenAsked(t *testing.T) {
	m, _, pulled := restartRig(t, family.ZNP, true)
	path := filepath.Join(socketDir(t), "status.sock")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = m.ServeStatus(ctx, path) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := ReadStatus(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing answered on the status socket")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := pulled.Load(); n != 0 {
		t.Fatalf("reading the card pulled the reset line %d times", n)
	}

	got, err := RequestRestart(path)
	if err != nil {
		t.Fatalf("RequestRestart: %v", err)
	}
	if !got.OK {
		t.Errorf("the restart over the socket failed: %s", got.Error)
	}
	if n := pulled.Load(); n != 1 {
		t.Errorf("the reset line was pulled %d times, want once", n)
	}
}
