package discovery

import "net"

// fixedAddress is the library's `IPs`: set, it is what every registered interface announces
// in place of its own addresses; empty, each interface announces its own.
func fixedAddress(address net.IP) []net.IP {
	if address == nil {
		return nil
	}
	return []net.IP{address}
}

// advertisable is the set of interfaces an advert should be registered on, by name: up,
// multicast-capable, and not loopback.
//
// The library's own filter stops one flag short — it keeps anything that is Up and Multicast —
// which is right on Linux, where `lo` carries no MULTICAST flag, and wrong on Windows and macOS,
// where the loopback interface does. There the responder announced `127.0.0.1`/`::1` for the
// host name beside the LAN address, and zigbee-herdsman, which takes the first address a browse
// hands it, resolved `mdns://zigbee-coordinator` to itself and got ECONNREFUSED — measured
// with a real client. One flag more is the whole fix.
func advertisable(ifaces []net.Interface) []string {
	var names []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		names = append(names, iface.Name)
	}
	return names
}
