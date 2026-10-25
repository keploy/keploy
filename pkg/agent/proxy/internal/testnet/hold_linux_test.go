//go:build linux

package testnet

import (
	"context"
	"io"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ipv4Binds are the binds a listener of another test could make at port at
// an IPv4 address: at loopback, at another loopback address and at every
// address (which net.Listen makes dual-stack where there is IPv6), in that
// order: the every-address bind is last, so a caller that wants only the
// loopback binds takes the first two.
func ipv4Binds(port string) []struct{ network, addr string } {
	return []struct{ network, addr string }{
		{"tcp", net.JoinHostPort("127.0.0.1", port)},
		{"tcp", net.JoinHostPort("127.0.0.2", port)},
		{"tcp", ":" + port},
	}
}

// foreignBinds are ipv4Binds and, where this machine has IPv6 loopback, a
// bind at [::1]. dials are the loopback addresses a dial to the port can
// reach.
func foreignBinds(port string) (binds []struct{ network, addr string }, dials []string) {
	binds = ipv4Binds(port)
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

// requireSharedBindsRefused is requireForeignBindsRefused for a socket that
// sets SO_REUSEPORT before it binds (OpenShared), as keploy's DNS server does
// at the port it is given. Two sockets of one user that both set it may share
// a port, so such a socket is refused a port only where a socket that does
// not set it, or another user's, is at an address the bind overlaps.
func requireSharedBindsRefused(t *testing.T, binds []struct{ network, addr string }, when string) {
	t.Helper()
	for _, b := range binds {
		ln, err := OpenShared(b.network, b.addr)
		if err == nil {
			_ = ln.Close()
		}
		require.ErrorIs(t, err, syscall.EADDRINUSE, "%s, a listener that sets SO_REUSEPORT took the port at %s", when, b.addr)
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
// opened on it, as one of a package tested beside this one can be, by
// net.Listen or one that sets SO_REUSEPORT, is refused the port at loopback,
// at another address, at every address and at [::1]; a UDP socket, made by
// net.ListenPacket or one that sets SO_REUSEPORT, is refused it at
// 127.0.0.1, where it is held. A socket that sets SO_REUSEPORT is tried
// because one of this user that does may share a port with a hold that sets
// it too, as keploy's DNS server does at the port it is given.
func TestARefusedAddressRefusesEveryDialForTheTestsLife(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		_, port, err := net.SplitHostPort(RefusedAddr(t, "tcp"))
		require.NoError(t, err)
		binds, dials := foreignBinds(port)
		requireForeignBindsRefused(t, binds, "while held")
		requireSharedBindsRefused(t, binds, "while held")
		requireDialsRefused(t, dials, "while held")
	})
	t.Run("udp", func(t *testing.T) {
		addr := RefusedAddr(t, "udp")
		pc, err := net.ListenPacket("udp", addr)
		if err == nil {
			_ = pc.Close()
		}
		require.ErrorIs(t, err, syscall.EADDRINUSE, "another socket took the refused port")
		lc := net.ListenConfig{Control: setReusePort}
		pc, err = lc.ListenPacket(context.Background(), "udp", addr)
		if err == nil {
			_ = pc.Close()
		}
		require.ErrorIs(t, err, syscall.EADDRINUSE, "a socket that sets SO_REUSEPORT took the refused port")
		c, err := net.Dial("udp", addr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		// The held port answers at once: port unreachable. A hold that
		// took the query would answer nothing. The deadline makes that a
		// failure here, not a hang to the package's timeout.
		require.NoError(t, c.SetReadDeadline(time.Now().Add(time.Minute)))
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

// The port of a listener ListenIPv6Only opens is held at every IPv4 address
// for the test's life. A listener of another test, made by net.Listen or one
// that sets SO_REUSEPORT, is refused the port at loopback, at another
// loopback address and at every address. A dial to the port at 127.0.0.1 is
// refused. All of it holds while the test's listener is open and after it
// closed. While it is open, a bind at every address overlaps it at [::1],
// and it refuses that bind whatever the hold does, so only the two loopback
// binds there ask the hold.
func TestAnIPv6OnlyListenersPortIsHeldAtEveryIPv4AddressForTheTestsLife(t *testing.T) {
	ln := ListenIPv6Only(t)
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	binds, _ := foreignBinds(port)
	at4 := []string{net.JoinHostPort("127.0.0.1", port)}
	requireForeignBindsRefused(t, binds, "while the test listens at [::1]")
	requireSharedBindsRefused(t, ipv4Binds(port)[:2], "while the test listens at [::1]")
	requireDialsRefused(t, at4, "while the test listens at [::1]")
	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	_ = c.Close()
	require.NoError(t, ln.Close())
	requireForeignBindsRefused(t, ipv4Binds(port), "after the test's listener closed")
	requireSharedBindsRefused(t, ipv4Binds(port), "after the test's listener closed")
	requireDialsRefused(t, at4, "after the test's listener closed")
}

// timeWaitAt leaves one socket at 127.0.0.1:port: the server's side of a
// connection the server closed first. It is in TIME_WAIT once the client's
// close reaches it. timeWaitAt is false, and leaves nothing, when nothing
// can listen at 127.0.0.1:port.
func timeWaitAt(t *testing.T, port int) bool {
	t.Helper()
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		require.ErrorIs(t, err, syscall.EADDRINUSE)
		return false
	}
	defer func() { _ = ln.Close() }()
	c, err := net.Dial("tcp4", ln.Addr().String())
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	s, err := ln.Accept()
	require.NoError(t, err)
	require.NoError(t, ln.Close())
	require.NoError(t, s.Close())
	_, err = c.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, c.Close())
	return true
}

// ListenIPv6Only holds the port before its listener opens there, so no IPv4
// socket can come to the port between the two. A TIME_WAIT at 127.0.0.1
// cannot be made at the port once it is held. The listener opens. Taken the
// other way round, an IPv6 bind picks the port. An IPv4 TIME_WAIT can then
// be at that port. The hold, a socket without SO_REUSEADDR, then cannot bind
// beside it.
func TestNoIPv4TimeWaitComesBetweenTheHoldAndTheIPv6OnlyListener(t *testing.T) {
	made := 0
	ln := listenIPv6Only(t, func(port int) {
		if timeWaitAt(t, port) {
			made++
		}
	})
	require.Zero(t, made, "a TIME_WAIT was made at a port the hold has")
	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	_ = c.Close()
}

// When another socket has [::1] at the port ListenIPv6Only held, it holds
// another port and listens there. It does not skip the test.
func TestAnIPv6OnlyListenerTakesAnotherPortWhenIPv6LoopbackIsTakenAtTheHeldOne(t *testing.T) {
	l6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	_ = l6.Close()
	// In a round where a socket of another test had [::1] at the first
	// port, that socket may close before ListenIPv6Only looks. Such a
	// round tells nothing. The row takes the next one.
	for range 10 {
		var held []int
		took, got, opened := false, 0, false
		t.Run("taken", func(t *testing.T) {
			ln := listenIPv6Only(t, func(port int) {
				held = append(held, port)
				if len(held) > 1 {
					return
				}
				other, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
				if err != nil {
					require.ErrorIs(t, err, syscall.EADDRINUSE)
					return
				}
				took = true
				t.Cleanup(func() { _ = other.Close() })
			})
			got, opened = ln.Addr().(*net.TCPAddr).Port, true
		})
		require.True(t, opened, "ListenIPv6Only skipped or failed the test when [::1] was taken at the port it held")
		if !took {
			continue
		}
		require.GreaterOrEqual(t, len(held), 2, "the ports ListenIPv6Only held")
		require.Equal(t, held[len(held)-1], got, "the listener's port is the last one held")
		return
	}
	t.Fatal("another socket had [::1] at the first port ListenIPv6Only held in ten rounds")
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
