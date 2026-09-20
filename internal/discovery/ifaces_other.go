//go:build !linux

package discovery

import "net"

// registerOn is the interfaces the advert is registered at: the multicast-capable ones that
// are not loopback, fixed at advert time. Fixed costs nothing here — the library cannot follow
// link changes off Linux ("unable to wait for link updates", in tether's own log on Windows) —
// and the loopback interface has to be named out, because on Windows and macOS it is
// multicast-capable and the library's default would announce its addresses (see advertisable).
func registerOn() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	return advertisable(ifaces)
}
