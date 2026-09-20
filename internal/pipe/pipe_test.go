package pipe

import (
	"io"
	"net"
	"slices"
	"testing"
	"time"
)

// blockingDev is a device whose Write does not return until the test lets it, and which says
// when a write has entered. That is what makes the ordering question below decidable rather
// than a race to observe. A real UART blocks under load for the same reason.
type blockingDev struct {
	entered chan struct{} // signalled as a write enters, before it blocks
	writes  chan []byte   // receiving from this releases the blocked write
	reads   chan []byte
}

func newBlockingDev() *blockingDev {
	return &blockingDev{
		entered: make(chan struct{}, 1),
		writes:  make(chan []byte),
		reads:   make(chan []byte),
	}
}

func (d *blockingDev) Write(b []byte) (int, error) {
	select {
	case d.entered <- struct{}{}:
	default:
	}
	d.writes <- append([]byte(nil), b...)
	return len(b), nil
}

func (d *blockingDev) Read(b []byte) (int, error) {
	chunk, ok := <-d.reads
	if !ok {
		return 0, io.EOF
	}
	return copy(b, chunk), nil
}

// Contention is the price of kick-old (INV 4) and the only way it goes wrong: two clients
// pointed at one coordinator, each displacing the other forever, neither ever settling. The
// decision is which of three stories a run of takeovers is telling, so it is tested as the
// three stories rather than as a function.
func TestContentionTellsAFightFromAClientThatKeepsComingBack(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	for _, tc := range []struct {
		name       string
		events     []takeover
		wantRecent int
		wantHosts  []string
	}{
		{
			name: "one client reconnecting is not a fight, however often it does it",
			// The case that makes hosts the unit rather than addresses: every reconnect
			// arrives on a fresh ephemeral port, so counting addresses would call this a
			// crowd and send somebody looking for a second machine that does not exist.
			events: []takeover{
				{host: "10.0.0.5", at: ago(30 * time.Second)},
				{host: "10.0.0.5", at: ago(20 * time.Second)},
				{host: "10.0.0.5", at: ago(10 * time.Second)},
			},
			wantRecent: 3,
			wantHosts:  []string{"10.0.0.5"},
		},
		{
			name: "two hosts inside the window is the fight",
			events: []takeover{
				{host: "10.0.0.5", at: ago(30 * time.Second)},
				{host: "10.0.0.9", at: ago(20 * time.Second)},
				{host: "10.0.0.5", at: ago(10 * time.Second)},
			},
			wantRecent: 3,
			wantHosts:  []string{"10.0.0.5", "10.0.0.9"},
		},
		{
			name: "a handover is not a fight once the old host has aged out",
			// A client that genuinely moved machines: two hosts in the history, but only one
			// of them in the last minute. Reporting this as contention would send somebody
			// hunting a second client that stopped an hour ago.
			events: []takeover{
				{host: "10.0.0.5", at: ago(2 * time.Hour)},
				{host: "10.0.0.9", at: ago(5 * time.Second)},
			},
			wantRecent: 1,
			wantHosts:  []string{"10.0.0.9"},
		},
		{
			name:       "nothing at all",
			events:     nil,
			wantRecent: 0,
			wantHosts:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recent, hosts := contention(tc.events, now)
			if len(recent) != tc.wantRecent {
				t.Errorf("counted %d takeovers in the window, want %d", len(recent), tc.wantRecent)
			}
			if !slices.Equal(hosts, tc.wantHosts) {
				t.Errorf("named hosts %v, want %v", hosts, tc.wantHosts)
			}
		})
	}
}

// The window has to actually drop things, or a tether that lives for months accumulates every
// takeover it ever saw — a slow leak dressed as a counter.
func TestContentionForgetsWhatIsOutsideTheWindow(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	events := []takeover{
		{host: "10.0.0.5", at: now.Add(-contentionWindow - time.Second)},
		{host: "10.0.0.9", at: now.Add(-contentionWindow + time.Second)},
	}
	recent, hosts := contention(events, now)
	if len(recent) != 1 {
		t.Errorf("kept %d takeovers, want only the one inside the window", len(recent))
	}
	if !slices.Equal(hosts, []string{"10.0.0.9"}) {
		t.Errorf("named %v, want only the host inside the window", hosts)
	}
}

// INV 4, stated as the property that actually enforces it: Attach must not return — and so
// the new client's reader must not start — while the previous client's write into the UART is
// still in flight. Two writers interleaving into one UART is the failure this prevents.
//
// It is asserted here rather than end-to-end because end-to-end it is unobservable: by the
// time a displaced client's bytes could interleave, its socket is already closed and its
// writes fail locally. A test at that level passes whether or not the wait exists.
func TestAttachWaitsForThePreviousClientToStopWriting(t *testing.T) {
	dev := newBlockingDev()
	p := New(nil)
	p.Serve(dev)

	srv1, cli1 := net.Pipe()
	defer srv1.Close()
	defer cli1.Close()
	p.Attach(srv1)

	// Put a write into the device and leave it there, blocked.
	go cli1.Write([]byte("FIRST"))
	select {
	case <-dev.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first client's write never reached the device")
	}

	srv2, cli2 := net.Pipe()
	defer srv2.Close()
	defer cli2.Close()

	attached := make(chan struct{})
	go func() {
		p.Attach(srv2)
		close(attached)
	}()

	select {
	case <-attached:
		t.Fatal("Attach returned while the previous client's write was still in the UART")
	case <-time.After(300 * time.Millisecond):
		// Correct: still waiting on the old session.
	}

	// Release the blocked write. The old session can now finish, and only then may Attach.
	select {
	case got := <-dev.writes:
		if string(got) != "FIRST" {
			t.Errorf("device received %q, want %q", got, "FIRST")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked write never completed")
	}

	select {
	case <-attached:
	case <-time.After(5 * time.Second):
		t.Fatal("Attach never returned after the previous client stopped")
	}

	// And the new client is the one that now reaches the device.
	go cli2.Write([]byte("SECOND"))
	select {
	case got := <-dev.writes:
		if string(got) != "SECOND" {
			t.Errorf("device received %q, want %q", got, "SECOND")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the new client's write never reached the device")
	}
}

// A reply the radio is already sending must never be handed to a client that connected in the
// middle of the exchange. So Attach waits for one in flight instead of taking over, and the
// bytes go where they were asked for. This is INV 6 seen from the other side: a client's
// stream must contain what it asked for and nothing else.
func TestAttachWaitsForAnExchangeSoItsReplyNeverReachesAClient(t *testing.T) {
	dev := newBlockingDev()
	defer close(dev.reads)
	p := New(nil)
	p.Serve(dev)

	reply := []byte{0xFE, 0x02, 0x61, 0x01, 0x79, 0x01, 0x1A}
	whole := func(b []byte) bool { return len(b) >= len(reply) }

	exchanged := make(chan []byte, 1)
	go func() {
		got, err := p.Exchange([]byte{0xFE, 0x00, 0x21, 0x01, 0x20}, whole, 2*time.Second)
		if err != nil {
			t.Errorf("Exchange: %v", err)
		}
		exchanged <- got
	}()

	// The request has entered the device; the exchange is now in flight and owns the UART.
	<-dev.entered
	<-dev.writes

	attached := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close() })
		p.Attach(server)
		attached <- time.Since(start)
		// Anything the client can read here is a byte it never asked for.
		client.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		var buf [16]byte
		if n, err := client.Read(buf[:]); n > 0 {
			t.Errorf("the client was handed %d bytes of the exchange's reply: % x", n, buf[:n])
		} else if err == nil {
			t.Error("the client read succeeded with no bytes and no error")
		}
		close(attached)
	}()

	// Attach must still be waiting: nothing has answered the exchange yet.
	select {
	case took := <-attached:
		t.Fatalf("Attach cut into an exchange in flight after %v", took)
	case <-time.After(200 * time.Millisecond):
	}

	// Now the radio answers. The exchange takes it, and only then does the client attach.
	dev.reads <- reply
	if got := <-exchanged; len(got) != len(reply) {
		t.Errorf("the exchange collected % x, want % x", got, reply)
	}
	select {
	case <-attached:
	case <-time.After(2 * time.Second):
		t.Fatal("Attach never returned after the exchange finished")
	}
	<-attached // the client-read check above has run
}

// An exchange while a client holds the device would be a second writer on one UART, which is
// the thing INV 4 exists to prevent.
func TestExchangeRefusesWhileAClientIsAttached(t *testing.T) {
	dev := newBlockingDev()
	defer close(dev.reads)
	p := New(nil)
	p.Serve(dev)

	client, server := net.Pipe()
	defer client.Close()
	p.Attach(server)

	// Refusal has to be immediate, and asserted so that a *failure* to refuse shows up as a
	// failure rather than as a hang: the exchange would otherwise block in the device write,
	// and a test that hangs reports a CI timeout instead of the reason.
	refused := make(chan error, 1)
	go func() {
		_, err := p.Exchange([]byte{0x00}, func([]byte) bool { return true }, time.Second)
		refused <- err
	}()
	select {
	case err := <-refused:
		if err != ErrBusy {
			t.Errorf("Exchange returned %v with a client attached, want ErrBusy", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exchange did not refuse; it went to the device with a client attached")
	}
}

// The radio that answers too late is by definition the one that was already unwell, so the
// guarantee above has to cover it too: a timed-out exchange keeps the device until the drain
// window closes, and the late answer is absorbed instead of landing on whichever client
// happened to connect in the meantime.
func TestATimedOutExchangeAbsorbsTheLateReply(t *testing.T) {
	dev := newBlockingDev()
	defer close(dev.reads)
	p := New(nil)
	p.Serve(dev)

	reply := []byte{0xFE, 0x02, 0x61, 0x01, 0x79, 0x01, 0x1A}
	const timeout = 50 * time.Millisecond

	exchanged := make(chan []byte, 1)
	go func() {
		got, err := p.Exchange([]byte{0xFE, 0x00, 0x21, 0x01, 0x20},
			func(b []byte) bool { return len(b) >= len(reply) }, timeout)
		if err != nil {
			t.Errorf("Exchange: %v", err)
		}
		exchanged <- got
	}()
	<-dev.entered
	<-dev.writes

	// Past the deadline, inside the drain window.
	time.Sleep(timeout + 50*time.Millisecond)

	attached := make(chan struct{})
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		p.Attach(server)
		close(attached)
	}()

	// The exchange timed out, but it has not let go: a client must still be waiting.
	select {
	case <-attached:
		t.Fatal("the device was handed to a client while a late reply was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	// The answer the radio was too slow to give. It belongs to nobody now.
	dev.reads <- reply

	if got := <-exchanged; len(got) != 0 {
		t.Errorf("the exchange reported % x as the answer, want nothing by its deadline", got)
	}
	select {
	case <-attached:
	case <-time.After(2 * time.Second):
		t.Fatal("Attach never returned after the drain window")
	}

	client.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	var buf [16]byte
	if n, err := client.Read(buf[:]); n > 0 {
		t.Errorf("the client was handed %d bytes of a late probe reply: % x", n, buf[:n])
	} else if err == nil {
		t.Error("the client read succeeded with no bytes and no error")
	}
}

// The report card's numbers are only worth having if they distinguish the cases an operator is
// actually arguing about — failure class 4, beneath the bridge. So: bytes each way, and a
// takeover told apart from a client that left on its own.
func TestStatsCountWhatHappenedAndWhy(t *testing.T) {
	dev := newBlockingDev()
	defer close(dev.reads)
	go func() {
		for range dev.writes { // the device accepts whatever the client sends
		}
	}()
	p := New(nil)
	p.Serve(dev)

	if s := p.Stats(); s.Connects != 0 || s.Client != "" {
		t.Fatalf("a pipe with no client reports %+v", s)
	}

	first, firstServer := net.Pipe()
	defer first.Close()
	p.Attach(firstServer)

	// Client to device.
	if _, err := first.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// Device to client.
	go func() { dev.reads <- []byte{0x01, 0x02, 0x03} }()
	got := make([]byte, 3)
	if _, err := io.ReadFull(first, got); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		s := p.Stats()
		return s.BytesFromClient == 5 && s.BytesToClient == 3
	}, "the byte counts to settle")

	s := p.Stats()
	if s.Connects != 1 || s.Takeovers != 0 || s.Disconnects != 0 {
		t.Errorf("after one client: %d connects, %d takeovers, %d disconnects; want 1, 0, 0",
			s.Connects, s.Takeovers, s.Disconnects)
	}
	if s.Client == "" {
		t.Error("no client address reported while one is attached")
	}

	// A takeover is not a disconnect. If it counted as both, the disconnect number could no
	// longer tell "this client keeps dropping" from "clients keep replacing each other" — and
	// telling those apart is the entire job of the number.
	second, secondServer := net.Pipe()
	defer second.Close()
	p.Attach(secondServer)

	waitFor(t, func() bool { return p.Stats().Takeovers == 1 }, "the takeover to be counted")
	if s := p.Stats(); s.Connects != 2 || s.Disconnects != 0 {
		t.Errorf("after a takeover: %d connects, %d disconnects; want 2, 0", s.Connects, s.Disconnects)
	}

	// A client that leaves of its own accord is one, and the reason is recorded.
	second.Close()
	waitFor(t, func() bool { return p.Stats().Disconnects == 1 }, "the disconnect to be counted")
	if s := p.Stats(); s.LastEvent == "" {
		t.Error("a client left and the card cannot say why")
	}
}

func waitFor(t *testing.T, done func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The counts say what has happened ever; these say whether it is still happening. That is the
// difference between "the radio is quiet" and "the radio stopped talking while the client kept
// asking", and it is the only liveness signal available while a client is attached — the probe
// deliberately never runs then.
func TestStatsRecordWhenEachSideLastSpoke(t *testing.T) {
	dev := newBlockingDev()
	defer close(dev.reads)
	go func() {
		for range dev.writes {
		}
	}()
	p := New(nil)
	p.Serve(dev)

	// Never is not the same as now, and must not be reported as a zero-age.
	if s := p.Stats(); !s.LastDeviceByteAt.IsZero() || !s.LastClientByteAt.IsZero() {
		t.Fatalf("a pipe that has carried nothing reports device=%v client=%v",
			s.LastDeviceByteAt, s.LastClientByteAt)
	}

	client, server := net.Pipe()
	defer client.Close()
	p.Attach(server)

	// The radio speaks, the client does not.
	go func() { dev.reads <- []byte{0x01} }()
	buf := make([]byte, 1)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !p.Stats().LastDeviceByteAt.IsZero() }, "the device stamp")
	if s := p.Stats(); !s.LastClientByteAt.IsZero() {
		t.Error("the client stamp moved when only the radio spoke")
	}

	// Now the client does. The two are independent, which is the whole point: one moving while
	// the other does not is the shape that localises the fault.
	deviceSpokeAt := p.Stats().LastDeviceByteAt
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !p.Stats().LastClientByteAt.IsZero() }, "the client stamp")
	if s := p.Stats(); !s.LastDeviceByteAt.Equal(deviceSpokeAt) {
		t.Error("the device stamp moved when only the client spoke")
	}
}
