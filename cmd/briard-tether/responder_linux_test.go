//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/brutella/dnssd"
	"golang.org/x/sys/unix"

	"briard.io/tether/internal/family"
	"briard.io/tether/internal/management"
)

// The scripted responder, and the whole of tier 1's Zigbee knowledge, which is none
// (ARCHITECTURE.md "Testing"). It sits on the far end of the pty where a coordinator would be,
// matches request bytes against a script, and says what the script says. What it buys over the
// raw pty in run_linux_test.go is the question tether exists to answer: not "do bytes cross"
// but "does the reply reach the client that asked for it, whole and in order".
//
// Matching is against an accumulating buffer rather than per read, because a stream has no
// frames: two client writes can arrive as one read, one write can arrive as two, and a
// responder that assumed otherwise would fail intermittently for reasons having nothing to do
// with tether. Bytes that match nothing are skipped rather than blocking what follows, which
// is what a real coordinator does with a frame it does not recognise, and what keeps the
// script from deadlocking on traffic the test did not write — tether's own liveness probe
// shares the UART and is entitled to speak on it.
type rule struct {
	request []byte

	// reply is what the coordinator says when it sees request. Empty is the "say nothing"
	// case — the request is consumed and the radio stays silent, which is what a coordinator
	// that is busy or wedged looks like from outside, and what lets a test place a late reply
	// deliberately rather than race one.
	reply []byte
}

// responder answers the script until the pty goes away.
type responder struct {
	t      *testing.T
	master *os.File
	rules  []rule

	// mu serialises writes to the master: the script's replies come from the read loop and
	// say's come from the test goroutine, and a UART has one writer or it has none.
	mu sync.Mutex
}

// newResponder starts a coordinator on master and returns it. It stops when master closes,
// which is how every test here ends — either at cleanup or as the staged unplug.
func newResponder(t *testing.T, master *os.File, rules ...rule) *responder {
	t.Helper()
	r := &responder{t: t, master: master, rules: rules}
	go r.run()
	return r
}

func (r *responder) run() {
	var pending []byte
	buf := make([]byte, 512)
	for {
		n, err := r.master.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			pending = r.answer(pending)
		}
		if err != nil {
			// The pty is gone: the test is over, or the unplug it staged has happened.
			return
		}
	}
}

// answer consumes every rule the buffer now satisfies, earliest request first so that replies
// leave in the order the questions arrived, and returns what is left over — which is either
// nothing the script knows about or the front half of a request still being written.
func (r *responder) answer(pending []byte) []byte {
	for {
		at, matched := -1, rule{}
		for _, rl := range r.rules {
			i := bytes.Index(pending, rl.request)
			if i >= 0 && (at < 0 || i < at) {
				at, matched = i, rl
			}
		}
		if at < 0 {
			return pending
		}
		pending = pending[at+len(matched.request):]
		if len(matched.reply) > 0 {
			r.say(matched.reply)
		}
	}
}

// say makes the radio speak unprompted — a reply arriving late, or traffic a coordinator emits
// on its own. A failure to write is not reported: by the time a test closes the pty the
// responder may still be mid-answer, and that is the test ending rather than anything to say.
func (r *responder) say(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.master.Write(b)
}

// A client's replies come back whole and in the order it asked for them. This is INV 6 read
// forwards: byte transparency is not only "every byte survives" but "every byte arrives where
// it belongs, once". 0xFF is in every payload on purpose — it is telnet's IAC and it occurs
// inside real ZNP frames, so it is the byte a path that thinks it understands the protocol
// corrupts.
//
// The long reply is not padding. The pipe reads the device in 4 KB mouthfuls (internal/pipe),
// so a reply that crosses that seam is the only one whose wholeness is in question at all; the
// short ones would pass against a pipe that truncated every read.
func TestRepliesComeBackWholeAndInOrder(t *testing.T) {
	master, slave := newPTY(t)
	long := bytes.Repeat([]byte{0xFE, 0xFF, 0x07}, 4000) // 12 KB, three pipe buffers

	newResponder(t, master,
		rule{request: []byte{0x01, 0xFF}, reply: []byte("FIRST")},
		rule{request: []byte{0x02, 0xFF}, reply: []byte("SECOND")},
		rule{request: []byte{0x03, 0xFF}, reply: long},
	)

	opts := servingOptions(t, slave)
	stopServing(t, opts)

	client := waitForClient(t, opts.Listen)
	client.SetDeadline(time.Now().Add(20 * time.Second))

	// Asked for in one write, so that nothing about the client's pacing is what puts the
	// replies in order. A radio that answered out of order, or a pipe that interleaved, would
	// be caught here and nowhere else.
	if _, err := client.Write([]byte{0x01, 0xFF, 0x02, 0xFF, 0x03, 0xFF}); err != nil {
		t.Fatalf("writing the requests: %v", err)
	}

	want := append([]byte("FIRSTSECOND"), long...)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("reading the replies: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the client read %d bytes that differ from the %d it was owed; first "+
			"difference at %d", len(got), len(want), firstDifference(got, want))
	}
}

// A reply the radio owed a client that has gone is dropped, not saved for whoever connects
// next. It is the promise internal/pipe's package comment makes — a client can never be handed
// bytes the radio emitted before it connected — and the reason it matters is framing: a ZNP
// client that receives the tail of somebody else's exchange as its own first bytes does not
// resynchronise gracefully, it reports a broken coordinator.
//
// Staged rather than raced. The script is silent for the second request, so the coordinator
// genuinely owes a reply when the client walks away, and the card is what says the radio spoke
// into an empty room — without that the assertion could pass by the late bytes simply not
// having arrived yet.
func TestAReplyOwedToADepartedClientIsDroppedNotQueued(t *testing.T) {
	master, slave := newPTY(t)
	late := []byte{0xFE, 0xFF, 0x4C, 0x41, 0x54, 0x45}

	responder := newResponder(t, master,
		rule{request: []byte{0x01, 0xFF}, reply: []byte("PONG")},
		rule{request: []byte{0x09, 0xFF}}, // the coordinator is thinking, and says nothing
	)

	opts := servingOptions(t, slave)
	stopServing(t, opts)

	first := waitForClient(t, opts.Listen)
	first.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := first.Write([]byte{0x01, 0xFF}); err != nil {
		t.Fatalf("writing the first request: %v", err)
	}
	if _, err := io.ReadFull(first, make([]byte, len("PONG"))); err != nil {
		t.Fatalf("reading the first reply: %v", err)
	}

	// The question that will never be answered to its asker.
	if _, err := first.Write([]byte{0x09, 0xFF}); err != nil {
		t.Fatalf("writing the unanswered request: %v", err)
	}
	first.Close()
	departed := waitForNoClient(t, opts.StatusSocket)

	// The radio answers the question the departed client asked.
	responder.say(late)
	waitForDeviceByteAfter(t, opts.StatusSocket, departed)

	// The successor must see its own reply and nothing before it. Reading exactly as many
	// bytes as PONG is what makes this able to fail: had the late answer been kept, these
	// would be its first four.
	second := waitForClient(t, opts.Listen)
	second.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := second.Write([]byte{0x01, 0xFF}); err != nil {
		t.Fatalf("writing the successor's request: %v", err)
	}
	got := make([]byte, len("PONG"))
	if _, err := io.ReadFull(second, got); err != nil {
		t.Fatalf("reading the successor's reply: %v", err)
	}
	if !bytes.Equal(got, []byte("PONG")) {
		t.Errorf("the successor read %q as its first bytes; it was handed the answer to a "+
			"question the previous client asked", got)
	}
}

// INV 4 and INV 1 in one event, which is the event the stale-lock failure class is about: a
// client that still holds the socket is displaced by the machine actually trying to work, and
// the radio does not notice. The first half is proved at server level already; the second
// cannot be, because that test's device is a net.Pipe and INV 1 is a claim about a *port*.
//
// What makes the port's continuity observable is the pty itself: closing the slave gives the
// master an immediate EIO, which would end the responder's read loop for good. So a coordinator
// that still answers after the takeover is a port that was never closed — not merely a pipe
// that kept its plumbing. The card's reopen count is the corroboration, and the line parameters
// are the third thing a reset would have to move.
//
// What this tier cannot prove is DTR and RTS, which is where an incumbent actually resets the
// radio: a pty implements no TIOCMGET at all (measured — ENOTTY on both ends). That
// measurement is INV 7's and it belongs to real hardware.
func TestATakeoverLeavesTheRadioUntouched(t *testing.T) {
	master, slave := newPTY(t)
	newResponder(t, master, rule{request: []byte{0x01, 0xFF}, reply: []byte("PONG")})

	opts := servingOptions(t, slave)
	stopServing(t, opts)

	first := waitForClient(t, opts.Listen)
	ask(t, first, []byte{0x01, 0xFF}, []byte("PONG"))

	before := readCard(t, opts.StatusSocket)
	lines := lineSettings(t, master)

	// The takeover. Nothing tells the first client this is coming, which is the point.
	second := waitForClient(t, opts.Listen)

	// INV 4: the displaced client is closed, not left holding a socket it believes is live.
	// A stall is the worse failure of the two — it is the stale lock that never resolves.
	first.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := first.Read(make([]byte, 1)); err == nil {
		t.Error("the displaced client's connection is still open after the takeover")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("the displaced client stalled rather than being closed; that is the stale " +
			"lock that strands a moved client, not a takeover")
	}

	// INV 1: the same open port, answering the new client.
	ask(t, second, []byte{0x01, 0xFF}, []byte("PONG"))

	after := readCard(t, opts.StatusSocket)
	if after.Takeovers != before.Takeovers+1 {
		t.Errorf("the card counted %d takeovers, was %d: a client displacing another is the "+
			"one event an operator needs named", after.Takeovers, before.Takeovers)
	}
	if after.Disconnects != before.Disconnects {
		t.Errorf("the card counted the displaced client as a disconnect (%d → %d); the count "+
			"can then no longer tell a client that keeps dropping from clients replacing it",
			before.Disconnects, after.Disconnects)
	}
	assertRadioUndisturbed(t, before, after, lines, lineSettings(t, master), "a takeover")
}

// INV 1 as a stranger meets it: restarting Home Assistant must not cost them their Zigbee
// network. A client leaving and another arriving is the most ordinary event there is, and the
// control lines are where an incumbent bridge resets the radio on exactly this one.
func TestAClientRestartLeavesTheRadioOpen(t *testing.T) {
	master, slave := newPTY(t)
	newResponder(t, master, rule{request: []byte{0x01, 0xFF}, reply: []byte("PONG")})

	opts := servingOptions(t, slave)
	stopServing(t, opts)

	first := waitForClient(t, opts.Listen)
	ask(t, first, []byte{0x01, 0xFF}, []byte("PONG"))

	before := readCard(t, opts.StatusSocket)
	lines := lineSettings(t, master)

	// The restart: gone, and properly gone before anything else happens.
	first.Close()
	waitForNoClient(t, opts.StatusSocket)

	second := waitForClient(t, opts.Listen)
	ask(t, second, []byte{0x01, 0xFF}, []byte("PONG"))

	after := readCard(t, opts.StatusSocket)
	if after.Disconnects != before.Disconnects+1 {
		t.Errorf("the card counted %d disconnects, was %d", after.Disconnects, before.Disconnects)
	}
	if after.Takeovers != before.Takeovers {
		t.Errorf("a client that connected after the previous one left was counted as a "+
			"takeover (%d → %d)", before.Takeovers, after.Takeovers)
	}
	assertRadioUndisturbed(t, before, after, lines, lineSettings(t, master), "a client restart")
}

// The zero-config path, walked in the order a client walks it: browse the wire, read the
// record, dial what the record says, talk to the radio. "Auto-discovered" is in this repo's
// definition of done and this is the only test that asks for the whole of it.
//
// It is not internal/discovery's wire test repeated. That one proves the advert reaches the
// multicast group and is withdrawn, over a Config a test handed it. What no test asked before
// is whether **serve** fills that Config in with the truth: a port that is the listener a
// client can reach, and a radio_type that is the family tether actually opened. Either could
// be wrong in a way every existing test passes, and the symptom would be a discovery card that
// appears and then fails to connect — the worst of the failure shapes, because it looks like
// the client's fault.
//
// The service type is written out rather than imported from internal/discovery, on purpose:
// `_zigbee-coordinator._tcp.local.` is the string in ZHA's own manifest, and a test that
// borrowed our constant would follow us if we ever changed it, which is exactly the change
// that would strand every client.
func TestTheAdvertLeadsAClientToTheRadio(t *testing.T) {
	requireMulticast(t)

	master, slave := newPTY(t)
	newResponder(t, master, rule{request: []byte{0x01, 0xFF}, reply: []byte("PONG")})

	opts := servingOptions(t, slave)
	opts.Advertise = true
	opts.Instance = fmt.Sprintf("tether-test-%d-%04x", os.Getpid(), rand.Intn(1<<16))
	// All interfaces, which is what defaultListen does and what the advert has to be telling
	// the truth about: a record carrying this host's LAN address is a lie if tether is bound
	// to loopback.
	_, port, err := net.SplitHostPort(freeAddr(t))
	if err != nil {
		t.Fatalf("splitting the address: %v", err)
	}
	opts.Listen = ":" + port
	stopServing(t, opts)

	entry, ok := browseForTether(opts.Instance, 30*time.Second)
	if !ok {
		t.Fatal("no advert for this tether reached the wire; a client would never find it")
	}

	// The record says what tether opened, not what a constant says. ZHA fails at startup on a
	// wrong radio_type and zigbee2mqtt does too, so this key being a guess is a client that
	// never starts.
	if got := entry.Text["radio_type"]; got != string(family.ZNP) {
		t.Errorf("the advert says radio_type %q for a port tether opened as %q", got, family.ZNP)
	}
	// serial_number is ZHA's unique_id: the identity the card is keyed on has to be this
	// tether's, not a second one invented at advertising time.
	if got := entry.Text["serial_number"]; got != opts.Instance {
		t.Errorf("the advert's serial_number is %q, and this tether is %q", got, opts.Instance)
	}

	// And now the part only an end-to-end test can ask: the record leads somewhere. Every
	// address the browse returned is tried, because that is what a client does with a record
	// carrying several.
	conn := dialAdvertised(t, entry)
	ask(t, conn, []byte{0x01, 0xFF}, []byte("PONG"))
}

// requireMulticast decides whether a test may put a real advert on a real multicast group, and
// the answer is no unless somebody asked for it.
//
// ⚠️ **A bind check on its own is the wrong gate, and backwards.** "Can this process bind
// 224.0.0.251:5353" skips in a sandbox, which has nothing to pollute, and runs at full volume
// on a developer's home network, which has their real Home Assistant on it.
// `TestTheAdvertLeadsAClientToTheRadio` publishes the service type out of ZHA's own manifest, so
// that is a discovery card on somebody's actual Home Assistant, every `go test`.
//
// (internal/discovery carries the same guard; sharing it would mean test-only API on a shipped
// package.) So it is opt-in, and `scripts/wire-tests.sh` is what opts in: it builds a network
// namespace with a veth pair in it and runs these there, where the segment is real and nobody
// else is on it. The bind check stays underneath as the second gate — an environment that
// cannot carry multicast still says so rather than failing obscurely.
func requireMulticast(t *testing.T) {
	t.Helper()
	if os.Getenv("TETHER_WIRE_TESTS") == "" {
		t.Skip("not putting an advert on this machine's network: these tests announce a real " +
			"_zigbee-coordinator._tcp, which a Home Assistant on the same LAN will offer as a " +
			"discovery card. `scripts/wire-tests.sh` runs them in a namespace of their own; set " +
			"TETHER_WIRE_TESTS=1 to run them wherever you are.")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353})
	if err != nil {
		t.Skipf("no mDNS multicast in this environment: %v", err)
	}
	conn.Close()
}

// browseForTether asks the question a client asks — a PTR query for the service type — until
// the named instance answers with a complete record. Retrying is not papering over flakiness:
// RFC 6762 wants up to a second of probing before a responder may answer at all, and a client
// arriving during it would retry too.
func browseForTether(instance string, within time.Duration) (dnssd.BrowseEntry, bool) {
	deadline := time.Now().Add(within)
	for {
		found := make(chan dnssd.BrowseEntry, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = dnssd.LookupType(ctx, "_zigbee-coordinator._tcp.local.",
			func(e dnssd.BrowseEntry) {
				if e.Name == instance && len(e.Text) > 0 {
					select {
					case found <- e:
					default:
					}
				}
			},
			func(dnssd.BrowseEntry) {})
		cancel()
		select {
		case e := <-found:
			return e, true
		default:
		}
		if time.Now().After(deadline) {
			return dnssd.BrowseEntry{}, false
		}
	}
}

// dialAdvertised connects to what the record says, trying each address it carries. A link-local
// IPv6 address needs a zone this test has no business inventing, so failing one and moving on
// is a client's own behaviour rather than leniency.
func dialAdvertised(t *testing.T, entry dnssd.BrowseEntry) net.Conn {
	t.Helper()
	if len(entry.IPs) == 0 {
		t.Fatal("the advert carries no address; a client has nothing to dial")
	}
	var last error
	for _, ip := range entry.IPs {
		addr := net.JoinHostPort(ip.String(), strconv.Itoa(entry.Port))
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			last = err
			continue
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	t.Fatalf("nothing tether advertised could be reached on port %d (%v); the record points "+
		"somewhere a client cannot go, which is a discovery card that appears and then fails",
		entry.Port, last)
	return nil
}

// The one way kick-old goes wrong, staged end to end: two clients on different hosts, each
// displacing the other. tether cannot stop it — it has no way to know which of two clients
// deserves the radio, and guessing would be worse — so the whole of its answer is to make the
// fight legible to whoever can stop it.
//
// The second host is real, not a fiction: all of 127.0.0.0/8 is loopback, so a client dialling
// from 127.0.0.2 is a genuinely different host as far as the accept side is concerned. That
// matters, because the thing under test is precisely the difference between two hosts and one
// host reconnecting on new ports, and a test that faked it would prove the wrong half.
func TestTwoClientsFightingAreNamedOnTheCard(t *testing.T) {
	master, slave := newPTY(t)
	newResponder(t, master, rule{request: []byte{0x01, 0xFF}, reply: []byte("PONG")})

	opts := servingOptions(t, slave)
	stopServing(t, opts)

	// Each one asks a question before the next arrives, which is how the test knows the
	// takeover really happened rather than racing the accept loop.
	first := waitForClient(t, opts.Listen)
	ask(t, first, []byte{0x01, 0xFF}, []byte("PONG"))

	other := dialFrom(t, "127.0.0.2", opts.Listen)
	ask(t, other, []byte{0x01, 0xFF}, []byte("PONG"))

	again := waitForClient(t, opts.Listen)
	ask(t, again, []byte{0x01, 0xFF}, []byte("PONG"))

	card := readCard(t, opts.StatusSocket)
	if len(card.Contenders) != 2 {
		t.Fatalf("the card names %v as contending; two hosts displacing each other is the "+
			"fault this exists to report", card.Contenders)
	}
	for _, want := range []string{"127.0.0.1", "127.0.0.2"} {
		if !slices.Contains(card.Contenders, want) {
			t.Errorf("the card does not name %s among %v, so it does not say which machines "+
				"to go and look at", want, card.Contenders)
		}
	}
	if card.RecentTakeovers < 2 {
		t.Errorf("the card counted %d takeovers in the last minute, and there were two",
			card.RecentTakeovers)
	}
	// The cumulative count keeps its old meaning; the new fields are about *now*.
	if card.Takeovers != 2 {
		t.Errorf("the card counted %d takeovers in total, want 2", card.Takeovers)
	}
}

// A single client reconnecting must not be reported as a fight, and it is the case that would
// be got wrong by counting addresses: every reconnect arrives on a fresh ephemeral port, so an
// address-counting version of this passes its own unit tests and then cries wolf at every
// operator whose client restarts twice.
func TestOneClientReconnectingIsNotReportedAsAFight(t *testing.T) {
	master, slave := newPTY(t)
	newResponder(t, master, rule{request: []byte{0x01, 0xFF}, reply: []byte("PONG")})

	opts := servingOptions(t, slave)
	stopServing(t, opts)

	for i := 0; i < 3; i++ {
		client := waitForClient(t, opts.Listen)
		ask(t, client, []byte{0x01, 0xFF}, []byte("PONG"))
	}

	card := readCard(t, opts.StatusSocket)
	if len(card.Contenders) != 0 {
		t.Errorf("one client reconnecting three times was reported as contention between %v",
			card.Contenders)
	}
	if card.RecentTakeovers != 2 {
		t.Errorf("the card counted %d recent takeovers across three connections, want 2",
			card.RecentTakeovers)
	}
}

// dialFrom connects from a chosen local address, so that a test can be two hosts.
func dialFrom(t *testing.T, host, addr string) net.Conn {
	t.Helper()
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(host)}}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dialing %s from %s: %v", addr, host, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// assertRadioUndisturbed is INV 1's evidence, and it is the same evidence for every client
// lifecycle event: the port was not reopened, nothing failed, and the line parameters did not
// move. The coordinator having answered afterwards is the fourth piece and the caller's job,
// since only the caller knows what it asked.
func assertRadioUndisturbed(t *testing.T, before, after management.Card, lines, now unix.Termios, event string) {
	t.Helper()
	if after.OpenAttempts != before.OpenAttempts {
		t.Errorf("the port was reopened across %s (%d → %d open attempts); INV 1 says the "+
			"client lifecycle and the radio are independent", event,
			before.OpenAttempts, after.OpenAttempts)
	}
	if after.DeviceErrors != before.DeviceErrors {
		t.Errorf("%s produced a device error (%d → %d)", event,
			before.DeviceErrors, after.DeviceErrors)
	}
	if now != lines {
		t.Errorf("the line parameters moved across %s: cflag %#x → %#x, iflag %#x → %#x. "+
			"Whatever did that would reset a real radio", event,
			lines.Cflag, now.Cflag, lines.Iflag, now.Iflag)
	}
}

// ask puts one scripted question to the coordinator and requires the answer. It is not
// run_linux_test.go's carries, which writes the device end itself: here the responder owns the
// master, and a second reader on it would take bytes the coordinator needs.
func ask(t *testing.T, client net.Conn, request, want []byte) {
	t.Helper()
	client.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := client.Write(request); err != nil {
		t.Fatalf("writing %#v: %v", request, err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("reading the answer to %#v: %v; the coordinator stopped answering, which is "+
			"what a closed and reopened port looks like from here", request, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the answer to %#v was %q, want %q", request, got, want)
	}
}

// lineSettings reads the pty pair's termios from the master, which is where internal/device's
// own tests read it too: what Open sets on the slave is visible here.
func lineSettings(t *testing.T, master *os.File) unix.Termios {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("reading the line settings: %v", err)
	}
	return *tio
}

// servingOptions points tether at a pty and starts it. The family comes from the override
// because a pty carries no USB descriptors — the same path a Windows host or an unrecognised
// stick takes — and Advertise is off because these tests assert on the data path and an mDNS
// advert on a developer's LAN is not theirs to put there. `scripts/wire-tests.sh` is where a
// real advert belongs.
func servingOptions(t *testing.T, devicePath string) Options {
	t.Helper()
	return Options{
		DevicePath:   devicePath,
		Listen:       freeAddr(t),
		Radio:        family.ZNP,
		StatusSocket: filepath.Join(socketDir(t), "status.sock"),
	}
}

// stopServing runs serve for the duration of the test and requires it to stop cleanly.
func stopServing(t *testing.T, opts Options) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, opts) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("serve returned %v; a shutdown is not a failure", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not return when it was told to stop")
		}
	})
}

// waitForNoClient waits until the card says nobody is attached, and returns the moment it said
// so. A close on the client's side and the pipe noticing are not the same instant.
func waitForNoClient(t *testing.T, socket string) time.Time {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		card := readCard(t, socket)
		if card.Client == "" {
			return card.Now
		}
		if time.Now().After(deadline) {
			t.Fatalf("the card still says %s is attached after it disconnected", card.Client)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForDeviceByteAfter waits until the card shows the radio has spoken since then.
func waitForDeviceByteAfter(t *testing.T, socket string, then time.Time) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		card := readCard(t, socket)
		if card.LastDeviceByteAt.After(then) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the radio's late reply never reached tether, so the test proved nothing")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// firstDifference reports where two byte slices part company, for a failure message that says
// something about 12 KB of payload rather than printing it.
func firstDifference(got, want []byte) int {
	for i := range got {
		if i >= len(want) || got[i] != want[i] {
			return i
		}
	}
	return len(got)
}
