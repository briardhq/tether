// Package device opens the coordinator's serial port and holds it for the lifetime of the
// process. It owns every platform-specific line in the program.
//
// The port is opened once and never reopened as a side effect of anything the network does:
// that is INV 1, and a bridge that does otherwise resets the radio under its own client.
package device

import (
	"errors"
	"fmt"
	"log"
	"sync/atomic"

	"go.bug.st/serial"

	"briard.io/tether/internal/family"
)

// ErrGone reports that the device stopped being there — unplugged, or the USB layer dropped
// it. It is distinguished from any other I/O error because the two want opposite handling:
// this one is a fact about the world that tether retries against, and anything else is a bug
// in us or a fault in the host. Both still close the client connection immediately (INV 3).
var ErrGone = errors.New("device is gone")

// Adapter is a coordinator found by looking rather than by being told where it is. It carries
// the path to open it at, what the USB layer said it was, and what the family table made of
// that — the same three things a configured path produces, so that a detected device and a
// named one are the same thing downstream.
type Adapter struct {
	// Path is a /dev/serial/by-id/… name wherever the device has one. That matters more here
	// than anywhere else: a detected path is reopened after a replug, and the kernel's
	// ttyUSB number is exactly what a replug is allowed to change.
	Path   string
	Device family.Device
	Params family.Params
	// Name is the table's human name for it, for logs and for the error that asks an operator
	// to choose between two.
	Name string
}

// Port is an open coordinator. Read and Write are byte-transparent (INV 6): nothing here
// inspects, rewrites, or reframes what passes through.
type Port struct {
	port serial.Port
	path string
	// control is the descriptor opened before the library's and held for the life of the
	// port, because it is the only way to reach termios once the library has TIOCEXCL on it.
	control int
	// closed records that we asked for the close, so a PortClosed coming back from a
	// blocked read is read as shutdown rather than as the dongle vanishing.
	closed atomic.Bool
}

// Open opens the coordinator at path and readies it for the pipe: family parameters applied,
// stale bytes drained, USB autosuspend disabled. It returns only once the port is usable, so
// that the listener can be started after it and never race port setup (INV 2).
//
// params are the family table's row for this adapter. Data bits, parity and stop bits are 8N1
// for every family in that table, so they are not parameters.
//
// Hardware RTS/CTS is applied for the families that need it, which the serial library cannot do
// — so it is done around the library rather than through it, via a descriptor opened before it
// and held for the life of the port. See openControl.
func Open(path string, params family.Params) (*Port, error) {
	// INV 7. The Linux tty layer raises DTR/RTS inside open(), and nothing can prevent that.
	// What we can prevent is it happening more than once: with HUPCL clear the lines stay
	// high on close, so every later open sets an already-set bit and puts no edge on the
	// wire. Must happen before the library opens — it exposes no Fd() and sets TIOCEXCL, so
	// a descriptor obtained afterwards is not available.
	control, err := openControl(path)
	if err != nil {
		return nil, fmt.Errorf("pinning the control lines on %s: %w", path, err)
	}

	port, err := serial.Open(path, &serial.Mode{
		BaudRate: params.Baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
		// nil is the one value that makes the library touch no modem bit at all. Asking for
		// DTR=false, RTS=false would be worse, not better: it lowers them a few milliseconds
		// after the kernel raised them, which is a second edge rather than none.
		InitialStatusBits: nil,
	})
	if err != nil {
		closeControl(control)
		return nil, fmt.Errorf("opening %s: %w", path, classify(err, false))
	}
	p := &Port{port: port, path: path, control: control}

	// Before the drain, so that the first bytes we take off the wire are taken under the same
	// flow control every later byte will be — INV 8 is a claim about what tether did, and a
	// drain done under the wrong terms would already have made it untrue.
	if err := applyFlowControl(control, params.Flow == family.FlowRTSCTS); err != nil {
		p.Close()
		return nil, fmt.Errorf("setting flow control on %s: %w", path, err)
	}

	// Whatever the radio said while nobody was listening is not part of any client's frame
	// stream. Draining before the listener exists is the other half of INV 2.
	if err := port.ResetInputBuffer(); err != nil {
		p.Close()
		return nil, fmt.Errorf("draining %s: %w", path, classify(err, false))
	}

	// The load-dependent failure class: autosuspend bites under load or on a timer, never at
	// open, so it is invisible to anyone inspecting a working setup. Best-effort by necessity
	// — it needs write access to sysfs — but never silently: a failure here is a fault we
	// would otherwise be blamed for hours later, so it says what to do about it.
	disableAutosuspend(path)

	// After the open, not before it: tether retries a failed open every few seconds while a
	// dongle is unplugged, and advice repeated at that rate is noise rather than advice.
	// Said once per port actually opened, it is one line per plug event.
	adviseUnstablePath(path)

	log.Printf("device: %s open at %d baud", path, params.Baud)
	return p, nil
}

// Read reads from the coordinator. A disappearance is reported as ErrGone.
func (p *Port) Read(b []byte) (int, error) {
	n, err := p.port.Read(b)
	if err != nil {
		return n, classify(err, p.closed.Load())
	}
	return n, nil
}

// Write writes to the coordinator. A disappearance is reported as ErrGone.
func (p *Port) Write(b []byte) (int, error) {
	n, err := p.port.Write(b)
	if err != nil {
		return n, classify(err, p.closed.Load())
	}
	return n, nil
}

// Close releases the port. It does not lower the control lines: HUPCL was cleared at open
// precisely so that this cannot reset the radio (INV 7), which is what makes a tether
// restart free — and restarts are routine, because an agent supervising it self-updates.
func (p *Port) Close() error {
	p.closed.Store(true)
	err := p.port.Close()
	closeControl(p.control)
	return err
}

// classify maps a driver or library error onto ErrGone where it means the device left.
// Everything else passes through unchanged — guessing wrongly in either direction costs more
// than the distinction is worth.
//
// selfClosed says whether we asked for the close. It has to be a parameter because the
// library reports both "the caller closed this port" and "the device was unplugged mid-read"
// as the same PortClosed code, and only we know which happened. Without it, an orderly
// shutdown would log as a dongle failure.
//
// The library's PortError does not implement Unwrap, so a wrapped errno is unreachable
// through it by errors.Is; its code is the only thing to read.
func classify(err error, selfClosed bool) error {
	if err == nil {
		return nil
	}
	var pe *serial.PortError
	if errors.As(err, &pe) {
		switch pe.Code() {
		case serial.PortClosed:
			if selfClosed {
				return err
			}
			// The library turns a disconnected read into this: on Linux a removed device
			// leaves the port "readable with zero-length data" and it maps that to
			// PortClosed. Since we did not close it, the device is gone.
			return fmt.Errorf("%w: %w", ErrGone, err)
		case serial.PortNotFound:
			return fmt.Errorf("%w: %w", ErrGone, err)
		}
		return err
	}
	for _, gone := range goneErrors {
		if errors.Is(err, gone) {
			return fmt.Errorf("%w: %w", ErrGone, err)
		}
	}
	return err
}
