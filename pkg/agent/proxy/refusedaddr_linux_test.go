//go:build linux

package proxy

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// refusedAddr is a loopback address, 127.0.0.1:P, that refuses every dial
// for the test's life: with network "tcp" a connect, with "udp" a datagram.
// Its port is held with a socket that never listens, so the kernel refuses a
// dial to it as it refuses one to a port nothing holds, and no other socket
// can take the port meanwhile at the addresses it is held at. A port taken
// from a listener that was then closed is free at once: another socket, of
// this test binary or of a package tested beside it, can take it and answer
// the dial the test expects refused. A TCP port is held at every address, IPv4 and IPv6, by one
// dual-stack socket at [::] (at 0.0.0.0 where there is no IPv6): one test
// dials it at this machine's own non-loopback address, and keploy's dials to
// a loopback address fall back to the other loopback family. A UDP port is
// held at 127.0.0.1.
func refusedAddr(t *testing.T, network string) string {
	t.Helper()
	host := "127.0.0.1"
	if network == "tcp" {
		host = "::"
	}
	port, err := bindHold(t, network, host, 0, false)
	if network == "tcp" && addrAbsent(err) {
		port, err = bindHold(t, network, "0.0.0.0", 0, false)
	}
	require.NoError(t, err)
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// holdPort holds TCP port at every IPv4 address, 0.0.0.0, for the test's life
// with a socket that never listens. It is false, and nothing is held, when
// another socket has the port at an IPv4 address.
func holdPort(t *testing.T, port int) bool {
	t.Helper()
	_, err := bindHold(t, "tcp", "0.0.0.0", port, false)
	if errors.Is(err, syscall.EADDRINUSE) {
		return false
	}
	require.NoError(t, err)
	return true
}

// sharedPort holds a free TCP port for the test's life at every address,
// IPv4 and IPv6, by one dual-stack socket at [::] that never listens (at
// 0.0.0.0 where there is no IPv6), and returns it. listenShared opens a
// listener at the port beside the hold: so a test can close its listener,
// find no listener at the port and a dial to it refused, and listen there
// again, while no other listener of this test binary or of a package tested
// beside it takes the port meanwhile. The hold and listenShared's listeners
// set SO_REUSEPORT; net.Listen does not, so a socket it makes cannot bind the
// port, and a bind to port 0 never lands on it. A socket of this user that
// sets SO_REUSEPORT and binds the port by number can, as keploy's DNS server
// does at the port it is given.
func sharedPort(t *testing.T) int {
	t.Helper()
	port, err := bindHold(t, "tcp", "::", 0, true)
	if addrAbsent(err) {
		port, err = bindHold(t, "tcp", "0.0.0.0", 0, true)
	}
	require.NoError(t, err)
	return port
}

// listenShared opens a TCP listener at addr, a port sharedPort holds, beside
// the hold; it is closed when the test ends if the test has not closed it.
func listenShared(t *testing.T, network, addr string) net.Listener {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(context.Background(), network, addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// bindHold binds a socket that never listens at host:port for the test's
// life and returns its port; on an error nothing is held. The socket is made
// with syscall.Socket, so without the SO_REUSEADDR that net.Listen sets, and
// no other socket can bind the port at an address it overlaps. shared sets
// SO_REUSEPORT, so a socket of this user that sets it too, as listenShared's
// listeners do, can bind beside it. An IPv6 socket is dual-stack: it holds
// the port at IPv4 addresses too. A UDP socket is connected to itself:
// bound alone it would take every datagram sent to its port, while
// connected it takes only its own, and the kernel answers any other
// sender's as it answers one to a port nothing holds, port unreachable,
// which a connected sender reads as refused.
func bindHold(t *testing.T, network, host string, port int, shared bool) (int, error) {
	t.Helper()
	sotype := syscall.SOCK_STREAM
	switch network {
	case "tcp":
	case "udp":
		sotype = syscall.SOCK_DGRAM
	default:
		t.Fatalf("bindHold: network %q is neither tcp nor udp", network)
	}
	ip := net.ParseIP(host)
	require.NotNil(t, ip, "bindHold: %q is not an IP address", host)
	family, sa := syscall.AF_INET, syscall.Sockaddr(nil)
	if ip4 := ip.To4(); ip4 != nil {
		sa = &syscall.SockaddrInet4{Port: port, Addr: [4]byte(ip4)}
	} else {
		family, sa = syscall.AF_INET6, &syscall.SockaddrInet6{Port: port, Addr: [16]byte(ip.To16())}
	}
	fd, err := syscall.Socket(family, sotype|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	fail := func(err error) (int, error) {
		_ = syscall.Close(fd)
		return 0, err
	}
	if family == syscall.AF_INET6 {
		if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0); err != nil {
			return fail(err)
		}
	}
	if shared {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
			return fail(err)
		}
	}
	if err := syscall.Bind(fd, sa); err != nil {
		return fail(err)
	}
	name, err := syscall.Getsockname(fd)
	if err != nil {
		return fail(err)
	}
	if sotype == syscall.SOCK_DGRAM {
		if err := syscall.Connect(fd, name); err != nil {
			return fail(err)
		}
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	switch n := name.(type) {
	case *syscall.SockaddrInet4:
		port = n.Port
	case *syscall.SockaddrInet6:
		port = n.Port
	}
	return port, nil
}

// addrAbsent reports a socket or bind refused because this machine has no
// such address or address family, so no socket can take a port there either.
func addrAbsent(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT)
}

// foreignBinds are the binds a listener of another test could make at port:
// at loopback, at another loopback address, at every address and, where this
// machine has IPv6 loopback, at [::1]. dials are the loopback addresses a
// dial to the port can reach.
func foreignBinds(port string) (binds []struct{ network, addr string }, dials []string) {
	binds = []struct{ network, addr string }{
		{"tcp", net.JoinHostPort("127.0.0.1", port)},
		{"tcp", net.JoinHostPort("127.0.0.2", port)},
		{"tcp", ":" + port},
	}
	dials = []string{net.JoinHostPort("127.0.0.1", port)}
	if l6, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		_ = l6.Close()
		binds = append(binds, struct{ network, addr string }{"tcp6", net.JoinHostPort("::1", port)})
		dials = append(dials, net.JoinHostPort("::1", port))
	}
	return binds, dials
}

// requireForeignBindsRefused binds each of binds with net.Listen, as a test
// of a package tested beside this one can, and expects each bind refused.
func requireForeignBindsRefused(t *testing.T, binds []struct{ network, addr string }, when string) {
	t.Helper()
	for _, b := range binds {
		ln, err := net.Listen(b.network, b.addr)
		if err == nil {
			_ = ln.Close()
		}
		require.ErrorIs(t, err, syscall.EADDRINUSE, "%s, another listener took the port at %s", when, b.addr)
	}
}

// requireDialsRefused dials each of dials and expects each refused.
func requireDialsRefused(t *testing.T, dials []string, when string) {
	t.Helper()
	for _, addr := range dials {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
		}
		require.ErrorIs(t, err, syscall.ECONNREFUSED, "%s, a dial to %s", when, addr)
	}
}

// A refused address refuses every dial for as long as the test that took it
// runs, and no other socket can take its port meanwhile. A TCP listener
// opened on it, as one of a package tested beside this one can be, is
// refused the port at loopback, at another address, at every address and at
// [::1]; a UDP socket is refused it at 127.0.0.1, where it is held.
func TestARefusedAddressRefusesEveryDialForTheTestsLife(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		_, port, err := net.SplitHostPort(refusedAddr(t, "tcp"))
		require.NoError(t, err)
		binds, dials := foreignBinds(port)
		requireForeignBindsRefused(t, binds, "while held")
		requireDialsRefused(t, dials, "while held")
	})
	t.Run("udp", func(t *testing.T) {
		addr := refusedAddr(t, "udp")
		pc, err := net.ListenPacket("udp", addr)
		if err == nil {
			_ = pc.Close()
		}
		require.ErrorIs(t, err, syscall.EADDRINUSE, "another socket took the refused port")
		c, err := net.Dial("udp", addr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		_, err = c.Write([]byte("a query"))
		require.NoError(t, err)
		_, err = c.Read(make([]byte, 1))
		require.ErrorIs(t, err, syscall.ECONNREFUSED)
	})
}

// A shared port is held for the test's life: a listener of another test,
// made by net.Listen, is refused it at loopback, at another address, at every
// address and at [::1], while nothing or the test's own listener
// (listenShared) listens there. The test's listener opens, closes and opens
// again at the port, and while none is open a dial to the port is refused.
func TestASharedPortIsHeldForTheTestsLifeBesideItsOwnListeners(t *testing.T) {
	port := strconv.Itoa(sharedPort(t))
	binds, dials := foreignBinds(port)
	requireForeignBindsRefused(t, binds, "before the test listens")
	for _, round := range []string{"first", "again"} {
		ln := listenShared(t, "tcp", net.JoinHostPort("127.0.0.1", port))
		requireForeignBindsRefused(t, binds, "while the test listens "+round)
		c, err := net.Dial("tcp", ln.Addr().String())
		require.NoError(t, err)
		_ = c.Close()
		require.NoError(t, ln.Close())
		requireForeignBindsRefused(t, binds, "after the test's listener closed "+round)
		requireDialsRefused(t, dials, "after the test's listener closed "+round)
	}
}
