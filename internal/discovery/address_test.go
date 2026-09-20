package discovery

import (
	"net"
	"testing"

	"briard.io/tether/internal/family"
)

// An address the caller fixes is what the advert carries — on every interface it goes out on,
// in place of that interface's own — and no address means each interface speaks for itself.
func TestAFixedAddressIsWhatTheAdvertCarries(t *testing.T) {
	params, _ := family.Defaults(family.ZNP)
	fixed, err := New(Config{Instance: "attic", Port: 6638, Params: params, Address: net.ParseIP("192.168.1.5")})
	if err != nil {
		t.Fatal(err)
	}
	if got := fixed.service.IPs; len(got) != 1 || !got[0].Equal(net.ParseIP("192.168.1.5")) {
		t.Errorf("IPs = %v, want [192.168.1.5]", got)
	}
	derived, err := New(Config{Instance: "attic", Port: 6638, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if got := derived.service.IPs; len(got) != 0 {
		t.Errorf("IPs = %v, want none — the interfaces answer for themselves", got)
	}
}
