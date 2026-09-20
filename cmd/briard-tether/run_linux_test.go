//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"briard.io/tether/internal/device"
	"briard.io/tether/internal/family"
	"briard.io/tether/internal/management"
)

// The unplug-and-replug acceptance test, and it uses the real mechanisms throughout: a real
// tty, a real TCP client, the real listener and the real supervision loop. The one piece that
// is staged rather than real is the unplug — closing a pty master is what internal/device
// already uses for that, and a genuine USB removal belongs to the hardware tier.
//
// The symlink matters as much as the pty. A /dev/serial/by-id/… name is a stable link over a
// node that moves, and a replug is exactly "the same link, a different node underneath" — so
// pointing the link at a second pty is the re-resolution itself, not a stand-in for it.
func TestServeSurvivesUnplugAndReplug(t *testing.T) {
	master, slave := newPTY(t)
	link := filepath.Join(t.TempDir(), "coordinator")
	relink(t, link, slave)

	opts := Options{
		DevicePath: link,
		Listen:     freeAddr(t),
		// A pty carries no USB descriptors, so the family comes from the config override —
		// which is the same path a Windows host or an unrecognised stick takes.
		Radio:        family.ZNP,
		Instance:     fmt.Sprintf("tether-test-%d-%d", os.Getpid(), rand.Int31()),
		Advertise:    true,
		StatusSocket: filepath.Join(socketDir(t), "status.sock"),
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, opts) }()
	defer func() {
		stop()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("serve returned %v; a shutdown is not a failure", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not return when it was told to stop")
		}
	}()

	// The first generation: an ordinary working tether.
	first := waitForClient(t, opts.Listen)
	carries(t, first, master, []byte{0x02, 0xFF, 0x00, 0x41})

	// The unplug. Both halves of it: the node goes, and so does the link that named it.
	if err := master.Close(); err != nil {
		t.Fatalf("closing the device side: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatalf("removing the device link: %v", err)
	}

	// INV 3: the client is told, rather than left holding a socket that will never say
	// anything again.
	first.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := first.Read(make([]byte, 1)); err == nil {
		t.Fatal("the client connection survived the device disappearing; INV 3 requires a close")
	}

	// And while there is no device there is no listener, so a client that tries again is
	// refused outright rather than accepted into a silence. That is INV 2 read backwards, and
	// it is the difference between a client retrying and a client hanging.
	refusedWhileAbsent(t, opts.Listen)

	// The card is the other half of the promise: someone who runs `tether status` during an
	// outage must be told there is an outage.
	absent := readCard(t, opts.StatusSocket)
	if absent.DevicePresent {
		t.Error("the card claims a device is present while the adapter is unplugged")
	}
	if absent.OpenAttempts < 2 {
		t.Errorf("the card counted %d open attempts during an outage; the reopening is not being counted", absent.OpenAttempts)
	}
	if absent.DeviceErrors == 0 {
		t.Error("the card counted no device errors after the device vanished")
	}

	// The replug: a new node under the same name, which is what udev does.
	replaced, newSlave := newPTY(t)
	relink(t, link, newSlave)

	// No restart, no help: tether reopens on its own and serves the new node.
	second := waitForClient(t, opts.Listen)
	carries(t, second, replaced, []byte{0xFE, 0x00, 0xFF, 0x21})

	back := readCard(t, opts.StatusSocket)
	if !back.DevicePresent {
		t.Error("the card still says the device is absent after it came back")
	}
	// Who this tether is and where it serves, which is what tells two of them on one host
	// apart. Only knowable once there is a device, since the advertised name carries the
	// adapter — so this is also the assertion that the card learns it at all.
	if back.Instance == "" {
		t.Error("the card does not say what this tether advertises; two on one host would be " +
			"a pair of process ids and nothing a reader could recognise")
	}
	if back.Listen != opts.Listen {
		t.Errorf("the card says it serves %q, and it serves %q", back.Listen, opts.Listen)
	}
	// The counters are the same tether's, not a fresh one's: a Pipe that had been rebuilt per
	// device would have reset these, and the numbers would silently start over on every
	// unplug — which is precisely when somebody is reading them.
	if back.BytesFromClient <= absent.BytesFromClient {
		t.Errorf("bytes from client went %d → %d across the replug; the counters restarted",
			absent.BytesFromClient, back.BytesFromClient)
	}
	if back.Connects < 2 {
		t.Errorf("the card counted %d client connects across two generations", back.Connects)
	}
}

// The boot-order race, which is the other half of the same promise and the one that has no
// dramatic moment: tether starts before the adapter exists, because systemd and udev do not
// agree about who is first. It must wait rather than exit — a service that dies at boot because
// a USB device took another second stays dead until a human notices.
//
// It must also stay diagnosable while it waits. `tether status` answering "ABSENT" is a very
// different experience from a socket nobody is listening on, and the wait is exactly when
// somebody asks.
func TestServeWaitsForADeviceThatIsNotThereYet(t *testing.T) {
	link := filepath.Join(t.TempDir(), "coordinator")

	opts := Options{
		DevicePath:   link,
		Listen:       freeAddr(t),
		Radio:        family.ZNP,
		Instance:     fmt.Sprintf("tether-test-%d-%d", os.Getpid(), rand.Int31()),
		Advertise:    true,
		StatusSocket: filepath.Join(socketDir(t), "status.sock"),
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, opts) }()

	// Nothing is there, so nothing may be accepted: a client must be refused rather than
	// accepted into a silence (INV 2 and INV 3 together).
	refusedWhileAbsent(t, opts.Listen)

	waiting := readCard(t, opts.StatusSocket)
	if waiting.DevicePresent {
		t.Error("the card claims a device is present before one has ever been plugged in")
	}
	if waiting.OpenAttempts == 0 {
		t.Error("the card counted no open attempts while tether was waiting for a device")
	}

	// The adapter turns up. No restart, no prompting.
	master, slave := newPTY(t)
	relink(t, link, slave)

	client := waitForClient(t, opts.Listen)
	carries(t, client, master, []byte{0xFE, 0x00, 0x21, 0xFF})

	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve returned %v; a shutdown is not a failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("serve did not return when it was told to stop")
	}
}

// The refusal's contract, asserted without needing two dongles in the machine: it must name
// every adapter it found, by the name the operator recognises and by the path they will paste,
// and say what to do. A count alone tells them nothing they can act on.
func TestTheAmbiguityRefusalNamesEveryAdapter(t *testing.T) {
	err := ambiguous([]device.Adapter{
		{Path: "/dev/serial/by-id/usb-A-if00", Name: "Sonoff ZBDongle-P (CC2652P)"},
		{Path: "/dev/serial/by-id/usb-B-if00", Name: "ConBee II"},
		{Path: "/dev/serial/by-id/usb-C-if00", Name: "Home Assistant SkyConnect"},
	})
	for _, want := range []string{
		"usb-A-if00", "usb-B-if00", "usb-C-if00",
		"Sonoff ZBDongle-P", "ConBee II", "SkyConnect", "config",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	// Three adapters must not be told to "unplug the other". An instruction that is wrong in
	// the case it is given in is worse than no instruction.
	if strings.Contains(err.Error(), "the other,") || strings.Contains(err.Error(), "unplug the other") {
		t.Errorf("the refusal assumes there are exactly two: %v", err)
	}
}

// Zero-config against whatever is really plugged into this machine, in whichever of the three
// states it is in. The point is that the family table recognises hardware that *exists* — a
// fixture can only ever prove it recognises a fixture — so this asserts on real descriptors and
// skips when there are none to read.
//
// It covers the ambiguity for real when the host has two attached, which is the case that
// otherwise only exists as a constructed slice.
func TestDetectionAgreesWithTheHardwareOnThisHost(t *testing.T) {
	found, err := device.Detect()
	if err != nil {
		t.Skipf("no adapter detection on this host: %v", err)
	}
	if len(found) == 0 {
		t.Skip("no compatible adapter attached")
	}

	for _, a := range found {
		t.Logf("detected a %s (%s) at %s — %s at %d baud",
			a.Name, a.Device, a.Path, a.Params.Radio, a.Params.Baud)
		if a.Name == "" || a.Params.Radio == "" || a.Params.Baud == 0 {
			t.Errorf("a detected adapter carries no usable parameters: %+v", a)
		}
		// The detected path is what gets reopened after a replug, so it has to be the stable
		// one wherever udev made one.
		if !strings.HasPrefix(a.Path, "/dev/serial/by-id/") {
			t.Errorf("detected at %s, which the kernel is free to renumber", a.Path)
		}
	}

	at, err := locate(Options{})
	if len(found) == 1 {
		if err != nil {
			t.Fatalf("locate found nothing with one compatible adapter attached: %v", err)
		}
		if at.path != found[0].Path || at.params != found[0].Params {
			t.Errorf("locate chose %+v, detection found %+v", at, found[0])
		}
		return
	}

	// Two or more: refused, and the refusal names each of them.
	if err == nil {
		t.Fatalf("locate chose %s with %d compatible adapters attached", at.path, len(found))
	}
	for _, a := range found {
		if !strings.Contains(err.Error(), a.Path) {
			t.Errorf("the refusal does not name the adapter at %s: %v", a.Path, err)
		}
	}

	// And naming one settles it: detection is a default, not a policy.
	settled, err := locate(Options{DevicePath: found[0].Path})
	if err != nil {
		t.Fatalf("a configured path did not settle the ambiguity: %v", err)
	}
	if settled.path != found[0].Path || settled.params != found[0].Params {
		t.Errorf("a configured path resolved to %+v, want %+v", settled, found[0])
	}
}

// newPTY returns a pty master and the path of its slave. It is the same rig internal/device
// uses, for the same reason: a pty is a real tty, so what is being exercised here is the real
// open/read/close path rather than a stand-in for it.
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

// relink points the stable name at a node, the way udev repoints a by-id link.
func relink(t *testing.T, link, target string) {
	t.Helper()
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("linking %s to %s: %v", link, target, err)
	}
}

// freeAddr picks an address nothing is using. It has to be a fixed one rather than :0, because
// the point of the test is that a client reaches the same address again after an outage.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("picking a port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// waitForClient dials until tether is accepting, and returns the connection.
func waitForClient(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			t.Cleanup(func() { conn.Close() })
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("tether never accepted on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// refusedWhileAbsent asserts that no listener comes back while the device is missing. It waits
// for the listener to go — the device error and the close are a few microseconds apart, not
// simultaneous — and then requires it to stay gone, which it must, because nothing can open a
// path that is not there.
func refusedWhileAbsent(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("tether kept accepting clients with no device behind them")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			conn.Close()
			t.Fatal("tether started accepting again while the device was still missing")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// carries proves bytes cross in both directions, through whichever device generation is
// current. 0xFF is in the payloads on purpose: it is IAC to a telnet server and occurs inside
// real ZNP frames, so it is the byte a non-transparent path corrupts (INV 6).
func carries(t *testing.T, client net.Conn, dev *os.File, payload []byte) {
	t.Helper()

	client.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("writing from the client: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(dev, got); err != nil {
		t.Fatalf("reading at the device: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("the device saw %#v, want %#v", got, payload)
	}

	answer := append([]byte{0xFE}, payload...)
	if _, err := dev.Write(answer); err != nil {
		t.Fatalf("writing at the device: %v", err)
	}
	back := make([]byte, len(answer))
	if _, err := io.ReadFull(client, back); err != nil {
		t.Fatalf("reading at the client: %v", err)
	}
	if !bytes.Equal(back, answer) {
		t.Fatalf("the client saw %#v, want %#v", back, answer)
	}
}

// readCard asks the running tether for its status the way `tether status` does.
func readCard(t *testing.T, path string) management.Card {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		card, err := management.ReadStatus(path)
		if err == nil {
			return card
		}
		if time.Now().After(deadline) {
			t.Fatalf("the status socket never answered: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Naming the family in the config overrides the *parameters*, not the identity — which is what
// someone is doing when they reflash a stick the table knows perfectly well, since the name is
// about the hardware in their hand and the radio type is about the firmware on it.
//
// This needs a real adapter: the assertion is that descriptors get read on a path the config
// named, and a fixture can only prove that a fixture gets read. It skips when there is none,
// and it does not open the port — locate identifies, it does not connect.
func TestAFamilyOverrideStillNamesTheAdapter(t *testing.T) {
	found, err := device.Detect()
	if err != nil || len(found) == 0 {
		t.Skip("no compatible adapter attached")
	}
	real := found[0]

	// Deliberately the wrong family for most sticks, so the test cannot pass by the override
	// being ignored: the parameters must come from the override and the name from the device.
	at, err := locate(Options{DevicePath: real.Path, Radio: family.DeCONZ})
	if err != nil {
		t.Fatalf("locate refused a configured path with a configured family: %v", err)
	}
	if at.params.Radio != family.DeCONZ {
		t.Errorf("radio = %q, want the configured %q", at.params.Radio, family.DeCONZ)
	}
	// And only the radio type: the row's baud and flow control stay, because they are the
	// bridge's and not the firmware's. deconz is the family whose default baud differs from the
	// reference stick's, so an override that replaced the row would fail here.
	if at.params.Baud != real.Params.Baud || at.params.Flow != real.Params.Flow {
		t.Errorf("params = %+v, want the row's baud %d and flow %q with only the radio changed",
			at.params, real.Params.Baud, real.Params.Flow)
	}
	if at.name != real.Name {
		t.Errorf("name = %q, want %q — the override settles the parameters, not what the "+
			"adapter is", at.name, real.Name)
	}
	if got := adapterLabel(at); got != real.Name {
		t.Errorf("adapterLabel = %q, want the table's %q rather than a path", got, real.Name)
	}
	// The descriptors are kept even though the table answered, because they are what names an
	// adapter the table does not carry — and this is the branch that would reach one.
	if at.usb.Label() == "" {
		t.Errorf("locate kept no descriptors for %s; an adapter the table did not carry "+
			"would fall all the way through to its path", real.Path)
	}
}

// The same, on the branch that reads the descriptors for the parameters as well: naming only
// the path must leave the target knowing what the adapter said about itself.
func TestAPathAloneStillKeepsTheDescriptors(t *testing.T) {
	found, err := device.Detect()
	if err != nil || len(found) == 0 {
		t.Skip("no compatible adapter attached")
	}
	at, err := locate(Options{DevicePath: found[0].Path})
	if err != nil {
		t.Fatalf("locate refused a configured path: %v", err)
	}
	if at.name != found[0].Name {
		t.Errorf("name = %q, want the same %q detection gives — choosing the path by hand "+
			"must not change what the adapter is called", at.name, found[0].Name)
	}
	if at.usb.Label() == "" {
		t.Errorf("locate kept no descriptors for %s", found[0].Path)
	}
}

// fakePeer stands in for another tether running on this host: something answering on a status
// socket with a card. A test cannot be two processes, so the socket is named by hand — which is
// also the only way to reach the two-tether case at all.
func fakePeer(t *testing.T, dir string, pid int, instance, listen string) {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("status.%d.sock", pid))
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	card := management.Card{Instance: instance, Listen: listen, StartedAt: time.Now(), Now: time.Now()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = json.NewEncoder(conn).Encode(card)
			conn.Close()
		}
	}()
}

// captureLog collects what tether tells an operator, which for a refusal to advertise is the
// whole of the report — the radio keeps being served, so nothing else changes shape.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(out); log.SetFlags(flags) })
	return &buf
}

// The status socket is a file, and the only thing that removes it is its listener closing —
// so a serve that returns before its status server has shut down leaves `main` to exit with
// the unlink still pending, and a supervisor restarting tether a few hundred times fills a
// directory nothing sweeps (13 of 160 orderly stops on Windows). The contract is that when
// serve has returned, the socket is already gone: checked here on the very next line, with no
// wait that could hide a race.
func TestServeRemovesItsStatusSocketOnTheWayOut(t *testing.T) {
	master, slave := newPTY(t)
	defer master.Close()
	link := filepath.Join(t.TempDir(), "coordinator")
	relink(t, link, slave)

	opts := Options{
		DevicePath:   link,
		Listen:       freeAddr(t),
		Radio:        family.ZNP,
		Instance:     fmt.Sprintf("tether-test-%d-%d", os.Getpid(), rand.Int31()),
		StatusSocket: filepath.Join(socketDir(t), "status.sock"),
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, opts) }()

	// Up, with the socket in place — a stop before the listener existed would prove nothing.
	waitForClient(t, opts.Listen).Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(opts.StatusSocket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the status socket never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve returned %v; a shutdown is not a failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return when it was told to stop")
	}
	if _, err := os.Stat(opts.StatusSocket); !os.IsNotExist(err) {
		t.Fatalf("serve returned with its status socket still there (stat: %v)", err)
	}
}
