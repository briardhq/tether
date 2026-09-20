//go:build linux

package device

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"briard.io/tether/internal/family"
)

// znp is the reference family's parameters, which is what every test here opens with: these
// tests are about the tty, not about the table.
var znp = family.Params{Radio: family.ZNP, Baud: 115200, Flow: family.FlowNone}

// newPTY returns a pty master and the path of its slave. A pty is a real tty with real termios
// — the mechanism this package manipulates — so the control-line and drain behaviour is
// exercised rather than mocked. It is not a USB serial adapter, and the two things only
// hardware can show, the actual line transitions and autosuspend, belong to the hardware tier.
func newPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlockpt: %v", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("ptsname: %v", err)
	}
	return master, fmt.Sprintf("/dev/pts/%d", n)
}

func TestOpenCarriesBytesBothWays(t *testing.T) {
	master, slave := newPTY(t)

	port, err := Open(slave, znp)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer port.Close()

	// Device to client. 0xFF is in here on purpose: it is IAC to a telnet server and occurs
	// inside real ZNP and ASH frames, so it is the byte a non-transparent path corrupts (INV 6).
	want := []byte{0x02, 0xFF, 0x00, 0xFE, 0xFF, 0xFF, 0x41}
	if _, err := master.Write(want); err != nil {
		t.Fatalf("writing to the device side: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(port, got); err != nil {
		t.Fatalf("reading from the port: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("read %#v, want %#v", got, want)
	}

	// Client to device.
	out := []byte{0xFE, 0x00, 0xFF, 0x21}
	if _, err := port.Write(out); err != nil {
		t.Fatalf("writing to the port: %v", err)
	}
	echo := make([]byte, len(out))
	if _, err := io.ReadFull(master, echo); err != nil {
		t.Fatalf("reading from the device side: %v", err)
	}
	if !bytes.Equal(echo, out) {
		t.Errorf("device side saw %#v, want %#v", echo, out)
	}
}

// INV 7's mechanism, asserted where it is observable: after Open the port's termios must have
// HUPCL clear, so that closing it leaves the control lines asserted instead of dropping them.
// A pty shares one termios between master and slave, so the master can read it back — which
// the serial library will not let us do, since it holds the slave under TIOCEXCL.
func TestOpenClearsHUPCL(t *testing.T) {
	master, slave := newPTY(t)

	// A fresh pty comes up with HUPCL clear, so set it first: without this the assertion
	// below would pass whether or not Open did anything, and an assertion that cannot fail
	// is worse than no assertion at all.
	before, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("reading termios: %v", err)
	}
	before.Cflag |= unix.HUPCL
	if err := unix.IoctlSetTermios(int(master.Fd()), unix.TCSETS, before); err != nil {
		t.Fatalf("setting HUPCL: %v", err)
	}
	if check, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS); err != nil {
		t.Fatalf("reading termios: %v", err)
	} else if check.Cflag&unix.HUPCL == 0 {
		t.Fatal("could not set HUPCL on the pty; this test could not fail")
	}

	port, err := Open(slave, znp)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer port.Close()

	after, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("reading termios: %v", err)
	}
	if after.Cflag&unix.HUPCL != 0 {
		t.Error("HUPCL still set after Open: closing the port would drop DTR and reset the radio")
	}
}

// INV 2's other half: whatever the radio said while nobody was listening must not be delivered
// as the first bytes of a client's frame stream.
func TestOpenDrainsStaleBytes(t *testing.T) {
	master, slave := newPTY(t)

	if _, err := master.Write([]byte("stale chatter from before anyone was listening")); err != nil {
		t.Fatalf("writing stale bytes: %v", err)
	}
	// Let them land in the tty buffer; otherwise the drain may run before they arrive and the
	// test would pass without proving anything.
	time.Sleep(50 * time.Millisecond)

	port, err := Open(slave, znp)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer port.Close()

	fresh := []byte("fresh")
	if _, err := master.Write(fresh); err != nil {
		t.Fatalf("writing fresh bytes: %v", err)
	}
	got := make([]byte, len(fresh))
	if _, err := io.ReadFull(port, got); err != nil {
		t.Fatalf("reading from the port: %v", err)
	}
	if !bytes.Equal(got, fresh) {
		t.Errorf("first bytes after Open were %q, want %q — the drain missed them", got, fresh)
	}
}

// A vanished device must surface as ErrGone rather than as a stall (INV 3) or as an
// indistinguishable I/O error, because the generation loop retries against this one and
// nothing else.
func TestReadReportsDisappearanceAsErrGone(t *testing.T) {
	master, slave := newPTY(t)

	port, err := Open(slave, znp)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer port.Close()

	// Closing the master is a pty's version of the adapter being unplugged.
	if err := master.Close(); err != nil {
		t.Fatalf("closing the device side: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := port.Read(make([]byte, 64))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrGone) {
			t.Errorf("read after disappearance returned %v, want ErrGone", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("read after disappearance stalled; INV 3 requires it to fail loudly")
	}
}

func TestClassifyDistinguishesDisappearanceFromEverythingElse(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		gone bool
	}{
		{"nil stays nil", nil, false},
		{"EIO is gone", unix.EIO, true},
		{"ENODEV is gone", unix.ENODEV, true},
		{"ENXIO is gone", unix.ENXIO, true},
		{"wrapped EIO is gone", fmt.Errorf("reading: %w", unix.EIO), true},
		{"EACCES is not gone", unix.EACCES, false},
		{"EINVAL is not gone", unix.EINVAL, false},
		{"a plain error is not gone", errors.New("something else"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.err, false)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("classify(nil) = %v, want nil", got)
				}
				return
			}
			if errors.Is(got, ErrGone) != tc.gone {
				t.Errorf("classify(%v) gone=%v, want %v", tc.err, !tc.gone, tc.gone)
			}
			if !errors.Is(got, tc.err) {
				t.Errorf("classify(%v) lost the underlying error", tc.err)
			}
		})
	}
}

// The six rows that need hardware flow control must actually get it. This is the assertion
// that stops the TXT record being a lie (INV 8) and stops the load-dependent failure class — a
// flow-control mismatch does not fail, it corrupts under load, so nothing else would notice.
//
// It is worth asserting precisely because of *how* it is done: the serial library hardcodes
// RTS/CTS off inside its own open, and we reach around it through a descriptor opened first and
// kept. If a future version of the library reapplies termios after that point, nothing would
// break loudly — this test is what would catch it.
func TestOpenAppliesHardwareFlowControlWhenTheFamilyNeedsIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		flow family.Flow
		want bool
	}{
		{"a family that needs it", family.FlowRTSCTS, true},
		{"a family that does not", family.FlowNone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave := newPTY(t)

			port, err := Open(slave, family.Params{Radio: family.EZSP, Baud: 115200, Flow: tc.flow})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer port.Close()

			// A pty shares one termios between master and slave, so the master reads back what
			// the tty actually ended up configured as — which the library will not show us.
			tio, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatalf("TCGETS: %v", err)
			}
			if got := tio.Cflag&unix.CRTSCTS != 0; got != tc.want {
				t.Errorf("CRTSCTS = %v, want %v — the port's parameters are not what the "+
					"family table says they are", got, tc.want)
			}
			// INV 7 must survive it: HUPCL stays clear whatever the flow control is, or a
			// close would drop the lines and reset the radio.
			if tio.Cflag&unix.HUPCL != 0 {
				t.Error("HUPCL is set; closing this port would reset the radio (INV 7)")
			}

			// And the port still carries bytes, which the ioctl could plausibly have disturbed.
			want := []byte{0xFE, 0x00, 0xFF, 0x21}
			if _, err := master.Write(want); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if _, err := io.ReadFull(port, got); err != nil {
				t.Fatalf("reading after the flow-control ioctl: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("read %#v, want %#v", got, want)
			}
		})
	}
}
