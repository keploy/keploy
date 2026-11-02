//go:build linux

package proxy

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// exactLookupSays makes lookupOrdered's exact lookup answer s, err, as the
// kernel's lockless lookup can for a socket that is moving between its
// tables: a miss, or the socket closed. It returns how many lookups it has
// answered so far: a row whose read never asked it tests nothing (a read that
// is not lookupOrdered's goes to the kernel, which, unfaked, finds the socket).
func exactLookupSays(t *testing.T, s diagTCP, err error) (asked func() int64) {
	t.Helper()
	var n atomic.Int64
	was := lookupExact
	lookupExact = func(*diagSock, netip.AddrPort, netip.AddrPort) (diagTCP, error) {
		n.Add(1)
		return s, err
	}
	t.Cleanup(func() { lookupExact = was })
	return n.Load
}

// stateOf waits until the socket whose ends are local and remote is in state
// (an exact lookup), and returns what that lookup read, or until the kernel
// has destroyed it, and reports gone: a read ordered against its moves
// (lookupTCPOrdered), made once the exact lookup misses it, finds it nowhere.
// A socket that becomes a timewait socket with no timewait bucket left
// (TCPTimeWaitOverflow) is destroyed instead, and no lookup finds it again.
func stateOf(t *testing.T, local, remote netip.AddrPort, state uint8) (d diagTCP, gone bool) {
	t.Helper()
	return socketUntil(t, local, remote, func(d diagTCP) bool { return d.state == state })
}

// timewaitIn waits until the socket whose ends are local and remote is a
// timewait socket in state (FIN_WAIT2 or TIME_WAIT), and reports false, or
// until the kernel has destroyed it, and reports true, as stateOf does. A
// timewait socket is the one with no tcp_info: inet_diag fills
// INET_DIAG_INFO for a full socket only. A full socket read in FIN_WAIT2 is
// not it. The rows that wait here close the socket into the timewait table
// (closeIntoTimewait), a move made before the close returns; a read finds the
// full socket still if the kernel has not run that close yet, as when a
// process this one forked holds the descriptor until it execs.
func timewaitIn(t *testing.T, local, remote netip.AddrPort, state uint8) (gone bool) {
	t.Helper()
	_, gone = socketUntil(t, local, remote, func(d diagTCP) bool { return d.state == state && d.info == nil })
	return gone
}

// noTimewaitBucket is why a row whose socket it closed into the timewait
// table (closeIntoTimewait) skips when it reads it gone there: the close
// moved it on no timer, after which no read falls in a step of the move.
const noTimewaitBucket = "the kernel destroyed the closed socket in place of moving it to the timewait table: no timewait bucket was left, or no memory for one (TCPTimeWaitOverflow)"

// timewaitLen is TCP_TIMEWAIT_LEN in seconds. A socket closed in FIN_WAIT2
// whose TCP_LINGER2 it is moves to the timewait table in its close, whatever
// net.ipv4.tcp_fin_timeout is, and its timewait socket waits 60 s in
// FIN_WAIT2.
const timewaitLen = 60

// closeIntoTimewait closes c so that its close moves it to the timewait
// table, as a timewait socket in FIN_WAIT2, before it returns, or, with no
// timewait bucket left, destroys it (TCPTimeWaitOverflow): either way on no
// timer, and with no step in which a read finds it moving. closeIt is how c
// is closed.
//
// It shuts c's write side first and waits until c, still held, is a full
// socket in FIN_WAIT2 with nothing unacknowledged: its peer took the FIN. A
// socket with its descriptor is not dead, so the ACK that takes it to
// FIN_WAIT2 arms no timer and does not move it. The close then finds it in
// FIN_WAIT2 and, with its TCP_LINGER2 timewaitLen, calls tcp_time_wait itself
// (__tcp_close), under the socket's lock.
//
// An orphan closed in FIN_WAIT1 is moved by the ACK of its FIN instead
// (tcp_rcv_state_process). If that ACK comes while the close still holds the
// socket, it waits in the socket's backlog, and the kernel, finding the
// socket owned there, keeps it a full socket in FIN_WAIT2 on its fin timer,
// then sends a reset and destroys it when the timer expires, 60 s on: it is
// never moved. On 7.2.11 rows that closed that way read the full socket in
// FIN_WAIT2 for 61 to 63 s and then gone, in 4 of 17,000 waits.
func closeIntoTimewait(t *testing.T, c *net.TCPConn, closeIt func() error) {
	t.Helper()
	local, remote := mustAddrPort(t, c.LocalAddr()), mustAddrPort(t, c.RemoteAddr())
	setLinger2(t, c, timewaitLen)
	if err := c.CloseWrite(); err != nil {
		t.Fatalf("shut down the write side of %s -> %s: %v", local, remote, err)
	}
	if _, gone := socketUntil(t, local, remote, func(d diagTCP) bool {
		return d.state == tcpFinWait2 && d.wqueue == 0 && d.info != nil
	}); gone {
		t.Fatalf("the socket %s -> %s, still held, is gone after its write side was shut down", local, remote)
	}
	if err := closeIt(); err != nil {
		t.Fatalf("close %s -> %s: %v", local, remote, err)
	}
}

// setLinger2 sets c's TCP_LINGER2 to secs (below 0: the kernel destroys the
// orphan as it reaches FIN_WAIT2).
func setLinger2(t *testing.T, c *net.TCPConn, secs int) {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_LINGER2, secs) }); err != nil || serr != nil {
		t.Fatalf("TCP_LINGER2 %d: %v %v", secs, err, serr)
	}
}

// socketUntil is stateOf with the socket's read matched by want.
func socketUntil(t *testing.T, local, remote netip.AddrPort, want func(diagTCP) bool) (diagTCP, bool) {
	t.Helper()
	for {
		d, err := lookupTCP(local, remote)
		if err == nil && want(d) {
			return d, false
		}
		if errors.Is(err, errNoSocket) {
			if _, err = lookupTCPOrdered(local, remote); errors.Is(err, errNoSocket) {
				return diagTCP{}, true
			}
		}
		if err != nil {
			t.Fatalf("lookup %s -> %s: %v", local, remote, err)
		}
		runtime.Gosched()
	}
}

// orphanGoneAtItsMove is why a row that closes the proxy's socket in
// FIN_WAIT1, and needs its orphan in the timewait table once it takes the
// application's FIN, skips when its read finds the orphan gone.
const orphanGoneAtItsMove = "the orphan read gone: the kernel destroyed it at its close (its orphan or memory limits, TCPAbortOnMemory) or at its move to the timewait table (no timewait bucket left, or no memory for one, TCPTimeWaitOverflow), or, before 6.11, the read fell in the step of that move where no read finds it (lookupOrdered)"

// loopPair is a connection over loopback: the dialling end and the accepted
// one, from a listener on network and addr that it closes, dialled from the
// address from ("" for the kernel's choice).
func loopPair(t *testing.T, listenNet, listenAddr, dialNet, from string) (dialed, accepted *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen(listenNet, listenAddr)
	if err != nil {
		t.Skipf("no %s here: %v", listenNet, err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	host := "127.0.0.1"
	if dialNet == "tcp6" {
		host = "::1"
	}
	var dialer net.Dialer
	if from != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(from)}
	}
	c, err := dialer.Dial(dialNet, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Skipf("no %s to %s here: %v", dialNet, listenNet, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return c.(*net.TCPConn), s.(*net.TCPConn)
}

// exactMiss is how a row makes lookupOrdered's exact lookup answer.
type exactMiss struct {
	name string
	s    diagTCP
	err  error
}

// exactMisses are the two answers the kernel's exact lookup can give for a
// socket as it moves between its tables: a miss, and the socket closed.
var exactMisses = []exactMiss{
	{"missed", diagTCP{}, errNoSocket},
	{"found closed", diagTCP{state: tcpClose}, nil},
}

// orderedReadAfter is lookupTCPOrdered of local -> remote with its exact
// lookup made to answer as m says. A read that never made that lookup fails
// the row: it went to the kernel unfaked, and tests nothing.
func orderedReadAfter(t *testing.T, m exactMiss, local, remote netip.AddrPort) (diagTCP, error) {
	t.Helper()
	asked := exactLookupSays(t, m.s, m.err)
	got, err := lookupTCPOrdered(local, remote)
	if asked() == 0 {
		t.Fatalf("the ordered read of %s -> %s made no exact lookup through lookupExact", local, remote)
	}
	return got, err
}

// A read ordered against a socket's move finds it wherever the kernel holds
// it, as the exact lookup does, when the exact lookup misses it or finds it
// closed: a connected socket, over IPv4, IPv6, and an IPv6 listener's socket
// connected over v4-mapped addresses (found by its IPv4 ends, as the exact
// lookup finds it), with its two ends on one address or on two (an
// application in another network namespace than the proxy's, as in docker
// mode); a timewait socket in FIN_WAIT2 and in TIME_WAIT. A socket the kernel
// destroyed, even beside others on its two ports, or one that never was, it
// finds nowhere.
func TestAnOrderedReadFindsTheSocketTheExactLookupMissed(t *testing.T) {
	type pairCase struct{ name, listenNet, listenAddr, dialNet, from string }
	pairs := []pairCase{
		{"IPv4", "tcp4", "127.0.0.1:0", "tcp4", ""},
		{"IPv6", "tcp6", "[::1]:0", "tcp6", ""},
		{"v4-mapped", "tcp", ":0", "tcp4", ""},
		{"IPv4 from another address", "tcp4", "127.0.0.1:0", "tcp4", "127.0.0.2"},
		{"v4-mapped from another address", "tcp", ":0", "tcp4", "127.0.0.2"},
	}
	for _, pcase := range pairs {
		t.Run(pcase.name+"/connected", func(t *testing.T) {
			dialed, accepted := loopPair(t, pcase.listenNet, pcase.listenAddr, pcase.dialNet, pcase.from)
			for _, c := range []*net.TCPConn{dialed, accepted} {
				local, remote := mustAddrPort(t, c.LocalAddr()), mustAddrPort(t, c.RemoteAddr())
				want, gone := stateOf(t, local, remote, tcpEstablished)
				if gone {
					t.Fatalf("the connected socket %s -> %s is gone", local, remote)
				}
				for _, m := range exactMisses {
					got, err := orderedReadAfter(t, m, local, remote)
					if err != nil || got.state != tcpEstablished || got.info == nil || got.info.Bytes_acked != want.info.Bytes_acked {
						t.Fatalf("exact lookup %s; the ordered read of %s -> %s = state %d, tcp_info %v, %v; want ESTABLISHED, as the exact lookup read it", m.name, local, remote, got.state, got.info != nil, err)
					}
				}
			}
		})
		t.Run(pcase.name+"/timewait", func(t *testing.T) {
			dialed, accepted := loopPair(t, pcase.listenNet, pcase.listenAddr, pcase.dialNet, pcase.from)
			local, remote := mustAddrPort(t, accepted.LocalAddr()), mustAddrPort(t, accepted.RemoteAddr())
			closeIntoTimewait(t, accepted, accepted.Close) // an orphan, acknowledged: a timewait socket in FIN_WAIT2
			if _, err := dialed.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("the dialled end's read after the close: %v, want EOF", err)
			}
			for _, state := range []uint8{tcpFinWait2, tcpTimeWait} {
				if state == tcpTimeWait {
					_ = dialed.Close() // its FIN, which the orphan takes
				}
				if timewaitIn(t, local, remote, state) {
					if state == tcpFinWait2 {
						t.Skip(noTimewaitBucket)
					}
					// A timewait socket in FIN_WAIT2 takes the FIN where it
					// is: no move, and nothing destroys it before its
					// timer.
					t.Fatalf("the orphan %s -> %s, a timewait socket in FIN_WAIT2, went as it took the FIN; want TIME_WAIT", local, remote)
				}
				for _, m := range exactMisses {
					got, err := orderedReadAfter(t, m, local, remote)
					if err != nil || got.state != state || got.info != nil {
						t.Fatalf("exact lookup %s; the ordered read of the orphan %s -> %s = state %d, tcp_info %v, %v; want %s, a timewait socket, as the exact lookup read it", m.name, local, remote, got.state, got.info != nil, err, tcpStateName(state))
					}
				}
			}
		})
	}
	t.Run("destroyed", func(t *testing.T) {
		dialed, accepted := loopPair(t, "tcp4", "127.0.0.1:0", "tcp4", "")
		local, remote := destroyAtFinWait2(t, dialed, accepted)
		for _, m := range exactMisses {
			if got, err := orderedReadAfter(t, m, local, remote); !errors.Is(err, errNoSocket) {
				t.Fatalf("exact lookup %s; the ordered read of the destroyed orphan = state %d, %v; want errNoSocket", m.name, got.state, err)
			}
		}
	})
	// The dump is filtered to the two ports by the kernel and to the two
	// addresses here: two applications' connections to the proxy's port can
	// come from the same port on two addresses. The read of a destroyed
	// socket finds none of the others on its two ports (one differs from it
	// in its local address only, one in its remote address only), and the
	// read of each of those finds that one: each has taken its own count of
	// bytes. Over IPv4 listeners, and over one dual-stack listener, as the
	// proxy's, whose sockets are IPv6 ones on v4-mapped addresses (the IPv6
	// dump's filter).
	for _, kind := range []struct{ name, network string }{{"IPv4", "tcp4"}, {"v4-mapped", "tcp"}} {
		t.Run("destroyed, beside sockets on the same ports/"+kind.name, func(t *testing.T) {
			listen := func(addr string) net.Listener {
				ln, err := net.Listen(kind.network, addr)
				if err != nil {
					t.Fatalf("listen on %s %s: %v", kind.network, addr, err)
				}
				t.Cleanup(func() { _ = ln.Close() })
				return ln
			}
			// A listener on each of 127.0.0.1 and 127.0.0.2, on one port; a
			// dual-stack listener takes both.
			var ln1, ln2 net.Listener
			if kind.network == "tcp4" {
				ln1 = listen("127.0.0.1:0")
				ln2 = listen(net.JoinHostPort("127.0.0.2", strconv.Itoa(ln1.Addr().(*net.TCPAddr).Port)))
			} else {
				ln1 = listen(":0")
				if ln1.Addr().(*net.TCPAddr).IP.To4() != nil {
					t.Skip("no dual-stack listener here: no IPv6")
				}
				ln2 = ln1
			}
			lport := strconv.Itoa(ln1.Addr().(*net.TCPAddr).Port)
			// SO_REUSEADDR lets two dialling sockets share one local address and
			// port, connected to different ends.
			reuse := func(_, _ string, c syscall.RawConn) error {
				var serr error
				if err := c.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1) }); err != nil {
					return err
				}
				return serr
			}
			// connect dials ln on host from the address from, and sends the
			// accepted end n bytes, which it reads: its tcp_info counts them
			// received.
			connect := func(ln net.Listener, host string, from netip.AddrPort, n int) (dialed, accepted *net.TCPConn) {
				t.Helper()
				c, err := (&net.Dialer{LocalAddr: net.TCPAddrFromAddrPort(from), Control: reuse}).Dial("tcp4", net.JoinHostPort(host, lport))
				if err != nil {
					t.Fatalf("dial %s:%s from %s: %v", host, lport, from, err)
				}
				t.Cleanup(func() { _ = c.Close() })
				s, err := ln.Accept()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
				if s.RemoteAddr().String() != c.LocalAddr().String() {
					t.Fatalf("accepted from %s, dialled from %s", s.RemoteAddr(), c.LocalAddr())
				}
				if kind.network == "tcp" && !mustAddrPort(t, s.LocalAddr()).Addr().Is4In6() {
					t.Fatalf("the dual-stack listener's socket %s is not on v4-mapped addresses", s.LocalAddr())
				}
				if n > 0 {
					if _, err := c.Write(make([]byte, n)); err != nil {
						t.Fatal(err)
					}
					if _, err := io.ReadFull(s, make([]byte, n)); err != nil {
						t.Fatal(err)
					}
				}
				return c.(*net.TCPConn), s.(*net.TCPConn)
			}
			dialed, gone := connect(ln1, "127.0.0.1", netip.MustParseAddrPort("127.0.0.1:0"), 0)
			port := mustAddrPort(t, dialed.LocalAddr()).Port()
			_, otherRemote := connect(ln1, "127.0.0.1", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.2"), port), 1)
			_, otherLocal := connect(ln2, "127.0.0.2", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port), 2)
			neighbours := []struct {
				c *net.TCPConn
				n uint64
			}{{otherRemote, 1}, {otherLocal, 2}}
			local, remote := destroyAtFinWait2(t, dialed, gone)
			for _, m := range exactMisses {
				if got, err := orderedReadAfter(t, m, local, remote); !errors.Is(err, errNoSocket) {
					t.Fatalf("exact lookup %s; the ordered read of the destroyed %s -> %s = state %d, %v; want errNoSocket: another socket on its two ports is not it", m.name, local, remote, got.state, err)
				}
				for _, nb := range neighbours {
					l, r := mustAddrPort(t, nb.c.LocalAddr()), mustAddrPort(t, nb.c.RemoteAddr())
					got, err := orderedReadAfter(t, m, l, r)
					if err != nil || got.state != tcpEstablished || got.info == nil || got.info.Bytes_received != nb.n {
						var received uint64
						if got.info != nil {
							received = got.info.Bytes_received
						}
						t.Fatalf("exact lookup %s; the ordered read of %s -> %s = state %d, %d bytes received, %v; want ESTABLISHED with %d: that socket, not another on its ports", m.name, l, r, got.state, received, err, nb.n)
					}
				}
			}
		})
	}
	t.Run("never was", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		local := mustAddrPort(t, ln.Addr())
		remote := netip.MustParseAddrPort("127.0.0.1:9")
		// The kernel's exact lookup falls back to the listener on the local
		// port; neither read takes it for the socket.
		if got, err := lookupTCP(local, remote); !errors.Is(err, errNoSocket) {
			t.Fatalf("the exact lookup of a pair with no socket = state %d, %v; want errNoSocket", got.state, err)
		}
		if got, err := lookupTCPOrdered(local, remote); !errors.Is(err, errNoSocket) {
			t.Fatalf("the ordered read of a pair with no socket = state %d, %v; want errNoSocket", got.state, err)
		}
	})
}

// The ordered read is a dump, not a second exact lookup, here against a
// miss of the kernel's own exact lookup, with nothing faked. The exact
// sock_diag lookup asks with no device (idiag_if 0), so it misses a socket
// bound to one (SO_BINDTODEVICE: the established hash's match checks the
// bound device); the dump, which walks the chain under its lock, does not
// filter on the device and finds it. A second exact lookup in place of the
// dump misses it again.
func TestAnOrderedReadIsADumpNotASecondExactLookup(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = unix.BindToDevice(int(fd), "lo") }); err != nil {
			return err
		}
		return serr
	}}
	c, err := d.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)))
	if err != nil {
		t.Skipf("no socket bound to lo here: %v", err)
	}
	defer func() { _ = c.Close() }()
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	local, remote := mustAddrPort(t, c.LocalAddr()), mustAddrPort(t, c.RemoteAddr())
	premiseExactMisses(t, local, remote)
	if got, err := lookupTCPOrdered(local, remote); err != nil || got.state != tcpEstablished {
		t.Fatalf("the ordered read once the kernel's exact lookup missed = state %d, %v; want ESTABLISHED, found by the dump", got.state, err)
	}
}

// destroyAtFinWait2 closes accepted so that the kernel destroys it as its
// orphan reaches FIN_WAIT2 (TCP_LINGER2 below 0), rather than move it to the
// timewait table, and returns its ends once the exact lookup no longer finds
// it.
func destroyAtFinWait2(t *testing.T, dialed, accepted *net.TCPConn) (local, remote netip.AddrPort) {
	t.Helper()
	local, remote = mustAddrPort(t, accepted.LocalAddr()), mustAddrPort(t, accepted.RemoteAddr())
	setLinger2(t, accepted, -1)
	_ = accepted.Close()
	if _, err := dialed.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the dialled end's read after the close: %v, want EOF", err)
	}
	for {
		if _, err := lookupTCP(local, remote); errors.Is(err, errNoSocket) {
			return local, remote
		}
		runtime.Gosched()
	}
}
