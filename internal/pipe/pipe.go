// Package pipe couples the coordinator to at most one client at a time and moves bytes
// between them. It is part 3 of ARCHITECTURE.md "The shape", and most of the invariants live
// here.
//
// The device is read by a single reader for as long as it is there, not per client. That is
// what makes the client lifecycle and the radio independent (INV 1): a client arriving or
// leaving changes where bytes go, and nothing else. It is also why a client can never be
// handed bytes the radio emitted before it connected.
//
// The Pipe outlives any one device. A dongle unplugged and plugged back in is the same tether
// with the same counters, so the device is handed to Serve rather than held from
// construction — and between generations there is no device, which is a state the write side
// has to answer for rather than assume away.
package pipe

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// One UART frame is a few dozen bytes and the link ceiling is 115200 baud ≈ 11.5 KB/s, so this
// is roughly a third of a second of traffic — large enough that a burst costs one syscall,
// small enough that it is not a queue anything could hide in.
const bufSize = 4096

// ErrBusy reports that the device is carrying a client, or another exchange, and so is not
// available for an out-of-band exchange. It is not a fault: a probe exists to say something
// about an *idle* radio, and a radio with a client on it is being proved alive by the client.
var ErrBusy = errors.New("the device is in use")

// errNoDevice reports that there is no device to talk to at all — tether is between
// generations, holding nothing while it waits for the adapter to come back. It is not
// ErrBusy: busy means something with a better claim holds the radio, this means there is no
// radio. Unexported because the only caller that sees it is the probe, and what the probe does
// with it is report the text.
var errNoDevice = errors.New("no device is open")

// Pipe owns the device end. Exactly one client is attached at a time (INV 4); there is no
// broadcast and no arbitration between two writers, because a UART cannot survive either.
type Pipe struct {
	dev     io.ReadWriter
	observe Observer

	// Counters for the report card. Bytes are atomic because they are touched on every
	// read and write and must not put a mutex in the data path; the rest sit under mu with the
	// state they describe, so that a count and the event it counts cannot disagree.
	bytesToClient   atomic.Uint64
	bytesFromClient atomic.Uint64

	// When each side last said anything, as Unix nanoseconds; zero means never. Cumulative
	// counts alone cannot answer "is the radio talking *now*", which is the question that
	// separates a wedged dongle from a quiet one while a client is attached — and while one is,
	// the probe deliberately never runs, so these are the only liveness signal there is.
	lastDeviceByteAt atomic.Int64
	lastClientByteAt atomic.Int64

	mu           sync.Mutex
	cur          *session
	connects     uint64
	takeovers    uint64
	disconnects  uint64
	deviceErrors uint64
	lastEvent    string
	lastEventAt  time.Time
	// recent is the takeovers of the last contentionWindow, by the host that did the taking.
	// A cumulative count cannot tell five takeovers in a month from five in half a minute, and
	// only the second is a fault; this is what makes the difference readable.
	recent []takeover
	// ex is the out-of-band exchange in flight, if any. It and cur are mutually exclusive:
	ex *exchange
}

// contentionWindow is how far back a takeover still counts as part of what is happening now.
// A minute is long enough that a client restarting slowly is still one story, and short enough
// that a card read during a fight is describing the fight rather than last week.
const contentionWindow = time.Minute

// takeover is one client displacing another, recorded by *host* rather than by address.
// The distinction is the whole feature: a client that reconnects arrives on a new ephemeral
// port every time, so counting addresses would report one restarting client as a crowd.
type takeover struct {
	host string
	at   time.Time
}

// contention summarises a window of takeovers: how many there were, and which hosts did them.
//
// More than one host is the canonical bad case for kick-old (INV 4) and the only one worth a
// word on the card: two clients configured against the same coordinator, each displacing the
// other forever, neither ever settling. One host taking over repeatedly is a different fault
// with a different fix — a client that keeps dying and coming back — so the hosts are named
// rather than merely counted.
//
// It takes `now` rather than reading the clock so that the decision is testable without one.
func contention(events []takeover, now time.Time) (recent []takeover, hosts []string) {
	seen := make(map[string]struct{}, 2)
	for _, e := range events {
		if now.Sub(e.at) > contentionWindow {
			continue
		}
		if _, ok := seen[e.host]; !ok {
			seen[e.host] = struct{}{}
			hosts = append(hosts, e.host)
		}
		recent = append(recent, e)
	}
	slices.Sort(hosts)
	return recent, hosts
}

// hostOf is the address without its port. A failure to split is not worth an error path: the
// only caller has an address the net package just handed it, and reporting it whole is a
// better answer than reporting nothing.
func hostOf(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// Stats is what the pipe knows about itself, for the report card. The point of these
// numbers is failure class 4: when the fault is really the dongle or the RF, they are what
// exonerates the transport instead of leaving it the obvious suspect.
type Stats struct {
	BytesToClient   uint64
	BytesFromClient uint64
	Connects        uint64
	Takeovers       uint64
	Disconnects     uint64
	DeviceErrors    uint64

	// Client is the address of whoever is attached, empty when nobody is.
	Client string

	// LastEvent is the most recent thing that happened to a client, in the words the log used,
	// and when. \"Disconnected\" without a reason is the answer that starts an argument.
	LastEvent   string
	LastEventAt time.Time

	// LastDeviceByteAt and LastClientByteAt are when each side last said anything. Zero means
	// it never has.
	LastDeviceByteAt time.Time
	LastClientByteAt time.Time

	// RecentTakeovers is how many of Takeovers happened in the last minute, and Contenders
	// names the hosts that did them when there was more than one. Two hosts here is a fight:
	// two clients pointed at one coordinator, displacing each other indefinitely. It is the
	// price of kick-old (INV 4) and the reason kick-old is still right — the alternative,
	// refusing, has a failure that says nothing at all.
	RecentTakeovers uint64
	Contenders      []string
}

// since turns a stored nanosecond stamp into a time, with zero meaning never.
func since(stamp int64) time.Time {
	if stamp == 0 {
		return time.Time{}
	}
	return time.Unix(0, stamp)
}

// Stats reports a consistent snapshot.
func (p *Pipe) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Stats{
		BytesToClient:    p.bytesToClient.Load(),
		BytesFromClient:  p.bytesFromClient.Load(),
		Connects:         p.connects,
		Takeovers:        p.takeovers,
		Disconnects:      p.disconnects,
		DeviceErrors:     p.deviceErrors,
		LastEvent:        p.lastEvent,
		LastEventAt:      p.lastEventAt,
		LastDeviceByteAt: since(p.lastDeviceByteAt.Load()),
		LastClientByteAt: since(p.lastClientByteAt.Load()),
	}
	if p.cur != nil {
		s.Client = p.cur.conn.RemoteAddr().String()
	}
	// Summarised on read rather than kept up to date on write, because the window moves on its
	// own: takeovers age out with the clock, not with anything the pipe does.
	recent, hosts := contention(p.recent, time.Now())
	s.RecentTakeovers = uint64(len(recent))
	if len(hosts) > 1 {
		s.Contenders = hosts
	}
	return s
}

// note records what just happened to a client. The caller holds mu.
func (p *Pipe) note(event string) {
	p.lastEvent = event
	p.lastEventAt = time.Now()
}

// exchange is one out-of-band request and the reply being collected for it. It exists so that
// management can ask the radio a question without becoming a second writer on the UART, which
// INV 4 forbids for exactly the reason it forbids two clients.
type exchange struct {
	buf      []byte
	complete func([]byte) bool
	replied  chan struct{}
	finished chan struct{}
	closed   bool
}

// gather appends what the device said and reports whether the reply is now whole. Callers
// hold the pipe mutex; this does no I/O.
func (e *exchange) gather(b []byte) {
	e.buf = append(e.buf, b...)
	if !e.closed && e.complete(e.buf) {
		e.closed = true
		close(e.replied)
	}
}

// session is one client's attachment. done closes when its reader has stopped, which is how
// a takeover knows the previous client can no longer write to the device.
type session struct {
	conn net.Conn
	done chan struct{}

	// displaced marks a session closed by a takeover rather than by its own client. Its reader
	// will see a closed connection either way, and only this says which happened — without it a
	// takeover would count as a takeover *and* a disconnect, and the disconnect count would
	// stop being able to tell "this client keeps dropping" from "clients keep replacing it".
	// Guarded by the pipe mutex.
	displaced bool
}

// Observer is handed a copy of bytes that have already been forwarded, and is told which way
// they went. It exists so that something else can count what passed — the frame census —
// without the pipe knowing what any of it means.
//
// It sees the client path only: what a client sent, and what the radio said to a client or
// into an empty room. An out-of-band exchange is tether's own traffic and is never shown to
// it — the census compares a client's questions against the radio's answers, and counting the
// probe's answers would put a thumb on that scale, one a minute, for as long as nobody is
// attached.
//
// It is called after the bytes are on their way, never before, and its return value is
// discarded — so an observer cannot alter, delay by refusing, reframe or drop anything, and
// INV 6 holds whatever it does. It runs on the data path's own goroutine and must be cheap:
// what it does slowly, the radio waits for.
type Observer func(fromRadio bool, b []byte)

// New returns a Pipe with no device yet. observe may be nil.
func New(observe Observer) *Pipe {
	return &Pipe{observe: observe}
}

// Serve takes dev — an already-open, already-drained port (INV 2; device.Open does that work,
// and this is only ever called after it has returned) — and starts moving bytes between it and
// whichever client is attached. It returns a channel carrying the device error that ends this
// generation, which ends neither the radio nor tether: the caller reopens.
//
// It binds the device before it returns, and that is the reason it is shaped this way rather
// than as a blocking call the caller puts in a goroutine. The listener must not exist before
// the device is bound, or a client's first byte can arrive with nothing to carry it to — which
// is precisely the race INV 2 forbids, and a goroutine would leave it open.
//
// When no client is attached the bytes are read and dropped. They belong to no client's frame
// stream: the previous client is gone and the next one has not connected, and handing a fresh
// client the tail of someone else's session is how a bridge corrupts a first frame. Reading
// regardless is also what stops the tty buffer filling and back-pressuring the radio while
// nobody is listening.
func (p *Pipe) Serve(dev io.ReadWriter) <-chan error {
	p.mu.Lock()
	p.dev = dev
	p.mu.Unlock()

	// Buffered, so a caller that never reads the result costs a value rather than a goroutine.
	failed := make(chan error, 1)
	go func() {
		defer func() {
			// Nothing may write to a device we no longer hold: the next thing to happen to
			// this one is a Close, and a client whose own reader has not noticed yet must be
			// told so rather than handed a descriptor that is about to go.
			p.mu.Lock()
			p.dev = nil
			p.mu.Unlock()
		}()
		failed <- p.copy(dev)
	}()
	return failed
}

// copy is Serve's loop, until the device fails.
func (p *Pipe) copy(dev io.ReadWriter) error {
	buf := make([]byte, bufSize)
	for {
		n, err := dev.Read(buf)
		if n > 0 {
			p.lastDeviceByteAt.Store(time.Now().UnixNano())
			if exchanged := p.deliver(buf[:n]); !exchanged && p.observe != nil {
				p.observe(true, buf[:n])
			}
		}
		if err != nil {
			// INV 3: fail loud, never stall. A silent stall is the worst possible input to
			// a client's timers; a close is the trigger for recovery paths it already has.
			p.mu.Lock()
			p.deviceErrors++
			p.mu.Unlock()
			p.closeCurrent("device failed: " + err.Error())
			return err
		}
	}
}

// Attach makes conn the current client, closing any previous one — kick-old takeover, so a
// half-dead connection can never lock out the machine that is actually trying to work.
// Callers are serialised by the accept loop being a single goroutine.
//
// The radio is not touched here. No reset, no line change, no drain: that is INV 1, and it is
// the whole reason a client can restart without the Zigbee network noticing.
func (p *Pipe) Attach(conn net.Conn) {
	p.waitForExchange()

	p.mu.Lock()
	old := p.cur
	p.mu.Unlock()

	if old != nil {
		p.mu.Lock()
		p.takeovers++
		old.displaced = true
		p.note(fmt.Sprintf("%s took over from %s", conn.RemoteAddr(), old.conn.RemoteAddr()))
		p.recent = append(p.recent, takeover{host: hostOf(conn.RemoteAddr()), at: time.Now()})
		// Pruned here so the slice cannot grow without bound on a tether that lives for
		// months, which is the only thing that stops this being a slow leak.
		recent, hosts := contention(p.recent, time.Now())
		p.recent = recent
		p.mu.Unlock()

		// The takeover was always logged; what is new is saying when it is a *fight*. This
		// costs no extra line — an operator reading one takeover learns nothing, and an
		// operator reading this learns which two machines to go and look at.
		if len(hosts) > 1 {
			log.Printf("pipe: %s takes over from %s — %d takeovers in the last minute between "+
				"%s; two clients look configured against this coordinator, and they will "+
				"displace each other until one is stopped",
				conn.RemoteAddr(), old.conn.RemoteAddr(), len(recent), strings.Join(hosts, " and "))
		} else {
			log.Printf("pipe: %s takes over from %s", conn.RemoteAddr(), old.conn.RemoteAddr())
		}
		old.conn.Close()
		// Wait for the old reader to stop before the new one starts. Without this the two
		// could interleave writes into the UART, which is INV 4's actual failure mode.
		<-old.done
	} else {
		log.Printf("pipe: %s connected", conn.RemoteAddr())
		p.mu.Lock()
		p.note(fmt.Sprintf("%s connected", conn.RemoteAddr()))
		p.mu.Unlock()
	}

	s := &session{conn: conn, done: make(chan struct{})}
	p.mu.Lock()
	p.cur = s
	p.connects++
	p.mu.Unlock()

	go p.fromClient(s)
}

// deliver hands what the device said to whoever is listening: the attached client, or an
// out-of-band exchange, or nobody. It reports whether an exchange took the bytes, because the
// observer is shown the client path only (see Observer). The client write is not made under
// the lock: a stuck client must be able to be taken over, and holding the lock here would
// block Attach behind exactly the connection it is trying to replace.
func (p *Pipe) deliver(b []byte) (exchanged bool) {
	p.mu.Lock()
	if p.ex != nil {
		// An exchange and a client never overlap, so this is not a choice between two
		// listeners — it is the only one there is.
		p.ex.gather(b)
		p.mu.Unlock()
		return true
	}
	s := p.cur
	p.mu.Unlock()
	if s == nil {
		return false
	}
	// A slow client blocks this write, which stops the device being read, which back-pressures
	// the radio. That is INV 5: never drop a byte, never overwrite one, make the sender wait.
	n, err := s.conn.Write(b)
	p.bytesToClient.Add(uint64(n))
	if err != nil {
		log.Printf("pipe: %s write failed (%v); dropping the client, the radio is untouched", s.conn.RemoteAddr(), err)
		s.conn.Close()
	}
	return false
}

// fromClient moves bytes from one client to the device until either end stops.
func (p *Pipe) fromClient(s *session) {
	// The guard is by identity, not by nil-ness: a displaced session must never clear the one
	// that replaced it. Together with deliver holding no buffer, that is what makes a
	// takeover's gap dry — anything the radio says while no session is current is written at a
	// closed socket or dropped outright, never kept for a successor that did not ask for it.
	// INV 4 rests on that property; `TestAReplyOwedToADepartedClientIsDroppedNotQueued`
	// is what holds it, end to end.
	defer func() {
		p.mu.Lock()
		if p.cur == s {
			p.cur = nil
		}
		p.mu.Unlock()
		s.conn.Close()
		close(s.done)
	}()

	buf := make([]byte, bufSize)
	for {
		n, err := s.conn.Read(buf)
		if n > 0 {
			p.bytesFromClient.Add(uint64(n))
			p.lastClientByteAt.Store(time.Now().UnixNano())
			if _, werr := p.writeDevice(buf[:n]); werr != nil {
				// The device is in trouble. Drop the client now rather than let it wait on
				// a reply that cannot come (INV 3); Run's read will fail independently and
				// is what actually reports the error upward.
				log.Printf("pipe: device write failed (%v); closing %s", werr, s.conn.RemoteAddr())
				p.recordDisconnect(s, fmt.Sprintf("dropped after a device write failed: %v", werr))
				return
			}
			if p.observe != nil {
				p.observe(false, buf[:n])
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("pipe: %s read failed (%v)", s.conn.RemoteAddr(), err)
				p.recordDisconnect(s, fmt.Sprintf("read failed: %v", err))
			} else {
				log.Printf("pipe: %s disconnected", s.conn.RemoteAddr())
				p.recordDisconnect(s, "disconnected")
			}
			return
		}
	}
}

// writeDevice writes to whatever device is being served, and fails rather than panicking when
// there is none — a session can outlive its generation by the moment its own reader takes to
// notice the close. The lock here is the one deliver already takes on every read from the
// device, so this makes the two directions symmetrical rather than putting something new in
// the data path.
func (p *Pipe) writeDevice(b []byte) (int, error) {
	p.mu.Lock()
	dev := p.dev
	p.mu.Unlock()
	if dev == nil {
		return 0, errNoDevice
	}
	return dev.Write(b)
}

// closeCurrent drops whatever client is attached, naming the reason for the log.
func (p *Pipe) closeCurrent(reason string) {
	p.mu.Lock()
	s := p.cur
	p.mu.Unlock()
	if s == nil {
		return
	}
	log.Printf("pipe: closing %s — %s", s.conn.RemoteAddr(), reason)
	s.conn.Close()
}

// maxExchangeWait bounds how long a connecting client waits for an out-of-band exchange to
// finish. It is a backstop, not a timeout anyone should reach: an exchange carries its own
// deadline and a probe's is a fraction of this. A client must never be locked out by a health
// check — that is the ser2net failure this project exists to not repeat.
const maxExchangeWait = 2 * time.Second

// drainWindow is how much longer a timed-out exchange keeps the device to itself, absorbing an
// answer that arrives too late to count. It is bounded by maxExchangeWait together with any
// sane exchange timeout, so it cannot become a way to lock a client out.
const drainWindow = 250 * time.Millisecond

// Exchange writes req to the device and returns what the device says back, stopping as soon as
// complete reports the reply whole or timeout passes. It is how the management part asks the
// radio a question — one of the two places anything is allowed to know what the bytes mean.
//
// It refuses with ErrBusy while a client is attached. That is the whole of the concurrency
// story: an exchange and a client are mutually exclusive, so nothing here can make two writers
// share a UART (INV 4), and no client can be handed a frame it did not ask for. Attach waits
// for an exchange rather than cutting in, because by the time a client arrives the radio's
// reply is already on its way back and someone has to consume it.
//
// A radio that misses the deadline may still answer, so a timed-out exchange holds the device
// for a further drainWindow and throws away whatever turns up. Without it the guarantee above
// would hold only for exchanges that *complete*, which is the wrong half: the radio that
// answers late is by definition the one that was not well.
//
// The reply is returned even when it never completed, because a partial or unparseable answer
// says something different about the radio than silence does, and the caller is the only thing
// that can tell them apart.
func (p *Pipe) Exchange(req []byte, complete func([]byte) bool, timeout time.Duration) ([]byte, error) {
	ex := &exchange{
		complete: complete,
		replied:  make(chan struct{}),
		finished: make(chan struct{}),
	}

	p.mu.Lock()
	if p.cur != nil || p.ex != nil {
		p.mu.Unlock()
		return nil, ErrBusy
	}
	dev := p.dev
	if dev == nil {
		// Nothing to ask. Distinct from ErrBusy on purpose: the probe records this and
		// discards that, because "there is no radio" is health news and "someone else is
		// using the radio" is not.
		p.mu.Unlock()
		return nil, errNoDevice
	}
	p.ex = ex
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.ex = nil
		p.mu.Unlock()
		close(ex.finished)
	}()

	if _, err := dev.Write(req); err != nil {
		return nil, fmt.Errorf("writing an out-of-band request: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var timedOut bool
	select {
	case <-ex.replied:
	case <-timer.C:
		timedOut = true
	}

	// Copied, not aliased: the drain below keeps appending to the exchange buffer, and the
	// caller is handed what the radio had said by the deadline and nothing after it.
	p.mu.Lock()
	reply := append([]byte(nil), ex.buf...)
	p.mu.Unlock()

	if timedOut {
		// Still registered, so anything arriving now is gathered and discarded rather than
		// delivered, and Attach still waits. The result was decided at the deadline: a late
		// answer does not retroactively become a healthy one, or the timeout would mean
		// nothing.
		time.Sleep(drainWindow)
	}
	return reply, nil
}

// waitForExchange blocks while an out-of-band exchange is in flight, so that its reply cannot
// be delivered to a client that connected in the middle of it. The wait is bounded and short;
// past that we take the device anyway and say so, because a client kept waiting is a worse
// failure than a stray frame a client's framer will resynchronise past.
func (p *Pipe) waitForExchange() {
	p.mu.Lock()
	ex := p.ex
	p.mu.Unlock()
	if ex == nil {
		return
	}
	timer := time.NewTimer(maxExchangeWait)
	defer timer.Stop()
	select {
	case <-ex.finished:
	case <-timer.C:
		log.Printf("pipe: an out-of-band exchange has held the device for %v; taking it for the "+
			"waiting client, which may see one frame it did not ask for", maxExchangeWait)
	}
}

// recordDisconnect counts a client leaving and remembers why. The reason is the whole point:
// "the client disconnected" with no cause is the log line that starts an argument about whose
// fault it was, which is failure class 4 in miniature.
//
// A session displaced by a takeover is not counted here — Attach already recorded that, and
// counting it twice would make a takeover look like a client that left of its own accord.
func (p *Pipe) recordDisconnect(s *session, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur != s || s.displaced {
		return
	}
	p.disconnects++
	p.note(s.conn.RemoteAddr().String() + " " + reason)
}
