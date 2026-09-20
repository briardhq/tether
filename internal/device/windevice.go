package device

import (
	"strconv"
	"strings"
)

// The pure part of the Windows identification path — instance IDs, which nodes are one piece
// of hardware, and what order COM names come in. It carries no build tag on purpose: none of
// it calls Windows, all of it can be wrong, and outside the tag its tests run on the
// per-commit gate rather than only when somebody presses the Windows button.

// parseInstanceID pulls the USB ids out of a Windows device instance ID.
//
// Two spellings reach it, because the node that owns the COM port is not always the USB node.
// A CP210x or a CDC device carries the port on the USB node itself,
// `USB\VID_10C4&PID_EA60\0001`; an FTDI device carries it on a child under its own enumerator,
// `FTDIBUS\VID_0403+PID_6015+DE03188111A\0000`. The separator differs and the enumerator differs,
// so this reads the ids by name rather than by position.
//
// The ids come back lowercase, which is what the family table and sysfs both use — the instance
// ID gives them uppercase.
func parseInstanceID(id string) (vendor, product string, ok bool) {
	vendor, okV := hexAfter(id, "VID_")
	product, okP := hexAfter(id, "PID_")
	return vendor, product, okV && okP
}

// hexAfter returns the four hex digits following marker, lowercased.
func hexAfter(id, marker string) (string, bool) {
	i := strings.Index(strings.ToUpper(id), marker)
	if i < 0 {
		return "", false
	}
	rest := id[i+len(marker):]
	if len(rest) < 4 {
		return "", false
	}
	out := strings.ToLower(rest[:4])
	for _, c := range out {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return out, true
}

// physical is the key that says which nodes are the same piece of hardware — the Windows form
// of one device being one adapter. A composite adapter exposes one node per function
// (`…&MI_00`, `…&MI_03`) and a device behind a per-vendor enumerator exposes a child node; in
// both the parent is the physical device and deduping on it is right. A plain single-port stick
// is its own device and must be keyed on itself: two of them on one hub share a parent, so
// keying everything on the parent would merge two adapters into one, which is the opposite
// mistake and the worse one.
func physical(instance, parent string) string {
	id := strings.ToUpper(instance)
	composite := strings.Contains(id, "&MI_")
	child := !strings.HasPrefix(id, `USB\`)
	if (composite || child) && parent != "" {
		return strings.ToUpper(parent)
	}
	return id
}

// lessPort orders COM names the way a human counts them. Which node of a composite adapter is the
// radio cannot be read off the descriptors — they are identical — so detection takes the first,
// as the other implementation does; zigbee-herdsman sorts those paths as strings, which puts
// COM10 before COM9, and that is the one detail of theirs not worth copying.
func lessPort(a, b string) bool {
	na, oka := comNumber(a)
	nb, okb := comNumber(b)
	if oka && okb {
		return na < nb
	}
	return a < b
}

func comNumber(port string) (int, bool) {
	if !strings.HasPrefix(strings.ToUpper(port), "COM") {
		return 0, false
	}
	n, err := strconv.Atoi(port[3:])
	return n, err == nil
}
