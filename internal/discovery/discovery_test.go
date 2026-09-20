package discovery

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"briard.io/tether/internal/family"
)

// The TXT record is the one thing a client reads before it decides what tether is, and one
// of its keys fails silently when wrong. So it is asserted key by key, per family, rather
// than "looks about right".
func TestTextRecord(t *testing.T) {
	for _, tc := range []struct {
		name     string
		instance string
		params   family.Params
		want     map[string]string
	}{
		{
			name:     "the CC2652 first-class case",
			instance: "TI CC2531 at nuc (tether)",
			params:   family.Params{Radio: family.ZNP, Baud: 115200, Flow: family.FlowNone},
			want: map[string]string{
				"radio_type":        "znp",
				"serial_number":     "TI CC2531 at nuc (tether)",
				"baud_rate":         "115200",
				"data_flow_control": "none",
			},
		},
		{
			name:     "EZSP, same baud, different radio",
			instance: "Sonoff ZBDongle-E V2 (CP variant) at attic (tether)",
			params:   family.Params{Radio: family.EZSP, Baud: 115200, Flow: family.FlowNone},
			want: map[string]string{
				"radio_type":        "ezsp",
				"serial_number":     "Sonoff ZBDongle-E V2 (CP variant) at attic (tether)",
				"baud_rate":         "115200",
				"data_flow_control": "none",
			},
		},
		{
			// The one family that disagrees on baud. If baud_rate were hardcoded rather
			// than taken from what the device layer used, this is the row that catches it.
			name:     "deCONZ, which is 38400",
			instance: "ConBee II at shed (tether)",
			params:   family.Params{Radio: family.DeCONZ, Baud: 38400, Flow: family.FlowNone},
			want: map[string]string{
				"radio_type":        "deconz",
				"serial_number":     "ConBee II at shed (tether)",
				"baud_rate":         "38400",
				"data_flow_control": "none",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := text(tc.instance, tc.params)
			if len(got) != len(tc.want) {
				t.Fatalf("TXT has %d keys, want %d: %v", len(got), len(tc.want), got)
			}
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("TXT %s = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}

// INV 8: the TXT record is the truth. If the device layer ever opens a port with hardware
// flow control, the advert must say so — the value may not be a constant baked in here, and
// it may never be the SLZB firmware's "software", which is a claim about a port that is not
// ours.
func TestFlowControlIsWhatWeUsed(t *testing.T) {
	got := text("TI CC2531 at nuc (tether)", family.Params{Radio: family.ZNP, Baud: 115200, Flow: family.FlowRTSCTS})
	if got["data_flow_control"] != string(family.FlowRTSCTS) {
		t.Errorf("data_flow_control = %q for an rtscts port, want %q",
			got["data_flow_control"], family.FlowRTSCTS)
	}
	if got["data_flow_control"] == "software" {
		t.Error("data_flow_control is the SLZB firmware's value, which nothing specifies and we do not do")
	}
}

// serial_number is ZHA's unique_id, so it must be the same identity the advert is named for
// and not a second one.
func TestSerialNumberIsTheInstanceName(t *testing.T) {
	const instance = "TI CC2531 at nuc (tether)"
	if got := text(instance, family.Params{Radio: family.ZNP, Baud: 115200, Flow: family.FlowNone}); got["serial_number"] != instance {
		t.Errorf("serial_number = %q, want the instance name %q", got["serial_number"], instance)
	}
}

// The name is read by one person, once, on ZHA's discovery card — so the plain case has to
// read like something a human wrote, and every other case has to stay inside one DNS label.
func TestInstanceName(t *testing.T) {
	const (
		longest = "Sonoff Dongle Max MG24 (ZBDongle-M)" // 35 bytes, the table's longest
		bare    = "Sonoff Dongle Max MG24"              // 22, the same with its aside removed
	)
	for _, tc := range []struct {
		what   string
		device string
		host   string
		want   string
	}{
		{
			what:   "the ordinary case, whole",
			device: "Sonoff ZBDongle-P (CC2652P)",
			host:   "nuc",
			want:   "Sonoff ZBDongle-P (CC2652P) at nuc (tether)",
		}, {
			what:   "an adapter the table cannot name drops out rather than being guessed at",
			device: "",
			host:   "nuc",
			want:   "nuc (tether)",
		}, {
			// The first label is the machine; the rest is the network it is on today, and
			// identity may not move when the machine changes network.
			what:   "a fully qualified hostname keeps only its first label",
			device: "TI CC2531",
			host:   "NUC.example.internal",
			want:   "TI CC2531 at nuc (tether)",
		},

		// The ladder, one case per rung. Each is one byte over what the rung above could fit,
		// so a rung that stopped firing would show up here rather than in a length assertion.
		{
			what:   "rung 1: the adapter's aside goes first",
			device: longest,
			host:   "homeassistant-livingroom", // 72 whole, 59 without the aside
			want:   bare + " at homeassistant-livingroom (tether)",
		}, {
			what:   "rung 2: then the suffix that only says who published it",
			device: longest,
			host:   "homeassistant-livingroom-upstairs", // 68 with the suffix, 59 without
			want:   bare + " at homeassistant-livingroom-upstairs",
		}, {
			what:   "rung 3: then the excess off the longer half, down to the shorter one",
			device: longest,
			host:   strings.Repeat("h", 40), // 66 once bare, 63 with three off the host
			want:   bare + " at " + strings.Repeat("h", 37),
		}, {
			what:   "rung 4: and when neither half is the spare one, both together",
			device: strings.Repeat("d", 40),
			host:   strings.Repeat("h", 40),
			want:   strings.Repeat("d", 29) + " at " + strings.Repeat("h", 29),
		}, {
			// Rung 3 cuts by what is needed and no further than the other half's length, so
			// it can leave the name still too long — which is the whole reason rung 4 exists.
			what:   "rung 3 refuses to cut past the shorter half, and rung 4 catches it",
			device: strings.Repeat("d", 40),
			host:   strings.Repeat("h", 30),
			want:   strings.Repeat("d", 29) + " at " + strings.Repeat("h", 29),
		}, {
			what:   "a cut does not leave a name ending mid-punctuation",
			device: strings.Repeat("d", 27) + " - " + strings.Repeat("x", 10),
			host:   strings.Repeat("h", 40),
			want:   strings.Repeat("d", 27) + " at " + strings.Repeat("h", 29),
		}, {
			// Bytes, not characters: the label limit is bytes and a cut must not split a rune.
			what:   "a multi-byte name is cut on a rune boundary",
			device: strings.Repeat("é", 30), // 60 bytes
			host:   strings.Repeat("h", 30),
			want:   strings.Repeat("é", 14) + " at " + strings.Repeat("h", 29),
		},
	} {
		got, err := instanceName(tc.device, tc.host)
		switch {
		case err != nil:
			t.Errorf("%s: instanceName(%q, %q) failed: %v", tc.what, tc.device, tc.host, err)
		case got != tc.want:
			t.Errorf("%s: instanceName(%q, %q) =\n  %q, want\n  %q", tc.what, tc.device, tc.host, got, tc.want)
		}
	}
}

// A name that identifies nothing is refused rather than advertised, the same way the family
// table refuses an adapter it does not recognise: this value is a client's unique_id, and a
// wrong one is a coordinator that quietly never appears — or quietly displaces another.
func TestInstanceNameRefusesANonIdentity(t *testing.T) {
	for _, host := range []string{"localhost", "localhost.localdomain", "", ".", "   "} {
		if got, err := instanceName("TI CC2531", host); err == nil {
			t.Errorf("instanceName(_, %q) = %q, want an error", host, got)
		}
	}
}

// The property the ladder exists for, asserted over inputs no table and no hostname would
// produce — because the failure it prevents is not a refusal but a record that will not pack,
// and dnssd reports that as "dns: bad rdata" from inside the responder.
func TestInstanceNameAlwaysFitsOneLabel(t *testing.T) {
	for _, n := range []int{0, 1, 20, 40, 62, 63, 64, 200} {
		for _, m := range []int{1, 20, 40, 62, 63, 64, 200} {
			device, host := strings.Repeat("d", n), strings.Repeat("h", m)
			got, err := instanceName(device, host)
			if err != nil {
				t.Errorf("instanceName(%d bytes, %d bytes) failed: %v", n, m, err)
				continue
			}
			if len(got) > maxLabel {
				t.Errorf("instanceName(%d bytes, %d bytes) = %d bytes, over the %d-byte label",
					n, m, len(got), maxLabel)
			}
			if got == "" {
				t.Errorf("instanceName(%d bytes, %d bytes) is empty", n, m)
			}
		}
	}
}

// A stranger reading a browse listing, or a discovery card they did not expect, should be able
// to tell what put it there — which is what the suffix is for, and why it is only dropped
// under pressure.
func TestInstanceNamesSayWhatPublishedThem(t *testing.T) {
	got, err := instanceName("TI CC2531", "nuc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "tether") {
		t.Errorf("instance name %q does not name the program advertising it", got)
	}
}

// The operator's own name is theirs: shrinking it would advertise something other than what
// they wrote, and that string is what a client keys on.
func TestNewRefusesAnOverlongConfiguredName(t *testing.T) {
	long := strings.Repeat("n", maxLabel+1)
	if _, err := New(Config{Instance: long, Port: 6638}); err == nil {
		t.Errorf("New accepted a %d-byte instance name, over the %d-byte label", len(long), maxLabel)
	}
	if _, err := New(Config{Instance: strings.Repeat("n", maxLabel), Port: 6638}); err != nil {
		t.Errorf("New refused a name that is exactly the %d-byte label: %v", maxLabel, err)
	}
}

// The context handed to the mDNS library can only ever end as Canceled, whatever the caller's
// own context does. That is not a nicety: its reader goroutines exit on Canceled and on nothing
// else, so a context that ends by deadline leaves two of them spinning on a closed socket for
// the life of the process — measured at 250% of a CPU on an idle tether (cancelOnly).
//
// Asserted on the discipline rather than on the goroutines, so that it runs everywhere and
// needs no wire; the wire test above asserts that the readers actually go.
func TestTheLibraryOnlyEverSeesACancelledContext(t *testing.T) {
	// A parent that expires by deadline is the case that caused this. A plain WithCancel child
	// would report the parent's DeadlineExceeded here, which is what the library cannot read.
	parent, stopParent := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stopParent()
	ctx, stop := cancelOnly(parent)
	defer stop()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the scan context outlived a parent that had finished")
	}
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("the library would see %v, and exits only on %v", err, context.Canceled)
	}

	// And the ordinary way it ends: our own stop, with the parent still live.
	live, stopLive := context.WithCancel(context.Background())
	defer stopLive()
	ctx, stop = cancelOnly(live)
	stop()
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("stopping the scan gave %v, want %v", err, context.Canceled)
	}
	if live.Err() != nil {
		t.Error("stopping the scan cancelled the caller's own context")
	}
}

// Stripping the asides must never empty the name — something is owed to the card.
func TestUnparenthesisedKeepsSomething(t *testing.T) {
	for _, device := range []string{"(CC2652P)", "()", "(a) (b)", "  (x)  "} {
		if got := unparenthesised(device); got == "" {
			t.Errorf("unparenthesised(%q) is empty", device)
		}
	}
}

func TestNewRejectsAPortThatIsNotOne(t *testing.T) {
	for _, port := range []int{0, -1, 65536} {
		if _, err := New(Config{Instance: "tether-test", Port: port}); err == nil {
			t.Errorf("New accepted port %d", port)
		}
	}
}

// A name the library would quietly rewrite is refused, because this string is what a client
// keys on and publishing a different one would be worse than saying so.
//
// The cases are measured against dnssd v1.2.14, not derived from its documentation: its
// `trimServiceNameSuffixRight` reads a trailing " (n)" as the suffix mDNS adds when renaming a
// clashing service, and its digits test slices one character short — so " (a)" and " (12a)" are
// trimmed as well as " (2)". New does not reproduce that rule; it compares what came back to
// what it asked for, which is why these all fail without anyone having to know it.
func TestNewRefusesANameTheLibraryWouldRewrite(t *testing.T) {
	for _, name := range []string{"attic (2)", "attic (12)", "attic (a)", "attic (12a)"} {
		if _, err := New(Config{Instance: name, Port: 6638}); err == nil {
			t.Errorf("New accepted %q, which the library publishes as something else", name)
		}
	}

	// And the names that are actually fine stay fine — including every shape tether derives,
	// which all end in "(tether)" and must not be caught by this.
	for _, name := range []string{
		"attic (ab)",
		"attic (CC2652P)",
		"Sonoff ZBDongle-P (CC2652P) at nuc (tether)",
		"Sonoff Dongle Max MG24 at nuc",
		"attic(2)",
		"attic (2) x",
	} {
		if _, err := New(Config{Instance: name, Port: 6638}); err != nil {
			t.Errorf("New refused %q, which the library carries verbatim: %v", name, err)
		}
	}
}

// The derived names are the ones nobody gets to choose, so the refusal above must never fire on
// one — a tether that could not advertise its own derived identity would be unusable.
func TestNoDerivedNameIsOneTheLibraryRewrites(t *testing.T) {
	for _, device := range []string{
		"", "Sonoff ZBDongle-P (CC2652P)", "Sonoff Dongle Max MG24 (ZBDongle-M)",
		"TubesZB (CC2652, CH340)", "radio (2)", "radio (a)",
		strings.Repeat("d", 80),
	} {
		for _, host := range []string{"nuc", "homeassistant-livingroom", strings.Repeat("h", 40)} {
			name, err := instanceName(device, host)
			if err != nil {
				t.Errorf("instanceName(%q, %q) failed: %v", device, host, err)
				continue
			}
			if _, err := New(Config{Instance: name, Port: 6638}); err != nil {
				t.Errorf("a derived name cannot be advertised: instanceName(%q, %q) = %q: %v",
					device, host, name, err)
			}
		}
	}
}

// The advert must NEVER claim this machine's own `.local` name. Left unset, the library defaults
// Config.Host to os.Hostname() -- the name the host's own responder already publishes addresses
// for -- and tether is a guest on that machine: a second claimant can end up renaming its host's
// mDNS identity, which is not ours to move.
func TestTheAdvertNeverClaimsTheMachinesOwnName(t *testing.T) {
	a, err := New(Config{Device: "Sonoff Dongle", Port: 6638, Params: family.Params{Radio: "ezsp"}})
	if err != nil {
		t.Fatal(err)
	}
	got := a.service.Host
	if !strings.HasPrefix(got, hostPrefix) {
		t.Errorf("the SRV target is %q -- it must be a name tether mints (%q...), never the "+
			"machine's own, which its resolver already claims", got, hostPrefix)
	}
	if got == "" {
		t.Error("an empty Host is the library's os.Hostname() fallback, which is the whole bug")
	}
}

// Two adapters on one machine are two tethers on two ports, and each publishes its own A record.
func TestHostNameTellsTwoAdaptersOnOneMachineApart(t *testing.T) {
	first, err := hostName("rpi", 6638)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hostName("rpi", 6639)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both adapters published %q, so one A record answers for two coordinators", first)
	}
	for _, name := range []string{first, second} {
		if !strings.HasPrefix(name, hostPrefix) || !strings.Contains(name, "rpi") {
			t.Errorf("%q says neither who published it nor which machine it is", name)
		}
	}
}

// A hostname may arrive fully qualified; the first label is this machine and the rest is the
// network it is on today -- the same reduction the instance name makes, through the same
// function, so the two cannot drift.
func TestHostNameReducesAFullyQualifiedHostname(t *testing.T) {
	got, err := hostName("rpi.lan.example.com", 6638)
	if err != nil {
		t.Fatal(err)
	}
	if want := hostPrefix + "rpi-6638"; got != want {
		t.Errorf("hostName = %q, want %q", got, want)
	}
}

// One DNS label, always -- the same limit the instance name has, and the same reason: over it,
// the library packs nothing and v1.2.14 says nothing about why.
func TestHostNameAlwaysFitsOneLabel(t *testing.T) {
	got, err := hostName(strings.Repeat("m", 200), 6638)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > maxLabel {
		t.Errorf("hostName is %d bytes: %q", len(got), got)
	}
	if !strings.HasSuffix(got, "-6638") {
		t.Errorf("the port was cut instead of the machine name: %q", got)
	}
}

// The refusal machineName already makes, reached from here too: a machine that calls itself
// localhost identifies nothing, and a name nobody can tell apart is worse than an error.
func TestHostNameRefusesANonIdentity(t *testing.T) {
	if got, err := hostName("localhost", 6638); err == nil {
		t.Errorf("hostName(localhost) = %q, want a refusal", got)
	}
}
