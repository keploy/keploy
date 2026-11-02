//go:build linux

package testnet

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
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
// net.Listen (which sets SO_REUSEADDR) or one that sets SO_REUSEPORT, is
// refused the port at loopback, at another address, at every address and at
// [::1]; a UDP socket, made by net.ListenPacket (which sets neither), one
// that sets SO_REUSEPORT or one that sets SO_REUSEADDR, is refused it at
// 127.0.0.1, where it is held. Each option is tried because a socket that
// sets it may share a port with a hold that sets it too: SO_REUSEPORT for a
// socket of this user, as keploy's DNS server does at the port it is given,
// and SO_REUSEADDR for a UDP socket of any user.
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
		for _, b := range []struct {
			by      string
			control func(network, address string, c syscall.RawConn) error
		}{
			{"net.ListenPacket", nil},
			{"a socket that sets SO_REUSEPORT", setReusePort},
			{"a socket that sets SO_REUSEADDR", ReservedControl},
		} {
			lc := net.ListenConfig{Control: b.control}
			pc, err := lc.ListenPacket(context.Background(), "udp", addr)
			if err == nil {
				_ = pc.Close()
			}
			require.ErrorIs(t, err, syscall.EADDRINUSE, "%s took the refused port", b.by)
		}
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

// listedAt returns the lines of /proc/net/tcp, tcp6, udp and udp6 whose
// local port is port.
func listedAt(t *testing.T, port int) []string {
	t.Helper()
	var at []string
	for _, table := range []string{"tcp", "tcp6", "udp", "udp6"} {
		b, err := os.ReadFile("/proc/net/" + table)
		if errors.Is(err, fs.ErrNotExist) {
			continue // no IPv6
		}
		require.NoError(t, err)
		for _, line := range strings.Split(string(b), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			_, hex, _ := strings.Cut(f[1], ":")
			if p, err := strconv.ParseUint(hex, 16, 16); err == nil && int(p) == port {
				at = append(at, table+": "+line)
			}
		}
	}
	return at
}

// An unlisted port is one the socket tables list nothing at, below the
// range a bind to port 0 is given a port from, held at TCP at every address
// for the test's life and free at UDP.
func TestAnUnlistedPortIsInNoTableHeldAtTCPAndBelowTheEphemeralRange(t *testing.T) {
	port := UnlistedPort(t)
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	require.NoError(t, err)
	first, err := strconv.Atoi(strings.Fields(string(b))[0])
	require.NoError(t, err)
	require.Less(t, port, first, "below the range a bind to port 0 is given a port from")
	require.GreaterOrEqual(t, port, 1024)
	require.Empty(t, listedAt(t, port), "no socket is listed at the port")
	for _, b := range ipv4Binds(strconv.Itoa(port)) {
		_, err := net.Listen(b.network, b.addr)
		require.ErrorIs(t, err, syscall.EADDRINUSE, "a listener at %s %s", b.network, b.addr)
	}
	pc, err := net.ListenPacket("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err, "the port is free at UDP")
	require.NoError(t, pc.Close())
}

// A port another socket is at, at TCP or at UDP, is not given: the search
// moves on to the next port down, and fails when none is left.
func TestAnUnlistedPortIsNoneAnotherSocketIsAt(t *testing.T) {
	held := UnlistedPort(t)
	next, err := unlistedPort(t, held-1000, held+1)
	require.NoError(t, err)
	require.Less(t, next, held, "the port held at TCP is passed over")
	_, err = unlistedPort(t, held, held+1)
	require.ErrorContains(t, err, "free at both TCP and UDP", "no port is left")

	// A port below next, below the range a bind to port 0 is given one
	// from, taken at UDP here: nothing is at it at TCP.
	udp := next - 1
	for ; udp > next-1000; udp-- {
		pc, err := net.ListenPacket("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(udp)))
		if err == nil {
			t.Cleanup(func() { _ = pc.Close() })
			break
		}
		require.ErrorIs(t, err, syscall.EADDRINUSE)
	}
	_, err = unlistedPort(t, udp, udp+1)
	require.ErrorContains(t, err, "free at both TCP and UDP", "the port taken at UDP is passed over")

	// The lowest port of the range is given when it is free.
	for p := udp - 1; ; p-- {
		require.Greater(t, p, udp-1000, "no port below %d was given alone", udp)
		if got, err := unlistedPort(t, p, p+1); err == nil {
			require.Equal(t, p, got)
			break
		}
	}
}

// plainBind binds a TCP socket that sets no option at addr, as a socket of
// another test or process that names the port does, and closes it.
func plainBind(t *testing.T, addr string) error {
	t.Helper()
	ap, err := netip.ParseAddrPort(addr)
	require.NoError(t, err)
	family, sa := syscall.AF_INET6, syscall.Sockaddr(&syscall.SockaddrInet6{Port: int(ap.Port()), Addr: ap.Addr().As16()})
	if ap.Addr().Is4() {
		family, sa = syscall.AF_INET, &syscall.SockaddrInet4{Port: int(ap.Port()), Addr: ap.Addr().As4()}
	}
	fd, err := syscall.Socket(family, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	require.NoError(t, err)
	defer func() { _ = syscall.Close(fd) }()
	return syscall.Bind(fd, sa)
}

// A port ReservedPort holds refuses a dial while nothing listens there, and
// a socket that names it and sets no option is refused it at loopback, at
// every IPv4 address, at [::1] and at [::]. The code's listener, made by
// net.Listen, opens beside the hold at each of those addresses in turn and
// answers a dial; once it closes, a dial is refused again. A dial from the
// port binds beside the hold with ReservedControl, and is refused the port
// without it.
func TestAReservedPortIsHeldForTheTestsLifeBesideTheCodesListener(t *testing.T) {
	p := ReservedPort(t)
	port := strconv.Itoa(p)
	_, dials := foreignBinds(port)
	addrs := []string{net.JoinHostPort("127.0.0.1", port), net.JoinHostPort("0.0.0.0", port)}
	if len(dials) > 1 {
		addrs = append(addrs, net.JoinHostPort("::1", port), net.JoinHostPort("::", port))
	}
	requireDialsRefused(t, dials, "before the code listens")
	for _, a := range addrs {
		require.ErrorIs(t, plainBind(t, a), syscall.EADDRINUSE, "a bind with no option at %s", a)
	}
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		require.NoError(t, err, "the code's listener at %s", a)
		c, err := net.Dial("tcp", ln.Addr().String())
		require.NoError(t, err, "a dial to the code's listener at %s", a)
		_ = c.Close()
		require.NoError(t, ln.Close())
		requireDialsRefused(t, dials, "after the code's listener at "+a+" closed")
	}

	srv, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = srv.Close() }()
	from := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p}
	_, err = (&net.Dialer{LocalAddr: from}).Dial("tcp4", srv.Addr().String())
	require.ErrorIs(t, err, syscall.EADDRINUSE, "a dial from the port without ReservedControl")
	c, err := (&net.Dialer{LocalAddr: from, Control: ReservedControl}).Dial("tcp4", srv.Addr().String())
	require.NoError(t, err, "a dial from the port with ReservedControl")
	defer func() { _ = c.Close() }()
	require.Equal(t, p, c.LocalAddr().(*net.TCPAddr).Port)
}
