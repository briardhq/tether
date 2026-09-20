package main

import (
	"net"
	"testing"
)

// The three answers to "what address does the advert carry", in the order they win: the
// operator's, the listener's when it is bound to one address, and nobody's — nil, which hands
// the question to the interfaces — when the bind was the wildcard.
func TestTheAdvertisedAddressIsTheOperatorsThenTheListenersThenNobodys(t *testing.T) {
	for _, tc := range []struct {
		what     string
		explicit string
		bound    net.IP
		want     net.IP
	}{
		{"the operator's word wins over a specific bind", "10.0.0.9", net.ParseIP("192.168.1.5"), net.ParseIP("10.0.0.9")},
		{"the operator's word wins over a wildcard bind", "10.0.0.9", net.IPv4zero, net.ParseIP("10.0.0.9")},
		{"a specific bind is advertised as itself", "", net.ParseIP("192.168.1.5"), net.ParseIP("192.168.1.5")},
		{"loopback is a specific bind too", "", net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.1")},
		{"an IPv4 wildcard leaves it to the interfaces", "", net.IPv4zero, nil},
		{"an IPv6 wildcard leaves it to the interfaces", "", net.IPv6unspecified, nil},
		{"no bound address at all leaves it to the interfaces", "", nil, nil},
	} {
		got := advertisedAddress(tc.explicit, tc.bound)
		if !got.Equal(tc.want) {
			t.Errorf("%s: advertisedAddress(%q, %v) = %v, want %v", tc.what, tc.explicit, tc.bound, got, tc.want)
		}
	}
}
