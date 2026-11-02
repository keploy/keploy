//go:build !linux

package util

import (
	"net"
	"net/netip"
	"strconv"
	"testing"
)

// Where the kernel's routes are not asked (every platform but Linux), an
// address is the host's own when one of its interfaces has it: each
// address an interface has, in its IPv4-mapped form too, and a link-local
// one with its interface's zone by name or by index. One that no interface
// has is not, nor is a link-local address with the zone of an interface
// that does not have it, nor the unspecified address. An invalid address,
// and a zone that names no device, are errors.
func TestIsLocalAddrSaysAnAddressIsTheHostsOwnWhenAnInterfaceHasIt(t *testing.T) {
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	check := func(a netip.Addr, want bool) {
		t.Helper()
		if got, err := IsLocalAddr(a); err != nil || got != want {
			t.Errorf("IsLocalAddr(%s) = %v, %v; want %v, nil", a, got, err, want)
		}
	}
	own := map[netip.Addr]bool{}
	type linkLocal struct {
		a   netip.Addr
		ifc net.Interface
	}
	var lls []linkLocal
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range addrs {
			n, ok := x.(*net.IPNet)
			if !ok {
				continue
			}
			a, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			a = a.Unmap()
			own[a] = true
			if a.Is6() && a.IsLinkLocalUnicast() {
				lls = append(lls, linkLocal{a, ifc})
				check(a.WithZone(ifc.Name), true)
				check(a.WithZone(strconv.Itoa(ifc.Index)), true)
				continue
			}
			check(a, true)
			if a.Is4() {
				check(netip.AddrFrom16(a.As16()), true)
			}
		}
	}
	if !own[netip.AddrFrom4([4]byte{127, 0, 0, 1})] {
		t.Fatalf("no interface has 127.0.0.1 here: %v", own)
	}
	if a := netip.MustParseAddr("192.0.2.77"); !own[a] {
		check(a, false)
	}
	check(netip.IPv4Unspecified(), false)
	check(netip.IPv6Unspecified(), false)
	// A link-local address with the zone of an interface that does not have it.
	for _, ll := range lls {
		for _, other := range ifs {
			if other.Index == ll.ifc.Index || hasAddr(t, other, ll.a) {
				continue
			}
			check(ll.a.WithZone(other.Name), false)
			break
		}
	}
	for _, a := range []netip.Addr{{}, netip.MustParseAddr("fe80::1%no-such-dev")} {
		if got, err := IsLocalAddr(a); err == nil {
			t.Errorf("IsLocalAddr(%v) = %v, nil; want an error", a, got)
		}
	}
}

func hasAddr(t *testing.T, ifc net.Interface, a netip.Addr) bool {
	t.Helper()
	addrs, err := ifc.Addrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range addrs {
		if n, ok := x.(*net.IPNet); ok {
			if b, ok := netip.AddrFromSlice(n.IP); ok && b.Unmap() == a {
				return true
			}
		}
	}
	return false
}
