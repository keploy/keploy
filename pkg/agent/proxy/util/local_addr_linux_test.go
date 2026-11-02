//go:build linux

package util

import (
	"encoding/binary"
	"net/netip"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// namespaceAddrs is every address of this namespace's interfaces, read with
// its flags from rtnetlink (RTM_GETADDR), with whether it is usable: not
// tentative (in duplicate address detection, which an IPv6 address is for
// about a second after its device comes up, as a container's veth does) and
// not dadfailed (found to be another host's). The kernel has no local route
// for an address that is not usable, and nothing can bind or connect to it,
// so IsLocalAddr rightly says no for it. A link-local IPv6 address carries
// its device's index as its zone.
func namespaceAddrs(t *testing.T) map[netip.Addr]bool {
	t.Helper()
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETADDR, syscall.AF_UNSPEC)
	if err != nil {
		t.Fatalf("RTM_GETADDR: %v", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		t.Fatalf("RTM_GETADDR: %v", err)
	}
	addrs := map[netip.Addr]bool{}
	for i := range msgs {
		m := &msgs[i]
		if m.Header.Type != syscall.RTM_NEWADDR || len(m.Data) < syscall.SizeofIfAddrmsg {
			continue
		}
		flags := uint32(m.Data[2]) // ifa_flags; IFA_FLAGS, when there, has them all
		index := binary.NativeEndian.Uint32(m.Data[4:8])
		attrs, err := syscall.ParseNetlinkRouteAttr(m)
		if err != nil {
			t.Fatalf("RTM_GETADDR: %v", err)
		}
		var local, address []byte
		for _, a := range attrs {
			switch a.Attr.Type {
			case unix.IFA_LOCAL:
				local = a.Value
			case unix.IFA_ADDRESS:
				address = a.Value
			case unix.IFA_FLAGS:
				if len(a.Value) >= 4 {
					flags = binary.NativeEndian.Uint32(a.Value)
				}
			}
		}
		raw := address
		if local != nil { // IFA_ADDRESS is a point-to-point link's peer
			raw = local
		}
		ip, ok := netip.AddrFromSlice(raw)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if ip.Is6() && ip.IsLinkLocalUnicast() {
			ip = ip.WithZone(strconv.FormatUint(uint64(index), 10))
		}
		addrs[ip] = flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0
	}
	return addrs
}

// IsLocalAddr says yes for the namespace's own addresses (the loopback's and
// each interface's, a link-local one on its device) and no for an address it
// does not have, which a socket can listen at only with IP_FREEBIND. An
// address still in duplicate address detection, or that failed it, is not
// yet (or not) the namespace's own, and is not asked about. Devices come and
// go while the test runs (containers start on the host), so an address
// IsLocalAddr says no for is read again, and fails the test only if it is
// still there and usable, and still said no for.
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
	own := map[netip.Addr]bool{}
	asked := 0
	for a, usable := range namespaceAddrs(t) {
		own[a.WithZone("")] = true
		if !usable {
			continue
		}
		asked++
		if got, err := IsLocalAddr(a); err == nil && got {
			continue
		}
		if !namespaceAddrs(t)[a] {
			continue // gone, or in duplicate address detection again
		}
		check(a, true)
	}
	if asked == 0 {
		t.Fatal("no usable address on any interface, not even the loopback's")
	}
	for _, s := range []string{"192.0.2.1", "203.0.113.7", "2001:db8::7"} {
		if a := netip.MustParseAddr(s); !own[a] {
			check(a, false)
		}
	}
}
