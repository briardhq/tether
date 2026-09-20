package discovery

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/brutella/dnssd"

	"briard.io/tether/internal/family"
)

// This is the mechanism test for discovery, and it uses the real one: a responder announcing
// on the real multicast group and a real browser finding it, the way a client does — don't
// mock the mechanism you are verifying. Asserting on the dnssd.Service we built
// would only prove we can fill in a struct; every failure that matters here — a service type
// nothing browses for, a TXT key that never reaches the wire, an advert that outlives the
// process — is invisible from inside the program.
func TestAdvertReachesTheWireAndIsWithdrawn(t *testing.T) {
	requireMulticast(t)

	const (
		port  = 20108
		radio = family.ZNP
		baud  = 115200
	)
	instance := uniqueInstance()

	advert, err := New(Config{
		Instance: instance,
		Port:     port,
		Params:   family.Params{Radio: radio, Baud: baud, Flow: family.FlowNone},
	})
	if err != nil {
		t.Fatalf("building the advert: %v", err)
	}

	// These two browsers run for the whole test, so the withdrawal below is observed as it
	// happens rather than inferred from a later silence.
	watchCtx, stopWatching := context.WithCancel(context.Background())
	defer stopWatching()
	ours := watch(t, watchCtx, serviceType+"."+domain+".")
	// ★ Never advertise _slzb-06._tcp. Asserted against a browser that would
	// really find it, because that is how Home Assistant finds it too.
	slzb := watch(t, watchCtx, "_slzb-06._tcp."+domain+".")

	runCtx, stopAdvertising := context.WithCancel(context.Background())
	run := make(chan error, 1)
	go func() { run <- advert.Run(runCtx) }()

	// A browse started now is what a client starting now would see, and it is answered only
	// once the responder has finished probing and announced — which is the point at which
	// the advert genuinely exists.
	entry, ok := browseFor(instance, 30*time.Second)
	if !ok {
		t.Fatalf("no advert for %q under %s after 30s", instance, serviceType)
	}

	if entry.Port != port {
		t.Errorf("advertised port %d, want %d", entry.Port, port)
	}
	if len(entry.IPs) == 0 {
		t.Error("advert carries no address, so a client that found it could not connect")
	}
	for key, want := range map[string]string{
		"radio_type":        string(radio),
		"serial_number":     instance,
		"baud_rate":         "115200",
		"data_flow_control": string(family.FlowNone),
	} {
		if got := entry.Text[key]; got != want {
			t.Errorf("TXT %s on the wire = %q, want %q (whole record: %v)", key, got, want, entry.Text)
		}
	}

	// "Withdrawn cleanly on shutdown" means the clients are told, not merely that we go
	// quiet: a browser already listening must see the service disappear. That is the goodbye
	// packet, and it is the difference between a coordinator that leaves ZHA's list now and
	// one that sits in it for the 450 s record TTL — with tether restarts being routine,
	// because an agent supervising it self-updates.
	stopAdvertising()
	if err := <-run; err != nil {
		t.Errorf("Run returned %v, want nil on a cancelled context", err)
	}
	if !ours.removed(instance, 15*time.Second) {
		t.Errorf("a browser still had %q 15s after shutdown; no removal was seen", instance)
	}

	if name, seen := slzb.any(); seen {
		t.Errorf("advertised %q as _slzb-06._tcp, which belongs to real SMLIGHT hardware", name)
	}
}

// requireMulticast decides whether a test may put a real advert on a real multicast group, and
// the answer is no unless somebody asked for it.
//
// ⚠️ **A bind check is not the gate, and taking it for one is backwards.** "Can this process
// bind 224.0.0.251:5353" skips in a sandbox, which has nothing to pollute, and runs at full
// volume on a developer's home network, which has their real Home Assistant on it.
// `TestTheAdvertLeadsAClientToTheRadio` publishes the service type out of ZHA's own manifest, so
// what that produces is a discovery card on somebody's actual Home Assistant, every `go test`.
//
// So it is opt-in, and `scripts/wire-tests.sh` is what opts in: it builds a network namespace
// with a veth pair in it and runs these there, where the segment is real and nobody else is on
// it. The bind check stays underneath as the second gate — an environment that cannot carry
// multicast still says so rather than failing obscurely.
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

// uniqueInstance keeps concurrent runs — and a real tether on the same LAN — out of each
// other's way. A name clash would be answered by the responder's own probing, which renames
// the service under the test rather than failing it.
func uniqueInstance() string {
	return fmt.Sprintf("tether-test-%d-%04x", os.Getpid(), rand.Intn(1<<16))
}

// browseFor browses until the named instance answers with a complete record, or the deadline
// passes. Each browse is short-lived and closes its sockets, and each one asks the question a
// client asks: a PTR query for the service type, answered with the SRV and TXT alongside.
//
// Retrying is not papering over flakiness — before the responder has probed (RFC 6762 wants
// up to a second of it) there is deliberately nothing to answer, and a client arriving then
// would retry too.
func browseFor(instance string, within time.Duration) (dnssd.BrowseEntry, bool) {
	deadline := time.Now().Add(within)
	for {
		found := make(chan dnssd.BrowseEntry, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = dnssd.LookupType(ctx, serviceType+"."+domain+".",
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

// watcher is a browse that runs for the length of the test, so that a disappearance is an
// event it sees rather than something inferred.
type watcher struct {
	mu   sync.Mutex
	seen map[string]bool
	gone map[string]bool
}

func watch(t *testing.T, ctx context.Context, service string) *watcher {
	t.Helper()
	w := &watcher{seen: map[string]bool{}, gone: map[string]bool{}}
	go func() {
		err := dnssd.LookupType(ctx, service,
			func(e dnssd.BrowseEntry) {
				w.mu.Lock()
				defer w.mu.Unlock()
				w.seen[e.Name] = true
			},
			func(e dnssd.BrowseEntry) {
				w.mu.Lock()
				defer w.mu.Unlock()
				w.gone[e.Name] = true
			})
		if err != nil && ctx.Err() == nil {
			t.Errorf("browsing for %s: %v", service, err)
		}
	}()
	return w
}

func (w *watcher) removed(name string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		w.mu.Lock()
		gone := w.gone[name]
		w.mu.Unlock()
		if gone || time.Now().After(deadline) {
			return gone
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (w *watcher) any() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for name := range w.seen {
		return name, true
	}
	return "", false
}

// The clash warning, on the real multicast group: an advert that is actually on the wire has
// to be visible to a browse, and our own must not be.
//
// This is the assertion that matters, because the whole value of the feature is telling an
// operator the true state of their LAN — a browse that quietly found nothing would look exactly
// like a LAN with nothing on it.
func TestNeighboursSeesAnotherCoordinatorAndNotItself(t *testing.T) {
	requireMulticast(t)

	neighbour := uniqueInstance()
	advert, err := New(Config{
		Instance: neighbour,
		Port:     16638,
		Params:   family.Params{Radio: family.ZNP, Baud: 115200, Flow: family.FlowNone},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = advert.Run(ctx) }()

	// Wait until it is genuinely answering before asking what is out there, so a slow announce
	// cannot be mistaken for the browse not working.
	if _, ok := browseFor(neighbour, 10*time.Second); !ok {
		t.Fatal("the neighbour never appeared on the wire")
	}

	// Counted with the advert up and answering, so that the only thing this can see is what the
	// two scans below leave behind. The advert's own goroutines are supposed to be there.
	before := runtime.NumGoroutine()

	found := Neighbours(ctx, "some-other-tether")
	if !slices.Contains(found, neighbour) {
		t.Errorf("Neighbours did not see %q, which is on the wire: %v", neighbour, found)
	}

	// And a tether does not report itself, which would make every single-coordinator LAN warn.
	if found = Neighbours(ctx, neighbour); slices.Contains(found, neighbour) {
		t.Errorf("Neighbours reported our own advert %q back to us: %v", neighbour, found)
	}

	// **A finished scan leaves nothing running.** Two scans have just happened, and each used
	// to leave the library's IPv4 and IPv6 readers looping on a closed socket — measured at
	// 250% of a CPU on an idle tether, for the life of the process (cancelOnly). The count is
	// the instrument because the symptom is invisible to pprof: those goroutines spin in the
	// runtime's preemption path rather than in any code of ours.
	settled := runtime.NumGoroutine()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if settled = runtime.NumGoroutine(); settled <= before+2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if settled > before+2 {
		buf := make([]byte, 1<<16)
		t.Errorf("two scans left %d goroutines behind (%d before, %d after); "+
			"the library's readers exit only on a cancelled context:\n%s",
			settled-before, before, settled, buf[:runtime.Stack(buf, true)])
	}
}
