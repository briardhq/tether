package device

import (
	"strings"
	"testing"
	"time"
)

// The instance ID is where Windows keeps the only two fields that identify an adapter without
// asking the device anything. Both spellings are real: the second is what an FTDI stick's
// port node looks like, and reading it positionally rather than by name would miss it.
func TestParseInstanceID(t *testing.T) {
	for _, tc := range []struct {
		id              string
		vendor, product string
		ok              bool
	}{
		{`USB\VID_10C4&PID_EA60\0001`, "10c4", "ea60", true},
		{`USB\VID_1CF1&PID_0030\DE0001`, "1cf1", "0030", true},
		// An FTDI port node: a different enumerator, "+" for "&", and a serial with a trailing
		// letter the driver appends. The ids are still in it.
		{`FTDIBUS\VID_0403+PID_6015+DE03188111A\0000`, "0403", "6015", true},
		// The parent of that node, which is where its bus-reported name lives.
		{`USB\VID_0403&PID_6015\DE03188111`, "0403", "6015", true},
		// Everything else on the machine, which is most of what the enumeration returns.
		{`ACPI\PNP0C02\1`, "", "", false},
		{`HTREE\ROOT\0`, "", "", false},
		{`USB\ROOT_HUB30\4&1b2f3c4d&0&0`, "", "", false},
		// Truncated and non-hex forms must not produce a plausible-looking id: a wrong pair
		// here resolves as the wrong family, which is the one outcome the table must never
		// produce.
		{`USB\VID_10C&PID_EA60\0001`, "", "", false},
		{`USB\VID_10C4&PID_EAZZ\0001`, "", "", false},
		{`USB\VID_10C4\0001`, "", "", false},
	} {
		vendor, product, ok := parseInstanceID(tc.id)
		if ok != tc.ok || (ok && (vendor != tc.vendor || product != tc.product)) {
			t.Errorf("parseInstanceID(%q) = %q, %q, %v; want %q, %q, %v",
				tc.id, vendor, product, ok, tc.vendor, tc.product, tc.ok)
		}
	}
}

// Which nodes are one piece of hardware is the Windows half of one device being one adapter,
// and both mistakes it can make are bad: splitting one adapter into two makes tether refuse a
// stick that is plainly there, and merging two into one makes it open a coordinator without
// saying there was a choice.
func TestPhysicalDevice(t *testing.T) {
	const (
		launchpad = `USB\VID_0451&PID_BEF3\L1100001`
		hub       = `USB\ROOT_HUB30\4&1b2f3c4d&0&0`
		ftdiUSB   = `USB\VID_0403&PID_6015\DE03188111`
	)
	// The two function nodes of one composite adapter are one device.
	a := physical(`USB\VID_0451&PID_BEF3&MI_00\6&1a2b3c&0&0000`, launchpad)
	b := physical(`USB\VID_0451&PID_BEF3&MI_03\6&1a2b3c&0&0003`, launchpad)
	if a != b {
		t.Errorf("the two functions of one LaunchPad keyed as %q and %q, want one key", a, b)
	}
	// A port node under a per-vendor enumerator is its USB parent.
	if got := physical(`FTDIBUS\VID_0403+PID_6015+DE03188111A\0000`, ftdiUSB); got != strings.ToUpper(ftdiUSB) {
		t.Errorf("an FTDI port node keyed as %q, want its USB parent", got)
	}
	// ⚠️ And the one that would be silent: two plain sticks on one hub share a parent and are
	// still two adapters. Keying every node on its parent would merge them and serve one.
	first := physical(`USB\VID_10C4&PID_EA60\0001`, hub)
	second := physical(`USB\VID_10C4&PID_EA60\0002`, hub)
	if first == second {
		t.Errorf("two sticks on one hub both keyed as %q, want two keys", first)
	}
}

// Detection takes the first port of a composite adapter, so what "first" means is load-bearing:
// the alternative is a debug port opened at 115200, which answers nothing and reads as a dead
// coordinator.
func TestLessPortCountsRatherThanSpells(t *testing.T) {
	if !lessPort("COM9", "COM10") {
		t.Error("COM10 sorted before COM9, which is the string order and not the human one")
	}
	if !lessPort("COM3", "COM4") {
		t.Error("COM4 sorted before COM3")
	}
	// A node with no port has nothing to count, so it falls back to string order and sorts
	// first. That costs nothing — the walk that uses this ordering skips portless nodes — and
	// is asserted only so the comparison stays total, which sort.SliceStable requires.
	if lessPort("COM3", "") || !lessPort("", "COM3") {
		t.Error("comparing a named port with an unnamed one is not a total order")
	}
}

// The two bytes that matter sit at offset 4, after the struct's own size. The fixtures are the
// Sonoff's own CM_POWER_DATA as Windows reported it, awake with its port open and asleep ten
// seconds after closing it — the field that changed, and nothing else did.
func TestPowerDataAsleep(t *testing.T) {
	awake := []byte{56, 0, 0, 0, 1, 0, 0, 0, 25, 0, 0, 0}
	asleep := []byte{56, 0, 0, 0, 4, 0, 0, 0, 25, 0, 0, 0}
	if got, err := powerDataAsleep(awake); err != nil || got {
		t.Errorf("D0 read as asleep=%v, err=%v", got, err)
	}
	if got, err := powerDataAsleep(asleep); err != nil || !got {
		t.Errorf("D3 read as asleep=%v, err=%v", got, err)
	}
	if _, err := powerDataAsleep([]byte{56, 0, 0}); err == nil {
		t.Error("a truncated struct read as a state, want an error")
	}
	if _, err := powerDataAsleep([]byte{56, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Error("PowerDeviceUnspecified read as a state, want an error rather than a guess")
	}
}

// The clock charges an interval only to a sample that saw the device asleep, and the first
// sample, having no interval behind it, charges nothing.
func TestSuspendClockChargesOnlyIntervalsThatEndAsleep(t *testing.T) {
	var c suspendClock
	t0 := time.Unix(1000, 0)
	steps := []struct {
		asleep bool
		at     time.Duration
		want   time.Duration
	}{
		{true, 0, 0}, // first sample: no interval yet, even asleep
		{false, 60 * time.Second, 0},
		{true, 120 * time.Second, 60 * time.Second},
		{true, 180 * time.Second, 120 * time.Second},
		{false, 240 * time.Second, 120 * time.Second},
	}
	for i, s := range steps {
		if got := c.observe(s.asleep, t0.Add(s.at)); got != s.want {
			t.Errorf("step %d: total %v, want %v", i, got, s.want)
		}
	}
}
