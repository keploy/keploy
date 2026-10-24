//go:build linux

package testnet

import (
	"net"
	"strconv"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

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
		_, port, err := net.SplitHostPort(RefusedAddr(t, "tcp"))
		require.NoError(t, err)
		binds, dials := foreignBinds(port)
		requireForeignBindsRefused(t, binds, "while held")
		requireDialsRefused(t, dials, "while held")
	})
	t.Run("udp", func(t *testing.T) {
		addr := RefusedAddr(t, "udp")
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
// (ListenShared) listens there. The test's listener opens, closes and opens
// again at the port, and while none is open a dial to the port is refused.
func TestASharedPortIsHeldForTheTestsLifeBesideItsOwnListeners(t *testing.T) {
	port := strconv.Itoa(SharedPort(t))
	binds, dials := foreignBinds(port)
	requireForeignBindsRefused(t, binds, "before the test listens")
	for _, round := range []string{"first", "again"} {
		ln := ListenShared(t, "tcp", net.JoinHostPort("127.0.0.1", port))
		requireForeignBindsRefused(t, binds, "while the test listens "+round)
		c, err := net.Dial("tcp", ln.Addr().String())
		require.NoError(t, err)
		_ = c.Close()
		require.NoError(t, ln.Close())
		requireForeignBindsRefused(t, binds, "after the test's listener closed "+round)
		requireDialsRefused(t, dials, "after the test's listener closed "+round)
	}
}

// BoundHere counts the sockets of this process at a port and no other
// process's: a listener of this test counts until it closes, and a socket
// another process keeps at the port does not.
func TestBoundHereCountsOnlyThisProcesssSockets(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	n, known := BoundHere(t, port)
	require.True(t, known)
	require.Equal(t, 1, n, "this test's listener")

	GiveAway(t, ln.(*net.TCPListener))
	_, err = net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.ErrorIs(t, err, syscall.EADDRINUSE, "the other process holds the port")
	n, _ = BoundHere(t, port)
	require.Zero(t, n, "the other process's socket")

	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port = closed.Addr().(*net.TCPAddr).Port
	require.NoError(t, closed.Close())
	n, _ = BoundHere(t, port)
	require.Zero(t, n, "a listener this test closed")
}
