// Package family turns a plugged-in USB adapter into the serial parameters it needs. It is the
// adapter table, as data: no code path anywhere else may branch on which stick is present. See
// ARCHITECTURE.md "The family table".
//
// It never guesses. An adapter it does not recognise produces an error naming exactly what it
// saw, because a wrong guess is the config-assembly failure class — a stick opened at the wrong
// parameters looks like a broken dongle, not like a misconfiguration.
package family

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Radio is the radio_type value from the discovery contract. These are zigpy's names, which is
// what the TXT convention uses; zigbee-herdsman calls the same three zstack, ember and deconz.
type Radio string

const (
	ZNP    Radio = "znp"
	EZSP   Radio = "ezsp"
	DeCONZ Radio = "deconz"
)

// Flow is UART flow control. Most rows are FlowNone, and ⚠️ **the six that are not matter more
// than their count suggests**. zigbee2mqtt's documented *default* is false, but its own
// `adapterDiscovery.ts` overrides that to `rtscts: true` for the Nabu Casa adapters and two
// SMLight ones. A row saying none for those would claim terms their own client does not use for
// them, and a flow-control mismatch surfaces as corruption under load — the failure hardest to
// attribute, and the one this program exists to take off the table.
//
// So the rows are honest and the device layer makes them true. The serial library cannot enable
// RTS/CTS — its mode struct has no field for it and its open hardcodes the flag off — so the
// device layer sets CRTSCTS itself, through a descriptor it opens before the library and holds
// for the life of the port. A row here describes the bridge rather than the firmware behind it,
// which is why a radio override settles the radio type and leaves this alone.
type Flow string

const (
	FlowNone   Flow = "none"
	FlowRTSCTS Flow = "rtscts"
)

// Params are what the device layer needs to open a port, and what discovery publishes.
type Params struct {
	Radio Radio
	Baud  int
	Flow  Flow
}

// Device is a plugged-in adapter as the USB layer describes it. Vendor and Product are the
// four-digit hex ids; Manufacturer and Name are the descriptor strings.
type Device struct {
	Vendor       string
	Product      string
	Manufacturer string
	Name         string
}

func (d Device) String() string {
	desc := d.Label()
	if desc == "" {
		desc = "no descriptor strings"
	}
	return fmt.Sprintf("%s:%s (%s)", d.Vendor, d.Product, desc)
}

// Label is what to call this adapter when the table has no name for it: the strings the
// adapter reports about itself. That is a better answer than anything derived from the device
// path, and the only case it cannot cover is a device with no USB descriptors at all.
//
// Both strings are optional in USB and either may be missing. Some adapters also repeat the
// manufacturer inside the product — manufacturer "SONOFF", product "SONOFF Dongle Plus MG24",
// measured — and printing both would name the thing twice.
func (d Device) Label() string {
	maker, name := strings.TrimSpace(d.Manufacturer), strings.TrimSpace(d.Name)
	switch {
	case maker == "":
		return name
	case name == "":
		return maker
	case strings.HasPrefix(strings.ToLower(name), strings.ToLower(maker)):
		return name
	}
	return maker + " " + name
}

// ErrUnknown is returned for an adapter the table does not identify.
var ErrUnknown = errors.New("unknown adapter family")

// row is one entry in the family table.
//
// match is the reason this is not keyed on VID:PID alone. 10c4:ea60 is the stock Silicon Labs
// CP210x bridge id and is shared by ZNP, EZSP and other adapters alike; 0403:6015 is shared by
// deCONZ and ZNP sticks. The descriptor strings are the only thing that separates them, which
// is what zigbee-herdsman's own table does too.
type row struct {
	vendor  string
	product string
	match   string // lowercase substring of "<manufacturer> <name>"
	name    string // for humans, in logs and errors
	params  Params
}

// The three families and the sticks that are actually in the field for each.
// Deliberately not exhaustive: a stick absent here gets an honest error, which is the design's
// stated preference over a guess.
var table = []row{
	// ─── TI CC2652 / CC2538 / CC2531 — ZNP. The first-class target. ────────────────────────
	// Some ZBDongle-P units report manufacturer "Silicon Labs" rather than "ITead", which is
	// why the match is on the product words and not on the manufacturer — herdsman's own
	// comment carries both by-id forms.
	{"10c4", "ea60", "dongle plus", "Sonoff ZBDongle-P (CC2652P)", Params{ZNP, 115200, FlowNone}},
	{"10c4", "ea60", "dongle plus cc2674p10", "Sonoff Dongle-PP10 (CC2674P10)", Params{ZNP, 115200, FlowNone}},
	{"10c4", "ea60", "slae.sh cc2652rb", "slae.sh CC2652RB", Params{ZNP, 115200, FlowNone}},
	{"0451", "bef3", "texas instruments", "TI LaunchPad (CC2652)", Params{ZNP, 115200, FlowNone}},
	{"0451", "16c8", "cc2538", "TI CC2538", Params{ZNP, 115200, FlowNone}},
	{"0451", "16a8", "cc2531", "TI CC2531", Params{ZNP, 115200, FlowNone}},
	// ⚠️ 0403:6015 is the one id in this table carried by two rows whose Params DISAGREE — this
	// one and the ConBee III below, ZNP at 115200 against deCONZ at 38400. It is therefore the
	// pair that must never be resolved on the ids alone, and getting it wrong is not cosmetic:
	// it is the wrong radio driver at the wrong baud.
	//
	// This is the concrete reason there is no ids-alone fallback anywhere in this package, on
	// any platform. Matching on the descriptor strings is required everywhere, and an adapter
	// this table cannot resolve is refused by name rather than guessed at — a refusal an
	// operator can read beats a misidentification they cannot.
	{"0403", "6015", "electrolama", "Electrolama zzh (CC2652R)", Params{ZNP, 115200, FlowNone}},
	// SMLight's ZNP models. Note slzb-06p7/06p10 against the EZSP slzb-06m below, and
	// slzb-07p7 against the EZSP slzb-07: the families differ between models whose names
	// differ by two characters, which is what most-specific-wins is for.
	{"10c4", "ea60", "slzb-06p7", "SMLIGHT SLZB-06p7", Params{ZNP, 115200, FlowNone}},
	{"10c4", "ea60", "slzb-06p10", "SMLIGHT SLZB-06p10", Params{ZNP, 115200, FlowNone}},
	{"10c4", "ea60", "slzb-07p7", "SMLIGHT SLZB-07p7", Params{ZNP, 115200, FlowNone}},
	// Community boards. herdsman matches these on the path alone with no manufacturer, so the
	// product word is all there is to go on — and it is distinctive enough. 1a86:7523 is the
	// stock CH340 id worn by half the Arduinos ever made, so the word is load-bearing.
	{"10c4", "ea60", "tubeszb", "TubesZB (CC2652)", Params{ZNP, 115200, FlowNone}},
	{"1a86", "7523", "tubeszb", "TubesZB (CC2652, CH340)", Params{ZNP, 115200, FlowNone}},
	{"1a86", "7523", "zigstar", "ZigStar (CC2652)", Params{ZNP, 115200, FlowNone}},

	// ─── Silabs EFR32 — EZSP. Second, with care. ───────────────────────────────────────────
	// ITead's "Dongle Plus" line is four products across two families, told apart only by a
	// suffix; the ZNP rows above carry the other two.
	{"10c4", "ea60", "dongle plus v2", "Sonoff ZBDongle-E V2 (CP variant)", Params{EZSP, 115200, FlowNone}},
	{"1a86", "55d4", "dongle plus v2", "Sonoff ZBDongle-E V2 (CH variant)", Params{EZSP, 115200, FlowNone}},
	{"10c4", "ea60", "dongle plus mg24", "Sonoff Dongle Plus MG24", Params{EZSP, 115200, FlowNone}},
	{"10c4", "ea60", "dongle max mg24", "Sonoff Dongle Max MG24 (ZBDongle-M)", Params{EZSP, 115200, FlowNone}},
	{"10c4", "ea60", "dongle lite mg21", "Sonoff Dongle Lite MG21", Params{EZSP, 115200, FlowNone}},
	// ⚠️ The rows herdsman configures with rtscts on. They say so, and the device layer sets
	// CRTSCTS itself rather than relying on the serial library, which cannot. Saying none here
	// instead would be the quieter lie: the stick would open, work at rest, and corrupt under
	// load — which is the fault class hardest to attribute to a transport.
	{"10c4", "ea60", "skyconnect", "Home Assistant SkyConnect", Params{EZSP, 115200, FlowRTSCTS}},
	{"10c4", "ea60", "zbt-1", "Home Assistant Connect ZBT-1", Params{EZSP, 115200, FlowRTSCTS}},
	// ZBT-2 is an ESP32-S3 bridge, hence the Espressif ids and two of them, and the only
	// non-default baud in the table.
	{"303a", "4001", "zbt-2", "Home Assistant Connect ZBT-2", Params{EZSP, 460800, FlowRTSCTS}},
	{"303a", "831a", "zbt-2", "Home Assistant Connect ZBT-2", Params{EZSP, 460800, FlowRTSCTS}},
	{"10c4", "ea60", "slzb-07", "SMLIGHT SLZB-07", Params{EZSP, 115200, FlowRTSCTS}},
	{"10c4", "ea60", "slzb-07mg24", "SMLIGHT SLZB-07mg24", Params{EZSP, 115200, FlowRTSCTS}},
	{"10c4", "ea60", "slzb-06m", "SMLIGHT SLZB-06m", Params{EZSP, 115200, FlowNone}},

	// ─── dresden ConBee — deCONZ. 38400, the one family that differs on baud. ───────────────
	{"1cf1", "0030", "conbee", "ConBee II", Params{DeCONZ, 38400, FlowNone}},
	// The other half of the 0403:6015 pair — see the zzh row above before touching either.
	{"0403", "6015", "conbee", "ConBee III", Params{DeCONZ, 38400, FlowNone}},
}

// Resolve identifies an adapter's family. It matches on the ids and then on the descriptor
// strings, and the most specific match wins — that is what separates two sticks sharing one
// id. Two equally specific matches are an error rather than a coin toss.
func Resolve(d Device) (Params, error) {
	haystack := strings.ToLower(strings.TrimSpace(d.Manufacturer + " " + d.Name))

	var hits []row
	for _, r := range table {
		if !strings.EqualFold(r.vendor, d.Vendor) || !strings.EqualFold(r.product, d.Product) {
			continue
		}
		if !strings.Contains(haystack, r.match) {
			continue
		}
		hits = append(hits, r)
	}
	if len(hits) == 0 {
		return Params{}, fmt.Errorf("%w: %s — tether will not guess its radio type, "+
			"because opening a coordinator at the wrong parameters looks like a broken dongle "+
			"rather than a wrong setting; name the family explicitly in the config", ErrUnknown, d)
	}

	sort.SliceStable(hits, func(i, j int) bool { return len(hits[i].match) > len(hits[j].match) })
	if len(hits) > 1 && len(hits[0].match) == len(hits[1].match) {
		return Params{}, fmt.Errorf("%w: %s matches both %q and %q equally well; "+
			"name the family explicitly in the config", ErrUnknown, d, hits[0].name, hits[1].name)
	}
	return hits[0].params, nil
}

// Identify returns the human name of a recognised adapter, for logs. It is the same lookup as
// Resolve and exists so that a log line can say "Sonoff ZBDongle-P" rather than "10c4:ea60".
func Identify(d Device) (string, bool) {
	haystack := strings.ToLower(strings.TrimSpace(d.Manufacturer + " " + d.Name))
	best := ""
	bestLen := -1
	for _, r := range table {
		if strings.EqualFold(r.vendor, d.Vendor) && strings.EqualFold(r.product, d.Product) &&
			strings.Contains(haystack, r.match) && len(r.match) > bestLen {
			best, bestLen = r.name, len(r.match)
		}
	}
	return best, best != ""
}

// IDs is the USB vendor:product pairs this table carries, deduplicated and in a stable order.
//
// It exists for the packaging layer and for nothing else. The udev rule that keeps these
// adapters out of USB runtime suspend (the load-dependent failure class) has to name the
// devices it applies to — a blanket rule over the whole bus is a policy statement about
// somebody else's hardware — and a rule that transcribed the ids by hand would drift from this
// table the first time a row was added. ⚠️ Nothing in the data path may branch on this: the
// ids alone do not identify a family, which is the whole reason `row` carries a descriptor
// match as well.
//
// The Devices returned carry ids and no descriptor strings, which is all a udev rule can match
// on anyway.
func IDs() []Device {
	seen := make(map[Device]bool, len(table))
	ids := make([]Device, 0, len(table))
	for _, r := range table {
		d := Device{Vendor: r.vendor, Product: r.product}
		if seen[d] {
			continue
		}
		seen[d] = true
		ids = append(ids, d)
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].Vendor != ids[j].Vendor {
			return ids[i].Vendor < ids[j].Vendor
		}
		return ids[i].Product < ids[j].Product
	})
	return ids
}

// Defaults returns the parameters for a family named directly, which is how the config
// override reaches this package: the user says which radio they have and the table supplies
// the rest. Baud stays overridable on top of this, because some EZSP firmware wants 57600.
func Defaults(r Radio) (Params, error) {
	switch r {
	case ZNP:
		return Params{ZNP, 115200, FlowNone}, nil
	case EZSP:
		return Params{EZSP, 115200, FlowNone}, nil
	case DeCONZ:
		return Params{DeCONZ, 38400, FlowNone}, nil
	}
	return Params{}, fmt.Errorf("%w: %q is not a radio type; known types are %q, %q and %q",
		ErrUnknown, r, ZNP, EZSP, DeCONZ)
}
