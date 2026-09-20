//go:build linux

package discovery

// registerOn is the interfaces the advert is registered at — nil here, which is the library's
// default: every multicast interface, re-evaluated as links come and go, which on Linux it
// follows over netlink. A list fixed at advert time would miss the NIC that comes up after
// tether does — the boot-order race on a NUC — and `lo` is not multicast-capable here, so the
// default already excludes it (see advertisable, and its _other sibling for where it does not).
func registerOn() []string { return nil }
