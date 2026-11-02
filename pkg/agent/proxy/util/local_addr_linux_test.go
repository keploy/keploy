//go:build linux

package util

import (
	"net"
	"net/netip"
	"strconv"
	"testing"
)

// IsLocalAddr says yes for the namespace's own addresses (the loopback's and
// each interface's, a link-local one on its device) and no for an address it
// does not have, which a socket can listen at only with IP_FREEBIND.
func TestIsLocalAddrSaysWhichAddressesAreTheNamespacesOwn(t *testing.T) {
	check := func(a netip.Addr, want bool) {
		t.Helper()
		got, err := IsLocalAddr(a)
		if err != nil {
			t.Fatalf("IsLocalAddr(%s): %v", a, err)
		}
		if got != want {
			t.Errorf("IsLocalAddr(%s) = %v, want %v", a, got, want)
		}
	}
	check(netip.MustParseAddr("127.0.0.1"), true)
	check(netip.MustParseAddr("127.0.0.2"), true)
	check(netip.MustParseAddr("::ffff:127.0.0.1"), true)
	ifcs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	own := map[netip.Addr]bool{}
	for _, ifc := range ifcs {
		addrs, err := ifc.Addrs()
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, _ := netip.AddrFromSlice(n.IP)
			ip = ip.Unmap()
			own[ip] = true
			if ip.Is6() && ip.IsLinkLocalUnicast() {
				ip = ip.WithZone(strconv.Itoa(ifc.Index))
			}
			check(ip, true)
		}
	}
	for _, s := range []string{"192.0.2.1", "203.0.113.7", "2001:db8::7"} {
		if a := netip.MustParseAddr(s); !own[a] {
			check(a, false)
		}
	}
}
