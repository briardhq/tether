package server

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"briard.io/tether/internal/pipe"
)

// rig starts a real listener over a fake device and returns the address plus the device's
// other end. The TCP is real, and so is the takeover logic — those are the mechanisms under
// test. The device is a net.Pipe end because what the bytes came from is irrelevant here;
// the real serial port has its own tests in internal/device.
func rig(t *testing.T) (addr string, dev net.Conn, runErr <-chan error) {
	t.Helper()
	ours, theirs := net.Pipe()

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close(); ours.Close(); theirs.Close() })

	p := pipe.New(nil)
	errc := make(chan error, 2)
	// The device is bound before the listener is used, which is the order INV 2 requires of
	// the real program too.
	served := p.Serve(ours)
	go func() { errc <- <-served }()
	go func() { errc <- Serve(ln, p) }()

	return ln.Addr().String(), theirs, errc
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dialling %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// attach dials and does not return until the connection is the pipe's current client. The pipe
// reads and drops device bytes while nothing is attached -- deliberately, so that a fresh
// client is never handed the tail of someone else's session -- so a test that wrote to the
// device before attachment completed would be racing that rule rather than testing anything.
// A byte travelling client-to-device proves the attachment, because only an attached session
// has a reader to carry it.
func attach(t *testing.T, addr string, dev net.Conn) net.Conn {
	t.Helper()
	conn := dial(t, addr)
	go conn.Write([]byte{0x00})
	mustRead(t, dev, 1)
	return conn
}

func mustRead(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	if c, ok := r.(net.Conn); ok {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		defer c.SetReadDeadline(time.Time{})
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatalf("reading %d bytes: %v", n, err)
	}
	return b
}

// INV 6: raw TCP, never telnet. 0xFF is IAC to a telnet server and occurs inside real ZNP and
// ASH frames, so a path that is not byte-transparent corrupts it. 0x00 and 0x0A are here for
// the other two classic rewrites.
var transparencyProbe = []byte{0xFE, 0x00, 0xFF, 0xFF, 0x0D, 0x0A, 0x1A, 0xFF, 0x41, 0x00}

func TestBytesCrossBothWaysUntouched(t *testing.T) {
	addr, dev, _ := rig(t)
	client := attach(t, addr, dev)

	go dev.Write(transparencyProbe)
	if got := mustRead(t, client, len(transparencyProbe)); !bytes.Equal(got, transparencyProbe) {
		t.Errorf("device to client: got %#v, want %#v", got, transparencyProbe)
	}

	go client.Write(transparencyProbe)
	if got := mustRead(t, dev, len(transparencyProbe)); !bytes.Equal(got, transparencyProbe) {
		t.Errorf("client to device: got %#v, want %#v", got, transparencyProbe)
	}
}

// INV 4: a new connection takes over and the old one is closed. The old client must actually
// be closed, not merely ignored — a client that thinks it still holds the radio is the stale
// lock that strands a moved client.
func TestSecondClientTakesOverAndFirstIsClosed(t *testing.T) {
	addr, dev, _ := rig(t)
	first := attach(t, addr, dev)

	// Prove the first client is really attached before displacing it.
	go dev.Write([]byte("one"))
	if got := mustRead(t, first, 3); string(got) != "one" {
		t.Fatalf("first client got %q, want %q", got, "one")
	}

	second := attach(t, addr, dev)

	// The first must see its connection closed.
	first.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := first.Read(make([]byte, 1)); err == nil {
		t.Error("first client's connection is still open after takeover")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("first client's connection stalled rather than closing after takeover")
	}

	// And the second must be the one now receiving.
	go dev.Write([]byte("two"))
	if got := mustRead(t, second, 3); string(got) != "two" {
		t.Errorf("second client got %q, want %q", got, "two")
	}
}

// INV 3: on device failure the client connection is closed immediately. A stall is the worst
// possible input to a client's timers; a clean close is what its recovery path expects.
func TestDeviceFailureClosesTheClientLoudly(t *testing.T) {
	addr, dev, runErr := rig(t)
	client := attach(t, addr, dev)

	go dev.Write([]byte("alive"))
	mustRead(t, client, 5)

	// The device going away.
	dev.Close()

	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Error("client connection still open after the device failed")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("client stalled after the device failed; INV 3 requires a close")
	}

	select {
	case err := <-runErr:
		if err == nil {
			t.Error("pipe.Run returned nil on device failure; the error must reach the caller")
		}
	case <-time.After(5 * time.Second):
		t.Error("pipe.Run did not return after the device failed")
	}
}

// INV 1: the client lifecycle must not disturb the device. A client leaving and another
// arriving is an ordinary event; the port stays open across it, which is what the incumbent
// bridges get wrong.
func TestClientDisconnectLeavesTheDeviceUsable(t *testing.T) {
	addr, dev, runErr := rig(t)

	first := attach(t, addr, dev)
	go dev.Write([]byte("before"))
	mustRead(t, first, 6)
	first.Close()

	// The device must not have been closed or errored by that.
	select {
	case err := <-runErr:
		t.Fatalf("the pipe stopped when a client left: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	second := attach(t, addr, dev)
	go dev.Write([]byte("after"))
	if got := mustRead(t, second, 5); string(got) != "after" {
		t.Errorf("after reconnect got %q, want %q", got, "after")
	}
}
