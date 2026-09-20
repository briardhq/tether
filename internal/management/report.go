package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"briard.io/tether/internal/build"
	"briard.io/tether/internal/family"
	"briard.io/tether/internal/pipe"
)

// ReportInterval is how often the whole card goes into the log. Often enough that a support
// conversation can look back at what an hour ago was like, rare enough not to bury the lines
// that mean something happened.
const ReportInterval = 15 * time.Minute

// Card is the report card: what tether has done and what it currently believes, as one
// snapshot. It exists for failure class 4 — when the fault is really the dongle or the RF,
// these are the numbers that exonerate the transport rather than leaving it the suspect.
//
// This is a published shape. `tether status` renders it and briard reads it directly, so a
// field may be added but not repurposed or removed. Times are absolute rather than ages: a
// reader can subtract, and an age computed here would be stale by the time it is read.
type Card struct {
	// Version is which build is running — the release and the platform, `v0.1.0 linux/amd64` —
	// because a card pasted into a bug report is only as useful as its answer to "which binary".
	Version string `json:"version,omitempty"`

	StartedAt time.Time `json:"started_at"`
	Now       time.Time `json:"now"`

	RadioType string `json:"radio_type"`

	// Instance is the identity this tether advertises, and Listen the address it serves
	// clients on. They are here because they are what tells two tethers on one host apart —
	// a process id is not something anyone recognises — and because "which coordinator am I
	// talking to" is a question the card should be able to answer on its own. Both are empty
	// until there is a device, since the advertised name says which adapter this is.
	Instance string `json:"instance,omitempty"`
	Listen   string `json:"listen,omitempty"`

	BytesToClient   uint64 `json:"bytes_to_client"`
	BytesFromClient uint64 `json:"bytes_from_client"`

	Client      string    `json:"client,omitempty"`
	Connects    uint64    `json:"client_connects"`
	Takeovers   uint64    `json:"client_takeovers"`
	Disconnects uint64    `json:"client_disconnects"`
	LastEvent   string    `json:"last_event,omitempty"`
	LastEventAt time.Time `json:"last_event_at,omitzero"`

	// RecentTakeovers and Contenders are the takeovers of the last minute and, when more
	// than one host did them, which hosts. Two names here is the canonical bad case for
	// kick-old (INV 4): two clients pointed at one coordinator, displacing each other
	// forever. The cumulative count above cannot show it, because it cannot tell five
	// takeovers in a month from five in half a minute.
	RecentTakeovers uint64   `json:"client_takeovers_recent,omitempty"`
	Contenders      []string `json:"client_contenders,omitempty"`

	// LastDeviceByteAt and LastClientByteAt are what make the byte counts point-in-time. The
	// counts alone say what has happened ever; these say whether it is still happening, which
	// is the difference between "the radio is quiet" and "the radio stopped talking 40 minutes
	// ago while the client kept asking".
	LastDeviceByteAt time.Time `json:"last_device_byte_at,omitzero"`
	LastClientByteAt time.Time `json:"last_client_byte_at,omitzero"`

	DeviceErrors uint64 `json:"device_errors"`

	// DevicePresent is whether tether is holding the adapter right now, and OpenAttempts is
	// how many times it has tried to open one since it started — the first open included, so
	// "1" and "never lost it" are the same reading. They are the pair that makes an outage
	// legible: DeviceErrors says how many times the device went away, OpenAttempts says how
	// hard tether has had to work to get it back, and DevicePresent says whether it is back.
	// Without the last of them a card read during an outage — which is exactly when somebody
	// reads one — would describe a radio that is not there as though it were.
	//
	// DeviceAbsentReason says why there is no adapter, and is empty while there is one. With
	// detection in play, "absent" covers genuinely different situations — none attached
	// yet, two attached and tether refusing to guess between them, one attached that it cannot
	// open — and which of them it is decides what an operator does next.
	DevicePresent      bool   `json:"device_present"`
	OpenAttempts       uint64 `json:"reopen_attempts"`
	DeviceAbsentReason string `json:"device_absent_reason,omitempty"`

	Probe *ProbeReport `json:"probe,omitempty"`

	// Frames is the census, absent for the families it cannot read. Absent and all-zero say
	// different things and must not render alike.
	Frames *FrameCounts `json:"frames,omitempty"`
}

// ProbeReport is the last thing the radio said, and when. Absent while nothing has asked yet,
// and for the families that have no probe — an absent probe and a failed one mean very
// different things and must not render the same.
type ProbeReport struct {
	At           time.Time `json:"at"`
	OK           bool      `json:"ok"`
	RTTms        float64   `json:"rtt_ms,omitempty"`
	Capabilities uint16    `json:"capabilities,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// Monitor owns everything tether knows about its own health: it runs the probe, keeps the last
// result, and assembles the card. It is the management part, and the only thing that reaches
// across components to build a whole answer.
type Monitor struct {
	pipe    *pipe.Pipe
	census  *Census
	started time.Time

	mu        sync.Mutex
	lastProbe *ProbeReport
	// The device's own state, which no other component can answer for: the pipe knows the
	// device failed, but only the thing reopening it knows whether it has come back.
	//
	// radio is here rather than in the constructor because tether finds its adapter instead of
	// being told where one is, so at startup it may not know — and must still be able to
	// answer `tether status`, since "I am waiting, and here is why" is exactly the answer
	// somebody needs at that moment.
	radio         family.Radio
	devicePresent bool
	instance      string
	listen        string
	openAttempts  uint64
	absence       string
}

// NewMonitor returns a Monitor over an already-running pipe. census may be nil, and is the same
// one handed to pipe.New as its observer — it has to exist before the pipe does, which is why
// the caller makes it rather than this.
func NewMonitor(p *pipe.Pipe, census *Census) *Monitor {
	return &Monitor{pipe: p, census: census, started: time.Now()}
}

// Serving records what this tether is advertising and where, so the card can say which
// coordinator it is. Told rather than constructed with, for the same reason the radio is: the
// advertised name carries the adapter, and tether may be waiting for one to be plugged in.
func (m *Monitor) Serving(instance, listen string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instance, m.listen = instance, listen
}

// Probe asks the radio now and records the answer. It is the on-demand half of the liveness
// probe; the supervision loop drives the periodic half.
//
// pipe.ErrBusy is returned unrecorded: a client holds the device, so nothing was learned, and
// writing "unknown" over a good answer from a minute ago would lose the one fact worth having.
func (m *Monitor) Probe() (Result, error) {
	result, err := ping(m.pipe)
	if errors.Is(err, pipe.ErrBusy) {
		return Result{}, err
	}

	report := &ProbeReport{At: time.Now(), OK: err == nil}
	if err != nil {
		report.Error = err.Error()
	} else {
		report.RTTms = float64(result.RTT.Microseconds()) / 1000
		report.Capabilities = result.Capabilities
	}
	m.mu.Lock()
	m.lastProbe = report
	m.mu.Unlock()

	return result, err
}

// DeviceOpened records that tether now holds an adapter, and which radio it turned out to be.
// The radio arrives here rather than at construction because detection is what learns it, and
// detection happens inside the supervision loop.
func (m *Monitor) DeviceOpened(radio family.Radio) {
	m.census.Adopt(radio)

	m.mu.Lock()
	first := m.radio == ""
	changed := !first && m.radio != radio
	m.radio, m.devicePresent, m.absence = radio, true, ""
	m.mu.Unlock()

	// Said when it is learned rather than at startup, and said once: whether this radio has a
	// liveness probe at all is a known gap for two of the three families, and a gap nobody is
	// told about is a silent one.
	if first || changed {
		if radio == family.ZNP {
			log.Printf("management: probing the %s radio while no client is attached", radio)
		} else {
			log.Printf("management: no liveness probe for %s radios; health for this one is "+
				"whatever the link itself shows", radio)
		}
	}
}

// NoDevice records that there is no adapter open, and why. The reason is the entire point: an
// outage with no cause on it is the status line that starts an argument, which is failure class
// 4 in miniature — and with detection in play the causes are genuinely different from each
// other ("none attached yet", "two attached and I will not guess", "permission denied").
func (m *Monitor) NoDevice(reason string) {
	m.mu.Lock()
	m.devicePresent = false
	m.absence = reason
	m.mu.Unlock()
}

// CountOpenAttempt records one attempt to open the device, whether or not it worked. Counting
// the successful ones too is what makes the number readable without a second one beside it: a
// tether that has held the same dongle since boot says 1.
func (m *Monitor) CountOpenAttempt() {
	m.mu.Lock()
	m.openAttempts++
	m.mu.Unlock()
}

// Card assembles the current snapshot.
func (m *Monitor) Card() Card {
	stats := m.pipe.Stats()
	m.mu.Lock()
	probe := m.lastProbe
	radio, present, attempts, absence := m.radio, m.devicePresent, m.openAttempts, m.absence
	instance, listen := m.instance, m.listen
	m.mu.Unlock()

	var frames *FrameCounts
	if m.census.Enabled() {
		counts := m.census.Counts()
		frames = &counts
	}

	return Card{
		Version:            build.Version + " " + build.Platform(),
		StartedAt:          m.started,
		Now:                time.Now(),
		RadioType:          string(radio),
		Instance:           instance,
		Listen:             listen,
		BytesToClient:      stats.BytesToClient,
		BytesFromClient:    stats.BytesFromClient,
		Client:             stats.Client,
		Connects:           stats.Connects,
		Takeovers:          stats.Takeovers,
		RecentTakeovers:    stats.RecentTakeovers,
		Contenders:         stats.Contenders,
		Disconnects:        stats.Disconnects,
		LastEvent:          stats.LastEvent,
		LastEventAt:        stats.LastEventAt,
		LastDeviceByteAt:   stats.LastDeviceByteAt,
		LastClientByteAt:   stats.LastClientByteAt,
		DeviceErrors:       stats.DeviceErrors,
		DevicePresent:      present,
		OpenAttempts:       attempts,
		DeviceAbsentReason: absence,
		Probe:              probe,
		Frames:             frames,
	}
}

// Run probes on one timer and logs the whole card on a slower one, until ctx ends.
func (m *Monitor) Run(ctx context.Context, probeEvery, reportEvery time.Duration) {
	probes := time.NewTicker(probeEvery)
	defer probes.Stop()
	reports := time.NewTicker(reportEvery)
	defer reports.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-probes.C:
			// Asked each tick rather than once at startup: which radio this is gets learned
			// when one is found, not when tether starts, and it can change under us if
			// somebody swaps the dongle for a different family.
			m.mu.Lock()
			probeable := m.radio == family.ZNP
			m.mu.Unlock()
			if !probeable {
				continue
			}
			result, err := m.Probe()
			switch {
			case errors.Is(err, pipe.ErrBusy):
				// A client is on the device, so the radio is already being proved alive by
				// something with a better claim to it. Silent on purpose: one line a minute
				// saying "did not need to check" is noise in the log that matters.
			case err != nil:
				log.Printf("management: the radio did not answer: %v", err)
			default:
				log.Printf("management: the radio answered in %v (capabilities 0x%04x)",
					result.RTT.Round(time.Microsecond), result.Capabilities)
			}
		case <-reports.C:
			log.Printf("management: %s", m.Card().Summary())
		}
	}
}

// Summary is the card on one line, for the log. Dense and greppable on purpose: this is what
// somebody scrolls back through at the point where they have stopped trusting anything.
func (c Card) Summary() string {
	s := fmt.Sprintf("up %v, %s, %d B to client / %d B from client, "+
		"%d connects / %d takeovers / %d disconnects, %d device errors",
		c.Now.Sub(c.StartedAt).Round(time.Second), c.RadioType,
		c.BytesToClient, c.BytesFromClient,
		c.Connects, c.Takeovers, c.Disconnects, c.DeviceErrors)
	if c.Client != "" {
		s += ", client " + c.Client
	} else {
		s += ", no client"
	}
	// Only when the device is missing, and in shouting case, for the same reason the resets
	// below are: this is a line somebody greps for, and a permanent ", device present" would
	// make the search useless.
	if !c.DevicePresent {
		s += fmt.Sprintf(", NO DEVICE after %d open attempts", c.OpenAttempts)
		if c.DeviceAbsentReason != "" {
			s += " — " + c.DeviceAbsentReason
		}
	}
	if !c.LastDeviceByteAt.IsZero() {
		s += fmt.Sprintf(", radio last spoke %v ago", c.Now.Sub(c.LastDeviceByteAt).Round(time.Second))
	}
	switch {
	case c.Probe == nil:
		s += ", radio not probed"
	case c.Probe.OK:
		s += fmt.Sprintf(", radio answered %v ago in %.2fms",
			c.Now.Sub(c.Probe.At).Round(time.Second), c.Probe.RTTms)
	default:
		s += fmt.Sprintf(", radio silent as of %v ago (%s)",
			c.Now.Sub(c.Probe.At).Round(time.Second), c.Probe.Error)
	}
	if f := c.Frames; f != nil {
		s += fmt.Sprintf(", %d radio frames / %d client requests", f.RadioFrames, f.ClientSREQ)
		// Only when nonzero: these are the lines somebody greps for, and a permanent ", 0
		// resets" would make them worthless as a search.
		if resets := f.ResetsPowerUp + f.ResetsExternal + f.ResetsWatchdog; resets > 0 {
			s += fmt.Sprintf(", %d RADIO RESETS (last %s)", resets, f.LastResetReason)
		}
		if f.Rejections > 0 {
			s += fmt.Sprintf(", %d REJECTED FRAMES (last %s)", f.Rejections, f.LastRejectionCode)
		}
		if f.UnframedBytes > 0 {
			s += fmt.Sprintf(", %d unframed bytes", f.UnframedBytes)
		}
	}
	if c.LastEvent != "" {
		s += fmt.Sprintf("; last: %s (%v ago)", c.LastEvent, c.Now.Sub(c.LastEventAt).Round(time.Second))
	}
	return s
}

// ServeStatus answers `tether status` on a unix socket until ctx ends. One connection, one
// card, one close — there is no protocol here to get wrong or to version.
//
// A failure to listen is returned rather than fatal, and the caller must treat it that way:
// losing the status socket costs observability, and taking the radio down to punish that would
// be the cure being worse than the disease.
func (m *Monitor) ServeStatus(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("making the directory for the status socket: %w", err)
	}
	// A socket left behind by a process that was killed would otherwise make this fail
	// forever. Removing it is safe because the path carries our process id and no live process
	// shares it, so whatever is here was left by a dead tether. With one shared path it would
	// not be safe: a second tether would remove the first's socket, and the first would go on
	// listening on an inode nothing could reach.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing a stale status socket: %w", err)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listening on the status socket %s: %w", path, err)
	}
	defer listener.Close()
	// Connectable by any local user, not only the one tether runs as. net.Listen leaves the
	// file at 0777 &^ umask — 0755 under a service manager — and connecting to a unix socket
	// wants the write bit, so without this the card is readable by the service user and by
	// root and by nobody else, which is "observable from outside" for nobody. The card is
	// read-only and every field on it is already in the log, so there is nothing here to keep
	// from a local reader. Set after the bind rather than through the umask, because the umask
	// is the service manager's and not ours. Best-effort like the rest of this: a socket only
	// the user tether runs as can reach is still a socket.
	if err := os.Chmod(path, 0o666); err != nil {
		log.Printf("management: could not make the status socket readable by every local user "+
			"(%v); `briard-tether status` works only as the user tether runs as", err)
	}
	log.Printf("management: status readable at %s", path)

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accepting on the status socket: %w", err)
		}
		go m.answer(conn)
	}
}

func (m *Monitor) answer(conn net.Conn) {
	defer conn.Close()
	// A reader that has gone away must not hold a goroutine open against a pipe that is busy
	// serving a radio.
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(m.Card()); err != nil {
		log.Printf("management: could not answer a status request: %v", err)
	}
}

// ReadStatus fetches and decodes a card from a running tether. It is the client half of the
// status verb, and lives here so that the shape is written down once.
func ReadStatus(path string) (Card, error) {
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return Card{}, fmt.Errorf("no tether answering at %s: %w", path, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var card Card
	if err := json.NewDecoder(conn).Decode(&card); err != nil {
		return Card{}, fmt.Errorf("reading the status from %s: %w", path, err)
	}
	return card, nil
}
