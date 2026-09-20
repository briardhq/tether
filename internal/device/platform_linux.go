//go:build linux

package device

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"briard.io/tether/internal/family"
)

// goneErrors are what the tty layer reports once the USB device behind it is removed. EIO is
// the common one; ENODEV and ENXIO turn up depending on where in teardown the read landed.
var goneErrors = []error{syscall.EIO, syscall.ENODEV, syscall.ENXIO}

// openControl opens the tty for termios work and returns the descriptor to hold onto, with
// HUPCL already cleared so that closing the port leaves DTR and RTS asserted rather than
// dropping them (INV 7: one assert per plug event, not one per open).
//
// It is deliberately its own open, before the serial library's, and — unlike an ordinary
// pre-open — it is **kept**. That is what makes flow control reachable at all: the library
// hardcodes RTS/CTS off inside its own open and exposes no descriptor, and it sets TIOCEXCL,
// which locks out opens coming *after* it but cannot invalidate one that came before. termios
// belongs to the tty rather than to a descriptor, so this one can still set CRTSCTS afterwards.
//
// Opening here raises the lines if they were low — the one edge we accept, reachable only
// straight after enumeration, when the plug event has just power-cycled the radio anyway.
func openControl(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return -1, fmt.Errorf("opening to set control-line policy: %w", classify(err, false))
	}

	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("reading termios: %w", classify(err, false))
	}
	if t.Cflag&unix.HUPCL != 0 {
		t.Cflag &^= unix.HUPCL
		if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("clearing HUPCL: %w", classify(err, false))
		}
	}
	return fd, nil
}

// applyFlowControl turns hardware RTS/CTS on for the families that need it, through the
// descriptor openControl kept. It runs after the library has opened and applied its own
// termios, because that ends with an unconditional "RTS/CTS off" which would otherwise undo us.
//
// ⚠️ Turning this on hands **RTS to the kernel** as a flow-control output. For these adapters
// that is correct, and is what their own client does — but it is a documented exception to
// INV 7 rather than a detail: RTS stops being a pinned management line on them, and therefore
// cannot also be a reset line, which constrains the management verbs for exactly these rows.
func applyFlowControl(fd int, on bool) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return fmt.Errorf("reading termios: %w", classify(err, false))
	}
	if (t.Cflag&unix.CRTSCTS != 0) == on {
		return nil
	}
	if on {
		t.Cflag |= unix.CRTSCTS
	} else {
		t.Cflag &^= unix.CRTSCTS
	}
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return fmt.Errorf("setting RTS/CTS flow control: %w", classify(err, false))
	}
	return nil
}

// closeControl releases the retained descriptor. HUPCL is clear, so this drops no line.
func closeControl(fd int) {
	if fd >= 0 {
		unix.Close(fd)
	}
}

// disableAutosuspend stops the kernel powering the adapter down when it looks idle. A Zigbee
// coordinator is idle for long stretches by design and must still answer instantly, so
// autosuspend turns into the "works fine, then dies after N minutes" class — the load-dependent
// failure this program exists to own.
//
// ⚠️ **What it buys is narrower than it looks, and worth knowing before anyone extends it.**
// Measured on both sticks in hand: `cp210x` and `cdc_acm` hold a runtime-PM reference for the
// lifetime of the open tty, so staging `auto` underneath an open port suspended neither — the
// Sonoff went down only 488 ms after the port was *closed*, which is the harmless case, since
// the next open resumes it. So this is insurance against a driver that *does* release the
// reference, not the guard keeping these two alive. It stays because the failure it insures
// against has no recovery path: neither stick can signal remote wakeup (`bmAttributes` 0x80,
// bit 5 clear), so a coordinator suspended with its port open has no way to announce an
// inbound frame at all.
//
// Best-effort: it needs write access to sysfs, which an unprivileged tether will not have. It
// never fails the open, but it never passes silently either — the log line carries the fix.
func disableAutosuspend(path string) {
	control, changed, err := autosuspendOff("/sys", path)
	switch {
	case control == "":
		log.Printf("device: could not locate the USB power control for %s (%v); "+
			"if this adapter drops out after minutes of quiet, autosuspend is the first suspect", path, err)
	case err != nil:
		log.Printf("device: could not disable USB autosuspend via %s (%v); "+
			"run tether with write access to sysfs, or set it out of band with a udev rule "+
			"(ATTR{power/control}=\"on\")", control, err)
	case changed:
		log.Printf("device: USB autosuspend disabled via %s", control)
	default:
		log.Printf("device: USB autosuspend is already off for %s, so nothing here changed it "+
			"— a udev rule is the usual reason, and is what an unprivileged tether needs", control)
	}
}

// autosuspendOnValue is what sysfs calls runtime PM being off for a device: `on` means "keep it
// powered", `auto` means "suspend it when it looks idle".
const autosuspendOnValue = "on"

// autosuspendOff forces runtime PM off for the USB device behind path, and reports whether it
// had to. sysRoot is a parameter for the same reason its neighbours take one — so this is
// testable against a fixture tree rather than the host's own /sys.
//
// **It reads before it writes, and that is not an optimisation.** A udev rule may have set this
// already, which on an unprivileged tether is the *expected* deployment rather than an edge
// case: writing anyway fails for want of privilege and warns about a problem that is not there,
// which is the worst kind of log line — one that sends an operator to fix something already
// correct.
func autosuspendOff(sysRoot, path string) (control string, changed bool, err error) {
	control, err = autosuspendControlPath(sysRoot, path)
	if err != nil {
		return "", false, err
	}
	if current, rerr := os.ReadFile(control); rerr == nil &&
		strings.TrimSpace(string(current)) == autosuspendOnValue {
		return control, false, nil
	}
	// A failed read falls through to the write on purpose: the write's error is the one that
	// carries the fix, and a control file we cannot read is not evidence about its contents.
	if err = os.WriteFile(control, []byte(autosuspendOnValue), 0o644); err != nil {
		return control, false, err
	}
	return control, true, nil
}

// usbDeviceDir finds the sysfs directory of the USB device behind a tty. sysRoot is a
// parameter so the walk is testable against a fixture tree rather than the host's own /sys,
// which on a CI runner has no USB serial adapter in it at all.
//
// The walk: /dev/… resolves to a tty name, /sys/class/tty/<name>/device is the USB *interface*,
// and the device that owns power management and the descriptors is its parent — identified by
// carrying idVendor, which interfaces do not.
func usbDeviceDir(sysRoot, devPath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", devPath, err)
	}
	name := filepath.Base(resolved)
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("%s does not name a tty", devPath)
	}
	return usbDeviceDirOf(sysRoot, name)
}

// usbDeviceDirOf is the same walk starting from a tty name rather than a device path, which is
// the direction detection reads in: it already has the names, from /sys/class/tty itself, and
// resolving each back through /dev only to derive the name again would be work to undo work.
func usbDeviceDirOf(sysRoot, name string) (string, error) {
	dir, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "class", "tty", name, "device"))
	if err != nil {
		return "", fmt.Errorf("resolving the sysfs device for %s: %w", name, err)
	}

	// Walk up to the USB device. Bounded so a symlink loop or an unfamiliar bus topology
	// cannot spin here; USB nesting is shallow and this is far past any real depth.
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "idVendor")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no USB device above %s (not a USB adapter?)", name)
}

// autosuspendControlPath is the power/control file of that device.
func autosuspendControlPath(sysRoot, devPath string) (string, error) {
	dir, err := usbDeviceDir(sysRoot, devPath)
	if err != nil {
		return "", err
	}
	control := filepath.Join(dir, "power", "control")
	if _, err := os.Stat(control); err != nil {
		return "", fmt.Errorf("%s has no power/control: %w", dir, err)
	}
	return control, nil
}

// describeUSB reads the descriptors that identify which adapter this is. They are what the
// family table is keyed on, because the ids alone do not identify anything: 10c4:ea60 is the
// stock CP210x bridge id and is worn by ZNP, EZSP and unrelated hardware alike — see
// ARCHITECTURE.md "The family table".
//
// Manufacturer and product are optional in USB and absent on some adapters; a missing one is
// an empty string, not an error, and the table then simply fails to match — which is the
// honest outcome.
func describeUSB(sysRoot, devPath string) (family.Device, error) {
	dir, err := usbDeviceDir(sysRoot, devPath)
	if err != nil {
		return family.Device{}, err
	}
	return describeAt(dir)
}

// describeAt reads the descriptors out of a USB device directory already found.
func describeAt(dir string) (family.Device, error) {
	read := func(attr string) string {
		b, err := os.ReadFile(filepath.Join(dir, attr))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	d := family.Device{
		Vendor:       read("idVendor"),
		Product:      read("idProduct"),
		Manufacturer: read("manufacturer"),
		Name:         read("product"),
	}
	if d.Vendor == "" || d.Product == "" {
		return family.Device{}, fmt.Errorf("%s carries no USB ids", dir)
	}
	return d, nil
}

// DescribeUSB identifies the adapter behind a device path, for the family table to resolve.
func DescribeUSB(devPath string) (family.Device, error) {
	return describeUSB("/sys", devPath)
}

// adviseUnstablePath warns about a kernel-assigned name. /dev/ttyUSB0 is handed out in
// enumeration order, so a reboot or a second adapter silently renames the dongle and the
// config points at something else — the config-assembly failure class, and one of the failures
// that is pure paperwork rather than a real fault. Advice only: a caller who means ttyUSB0
// gets it.
func adviseUnstablePath(path string) {
	if strings.HasPrefix(path, "/dev/serial/by-id/") {
		return
	}
	if byID := findByIDPath(path); byID != "" {
		log.Printf("device: %s is a kernel-assigned name and can move between boots; "+
			"the same device is at %s, which cannot", path, byID)
		return
	}
	log.Printf("device: %s is a kernel-assigned name and can move between boots; "+
		"prefer a /dev/serial/by-id/… path if this device has one", path)
}

// findByIDPath returns the /dev/serial/by-id link pointing at the same device as path, or ""
// if there is none. Every failure to find one is the same answer: nothing useful to say.
func findByIDPath(path string) string {
	return findByIDIn("/dev/serial/by-id", path)
}

// findByIDIn is that lookup against a given directory, so the fixture tests can hold one.
func findByIDIn(dir, path string) string {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	links, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return ""
	}
	for _, link := range links {
		if resolved, err := filepath.EvalSymlinks(link); err == nil && resolved == target {
			return link
		}
	}
	return ""
}

// Detect returns every attached adapter the family table recognises, so that tether can find
// the coordinator rather than be told where it is. This is the family table read backwards —
// "which of these is a coordinator" instead of "what is this one" — and it adds no knowledge:
// an adapter the table does not name is not a candidate, which is what keeps a random
// USB-serial cable or an Arduino out of the answer.
//
// It opens nothing and touches no control line. Deciding what to do with none, one, or several
// is the caller's, not this function's: the ambiguity is a policy question and this is a fact.
func Detect() ([]Adapter, error) {
	return detect("/sys", "/dev", "/dev/serial/by-id")
}

func detect(sysRoot, devRoot, byIDRoot string) ([]Adapter, error) {
	entries, err := os.ReadDir(filepath.Join(sysRoot, "class", "tty"))
	if err != nil {
		return nil, fmt.Errorf("looking for attached adapters: %w", err)
	}

	var found []Adapter
	// One USB device is one adapter, however many ttys it exposes. A composite adapter
	// puts two of them on one device — the TI LaunchPad's XDS110 carries the radio on one and a
	// debug port on the other — and both read the same descriptors, so counting ttys would
	// resolve the same stick twice and refuse it as two attached coordinators. Which tty is the
	// radio cannot be read off the descriptors either; zigbee-herdsman owns that board and says
	// it is the first by path, so take the first and let ReadDir's ordering supply it.
	seen := make(map[string]bool)
	for _, entry := range entries {
		// Most of /sys/class/tty is virtual consoles with no USB device above them, so every
		// step here is allowed to fail quietly: failing to be a coordinator is the common
		// case and not a fault worth reporting.
		dir, err := usbDeviceDirOf(sysRoot, entry.Name())
		if err != nil {
			continue
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		d, err := describeAt(dir)
		if err != nil {
			continue
		}
		params, err := family.Resolve(d)
		if err != nil {
			continue
		}
		name, _ := family.Identify(d)

		path := filepath.Join(devRoot, entry.Name())
		if byID := findByIDIn(byIDRoot, path); byID != "" {
			path = byID
		}
		found = append(found, Adapter{Path: path, Device: d, Params: params, Name: name})
	}

	// Ordered, so that two adapters are reported to an operator in the same order twice
	// running and a test can say which is which.
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	return found, nil
}
