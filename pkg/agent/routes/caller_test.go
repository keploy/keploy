package routes

import (
	"net/netip"
	"testing"
)

// A link-local IPv6 caller's address names its zone (fe80::1%eth0), which
// the kernel's socket tables do not: the two are one endpoint. Another
// address or port is not, and IPv4-mapped IPv6 is its IPv4 address.
func TestSameEndpointReadsAnAddressWithoutItsZone(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"[fe80::1%eth0]:5000", "[fe80::1]:5000", true},
		{"[fe80::1]:5000", "[fe80::1%lo]:5000", true},
		{"[::ffff:127.0.0.1]:5000", "127.0.0.1:5000", true},
		{"[fe80::1%eth0]:5000", "[fe80::2]:5000", false},
		{"[fe80::1%eth0]:5000", "[fe80::1]:5001", false},
	} {
		if got := sameEndpoint(netip.MustParseAddrPort(c.a), netip.MustParseAddrPort(c.b)); got != c.same {
			t.Errorf("sameEndpoint(%s, %s) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}
