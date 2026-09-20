package family

import (
	"errors"
	"strings"
	"testing"
)

// The reference device, read off the development machine's own sysfs. Written out here so the
// table is pinned to a real descriptor rather than to what one was assumed to say.
var sonoffP = Device{
	Vendor: "10c4", Product: "ea60",
	Manufacturer: "ITead", Name: "Sonoff Zigbee 3.0 USB Dongle Plus",
}

func TestResolveIdentifiesTheReferenceStick(t *testing.T) {
	got, err := Resolve(sonoffP)
	if err != nil {
		t.Fatalf("Resolve(%v): %v", sonoffP, err)
	}
	want := Params{ZNP, 115200, FlowNone}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if name, ok := Identify(sonoffP); !ok || !strings.Contains(name, "ZBDongle-P") {
		t.Errorf("Identify = %q, %v; want the Sonoff ZBDongle-P", name, ok)
	}
}

// The collision that makes VID:PID insufficient, and the reason the table matches descriptor
// strings at all: these two sticks share an id AND the words "dongle plus", and are different
// radios. Getting this backwards opens an EZSP radio with ZNP parameters.
func TestResolveSeparatesTwoSticksSharingOneID(t *testing.T) {
	e := Device{
		Vendor: "10c4", Product: "ea60",
		Manufacturer: "ITEAD", Name: "Sonoff Zigbee 3.0 USB Dongle Plus V2",
	}
	got, err := Resolve(e)
	if err != nil {
		t.Fatalf("Resolve(%v): %v", e, err)
	}
	if got.Radio != EZSP {
		t.Errorf("the V2 resolved to %q, want %q — the more specific row must win", got.Radio, EZSP)
	}
	p, err := Resolve(sonoffP)
	if err != nil {
		t.Fatalf("Resolve(%v): %v", sonoffP, err)
	}
	if p.Radio != ZNP {
		t.Errorf("the non-V2 resolved to %q, want %q", p.Radio, ZNP)
	}
}

// Every row must be reachable, or it is decoration. This also catches a row whose match string
// is shadowed by a shorter one on the same id.
func TestEveryRowResolvesToItself(t *testing.T) {
	for _, r := range table {
		t.Run(r.name, func(t *testing.T) {
			d := Device{Vendor: r.vendor, Product: r.product, Name: r.match}
			got, err := Resolve(d)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != r.params {
				t.Errorf("got %+v, want %+v", got, r.params)
			}
			if name, ok := Identify(d); !ok || name != r.name {
				t.Errorf("Identify = %q, %v; want %q", name, ok, r.name)
			}
		})
	}
}

// Refusing to guess is the behaviour under test, not an edge case: the biggest failure class
// here is a coordinator opened at the wrong parameters, which looks like broken hardware.
func TestResolveRefusesToGuess(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Device
	}{
		{"an id nothing claims", Device{Vendor: "dead", Product: "beef", Name: "Some Adapter"}},
		{"the stock CP210x id with no useful strings", Device{
			Vendor: "10c4", Product: "ea60",
			Manufacturer: "Silicon Labs", Name: "CP2102N USB to UART Bridge Controller",
		}},
		{"the stock CP210x id on unrelated hardware", Device{
			Vendor: "10c4", Product: "ea60", Manufacturer: "Espressif", Name: "ESP32 DevKit",
		}},
		// 0403:6015 is now claimed three times over — ConBee III, the Electrolama zzh and a
		// ZiGate — so the id alone still settles nothing, which is the point.
		{"an FTDI id whose descriptors match no row", Device{
			Vendor: "0403", Product: "6015", Manufacturer: "FTDI", Name: "USB Serial Converter",
		}},
		{"no descriptor strings at all", Device{Vendor: "10c4", Product: "ea60"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.d)
			if err == nil {
				t.Fatalf("Resolve returned %+v; it must refuse rather than guess", got)
			}
			if !errors.Is(err, ErrUnknown) {
				t.Errorf("error %v does not wrap ErrUnknown", err)
			}
			// The error is the whole product here: it must name what was seen, or the user
			// cannot act on it.
			if !strings.Contains(err.Error(), tc.d.Vendor) || !strings.Contains(err.Error(), tc.d.Product) {
				t.Errorf("error %q does not name the ids it saw", err)
			}
		})
	}
}

func TestDefaultsCoverEveryRadioAndRejectOthers(t *testing.T) {
	for _, tc := range []struct {
		radio Radio
		baud  int
	}{
		{ZNP, 115200},
		{EZSP, 115200},
		// 38400, not 115200: the one family that differs, and the correction that reading
		// zigbee2mqtt's own defaults produced.
		{DeCONZ, 38400},
	} {
		got, err := Defaults(tc.radio)
		if err != nil {
			t.Fatalf("Defaults(%q): %v", tc.radio, err)
		}
		if got.Radio != tc.radio || got.Baud != tc.baud {
			t.Errorf("Defaults(%q) = %+v, want radio %q baud %d", tc.radio, got, tc.radio, tc.baud)
		}
	}
	if _, err := Defaults("zstack"); !errors.Is(err, ErrUnknown) {
		t.Error(`Defaults("zstack") must fail: that is zigbee-herdsman's name, not the TXT contract's`)
	}
	if _, err := Defaults(""); !errors.Is(err, ErrUnknown) {
		t.Error("Defaults(\"\") must fail rather than pick a family")
	}
}

// Every row must be internally coherent. Flow is not asserted to be none: five rows honestly
// say rtscts, because herdsman configures those adapters that way and a row that lied about it
// would be the worse failure. The device layer refuses to open them, so the promise is kept by
// never getting as far as opening rather than by pretending.
func TestEveryRowIsCoherent(t *testing.T) {
	for _, r := range table {
		if r.params.Flow != FlowNone && r.params.Flow != FlowRTSCTS {
			t.Errorf("%s claims flow %q, which is not a value the TXT contract has", r.name, r.params.Flow)
		}
		if r.params.Radio != ZNP && r.params.Radio != EZSP && r.params.Radio != DeCONZ {
			t.Errorf("%s claims radio %q, which is not one the family table carries", r.name, r.params.Radio)
		}
		if r.params.Baud <= 0 {
			t.Errorf("%s has baud %d", r.name, r.params.Baud)
		}
		if r.match != strings.ToLower(r.match) {
			t.Errorf("%s has match %q, which is not lowercase and so can never match", r.name, r.match)
		}
	}
}

// ⚠️ The defect this row was added for, kept as a test because the shape of it will recur:
// ITead now ships at least three products whose names contain "Dongle Plus", and the ZNP row
// matches that substring. A Dongle Plus MG24 is an EZSP radio behind the same 10c4:ea60
// CP2102N as the ZNP stick, so without a longer match it resolves as znp at 115200 — a
// confident wrong family, which is exactly what must never happen. The descriptors here are
// read from zigbee-herdsman's adapterDiscovery.ts, whose by-id example is
// usb-SONOFF_SONOFF_Dongle_Plus_MG24_b023a583a66bef118e30a3adc169b110-if00-port0.
func TestTheSonoffDonglePlusVariantsDoNotCollapseIntoTheZNPRow(t *testing.T) {
	for _, tc := range []struct {
		name         string
		manufacturer string
		product      string
		want         Radio
	}{
		// The stick in hand, whose descriptors are read off real hardware.
		{"ZBDongle-P", "ITead", "Sonoff Zigbee 3.0 USB Dongle Plus", ZNP},
		{"ZBDongle-E V2 (CP)", "Itead", "Sonoff Zigbee 3.0 USB Dongle Plus V2", EZSP},
		{"Dongle Plus MG24", "SONOFF", "SONOFF Dongle Plus MG24", EZSP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params, err := Resolve(Device{
				Vendor: "10c4", Product: "ea60",
				Manufacturer: tc.manufacturer, Name: tc.product,
			})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if params.Radio != tc.want {
				t.Errorf("%q resolved to %q, want %q — opening a coordinator as the wrong "+
					"family looks like a broken dongle, not a wrong table", tc.product, params.Radio, tc.want)
			}
		})
	}
}

// What to call an adapter the table does not carry. Its own descriptor strings are the best
// answer available and the one a person would recognise, so they have to come out readable.
func TestDeviceLabel(t *testing.T) {
	for _, tc := range []struct {
		what string
		d    Device
		want string
	}{
		{
			what: "both strings, and they say different things",
			d:    Device{Manufacturer: "ITead", Name: "Sonoff Zigbee 3.0 USB Dongle Plus V2"},
			want: "ITead Sonoff Zigbee 3.0 USB Dongle Plus V2",
		}, {
			// Measured: this adapter really does report both.
			what: "the product already carries the manufacturer",
			d:    Device{Manufacturer: "SONOFF", Name: "SONOFF Dongle Plus MG24"},
			want: "SONOFF Dongle Plus MG24",
		}, {
			what: "and it carries it in a different case",
			d:    Device{Manufacturer: "sonoff", Name: "SONOFF Dongle Plus MG24"},
			want: "SONOFF Dongle Plus MG24",
		}, {
			// Both are optional in USB, and adapters in the field are missing each of them.
			what: "no manufacturer",
			d:    Device{Name: "Some Radio"},
			want: "Some Radio",
		}, {
			what: "no product",
			d:    Device{Manufacturer: "Some Vendor"},
			want: "Some Vendor",
		}, {
			what: "neither, which is the one case the path has to answer",
			d:    Device{Vendor: "10c4", Product: "ea60"},
			want: "",
		},
	} {
		if got := tc.d.Label(); got != tc.want {
			t.Errorf("%s: Label() = %q, want %q", tc.what, got, tc.want)
		}
	}
}
