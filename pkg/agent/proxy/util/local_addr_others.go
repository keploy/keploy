//go:build !linux

package util

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// IsLocalAddr reports whether a is one of this host's own addresses: an
// address one of its interfaces has now (net.InterfaceAddrs; for an address
// with a zone, one the zone's interface has), the loopback's among them, so
// a connection to it is delivered on this host and never leaves it. The
// kernel's routes are not asked here as they are on Linux, so an address
// routed local that no interface has is not one; nor is the unspecified
// address, which no interface has.
func IsLocalAddr(a netip.Addr) (bool, error) {
	if !a.IsValid() {
		return false, fmt.Errorf("interface lookup of an invalid address")
	}
	a = a.Unmap()
	var addrs []net.Addr
	var err error
	if z := a.Zone(); z != "" {
		var ifc *net.Interface
		if n, perr := strconv.Atoi(z); perr == nil {
			ifc, err = net.InterfaceByIndex(n)
		} else {
			ifc, err = net.InterfaceByName(z)
		}
		if err != nil {
			return false, fmt.Errorf("interface lookup of %s: no device %q: %w", a, z, err)
		}
		addrs, err = ifc.Addrs()
	} else {
		addrs, err = net.InterfaceAddrs()
	}
	if err != nil {
		return false, fmt.Errorf("interface lookup of %s: %w", a, err)
	}
	bare := a.WithZone("")
	for _, x := range addrs {
		n, ok := x.(*net.IPNet)
		if !ok {
			continue
		}
		if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == bare {
			return true, nil
		}
	}
	return false, nil
}
