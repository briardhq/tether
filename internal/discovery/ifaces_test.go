package discovery

import (
	"net"
	"reflect"
	"testing"
)

// The Windows shape, on any OS: a loopback interface that is also multicast-capable. That is
// the one the library's filter lets through, and the one whose address a client then connects
// to — itself.
func TestAdvertisableLeavesOutLoopbackEvenWhenItIsMulticastCapable(t *testing.T) {
	ifaces := []net.Interface{
		{Name: "Loopback Pseudo-Interface 1", Flags: net.FlagUp | net.FlagLoopback | net.FlagMulticast},
		{Name: "Ethernet", Flags: net.FlagUp | net.FlagMulticast | net.FlagRunning},
		{Name: "Wi-Fi (down)", Flags: net.FlagMulticast},
		{Name: "Teredo (no multicast)", Flags: net.FlagUp | net.FlagPointToPoint},
		{Name: "Ethernet 2", Flags: net.FlagUp | net.FlagMulticast},
	}
	got := advertisable(ifaces)
	want := []string{"Ethernet", "Ethernet 2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("advertisable = %v, want %v", got, want)
	}
}

// And with nothing eligible the answer is nil, which hands the decision back to the library —
// a machine with no usable interface has no LAN to be wrong on.
func TestAdvertisableWithNothingEligibleIsNil(t *testing.T) {
	if got := advertisable([]net.Interface{{Name: "lo", Flags: net.FlagUp | net.FlagLoopback}}); got != nil {
		t.Fatalf("advertisable = %v, want nil", got)
	}
}
