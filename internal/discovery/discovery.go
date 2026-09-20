// Package discovery advertises the served port over mDNS, so that ZHA and zigbee2mqtt find
// the coordinator instead of having to be told where it is.
//
// The contract is narrower than the convention around it looks, and ARCHITECTURE.md
// "Discovery" carries the evidence read out of both clients: `radio_type` and `serial_number`
// are the whole of what anything consumes, and a missing `serial_number` makes ZHA drop the
// advert silently rather than complain. So the job here is to be exactly that advert,
// correctly, and nothing else — no vendor keys, no second service type, no state.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/brutella/dnssd"

	"briard.io/tether/internal/family"
)

const (
	// serviceType is the vendor-neutral type both clients already look for: ZHA's manifest
	// matches it with a `*` name filter and zigbee2mqtt reaches it as
	// `port: mdns://zigbee-coordinator`, so auto-discovery costs no upstream change.
	//
	// ★ It is never `_slzb-06._tcp`. That type belongs to real SMLIGHT hardware:
	// it drives Home Assistant into an HTTP+BLE integration we do not implement, ZHA's
	// `slzb-06*` name filter would put a product name we are not into the user's UI, and it
	// collides with genuine SLZB devices on the same LAN.
	serviceType = "_zigbee-coordinator._tcp"

	// domain is the only one mDNS has.
	domain = "local"

	// nameSuffix makes `avahi-browse` output and ZHA's discovery card traceable: whatever
	// else is on the LAN, the thing answering for this port says what published it. It is the
	// first thing dropped when the name will not fit — see instanceName.
	nameSuffix = " (tether)"

	// nameJoin separates the adapter from the machine it is plugged into.
	nameJoin = " at "

	// maxLabel is the DNS label limit, and the instance name is one label — the service type
	// and the domain are their own, so they do not count against it (RFC 1035 §2.3.4,
	// measured against the library: 63 bytes packs, 64 is "dns: bad rdata").
	//
	// ⚠️ THE GUARD IS LOAD-BEARING, AND WHAT IT PREVENTS IS SILENCE. `dnssd.NewService` accepts
	// an over-long name without complaint, and v1.2.14 then puts NOTHING on the wire and says so
	// to no one: both of its send paths pack the message under `if out, err := m.Pack(); err ==
	// nil` (mdns.go), so an unpackable record is dropped with the error discarded. Measured on a
	// 64-byte label: the responder logs `Sending probe` and `Sending 1st announcement`, returns
	// no error anywhere, and `tcpdump` on the sending interface counts ZERO packets — against 3
	// probes + 2 announcements + 2 goodbyes for a name that fits. An advert nobody can see, from
	// a process reporting success, is the worst of the outcomes available.
	maxLabel = 63
)

// Config is what an advert needs: where the port is, and what is behind it.
type Config struct {
	// Instance is the advertised instance name. Empty derives it from Device and the
	// hostname, which is the only identity available that is stable across restarts without
	// tether storing state — and tether stores none. The config file fills this in for a
	// host that needs to override it.
	Instance string

	// Device is what to call the adapter: the family table's name for it, or — for a stick
	// the table does not carry, named in the config by path and radio type — the last element
	// of that path, which for a /dev/serial/by-id/… name is the vendor's own strings and the
	// serial. Empty is allowed and means the name says only which machine this is, because
	// the derived name says what it knows rather than guessing at the rest.
	Device string

	// Port is the TCP port the server accepted on.
	Port int

	// Address is the one address the advert carries, or nil for the addresses of each
	// interface it goes out on — which is the library's own behaviour, and right whenever the
	// listener is bound to every interface. The caller decides (cmd's advertisedAddress);
	// this only carries it.
	Address net.IP

	// Params are the parameters the device layer actually opened the port with — INV 8: the
	// TXT record says what tether did, never what it would have liked to do.
	Params family.Params
}

// Advert is a built, not-yet-announced advertisement. New makes one out of a Config; Run puts
// it on the wire and takes it off again. Building it touches no socket, so a caller can fail
// on a malformed config before anything has been said to the LAN.
type Advert struct {
	service dnssd.Service
}

// New builds the advert. It validates and derives; it sends nothing and opens nothing.
func New(cfg Config) (*Advert, error) {
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("advertising: %d is not a TCP port", cfg.Port)
	}
	instance := cfg.Instance
	if instance == "" {
		var err error
		if instance, err = hostInstance(cfg.Device); err != nil {
			return nil, err
		}
	} else if n := len(instance); n > maxLabel {
		// A configured name is refused rather than shrunk. The ladder below exists because a
		// derived name is ours to shorten; this one is the operator's, it is what a client
		// keys on, and quietly advertising something other than what they wrote is the worst
		// of the three outcomes available.
		return nil, fmt.Errorf("advertising: the configured instance name is %d bytes and the "+
			"limit is %d — mDNS gives an instance name one DNS label: %q", n, maxLabel, instance)
	}
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("advertising: reading this machine's name: %w", err)
	}
	advertHost, err := hostName(host, cfg.Port)
	if err != nil {
		return nil, err
	}
	service, err := dnssd.NewService(dnssd.Config{
		Name:   instance,
		Type:   serviceType,
		Domain: domain,
		Host:   advertHost,
		Port:   cfg.Port,
		Text:   text(instance, cfg.Params),
		Ifaces: registerOn(),
		IPs:    fixedAddress(cfg.Address),
	})
	if err != nil {
		return nil, fmt.Errorf("building the %s advert for %q: %w", serviceType, instance, err)
	}
	// Asked rather than predicted: the library rewrites a name it reads as carrying mDNS's own
	// rename suffix, and its rule is not the one it looks like. `trimServiceNameSuffixRight`
	// slices the digits test one character short, so " (2)" is trimmed as documented but so are
	// " (a)" and " (12a)" — measured against v1.2.14, all three. Comparing what came back to
	// what we asked for costs one line, needs none of that reasoning, and keeps working if they
	// fix it.
	//
	// Refusing is the same answer an over-long name gets, and for the same reason: this string
	// is what a client keys on, and publishing a quietly different one is worse than saying so.
	if service.Name != instance {
		return nil, fmt.Errorf("advertising: the mDNS library will not carry the instance name "+
			"%q as written — it reads the trailing bracket as the suffix mDNS adds when renaming "+
			"a clashing service, and would publish %q instead. Choose a name that does not end "+
			"in a short bracketed suffix", instance, service.Name)
	}
	return &Advert{service: service}, nil
}

// Instance is the identity this advert carries: the name a ZHA user reads off the discovery
// card, and the serial_number the clients key on. Worth asking after New rather than before,
// because New is where a name gets derived and, if it had to, shrunk to fit its label.
func (a *Advert) Instance() string { return a.service.Name }

// Run announces the service, answers queries for it, and — when ctx is cancelled — withdraws
// it before returning.
//
// The withdrawal is why this is shaped as a blocking call rather than a background goroutine
// with a Stop method: the responder sends the goodbye (a PTR with TTL 0, twice) on the way out
// of Respond, so by the time Run returns the clients have already been told. Skipping it would
// leave a dead coordinator in ZHA's discovery list for the record TTL, 450 s — and a tether
// restart is routine, because an agent supervising it self-updates.
//
// An error here is not fatal to tether and callers must not treat it as such: discovery is how
// a client is spared typing an address, and `socket://host:port` must always work without it.
func (a *Advert) Run(ctx context.Context) error {
	// ⚠️ Cancelled, never expired — see cancelOnly. An advert whose context ends by deadline
	// leaves the library's reader goroutines spinning for the life of the process.
	ctx, stop := cancelOnly(ctx)
	defer stop()

	responder, err := dnssd.NewResponder()
	if err != nil {
		return fmt.Errorf("opening an mDNS responder: %w", err)
	}
	if _, err := responder.Add(a.service); err != nil {
		return fmt.Errorf("registering %q: %w", a.service.ServiceInstanceName(), err)
	}
	log.Printf("discovery: advertising %s on port %d as %v",
		a.service.ServiceInstanceName(), a.service.Port, a.service.Text)
	err = responder.Respond(ctx)
	// Canceled is the only way this context can end now (cancelOnly), whatever the caller's
	// own context did.
	if errors.Is(err, context.Canceled) {
		// Not a failure: the goodbye is already sent, which is the whole point of asking.
		log.Printf("discovery: withdrew %s", a.service.ServiceInstanceName())
		return nil
	}
	return fmt.Errorf("advertising %s: %w", a.service.ServiceInstanceName(), err)
}

// text builds the TXT record. Every key here was read out of the consumers rather than
// copied from the convention:
//
//   - radio_type is the only key with a consumer on both sides. It must be exactly a zigpy
//     RadioType member name, which is what family.Radio holds.
//   - serial_number is required by ZHA >= 2025.1 and its absence is silent: the config flow
//     aborts with invalid_zeroconf_data and the user is never offered the coordinator at all.
//     ZHA uses it verbatim as the config entry's unique_id, so it is an identity and not a
//     label — hence the instance name, which is the one stable identity tether has.
//   - baud_rate and data_flow_control are read by nobody: ZHA hardcodes 115200 and no flow
//     control on this path, herdsman reads only radio_type. They are here because they are
//     the only place an operator can read back what the port was actually opened at, and
//     they carry tether's own values (INV 8) — never the SLZB firmware's "software", which
//     traces to a 2021 proposal, is specified nowhere, and is not true of our port.
//     Nothing may come to depend on them being consumed.
func text(instance string, p family.Params) map[string]string {
	return map[string]string{
		"radio_type":        string(p.Radio),
		"serial_number":     instance,
		"baud_rate":         strconv.Itoa(p.Baud),
		"data_flow_control": string(p.Flow),
	}
}

// hostInstance derives the instance name from the adapter and this machine's hostname.
func hostInstance(device string) (string, error) {
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("deriving an instance name: %w", err)
	}
	return instanceName(device, host)
}

// instanceName builds the advertised identity out of the two facts that distinguish one
// coordinator from another: which adapter, and which machine it is plugged into.
//
// It is read in exactly one place by exactly one person — ZHA renders it as the discovery
// card's title and asks "Do you want to set up <this>?" (config_flow.py strips the service
// type off discovery_info.name into title_placeholders, and strings.json renders
// flow_title "{name}"). After setup it disappears: the config entry is created with title "".
// zigbee2mqtt never reads it at all — herdsman's findMdnsAdapter does findOne by service type
// and takes the first responder. So the card is the whole audience, and the two things that
// card cannot otherwise tell its reader are the stick and the host.
//
// Invisibly, the same string is the TXT serial_number, which ZHA makes the discovery flow's
// unique_id. That is what fixes the two properties this function must hold to: it must be
// unique on the LAN — two coordinators sharing it means HA aborts the second flow as
// already_in_progress and one of them is never offered — and stable for a given adapter on a
// given host, because a user who dismissed the card with Ignore has that exact string
// persisted in an ignored config entry, and changing it brings the card back.
func instanceName(device, host string) (string, error) {
	host, err := machineName(host)
	if err != nil {
		return "", err
	}
	device = strings.TrimSpace(device)

	// The rungs, in order: shed decoration before information, and truncate the longer half
	// before the shorter one, so whichever fact is scarcer survives.
	for _, name := range []string{
		join(device, host) + nameSuffix,
		join(unparenthesised(device), host) + nameSuffix,
		// Dropping the suffix is not a rung of its own, though it reads like one: levelled
		// starts from the suffix-free name and cuts nothing when nothing needs cutting, so a
		// separate rung here would be the same string computed twice. Proved by deleting it —
		// no assertion moved.
		join(levelled(unparenthesised(device), host)),
		join(halved(unparenthesised(device), host)),
	} {
		if len(name) <= maxLabel {
			return name, nil
		}
	}
	// Unreachable: the last rung caps both halves so their total cannot exceed the label.
	// Saying so out loud beats returning something that silently does not fit.
	return "", fmt.Errorf("deriving an instance name: %q at %q will not fit in %d bytes",
		device, host, maxLabel)
}

// machineName reduces a hostname to the part that is this machine, and refuses the names that
// identify nothing — the same refusal the family table makes about an unknown adapter, and for
// the same reason: this value becomes a client's unique_id, and a wrong one is a coordinator
// that quietly never appears, or quietly displaces another.
func machineName(host string) (string, error) {
	// A hostname may arrive fully qualified; the first label is the part that is this
	// machine, and the rest is the network it happens to be on today.
	host, _, _ = strings.Cut(host, ".")
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || host == "localhost" {
		return "", fmt.Errorf("deriving an instance name: this machine calls itself %q, "+
			"which no other machine can tell apart from itself; name the instance "+
			"explicitly in the config", host)
	}
	return host, nil
}

// join puts the two halves together, and drops the adapter half when there is none rather
// than advertising a dangling "at".
func join(device, host string) string {
	if device == "" {
		return host
	}
	return device + nameJoin + host
}

// unparenthesised drops the bracketed asides from an adapter's name — "Sonoff ZBDongle-P
// (CC2652P)" is the chip, "(ZBDongle-M)" the vendor's other name for the same stick. Useful
// when it fits, and the first information to go when it does not. It never empties the name:
// a name that is nothing but brackets keeps them, because something is owed to the card.
func unparenthesised(device string) string {
	var b strings.Builder
	depth := 0
	for _, r := range device {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	if out := tidy(b.String()); out != "" {
		return out
	}
	return device
}

// levelled takes the excess off the longer half, which is where it is cheapest: two
// coordinators are told apart by whichever half differs between them, and the longer name has
// the most left over once it has said which thing it is. It cuts by what is needed and no
// further than the length of the shorter half — past that point the two are equally scarce,
// and halved is what handles it.
func levelled(device, host string) (string, string) {
	over := len(device) + len(nameJoin) + len(host) - maxLabel
	if device == "" || host == "" || over <= 0 {
		return device, host
	}
	switch {
	case len(device) > len(host):
		return cut(device, max(len(host), len(device)-over)), host
	case len(host) > len(device):
		return device, cut(host, max(len(device), len(host)-over))
	}
	return device, host
}

// halved is the last rung: both halves cut to the same length, chosen so the result fits
// whatever the inputs were. Reached only when the two are already the same length and still
// too long together, which needs a hostname and an adapter name of about thirty bytes each.
func halved(device, host string) (string, string) {
	each := (maxLabel - len(nameJoin)) / 2
	if device == "" {
		return "", cut(host, maxLabel)
	}
	return cut(device, each), cut(host, each)
}

// cut truncates to at most n bytes without splitting a rune, and tidies what is left so a
// truncated name does not end mid-punctuation.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return tidy(s[:n])
}

// tidy removes the whitespace and trailing joining punctuation a cut leaves behind.
func tidy(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), " -_.")
}

// cancelOnly returns a context that can only ever finish as context.Canceled, together with the
// function that finishes it. It ends when the parent does, for whatever reason the parent had.
//
// ⚠️ **This is a workaround for the mDNS library, not a style.** Its socket readers — two
// goroutines, one for IPv4 and one for IPv6 — test `ctx.Err() == context.Canceled` and exit on
// nothing else (`dnssd/mdns.go`, `readInto`). Hand it a context that ends by *deadline* and they
// keep looping on a socket somebody else has closed, with `ReadFrom` failing instantly and no
// syscall to sleep in: a tight spin, for the life of the process.
//
// **Measured on a Pi and reproduced here: 250% of a CPU, starting exactly 3 seconds after
// startup** — `ScanTime` — on an otherwise idle tether with no client attached. A neighbour
// scan handed a `context.WithTimeout` burns two cores for as long as the tether runs. `go tool
// pprof` shows nothing, because the spin makes no syscalls and the goroutines are in the
// runtime's preemption path rather than in ours; what shows it is `strace -c` — 27k `tgkill`
// preemption signals in six seconds — and CPU that starts at t+3s.
//
// ⚠️ **`context.WithCancel` alone is not enough**, which is why this is three lines rather than
// one: a cancel context whose *parent* expires by deadline reports the parent's error, so the
// library would see DeadlineExceeded through it. `WithoutCancel` detaches from the parent's
// error and `AfterFunc` reattaches to its completion, which keeps the lifetime and drops the
// reason.
func cancelOnly(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stop := context.AfterFunc(parent, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// ScanTime is how long Neighbours listens. mDNS answers arrive within a second or two on a
// quiet LAN; this is generous because the result is a log line nobody is waiting on.
const ScanTime = 3 * time.Second

// Neighbours browses for other coordinators advertising the service type we use, and returns
// their instance names — ours excluded.
//
// **It is a warning, not a check.** tether advertises whatever this finds: the operator learns
// the true state of their LAN and decides what to do about it, because the alternative —
// declining to advertise — produces the same outcome as the clash it would be avoiding, with
// tether invisible instead of one of the two coordinators.
//
// Only our own service type is browsed. A vendor gateway on `_slzb-06._tcp` is a separate
// discovery path in ZHA and not a rival for zigbee2mqtt's `findOne`, which asks for one type.
func Neighbours(ctx context.Context, self string) []string {
	// A timer that *cancels*, rather than a deadline — see cancelOnly, and note that
	// context.WithTimeout here is what put two spinning goroutines in every tether that
	// advertises.
	ctx, stop := cancelOnly(ctx)
	defer stop()
	defer time.AfterFunc(ScanTime, stop).Stop()

	var mu sync.Mutex
	seen := map[string]bool{}
	// The error is deliberately dropped: a LAN we cannot browse is not a reason to say
	// anything, and certainly not a reason to hold up an advert.
	_ = dnssd.LookupType(ctx, serviceType+"."+domain+".", func(e dnssd.BrowseEntry) {
		if e.Name == self {
			return
		}
		mu.Lock()
		seen[e.Name] = true
		mu.Unlock()
	}, func(dnssd.BrowseEntry) {})

	mu.Lock()
	defer mu.Unlock()
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// hostPrefix marks the A record as ours on a LAN that is not.
const hostPrefix = "briard-tether-"

// hostName is the SRV target: the one name this advert publishes an A record for.
//
// ⚠️ IT IS MINTED RATHER THAN INHERITED, and that is the whole reason it exists. Left empty,
// `dnssd.Config.Host` falls back to `os.Hostname()` — the name this machine's OWN responder
// (avahi, systemd-resolved) already claims and publishes addresses for. A second claimant is
// harmless while both answer the same addresses, because identical records are not a conflict
// (RFC6762 8.2) — and a real one the moment `-advertise-address` pins a single address that the
// host's own responder does not carry. RFC6762 §9 then has BOTH parties re-probe, so the
// machine's own `.local` identity can be renamed by us, on a machine we are a guest on.
//
// NOBODY RESOLVES IT, so it owes nothing to legibility. A responder ships the A record as an
// ADDITIONAL in its browse answer (RFC6763 §12), so ZHA and zigbee2mqtt take the address
// straight out of the reply — herdsman takes the first one it is handed, which is how a
// loopback address in that list once broke discovery outright. The name is a required slot in
// the record shape, not a thing anyone looks up.
//
// THE PORT IS IN IT for two adapters on one machine: same host, two tethers, two ports, two
// names. Without it both would claim one name — identical addresses, so mDNS shadows rather
// than conflicts, but two coordinators sharing an A record is a coincidence rather than a plan.
func hostName(host string, port int) (string, error) {
	machine, err := machineName(host)
	if err != nil {
		return "", err
	}
	suffix := "-" + strconv.Itoa(port)
	// One label, the same limit the instance name has. The machine half is what gets cut,
	// because the prefix says whose record this is and the port is what tells two adapters
	// apart. A cut that made two machines collide costs nothing a user sees: the responder
	// probes, renames its own host record, and the instance name — which IS user-visible, and
	// is what ZHA keys on — is untouched.
	if room := maxLabel - len(hostPrefix) - len(suffix); len(machine) > room {
		if room < 1 {
			return "", fmt.Errorf("deriving a host name: port %d leaves no room for a machine "+
				"name in %d bytes", port, maxLabel)
		}
		machine = strings.TrimRight(machine[:room], "-")
	}
	return hostPrefix + machine + suffix, nil
}
