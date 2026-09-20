package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"strings"
	"time"

	"briard.io/tether/internal/device"
	"briard.io/tether/internal/discovery"
	"briard.io/tether/internal/family"
	"briard.io/tether/internal/management"
	"briard.io/tether/internal/pipe"
	"briard.io/tether/internal/server"
)

// Options is where tether is pointed: which device, which address, which family. Nothing in
// here changes how tether behaves — it is deployment wiring, and the config file is what fills
// it in.
type Options struct {
	// DevicePath is the serial port. **Empty is the normal case**: tether detects the adapter
	// itself, because the family table already knows every supported stick and almost every
	// host has exactly one of them. Setting it is how an operator settles the case tether
	// refuses to guess at — two compatible adapters attached at once.
	//
	// When it is set, prefer a /dev/serial/by-id/… name: that one survives a reboot and a
	// replug, and the kernel's ttyUSB0 does not. Detection prefers it for you. Either way it
	// is resolved afresh on every open, which is what makes a replug recoverable — the name
	// is stable, the node behind it is not.
	DevicePath string

	// Listen is the TCP address for clients, host:port.
	Listen string

	// Radio names the family instead of asking the USB layer. Empty reads the adapter's
	// descriptors, which is the normal case and the one that needs no configuration at all.
	Radio family.Radio

	// Baud overrides the family's. Zero takes the table's, which is what almost everything
	// wants.
	Baud int

	// Instance overrides the advertised identity. Empty derives it from the hostname, which
	// is the whole of the identity story.
	Instance string

	// Advertise is whether to put the mDNS advert on the wire. True is the normal case and
	// the whole of the zero-config story; false is for the second coordinator on a LAN, which
	// discovery cannot serve correctly for either client.
	//
	// ⚠️ Its zero value is therefore the *unusual* answer, and the only thing that supplies
	// the usual one is loadOptions. A hand-built Options — which in practice means a test —
	// has to say `Advertise: true` or it will quietly not advertise, and a test asserting on
	// the advert would fail in a way that looks like the advert being broken.
	Advertise bool

	// AdvertiseAddress is the one address the advert carries, as a string an operator wrote.
	// Empty derives it — see advertisedAddress.
	AdvertiseAddress string

	// StatusSocket is where `tether status` reads the card.
	StatusSocket string
}

// The retry schedule for getting hold of the device. It starts fast because the boot-order
// race resolves in well under a second — tether starts, udev creates the node a moment later —
// and it caps low because the other case is a human plugging the dongle back in, where the
// cost of noticing late is dead time a client spends being refused. One attempt is an open(2)
// on a path that usually does not exist, so polling at the cap costs nothing worth measuring,
// and nothing here needs a device-notification mechanism to go with it.
const (
	firstRetry = 250 * time.Millisecond
	maxRetry   = 5 * time.Second

	// quietFor is how long a failure stays out of the log after it has been said once. A
	// dongle left unplugged over a weekend must not bury the lines around it, and the fact
	// being repeated does not change.
	quietFor = time.Minute
)

// retryDelay is how long to wait before attempt n, counting from 1.
func retryDelay(attempt int) time.Duration {
	d := firstRetry
	for i := 1; i < attempt; i++ {
		if d >= maxRetry {
			return maxRetry
		}
		d *= 2
	}
	if d > maxRetry {
		return maxRetry
	}
	return d
}

// serve runs tether until ctx ends: it stands up the parts that outlive any one adapter, then
// holds a device for as long as one is there, finding or reopening it whenever one is not.
//
// Note what is *not* done first: identifying the radio. tether may have to wait for an adapter
// to be plugged in, and it must be able to answer `tether status` throughout that wait — "I am
// waiting, and here is why" is the answer somebody needs at exactly that moment. So the census
// and the monitor are built not knowing, and told when there is something to know.
//
// It returns nil on a clean shutdown. The errors it does return are the ones no retry can fix —
// an address that will not bind, a dongle swapped for a different family — so whatever
// supervises tether should report them rather than restart blindly into them.
func serve(ctx context.Context, opts Options) error {
	census := management.NewCensus()
	p := pipe.New(census.Observe)
	monitor := management.NewMonitor(p, census)

	// The status server is waited for on the way out, because returning is how the socket
	// file gets removed: its listener unlinks the path when it closes, and a serve that
	// returns first leaves main to exit with that close still pending — measured at 13 of 160
	// orderly stops leaving a `status.<pid>.sock` behind, on a path nothing sweeps. The
	// context is derived so the wait cannot hang when hold returns on its own error, which
	// does not cancel the caller's.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		// Losing the status socket costs observability; taking the radio down to punish that
		// would be the cure being worse than the disease.
		if err := monitor.ServeStatus(ctx, opts.StatusSocket); err != nil {
			log.Printf("management: status is not readable from outside: %v", err)
		}
	}()
	go monitor.Run(ctx, management.DefaultInterval, management.ReportInterval)

	err := ignoreCancel(hold(ctx, opts, p, monitor))
	cancel()
	<-statusDone
	return err
}

// target is the answer to "which adapter, opened how" — one device, settled afresh.
type target struct {
	path   string
	params family.Params
	name   string // the table's human name, empty for an adapter it does not carry

	// usb is what the adapter says about itself, where there was anything to read. It is the
	// second-best answer to "what is this", and the only one for a stick the table does not
	// name — see adapterLabel.
	usb family.Device
}

// locate answers that question from scratch, every time it is asked, and asking it again is how
// a replug is survived: the by-id name is stable but the node under it is not, and a detected
// adapter may come back on a different tty entirely.
//
// It is the whole of the zero-config story. An unset device path means "find it" rather
// than "fail": the family table already knows every supported stick, so tether can look. What it
// will not do is guess between two of them — that is the one case where a wrong answer is worse
// than no answer, because opening the wrong coordinator is how a client adopts the wrong Zigbee
// network. Two attached is therefore an absence with a reason on it, not a choice.
func locate(opts Options) (target, error) {
	path, params, name := opts.DevicePath, family.Params{}, ""
	var usb family.Device

	switch {
	case path == "":
		found, err := device.Detect()
		if err != nil {
			return target{}, err
		}
		switch len(found) {
		case 0:
			return target{}, errors.New("no compatible adapter is attached")
		case 1:
			path, params, name = found[0].Path, found[0].Params, found[0].Name
		default:
			return target{}, ambiguous(found)
		}
	case opts.Radio == "":
		// A path we were handed, and no family named: the adapter's own descriptors say which
		// radio it is, and the family table would rather error than guess at one it does not
		// know.
		d, err := device.DescribeUSB(path)
		if err != nil {
			return target{}, fmt.Errorf("identifying the adapter at %s: %w", path, err)
		}
		if params, err = family.Resolve(d); err != nil {
			return target{}, err
		}
		name, _ = family.Identify(d)
		usb = d
	default:
		// A path and a family both configured. The override settles the radio type below, but
		// the adapter can still say what it is and usually does: naming the family is what
		// someone does after reflashing a stick the table knows perfectly well, and the name
		// — and the row's parameters, see withRadio — are about the hardware in their hand
		// rather than the firmware on it.
		//
		// A failure to read it is not an error on this branch, and that is the point of the
		// branch: this is also the path that serves a device with no USB descriptors to read
		// — an on-board UART, or a stick behind a bridge sysfs does not walk — where refusing
		// would turn the escape hatch into another way to fail.
		if d, err := device.DescribeUSB(path); err == nil {
			name, _ = family.Identify(d)
			usb = d
			// Best-effort for the same reason: a row is kept when there is one, and there is
			// none for a stick the table does not carry, which is what the override is for.
			params, _ = family.Resolve(d)
		}
	}

	if opts.Radio != "" {
		var err error
		if params, err = withRadio(params, opts.Radio); err != nil {
			return target{}, err
		}
	}
	return target{path: path, params: withBaud(params, opts.Baud), name: name, usb: usb}, nil
}

// withRadio applies the configured family, if there is one. It settles the radio type and
// nothing else: a stick the table recognises keeps its row's baud and flow control, because
// those belong to the bridge in the operator's hand — the ZBT-2's 460800, the six rows' RTS/CTS
// — and reflashing the radio behind it changes neither. Replacing the row with the family's
// defaults would open a ZBT-2 at 115200 with no flow control: confidently wrong parameters,
// which is the failure the family table exists to prevent, produced by a key an operator is
// told is safe to set.
//
// Only a stick with no row takes the family's defaults, which is the escape hatch the key
// exists for; `baud` sits on top of either for the firmware that wants another rate.
func withRadio(params family.Params, radio family.Radio) (family.Params, error) {
	defaults, err := family.Defaults(radio)
	if err != nil {
		return family.Params{}, err
	}
	if params == (family.Params{}) {
		return defaults, nil
	}
	params.Radio = radio
	return params, nil
}

// ambiguous is the refusal to choose between attached adapters. It names both, because the
// operator's remedy is to say which one they meant and they cannot do that from a count.
func ambiguous(found []device.Adapter) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d compatible adapters are attached and tether will not guess between them:", len(found))
	for i, a := range found {
		if i > 0 {
			b.WriteString(";")
		}
		// Name first, path second: the name is what an operator recognises as the thing in
		// their hand, and the path is what they then paste into the config.
		name := a.Name
		if name == "" {
			name = a.Device.String()
		}
		fmt.Fprintf(&b, " %s at %s", name, a.Path)
	}
	// Deliberately not "unplug the other" — there may be more than two, and an instruction
	// that is wrong in the three-adapter case is worse than one that is general.
	b.WriteString(" — name one of those paths in the config, or leave only one attached")
	return errors.New(b.String())
}

// withBaud applies the configured override, if there is one. An absent key means "derive it",
// never "zero".
func withBaud(params family.Params, baud int) family.Params {
	if baud != 0 {
		params.Baud = baud
	}
	return params
}

// hold keeps a device open for as long as one is there and finds it again when it is not —
// surviving an unplug and a replug without a restart, and without the radio being touched by
// any of it (INV 1).
func hold(ctx context.Context, opts Options, p *pipe.Pipe, m *management.Monitor) error {
	var radio family.Radio
	for {
		port, at, err := open(ctx, opts, m)
		if err != nil {
			return err
		}
		// A different family is a different coordinator, not a reopen: radio_type is half the
		// advertised identity a client keys its config entry on, and the census is built for
		// one protocol. Stopping hands a clean slate to whatever supervises tether, which is
		// free — INV 7 means a restart moves no line on the radio.
		if radio != "" && at.params.Radio != radio {
			port.Close()
			return fmt.Errorf("the adapter at %s is now a %s radio, not a %s; "+
				"stopping so this restarts as the coordinator it actually is",
				at.path, at.params.Radio, radio)
		}
		radio = at.params.Radio
		m.DeviceOpened(radio)

		err = generation(ctx, opts, at, p, port, m)

		m.NoDevice("the device stopped")
		// Closing a port whose device has already gone fails, and says nothing anyone can act
		// on: the interesting error was the one that ended the generation.
		_ = port.Close()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("tether: no device — the client is closed and the advert withdrawn; " +
			"looking for the adapter again until it comes back")
	}
}

// open finds a device and opens it, retrying until it does. Every failure is worth retrying: no
// adapter attached yet (the boot-order race), two attached and tether refusing to pick, one
// still enumerating, another process that has not let go. Staying up through all of them is the
// point — the alternative is a tether that needs a human to restart it after a dongle is
// plugged back in, and the whole reason for detecting one is that nobody should have to.
func open(ctx context.Context, opts Options, m *management.Monitor) (*device.Port, target, error) {
	var lastLog time.Time
	for attempt := 1; ; attempt++ {
		m.CountOpenAttempt()

		at, err := locate(opts)
		if err == nil {
			var port *device.Port
			if port, err = device.Open(at.path, at.params); err == nil {
				if at.name != "" {
					log.Printf("device: %s is a %s", at.path, at.name)
				}
				return port, at, nil
			}
			err = fmt.Errorf("cannot open %s: %w", at.path, err)
		}

		// Recorded on every attempt and logged on few: the card is read on demand and must be
		// current, the log is read in sequence and must stay legible through a long outage.
		m.NoDevice(err.Error())
		if sayAgain(&lastLog) {
			log.Printf("device: %v; retrying — tether stays up, and starts serving the moment "+
				"this resolves", err)
		}
		if err := pause(ctx, retryDelay(attempt)); err != nil {
			return nil, target{}, err
		}
	}
}

// generation serves one device, from open port to device failure. It returns nil when the
// device went away — go round again — and an error only for something a retry cannot fix.
//
// The listener and the advert live and die with the device, and that is not tidiness. INV 2
// says the port is open, configured and drained before the listener accepts; the same rule
// read the other way says that with no port there must be no listener, so a client is refused
// outright rather than accepted into a silence (INV 3). The advert follows for the same
// reason: a coordinator that is not there must stop saying it is.
func generation(ctx context.Context, opts Options, at target, p *pipe.Pipe, port *device.Port, m *management.Monitor) error {
	// The device is bound first and the listener opened second: that is INV 2's ordering, and
	// Serve is shaped to make it the only way round that compiles into sense.
	served := p.Serve(port)

	ln, err := server.Listen(opts.Listen)
	if err != nil {
		return listenFailure(opts.Listen, err)
	}

	gctx, endGeneration := context.WithCancel(ctx)
	defer endGeneration()

	accepting := make(chan error, 1)
	go func() { accepting <- server.Serve(ln, p) }()
	instance, withdrawn := advertise(gctx, opts, adapterLabel(at), at.params, ln.Addr())

	// What this tether is and where, on the card. It is how a second one on the same host is
	// told apart from the first — a process id is not something anyone recognises — and it is
	// known only now, because the advertised name carries the adapter.
	m.Serving(instance, ln.Addr().String())

	// A generation ends either because the device did, or because tether is shutting down.
	// Shutdown does not wait for a device error: the copy goroutine is blocked in a read that
	// only the port closing will end, and hold closes it the moment this returns.
	select {
	case err := <-served:
		log.Printf("tether: the device stopped: %v", err)
	case <-ctx.Done():
	}

	endGeneration()
	ln.Close()
	<-accepting
	// Wait for the goodbye to be on the wire before anything could announce again, so a
	// client is never told about a coordinator that has already gone.
	<-withdrawn
	return nil
}

// adapterLabel is what the advert calls this adapter — half of the identity a ZHA user reads
// off the discovery card, and ARCHITECTURE.md "Discovery" has the rest of that story. Three
// answers, in descending order of how much they know, and the first two are the same string
// whether the adapter was detected or named in the config: picking a path by hand must not
// change what the thing is called.
//
//  1. The family table's name, which is a name someone chose for humans to read.
//  2. What the adapter says about itself, for a stick the table does not carry. Still the
//     vendor's own words, and still something a person recognises.
//  3. The last element of the path — reached only when there are no USB descriptors at all,
//     which means a device that is not USB: an on-board UART, or a port behind a bridge the
//     sysfs walk does not follow. Thin, but it is what is in the config and what they will
//     recognise.
//
// Only the first goes in the log line above. "ttyUSB0 is a ttyUSB0" is noise, and that line
// means "the table recognised this", which is a narrower claim than the card needs to make.
func adapterLabel(at target) string {
	if at.name != "" {
		return at.name
	}
	if label := at.usb.Label(); label != "" {
		return label
	}
	return filepath.Base(at.path)
}

// advertisedAddress is what the advert says the coordinator's address is — one address, or nil
// for "the addresses of whichever interfaces the advert goes out on".
//
// The operator's word wins. Failing that, the listener's own bound address when it is a
// specific one: an operator who bound one interface has already said which address clients
// should use, and an advert naming any other would contradict the config it came from. Only a
// wildcard bind leaves the question open, and then the per-interface answer is the right one —
// it is the only one that follows a NIC that comes up after tether does.
func advertisedAddress(explicit string, bound net.IP) net.IP {
	if explicit != "" {
		return net.ParseIP(explicit) // validated at load; nil here would be a bug, not a config
	}
	if bound != nil && !bound.IsUnspecified() {
		return bound
	}
	return nil
}

// advertise puts the advert on the wire for as long as ctx lasts, and returns a channel closed
// once it has been withdrawn.
//
// A failure to advertise is never fatal and callers must not treat it as one: discovery is
// how a client is spared typing an address, and socket://host:port always works without it.
func advertise(ctx context.Context, opts Options, name string, params family.Params, addr net.Addr) (string, <-chan struct{}) {
	done := make(chan struct{})
	if !opts.Advertise {
		// Said once per generation rather than silently: an operator who expected a coordinator
		// to appear in ZHA and set this months ago needs the log to remind them they did.
		log.Printf("discovery: not advertising, because the config turned it off; "+
			"point clients at %s directly", addr)
		close(done)
		return "", done
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		close(done)
		return "", done
	}
	advert, err := discovery.New(discovery.Config{
		Instance: opts.Instance,
		// The adapter the table recognised, which is half of what the derived name says.
		// Empty when the table has no name for it, and the name then says less rather than
		// guessing at one.
		Device:  name,
		Port:    tcp.Port,
		Address: advertisedAddress(opts.AdvertiseAddress, tcp.IP),
		// INV 8: the TXT says what tether actually opened the port with, never what it would
		// have liked to.
		Params: params,
	})
	if err != nil {
		log.Printf("discovery: not advertising: %v; clients can still be pointed at %s directly", err, addr)
		close(done)
		return "", done
	}
	// What stands in for a clash check: say what else is out there and advertise anyway.
	// tether does not decide for the operator, and must never make itself invisible in the
	// name of tidiness — a refusal to advertise is the same outcome as the clash it would be
	// preventing, self-inflicted.
	//
	// In the background, because browsing costs seconds and the advert must not wait for it.
	go warnAboutNeighbours(ctx, advert.Instance())

	go func() {
		defer close(done)
		// A cancelled advert is this generation ending, not a failure to advertise: the
		// device went away and we asked for the withdrawal ourselves. Logging it as a fault
		// would put a scary line in the log on every unplug, which is exactly when the log
		// needs to be readable.
		if err := advert.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("discovery: not advertising: %v; clients can still be pointed at %s directly", err, addr)
		}
	}()
	return advert.Instance(), done
}

// pause waits, or gives up early because tether is shutting down.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// sayAgain rate-limits a repeating failure to one line per quietFor, and updates the stamp
// when it says yes.
func sayAgain(last *time.Time) bool {
	if !last.IsZero() && time.Since(*last) < quietFor {
		return false
	}
	*last = time.Now()
	return true
}

// ignoreCancel turns a shutdown into a clean exit. Being asked to stop is not a failure, and
// whatever supervises tether reads the exit code.
func ignoreCancel(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// listenFailure says the thing the kernel cannot. "address already in use" is a true account
// of what happened and a poor account of why: on a host with two dongles the cause is almost
// always the other tether, and the remedy is a key in this one's config.
//
// It is a fixed sentence rather than a lookup. Reading the other tether's status socket to name
// it would be a second way to discover what `tether status` already reports, for one line of a
// message — and this one sends the reader to that verb instead.
func listenFailure(addr string, err error) error {
	return fmt.Errorf("cannot serve on %s: %w — is tether already running on this host? "+
		"`briard-tether status` will say; a second one needs a listen address of its own in the config",
		addr, err)
}

// warnAboutNeighbours says what else is advertising a coordinator on this LAN, and then says
// nothing more about it — tether has already advertised by the time this finishes.
//
// One mDNS coordinator per LAN is the supported configuration, because zigbee2mqtt's mdns://
// takes the first responder at every start and cannot be talked out of it. This is how an
// operator finds out they are in the unsupported case, instead of watching a client attach to
// the wrong radio and wondering which part is broken.
func warnAboutNeighbours(ctx context.Context, self string) {
	others := discovery.Neighbours(ctx, self)
	if len(others) == 0 {
		return
	}
	log.Printf("discovery: %d other Zigbee coordinator(s) are advertising on this LAN: %s. "+
		"tether is advertising anyway, but only one of them can be discovered reliably — "+
		"zigbee2mqtt takes whichever answers first, at every start. Point clients at "+
		"tcp://host:port directly, or set \"advertise\": false on the ones that should not be "+
		"found", len(others), strings.Join(others, ", "))
}
