//go:build linux

package proxy

import (
	"errors"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/sys/unix"
)

// A socket bound to a device (SO_BINDTODEVICE; a VRF's sockets are bound to
// its device) is found by its two ends, over IPv4, IPv6, and an IPv6 socket
// connected over v4-mapped addresses: the kernel's exact lookup, asked with
// no device, misses it (its bound-device check), and the dump after the miss
// does not filter on the device. The kernel filters a dump on the two ports
// alone: beside an IPv4 or v4-mapped one, a socket with its ports and another
// local address, and one with its ports and another remote address, are not
// it. Those decoys are of its own family, so they are in the dump that finds
// it (a v4-mapped socket is in the IPv6 one), and its local and remote
// addresses differ (127.0.0.2 to 127.0.0.1), so a dump that compared its local
// address with the remote one would miss it. ::1 is the only IPv6 address lo
// has: the tcp6 case has one address at both ends and no decoys.
func TestASocketBoundToADeviceIsFound(t *testing.T) {
	lo1, lo2, lo3 := netip.AddrFrom4([4]byte{127, 0, 0, 1}), netip.AddrFrom4([4]byte{127, 0, 0, 2}), netip.AddrFrom4([4]byte{127, 0, 0, 3})
	for _, tc := range []struct {
		name   string
		family int
		addr   netip.Addr // the socket's own
		peer   netip.Addr // the listener's
		decoys bool
	}{
		{"tcp4", unix.AF_INET, lo2, lo1, true},
		{"tcp6", unix.AF_INET6, netip.IPv6Loopback(), netip.IPv6Loopback(), false},
		{"tcp6 over v4-mapped addresses", unix.AF_INET6, lo2, lo1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", netip.AddrPortFrom(tc.peer, 0).String())
			if err != nil {
				t.Skipf("no listener on %s here: %v", tc.peer, err)
			}
			defer func() { _ = ln.Close() }()
			port := ln.Addr().(*net.TCPAddr).Port
			fd := boundSocket(t, tc.family, "lo")
			// Its local end may be shared by a socket of another remote end
			// (the second decoy).
			if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
				t.Fatal(err)
			}
			if err := unix.Bind(fd, sockaddrOf(tc.family, netip.AddrPortFrom(tc.addr, 0))); err != nil {
				t.Fatalf("bind a socket bound to lo to %s: %v", tc.addr, err)
			}
			if err := unix.Connect(fd, sockaddrOf(tc.family, netip.AddrPortFrom(tc.peer, uint16(port)))); err != nil {
				t.Fatalf("connect a socket bound to lo: %v", err)
			}
			s, err := ln.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			local, remote := socketEnds(t, fd)
			if tc.decoys {
				if local.Addr() == remote.Addr() {
					t.Fatalf("the socket's ends %s -> %s share an address (this case's premise: they differ)", local, remote)
				}
				ln3, err := net.Listen("tcp4", netip.AddrPortFrom(lo3, remote.Port()).String())
				if err != nil {
					t.Fatalf("listen on %s at the first listener's port %d: %v", lo3, remote.Port(), err)
				}
				defer func() { _ = ln3.Close() }()
				decoy(t, tc.family, netip.AddrPortFrom(lo3, local.Port()), remote)
				decoy(t, tc.family, local, netip.AddrPortFrom(lo3, remote.Port()))
			}
			premiseExactMisses(t, local, remote)
			got, err := lookupTCP(local, remote)
			if err != nil || got.state != tcpEstablished || got.info == nil {
				t.Fatalf("lookupTCP of a socket bound to lo, %s -> %s = state %d, tcp_info %v, %v; want ESTABLISHED with its tcp_info", local, remote, got.state, got.info != nil, err)
			}
		})
	}
}

// decoy is a socket of family (an IPv6 one over v4-mapped addresses) bound to
// no device, connected from local to remote (a listener's backlog completes
// its connect), closed when the test ends.
func decoy(t *testing.T, family int, local, remote netip.AddrPort) {
	t.Helper()
	fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if family == unix.AF_INET6 {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, sockaddrOf(family, local)); err != nil {
		t.Fatalf("bind a decoy to %s: %v", local, err)
	}
	if err := unix.Connect(fd, sockaddrOf(family, remote)); err != nil {
		t.Fatalf("connect a decoy %s -> %s: %v", local, remote, err)
	}
}

// sockaddrOf is ap as a socket of family takes it: an IPv4 address
// v4-mapped in an IPv6 one.
func sockaddrOf(family int, ap netip.AddrPort) unix.Sockaddr {
	if family == unix.AF_INET {
		return &unix.SockaddrInet4{Port: int(ap.Port()), Addr: ap.Addr().As4()}
	}
	return &unix.SockaddrInet6{Port: int(ap.Port()), Addr: ap.Addr().As16()}
}

// Two sockets bound to different devices may share their two ends (the
// kernel's own uniqueness check matches a socket's bound device). Neither is
// found by the exact lookup, and the dump finds both: which one is meant
// cannot be told from the ends, and a lookup that answered with one could
// read another connection's bytes. The lookup, and the ordered read of an
// orphan, answer an error, never errNoSocket nor one of them. The second socket is an IPv4 one, or an IPv6
// one connected over v4-mapped addresses (found by the IPv6 dump).
//
// Both connect to a listener on another device's address: the first bound to
// lo, the second to that device, from the first's local end. Neither needs
// its connect to complete: a connecting socket is in the established hash,
// with its two ends, from its connect on, and the listener keeps either from
// being refused (whichever SYN reaches it, its answer goes to the socket bound
// to lo, which takes neither the answer to the other's SYN nor a refusal).
func TestSocketsSharingTheirEndsOnTwoDevicesAreNoAnswer(t *testing.T) {
	dev, addr := deviceWithIPv4(t)
	for _, tc := range []struct {
		name   string
		family int
	}{{"both IPv4", unix.AF_INET}, {"the second IPv6 over v4-mapped addresses", unix.AF_INET6}} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp4", netip.AddrPortFrom(addr, 0).String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ln.Close() }()
			port := ln.Addr().(*net.TCPAddr).Port
			first := boundSocket(t, unix.AF_INET, "lo")
			if err := unix.Bind(first, &unix.SockaddrInet4{Addr: addr.As4()}); err != nil {
				t.Fatal(err)
			}
			connectNonblock(t, first, &unix.SockaddrInet4{Port: port, Addr: addr.As4()})
			sa, err := unix.Getsockname(first)
			if err != nil {
				t.Fatal(err)
			}
			local, remote := netip.AddrPortFrom(addr, uint16(sockaddrPort(sa))), netip.AddrPortFrom(addr, uint16(port))
			second := boundSocket(t, tc.family, dev)
			var bindTo, dialTo unix.Sockaddr = &unix.SockaddrInet4{Port: int(local.Port()), Addr: addr.As4()}, &unix.SockaddrInet4{Port: port, Addr: addr.As4()}
			if tc.family == unix.AF_INET6 {
				bindTo = &unix.SockaddrInet6{Port: int(local.Port()), Addr: addr.As16()}
				dialTo = &unix.SockaddrInet6{Port: port, Addr: addr.As16()}
			}
			if err := unix.Bind(second, bindTo); err != nil {
				t.Fatalf("bind a socket on %s to the first's local end %s: %v", dev, local, err)
			}
			connectNonblock(t, second, dialTo)
			premiseExactMisses(t, local, remote)
			for name, read := range map[string]func(local, remote netip.AddrPort) (diagTCP, error){
				"lookupTCP": lookupTCP, "lookupTCPOrdered": lookupTCPOrdered,
			} {
				if got, err := read(local, remote); err == nil || errors.Is(err, errNoSocket) {
					t.Errorf("%s of two sockets with one pair of ends, on lo and %s = state %d, %v; want an error, neither a socket nor errNoSocket", name, dev, got.state, err)
				}
			}
		})
	}
}

// connectNonblock starts fd's connect to sa without waiting for it.
func connectNonblock(t *testing.T, fd int, sa unix.Sockaddr) {
	t.Helper()
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	if err := unix.Connect(fd, sa); err != nil && !errors.Is(err, unix.EINPROGRESS) {
		t.Fatalf("connect to %v: %v", sa, err)
	}
}

// premiseExactMisses fails the row unless the kernel's exact lookup alone
// misses the socket: the row's premise, that a socket bound to a device is
// found by no exact lookup asked with no device.
func premiseExactMisses(t *testing.T, local, remote netip.AddrPort) {
	t.Helper()
	d, err := openDiag()
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if got, err := d.exact(local, remote); !errors.Is(err, errNoSocket) {
		t.Fatalf("the exact lookup of a socket bound to a device = state %d, %v; want a miss (this row's premise)", got.state, err)
	}
}

// boundSocket is a TCP socket of family bound to the device dev, closed when
// the test ends. An IPv6 one takes v4-mapped addresses too.
func boundSocket(t *testing.T, family int, dev string) int {
	t.Helper()
	fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		t.Skipf("no TCP socket of family %d here: %v", family, err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if family == unix.AF_INET6 {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.BindToDevice(fd, dev); err != nil {
		t.Skipf("no socket bound to %s here: %v", dev, err)
	}
	return fd
}

// socketEnds is fd's local and remote ends, an IPv4 address unmapped.
func socketEnds(t *testing.T, fd int) (local, remote netip.AddrPort) {
	t.Helper()
	ends := [2]netip.AddrPort{}
	for i, get := range []func(int) (unix.Sockaddr, error){unix.Getsockname, unix.Getpeername} {
		sa, err := get(fd)
		if err != nil {
			t.Fatal(err)
		}
		switch a := sa.(type) {
		case *unix.SockaddrInet4:
			ends[i] = netip.AddrPortFrom(netip.AddrFrom4(a.Addr), uint16(a.Port))
		case *unix.SockaddrInet6:
			ends[i] = netip.AddrPortFrom(netip.AddrFrom16(a.Addr).Unmap(), uint16(a.Port))
		default:
			t.Fatalf("not an IP socket: %T", sa)
		}
	}
	return ends[0], ends[1]
}

// sockaddrPort is sa's port, -1 for a socket address with none.
func sockaddrPort(sa unix.Sockaddr) int {
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		return a.Port
	case *unix.SockaddrInet6:
		return a.Port
	}
	return -1
}

// deviceWithIPv4 is an up device other than loopback, and an IPv4 address of
// its own.
func deviceWithIPv4(t *testing.T) (string, netip.Addr) {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() {
					return i.Name, ip.Unmap()
				}
			}
		}
	}
	t.Skip("no up device but loopback with an IPv4 address here")
	return "", netip.Addr{}
}
