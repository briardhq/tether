//go:build linux

package device

import (
	"os"
	"path/filepath"
	"testing"

	"briard.io/tether/internal/family"
)

// adapter describes one stick to place in a fixture tree.
type adapter struct {
	tty          string
	vendor       string
	product      string
	manufacturer string
	name         string
	byID         string // the /dev/serial/by-id link to make for it, if any
	// bus and iface name the sysfs USB device and interface directories. Both default to one
	// device per adapter, which is what a single-port stick looks like; giving two adapters the
	// same bus and different ifaces builds a composite one, two ttys on one device.
	bus   string
	iface string
}

// The reference stick, read off this machine's sysfs, and two others from the family table.
var (
	sonoffP = adapter{tty: "ttyUSB0", vendor: "10c4", product: "ea60",
		manufacturer: "ITead", name: "Sonoff Zigbee 3.0 USB Dongle Plus",
		byID: "usb-ITead_Sonoff_Zigbee-3.0_USB_Dongle_Plus_0001-if00-port0"}
	conbee = adapter{tty: "ttyACM0", vendor: "1cf1", product: "0030",
		manufacturer: "dresden elektronik ingenieurtechnik GmbH", name: "ConBee II",
		byID: "usb-dresden_elektronik_ingenieurtechnik_GmbH_ConBee_II_DE0001-if00"}
	// A plain USB-serial cable wearing the same CP210x id as the Sonoff. It is the reason
	// detection filters through the family table rather than counting USB serial ports: a
	// machine with one of these and one dongle has one coordinator, not two.
	cable = adapter{tty: "ttyUSB1", vendor: "10c4", product: "ea60",
		manufacturer: "Silicon Labs", name: "CP2102 USB to UART Bridge Controller"}
	// Virtual consoles, which is most of what /sys/class/tty actually holds.
	console = adapter{tty: "tty0"}
	// A TI LaunchPad: one USB device, two ttys. The XDS110 debug probe exposes the radio on one
	// interface and a debug port on the other, both behind the same descriptors, so the table
	// cannot tell them apart and must not be asked to. Not a stick we own — the
	// shape is read off zigbee-herdsman's getSerialPortList, which sorts for exactly this.
	launchpadRadio = adapter{tty: "ttyACM0", vendor: "0451", product: "bef3",
		manufacturer: "Texas Instruments", name: "XDS110 (03.00.00.20) Embed with CMSIS-DAP",
		bus: "1-4", iface: "1-4:1.0"}
	launchpadDebug = adapter{tty: "ttyACM1", vendor: "0451", product: "bef3",
		manufacturer: "Texas Instruments", name: "XDS110 (03.00.00.20) Embed with CMSIS-DAP",
		bus: "1-4", iface: "1-4:1.3"}
)

// fixtureTree builds a /sys, /dev and by-id directory holding the given adapters.
func fixtureTree(t *testing.T, adapters ...adapter) (sysRoot, devRoot, byIDRoot string) {
	t.Helper()
	root := t.TempDir()
	sysRoot = filepath.Join(root, "sys")
	devRoot = filepath.Join(root, "dev")
	byIDRoot = filepath.Join(devRoot, "serial", "by-id")
	for _, dir := range []string{filepath.Join(sysRoot, "class", "tty"), byIDRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for i, a := range adapters {
		classTTY := filepath.Join(sysRoot, "class", "tty", a.tty)
		if err := os.MkdirAll(classTTY, 0o755); err != nil {
			t.Fatal(err)
		}
		devPath := filepath.Join(devRoot, a.tty)
		if err := os.WriteFile(devPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if a.vendor == "" {
			continue // a virtual console: no device link at all, like the real thing
		}

		bus, ifaceName := a.bus, a.iface
		if bus == "" {
			bus = "1-" + string(rune('1'+i))
		}
		if ifaceName == "" {
			ifaceName = "1-1:1.0"
		}
		usbDev := filepath.Join(sysRoot, "devices", "pci0000:00", "usb1", bus)
		iface := filepath.Join(usbDev, ifaceName)
		if err := os.MkdirAll(iface, 0o755); err != nil {
			t.Fatal(err)
		}
		for attr, val := range map[string]string{
			"idVendor": a.vendor, "idProduct": a.product,
			"manufacturer": a.manufacturer, "product": a.name,
		} {
			if err := os.WriteFile(filepath.Join(usbDev, attr), []byte(val+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(iface, filepath.Join(classTTY, "device")); err != nil {
			t.Fatal(err)
		}
		if a.byID != "" {
			if err := os.Symlink(devPath, filepath.Join(byIDRoot, a.byID)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return sysRoot, devRoot, byIDRoot
}

// Detection is the family table read backwards, and what it must get right is *how many* — one
// is served, none is waited for, and two is refused. Each of those is a different outcome for
// the operator, so each is asserted rather than sampled.
func TestDetectCountsOnlyAdaptersTheTableKnows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attached []adapter
		want     []string // the human names expected, in path order
	}{
		{"nothing but virtual consoles", []adapter{console}, nil},
		{"one coordinator", []adapter{console, sonoffP}, []string{"Sonoff ZBDongle-P (CC2652P)"}},
		{
			// The case the whole filter exists for: a bare CP2102 cable shares 10c4:ea60 with
			// the Sonoff and is told apart only by its descriptor strings. Counting it would
			// turn a perfectly ordinary machine into an ambiguity tether refuses to resolve.
			"a coordinator beside an unrecognised cable",
			[]adapter{sonoffP, cable},
			[]string{"Sonoff ZBDongle-P (CC2652P)"},
		},
		{"nothing but an unrecognised cable", []adapter{cable}, nil},
		{
			// The case that gets refused rather than resolved. The order is by the path
			// detection settled on, which is the by-id name and not the tty — so "usb-ITead…"
			// precedes "usb-dresden…", uppercase sorting first. What matters is only that it
			// is the same order twice running, since an operator reads this list to pick one.
			"two coordinators of different families",
			[]adapter{sonoffP, conbee},
			[]string{"Sonoff ZBDongle-P (CC2652P)", "ConBee II"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sysRoot, devRoot, byIDRoot := fixtureTree(t, tc.attached...)
			found, err := detect(sysRoot, devRoot, byIDRoot)
			if err != nil {
				t.Fatalf("detect: %v", err)
			}
			if len(found) != len(tc.want) {
				t.Fatalf("detected %d adapters, want %d: %+v", len(found), len(tc.want), found)
			}
			for i, want := range tc.want {
				if found[i].Name != want {
					t.Errorf("adapter %d is %q, want %q", i, found[i].Name, want)
				}
			}
		})
	}
}

// The path detection hands back is reopened after a replug, so it has to be the stable name
// wherever one exists. Handing back ttyUSB0 would work exactly until the first unplug, which
// is the failure a by-id path exists to prevent.
func TestDetectPrefersTheStableName(t *testing.T) {
	sysRoot, devRoot, byIDRoot := fixtureTree(t, sonoffP)
	found, err := detect(sysRoot, devRoot, byIDRoot)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("detected %d adapters, want 1", len(found))
	}
	if want := filepath.Join(byIDRoot, sonoffP.byID); found[0].Path != want {
		t.Errorf("detected at %s, want the by-id name %s", found[0].Path, want)
	}

	// And when there is no by-id link — a host without udev's serial rules — it falls back to
	// the kernel name rather than to nothing, because an unstable path still serves.
	bare := sonoffP
	bare.byID = ""
	sysRoot, devRoot, byIDRoot = fixtureTree(t, bare)
	found, err = detect(sysRoot, devRoot, byIDRoot)
	if err != nil {
		t.Fatalf("detect without by-id: %v", err)
	}
	if len(found) != 1 || found[0].Path != filepath.Join(devRoot, bare.tty) {
		t.Fatalf("without a by-id link, detection produced %+v", found)
	}
}

// A composite adapter is one coordinator, not two. Counting ttys rather than USB devices
// makes a single LaunchPad look like two attached sticks, and tether refuses two — so the bug
// does not misidentify anything, it refuses to serve a stick that is plainly there.
func TestDetectCountsOneCompositeAdapterOnce(t *testing.T) {
	sysRoot, devRoot, byIDRoot := fixtureTree(t, launchpadRadio, launchpadDebug)
	found, err := detect(sysRoot, devRoot, byIDRoot)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("one LaunchPad detected as %d adapters, want 1: %+v", len(found), found)
	}
	// Which of the two is the radio is not in the descriptors, so this follows the other
	// implementation: the first by path. Asserted because the alternative is a debug port
	// opened at 115200, which answers nothing and looks like a dead coordinator.
	if want := filepath.Join(devRoot, launchpadRadio.tty); found[0].Path != want {
		t.Errorf("detected at %s, want the first tty %s", found[0].Path, want)
	}

	// Two *separate* sticks behind the same ids are still two, which is the case the dedupe
	// must not swallow: same descriptors, different USB devices, and refusing is correct.
	second := launchpadRadio
	second.tty, second.bus, second.iface = "ttyACM2", "1-5", "1-5:1.0"
	sysRoot, devRoot, byIDRoot = fixtureTree(t, launchpadRadio, launchpadDebug, second)
	found, err = detect(sysRoot, devRoot, byIDRoot)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("two LaunchPads detected as %d adapters, want 2: %+v", len(found), found)
	}
}

// The parameters come from the table and not from a default: a ConBee at 115200 would be a
// silently broken coordinator, which is the config-assembly failure class exactly.
func TestDetectCarriesTheFamilyParameters(t *testing.T) {
	sysRoot, devRoot, byIDRoot := fixtureTree(t, conbee)
	found, err := detect(sysRoot, devRoot, byIDRoot)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("detected %d adapters, want 1", len(found))
	}
	if found[0].Params.Radio != family.DeCONZ || found[0].Params.Baud != 38400 {
		t.Errorf("detected a ConBee as %+v, want deconz at 38400", found[0].Params)
	}
}
