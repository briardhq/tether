package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"briard.io/tether/internal/family"
)

// The retry schedule, which is decision logic and so gets asserted rather than eyeballed. The
// three properties that matter: the first wait is short, because the boot-order race resolves
// in under a second and a long first wait would be dead time on every boot; the waits never
// shrink; and they stop growing at the cap, because the other case is a human plugging a dongle
// back in and the cap is how long they wait for tether to notice.
func TestRetryDelayStartsShortGrowsAndCaps(t *testing.T) {
	if got := retryDelay(1); got != firstRetry {
		t.Errorf("the first retry waits %v, want %v", got, firstRetry)
	}

	prev := time.Duration(0)
	for attempt := 1; attempt <= 40; attempt++ {
		got := retryDelay(attempt)
		if got < prev {
			t.Errorf("retryDelay(%d) = %v, shorter than the previous %v", attempt, got, prev)
		}
		if got > maxRetry {
			t.Errorf("retryDelay(%d) = %v, past the %v cap", attempt, got, maxRetry)
		}
		prev = got
	}
	if prev != maxRetry {
		t.Errorf("the schedule settled at %v, not at the %v cap", prev, maxRetry)
	}

	// An hour of a dongle being absent must still be polling at the cap, not at some
	// accumulated overflow of it.
	if got := retryDelay(1 << 20); got != maxRetry {
		t.Errorf("retryDelay of a very large attempt = %v, want %v", got, maxRetry)
	}
}

// What the advert calls the adapter. The table's name is the good answer; the path's last
// element is the next best true thing, and it is reached far less often than it looks — a
// configured family does not stop tether reading the descriptors, so the fallback is for a
// stick the table does not carry or a device with no descriptors at all.
func TestAdapterLabelPrefersTheTableAndFallsBackToThePath(t *testing.T) {
	for _, tc := range []struct {
		what string
		at   target
		want string
	}{
		{
			what: "the table knows it",
			at:   target{name: "Sonoff ZBDongle-P (CC2652P)", path: "/dev/serial/by-id/usb-ITead-if00-port0"},
			want: "Sonoff ZBDongle-P (CC2652P)",
		}, {
			// The table does not carry every stick, but the stick still says what it is —
			// and its own words beat anything read off a path.
			what: "it does not, but the adapter says what it is",
			at: target{
				path: "/dev/serial/by-id/usb-Some_Vendor_Radio_A1B2-if00-port0",
				usb:  family.Device{Manufacturer: "Some Vendor", Name: "Radio"},
			},
			want: "Some Vendor Radio",
		}, {
			what: "the table's name still wins over the adapter's own",
			at: target{
				name: "Sonoff ZBDongle-P (CC2652P)",
				usb:  family.Device{Manufacturer: "ITead", Name: "Sonoff Zigbee 3.0 USB Dongle Plus"},
			},
			want: "Sonoff ZBDongle-P (CC2652P)",
		}, {
			// No descriptors at all means it is not USB — an on-board UART. Thin, but it is
			// what is in the config and what they will recognise.
			what: "nothing says anything, and the path is all there is",
			at:   target{path: "/dev/ttyAMA0"},
			want: "ttyAMA0",
		},
	} {
		if got := adapterLabel(tc.at); got != tc.want {
			t.Errorf("%s: adapterLabel = %q, want %q", tc.what, got, tc.want)
		}
	}
}

// Naming the family settles the radio type and nothing else. The case that makes this a
// decision rather than a detail is a stick whose row differs from the family's defaults: a
// Connect ZBT-2 is EZSP at 460800 with RTS/CTS and Defaults(ezsp) is 115200 with none, so an
// override that replaced the row would open it at the wrong speed with no flow control — the
// confident-wrong-parameters failure the family table exists to rule out, produced by a key
// ARCHITECTURE.md "Configuration" calls safe to set.
func TestARadioOverrideKeepsTheRowsBaudAndFlowControl(t *testing.T) {
	zbt2 := family.Params{Radio: family.EZSP, Baud: 460800, Flow: family.FlowRTSCTS}
	sonoff := family.Params{Radio: family.ZNP, Baud: 115200, Flow: family.FlowNone}
	for _, tc := range []struct {
		what  string
		row   family.Params
		radio family.Radio
		want  family.Params
	}{
		{
			what:  "the family the row already has changes nothing",
			row:   zbt2,
			radio: family.EZSP,
			want:  zbt2,
		}, {
			// The type moves; the bridge's parameters do not. deconz is the family whose
			// default baud differs, so a replaced row would show up as 38400 here.
			what:  "a different family on a known row moves only the type",
			row:   sonoff,
			radio: family.DeCONZ,
			want:  family.Params{Radio: family.DeCONZ, Baud: 115200, Flow: family.FlowNone},
		}, {
			what:  "no row at all is the escape hatch, and takes the family's defaults",
			row:   family.Params{},
			radio: family.DeCONZ,
			want:  family.Params{Radio: family.DeCONZ, Baud: 38400, Flow: family.FlowNone},
		},
	} {
		got, err := withRadio(tc.row, tc.radio)
		if err != nil {
			t.Errorf("%s: withRadio(%+v, %q): %v", tc.what, tc.row, tc.radio, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: withRadio(%+v, %q) = %+v, want %+v", tc.what, tc.row, tc.radio, got, tc.want)
		}
	}
	// A family that is not one is refused on both branches, not only on the one that reads
	// the defaults.
	for _, row := range []family.Params{zbt2, {}} {
		if got, err := withRadio(row, "znpp"); err == nil {
			t.Errorf("withRadio(%+v, \"znpp\") = %+v, want an error", row, got)
		}
	}
}

// A configured path plus a configured family is the family table's escape hatch, and it has to
// keep working for a device with no USB descriptors to read — an on-board UART, a stick behind
// a bridge the sysfs walk does not follow. Reading the descriptors for a *name* must therefore
// not turn a failure to read them into a failure to serve.
func TestAFamilyOverrideServesADeviceWithNoDescriptors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-usb-device")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	at, err := locate(Options{DevicePath: path, Radio: family.ZNP})
	if err != nil {
		t.Fatalf("locate refused the escape hatch: %v", err)
	}
	if at.params.Radio != family.ZNP {
		t.Errorf("radio = %q, want the configured %q", at.params.Radio, family.ZNP)
	}
	if at.name != "" {
		t.Errorf("name = %q, want none — there are no descriptors to have read it from", at.name)
	}
	if at.usb != (family.Device{}) {
		t.Errorf("usb = %+v, want none — there was nothing to read", at.usb)
	}
	// The advert still says something about it, which is the point of the fallback.
	if got := adapterLabel(at); got != "not-a-usb-device" {
		t.Errorf("adapterLabel = %q, want the path's last element", got)
	}
}

// "address already in use" is a true account of what the kernel said and a poor account of why:
// on a host with two dongles the cause is almost always the other tether. The sentence points
// at the verb that can confirm it rather than reading another process's socket to find out,
// which would be a second way to discover what `tether status` already reports.
func TestListenFailureSaysWhereToLook(t *testing.T) {
	kernel := errors.New("bind: address already in use")
	err := listenFailure(":6638", kernel)

	for _, want := range []string{":6638", "briard-tether status", "already running on this host", "config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not carry %q: %v", want, err)
		}
	}
	// The kernel's own account survives being explained, both to read and to match on: it is
	// the thing somebody pastes into a search.
	if !errors.Is(err, kernel) {
		t.Errorf("the original error is not reachable through the explanation: %v", err)
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("the original error does not appear in the message: %v", err)
	}
}
