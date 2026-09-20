//go:build linux

package device

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"briard.io/tether/internal/family"
)

// A fixture /sys, shaped like the real one: the tty's "device" link points at the USB
// *interface*, and the device that owns power management is its parent. Built rather than
// borrowed from the host because a CI runner has no USB serial adapter to borrow from.
func fixtureSys(t *testing.T, withIDVendor, withControl bool) (sysRoot, devPath string) {
	t.Helper()
	root := t.TempDir()

	usbDev := filepath.Join(root, "sys", "devices", "pci0000:00", "usb1", "1-1")
	iface := filepath.Join(usbDev, "1-1:1.0")
	if err := os.MkdirAll(filepath.Join(iface, "ttyUSB0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if withIDVendor {
		// The reference stick's real descriptors, as read off this machine's sysfs.
		for attr, val := range map[string]string{
			"idVendor":     "10c4",
			"idProduct":    "ea60",
			"manufacturer": "ITead",
			"product":      "Sonoff Zigbee 3.0 USB Dongle Plus",
		} {
			if err := os.WriteFile(filepath.Join(usbDev, attr), []byte(val+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if withControl {
		if err := os.MkdirAll(filepath.Join(usbDev, "power"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(usbDev, "power", "control"), []byte("auto\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	classTTY := filepath.Join(root, "sys", "class", "tty", "ttyUSB0")
	if err := os.MkdirAll(classTTY, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(iface, filepath.Join(classTTY, "device")); err != nil {
		t.Fatal(err)
	}

	devPath = filepath.Join(root, "dev", "ttyUSB0")
	if err := os.MkdirAll(filepath.Dir(devPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(devPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "sys"), devPath
}

func TestAutosuspendControlPathWalksToTheUSBDevice(t *testing.T) {
	sysRoot, devPath := fixtureSys(t, true, true)

	got, err := autosuspendControlPath(sysRoot, devPath)
	if err != nil {
		t.Fatalf("autosuspendControlPath: %v", err)
	}
	want := filepath.Join(sysRoot, "devices", "pci0000:00", "usb1", "1-1", "power", "control")
	if got != want {
		t.Errorf("walked to %s, want %s", got, want)
	}
	// The assertion that can actually fail: it must not stop at the interface, which is one
	// level down and has no power/control of its own.
	if filepath.Base(filepath.Dir(filepath.Dir(got))) == "1-1:1.0" {
		t.Error("stopped at the USB interface instead of the device")
	}
}

func TestAutosuspendOffWritesOnlyWhenItHasTo(t *testing.T) {
	t.Run("auto is turned off, and says it changed something", func(t *testing.T) {
		sysRoot, devPath := fixtureSys(t, true, true) // the fixture ships `auto`
		control, changed, err := autosuspendOff(sysRoot, devPath)
		if err != nil || !changed {
			t.Fatalf("autosuspendOff = (%q, %v, %v), want a change and no error", control, changed, err)
		}
		if got := readControl(t, control); got != "on" {
			t.Errorf("power/control is %q, want \"on\"", got)
		}
	})

	t.Run("already on is left alone", func(t *testing.T) {
		sysRoot, devPath := fixtureSys(t, true, true)
		control, err := autosuspendControlPath(sysRoot, devPath)
		if err != nil {
			t.Fatal(err)
		}
		// What a udev rule leaves behind, on a file an unprivileged tether may not write —
		// which is the whole case: read-only, so a write would fail and warn about a system
		// that is already correct.
		if err := os.WriteFile(control, []byte("on\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(control, 0o444); err != nil {
			t.Fatal(err)
		}
		got, changed, err := autosuspendOff(sysRoot, devPath)
		if err != nil {
			t.Fatalf("autosuspendOff on an already-off control: %v, want it to notice and not write", err)
		}
		if changed {
			t.Errorf("reported a change, want none: the control already said \"on\"")
		}
		if got != control {
			t.Errorf("control = %q, want %q", got, control)
		}
	})

	t.Run("a control it cannot write is an error carrying the path", func(t *testing.T) {
		sysRoot, devPath := fixtureSys(t, true, true) // still `auto`, so it must try
		control, err := autosuspendControlPath(sysRoot, devPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(control, 0o444); err != nil {
			t.Fatal(err)
		}
		got, changed, err := autosuspendOff(sysRoot, devPath)
		if err == nil {
			t.Fatalf("autosuspendOff = (%q, %v, nil), want the write to fail", got, changed)
		}
		// The path comes back with the error because the log line quotes it: an operator who
		// is told to write a udev rule needs to know which device it is for.
		if got != control {
			t.Errorf("control = %q, want %q even on failure", got, control)
		}
	})
}

func readControl(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestAutosuspendControlPathErrorsRatherThanGuessing(t *testing.T) {
	t.Run("not a USB device", func(t *testing.T) {
		sysRoot, devPath := fixtureSys(t, false, true)
		if got, err := autosuspendControlPath(sysRoot, devPath); err == nil {
			t.Errorf("walked to %s, want an error: nothing in the tree carries idVendor", got)
		}
	})
	t.Run("no power/control", func(t *testing.T) {
		sysRoot, devPath := fixtureSys(t, true, false)
		if got, err := autosuspendControlPath(sysRoot, devPath); err == nil {
			t.Errorf("walked to %s, want an error: the device has no power/control", got)
		}
	})
	t.Run("no such tty", func(t *testing.T) {
		sysRoot, _ := fixtureSys(t, true, true)
		if got, err := autosuspendControlPath(sysRoot, filepath.Join(t.TempDir(), "ttyNOPE")); err == nil {
			t.Errorf("walked to %s, want an error: the path does not exist", got)
		}
	})
}

func TestDescribeUSBReadsTheDescriptorsTheTableNeeds(t *testing.T) {
	sysRoot, devPath := fixtureSys(t, true, true)

	got, err := describeUSB(sysRoot, devPath)
	if err != nil {
		t.Fatalf("describeUSB: %v", err)
	}
	want := family.Device{
		Vendor: "10c4", Product: "ea60",
		Manufacturer: "ITead", Name: "Sonoff Zigbee 3.0 USB Dongle Plus",
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// The point of reading them: the ids alone are the stock CP210x bridge id and identify
	// nothing. Only together with the strings do they resolve.
	params, err := family.Resolve(got)
	if err != nil {
		t.Fatalf("the descriptors we read do not resolve: %v", err)
	}
	if params.Radio != family.ZNP {
		t.Errorf("resolved to %q, want %q", params.Radio, family.ZNP)
	}
}

func TestDescribeUSBFailsWhenThereIsNoUSBDeviceToDescribe(t *testing.T) {
	sysRoot, devPath := fixtureSys(t, false, true)
	if got, err := describeUSB(sysRoot, devPath); err == nil {
		t.Errorf("described %+v; want an error, nothing in the tree carries USB ids", got)
	}
}

// The whole chain against real hardware: a real adapter's real sysfs, read and resolved.
// Skipped where there is none, which includes CI — this is the seam between tier 1 and the
// hardware tier, and it is here because the machine that runs it has the stick.
func TestDescribeAndResolveARealAdapter(t *testing.T) {
	links, _ := filepath.Glob("/dev/serial/by-id/*")
	if len(links) == 0 {
		t.Skip("no USB serial adapter present")
	}
	for _, link := range links {
		d, err := DescribeUSB(link)
		if err != nil {
			t.Errorf("DescribeUSB(%s): %v", link, err)
			continue
		}
		t.Logf("%s → %s", filepath.Base(link), d)
		params, err := family.Resolve(d)
		if err != nil {
			// Not a failure: an unrecognised adapter is a legitimate answer, and the error
			// being readable is the thing worth seeing.
			t.Logf("  unresolved: %v", err)
			continue
		}
		name, _ := family.Identify(d)
		t.Logf("  → %s: radio=%s baud=%d flow=%s", name, params.Radio, params.Baud, params.Flow)
	}
}
