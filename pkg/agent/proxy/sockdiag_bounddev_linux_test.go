//go:build linux

package proxy

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
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
//
// Each port a case uses is held at every address it uses it at before the
// case connects (onOnePort): the kernel picks the socket's port, and the
// listener's, among those free at their own address alone, and another
// test's socket may hold either at 127.0.0.3.
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
			// The listener, and with the decoys a second one at 127.0.0.3
			// on its port, which the second decoy connects to.
			peerFamily := unix.AF_INET6
			if tc.peer.Is4() {
				peerFamily = unix.AF_INET
			}
			listeners := []boundSock{{family: peerFamily, addr: tc.peer, listen: true}}
			// The socket bound to lo, and with the decoys one at 127.0.0.3
			// and one at its own address (it shares the socket's local end:
			// it has another remote end), on its port.
			socks := []boundSock{{family: tc.family, dev: "lo", addr: tc.addr}}
			if tc.decoys {
				listeners = append(listeners, boundSock{family: unix.AF_INET, addr: lo3, listen: true})
				socks = append(socks, boundSock{family: tc.family, addr: lo3}, boundSock{family: tc.family, addr: tc.addr})
			}
			lns, port := onOnePort(t, listeners...)
			fds, _ := onOnePort(t, socks...)
			fd := fds[0]
			if err := unix.Connect(fd, sockaddrOf(tc.family, netip.AddrPortFrom(tc.peer, port))); err != nil {
				t.Fatalf("connect a socket bound to lo: %v", err)
			}
			s, _, err := unix.Accept4(lns[0], unix.SOCK_CLOEXEC)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = unix.Close(s) })
			local, remote := socketEnds(t, fd)
			if tc.decoys {
				if local.Addr() == remote.Addr() {
					t.Fatalf("the socket's ends %s -> %s share an address (this case's premise: they differ)", local, remote)
				}
				// A listener's backlog completes a decoy's connect. Each
				// has the socket's two ports, and one of its addresses.
				for i, ends := range [][2]netip.AddrPort{
					{netip.AddrPortFrom(lo3, local.Port()), remote},
					{local, netip.AddrPortFrom(lo3, remote.Port())},
				} {
					if err := unix.Connect(fds[1+i], sockaddrOf(tc.family, ends[1])); err != nil {
						t.Fatalf("connect a decoy at %s to %s: %v", socks[1+i].addr, ends[1], err)
					}
					if at, _ := socketEnds(t, fds[1+i]); at != ends[0] {
						t.Fatalf("a decoy connected to %s is at %s; want %s, on the socket's port (this case's premise)", ends[1], at, ends[0])
					}
				}
			}
			premiseExactMisses(t, local, remote)
			got, err := lookupTCP(local, remote)
			if err != nil || got.state != tcpEstablished || got.info == nil {
				t.Fatalf("lookupTCP of a socket bound to lo, %s -> %s = state %d, tcp_info %v, %v; want ESTABLISHED with its tcp_info", local, remote, got.state, got.info != nil, err)
			}
		})
	}
}

// boundSock is a TCP socket onOnePort binds: of family (an IPv6 one takes
// v4-mapped addresses too), bound to the device dev unless dev is "", at
// addr, listening if listen.
type boundSock struct {
	family int
	dev    string
	addr   netip.Addr
	listen bool
}

// onOnePort binds each of socks to one port, the first to one the kernel
// picks and each other to that one, and is their fds, closed when the test
// ends, and that port. The kernel picks the first's port among those free at
// the first's address alone, and another socket may hold it at another's
// (EADDRINUSE): then it closes them and tries again, at a port the kernel
// picks anew, so a case starts with the port held at every address it uses
// it at. Each is SO_REUSEADDR, so two that do not listen may share an
// address and port (the socket and its second decoy).
func onOnePort(t *testing.T, socks ...boundSock) ([]int, uint16) {
	t.Helper()
	const tries = 64
	var err error
	for range tries {
		var fds []int
		var port uint16
		if fds, port, err = bindOnOnePort(t, socks); err == nil {
			for _, fd := range fds {
				t.Cleanup(func() { _ = unix.Close(fd) })
			}
			return fds, port
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			t.Fatal(err)
		}
	}
	addrs := make([]netip.Addr, len(socks))
	for i, s := range socks {
		addrs[i] = s.addr
	}
	t.Fatalf("no port free at every one of %v in %d tries: %v", addrs, tries, err)
	return nil, 0
}

// bindOnOnePort is one try of onOnePort. Its sockets are closed unless it
// is all of them, bound (and listening): on an error, a skip or a failure.
// A first socket that listens at an address that is not here
// (EADDRNOTAVAIL: ::1 where lo has no IPv6) skips the test, whose case has
// no listener there, or, at an IPv6 address, fails it under
// KEPLOY_TEST_IPV6_REQUIRED=1 (withoutIPv6); any other error is the
// caller's.
func bindOnOnePort(t *testing.T, socks []boundSock) (fds []int, port uint16, err error) {
	t.Helper()
	held := false
	defer func() {
		if !held {
			for _, fd := range fds {
				_ = unix.Close(fd)
			}
			fds = nil
		}
	}()
	for i, s := range socks {
		fd := newSocket(t, s.family, s.dev)
		fds = append(fds, fd)
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			return fds, 0, err
		}
		at := netip.AddrPortFrom(s.addr, port)
		if err := unix.Bind(fd, sockaddrOf(s.family, at)); err != nil {
			if i == 0 && s.listen && errors.Is(err, unix.EADDRNOTAVAIL) {
				if s.addr.Is6() {
					withoutIPv6(t, "no listener at %s here: %v", at, err)
				}
				t.Skipf("no listener at %s here: %v", at, err)
			}
			return fds, 0, fmt.Errorf("bind socket %d of %d to %s: %w", i+1, len(socks), at, err)
		}
		if i == 0 {
			sa, err := unix.Getsockname(fd)
			if err != nil {
				return fds, 0, err
			}
			port = uint16(sockaddrPort(sa))
		}
		if s.listen {
			if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
				return fds, 0, fmt.Errorf("listen at %s: %w", netip.AddrPortFrom(s.addr, port), err)
			}
		}
	}
	held = true
	return fds, port, nil
}

// withoutIPv6 skips t where IPv6 is not here (no IPv6 socket, or no ::1 on
// lo). KEPLOY_TEST_IPV6_REQUIRED=1, which go-test.yaml sets, fails it
// instead, as util's requireIPv6 does, so a runner that lost IPv6 fails its
// IPv6 cases instead of passing without them.
func withoutIPv6(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("KEPLOY_TEST_IPV6_REQUIRED") == "1" {
		t.Fatalf("KEPLOY_TEST_IPV6_REQUIRED=1 but "+format, args...)
	}
	t.Skipf(format, args...)
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
	fd := newSocket(t, family, dev)
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

// newSocket is a TCP socket of family (an IPv6 one takes v4-mapped addresses
// too), bound to the device dev unless dev is "", for the caller to close. It
// skips the test where there is no such socket, closing it first, and fails
// it where there is no IPv6 one and KEPLOY_TEST_IPV6_REQUIRED=1
// (withoutIPv6).
func newSocket(t *testing.T, family int, dev string) int {
	t.Helper()
	fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		if family == unix.AF_INET6 {
			withoutIPv6(t, "no IPv6 TCP socket here: %v", err)
		}
		t.Skipf("no TCP socket of family %d here: %v", family, err)
	}
	if family == unix.AF_INET6 {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0); err != nil {
			_ = unix.Close(fd)
			t.Fatal(err)
		}
	}
	if dev != "" {
		if err := unix.BindToDevice(fd, dev); err != nil {
			_ = unix.Close(fd)
			t.Skipf("no socket bound to %s here: %v", dev, err)
		}
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
