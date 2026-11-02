//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
)

// heldPair is a connection as the proxy holds a pooled one: the
// application's end (app) and the proxy's (proxySide), paired as application
// connection 1 and kept as the accept loop keeps it under the proven switch,
// so the proxy's reads, through reader, are seen (inflight.go). The proxy's
// receive buffer is small and the application's send buffer large, so bytes
// the application writes wait in its own socket until the proxy reads:
// written, not yet received, as when they are on their way.
func heldPair(t *testing.T, network, addr string) (p *Proxy, app, proxySide *net.TCPConn, reader net.Conn) {
	t.Helper()
	p = &Proxy{logger: zap.NewNop()}
	app, proxySide, reader = heldPairOn(t, p, 1, network, addr)
	return p, app, proxySide, reader
}

// heldPairOn is heldPair on p, paired as application connection appSeq.
func heldPairOn(t *testing.T, p *Proxy, appSeq uint64, network, addr string) (app, proxySide *net.TCPConn, reader net.Conn) {
	t.Helper()
	return heldPairBound(t, p, appSeq, network, addr, "", "")
}

// heldPairBound is heldPairOn with the listener's socket bound to the device
// listenDev, and the application's to the device appDev (SO_BINDTODEVICE),
// each when named. The proxy's side, accepted from a listener bound to a
// device, is bound to it too, as one accepted on a VRF's device is.
//
// network "tcp" is the proxy's own listener (proxy.go listens on ":port"
// with "tcp"): one dual-stack IPv6 socket. The application dials it over
// IPv4 (127.0.0.1), so its own socket is an IPv4 one while the proxy's side
// reads both ends as v4-mapped addresses, which the lookups are given.
func heldPairBound(t *testing.T, p *Proxy, appSeq uint64, network, addr, listenDev, appDev string) (app, proxySide *net.TCPConn, reader net.Conn) {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) {
			// A segment far smaller than the buffer, so the window
			// reopens a segment at a time as the proxy reads (loopback's
			// own, 64 KiB, would hold it shut until the persist timer).
			if serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG, 1024); serr == nil {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 16384)
			}
			if serr == nil && listenDev != "" {
				serr = unix.BindToDevice(int(fd), listenDev)
			}
		}); err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(context.Background(), network, addr)
	if err != nil {
		t.Skipf("no %s here: %v", network, err)
	}
	defer ln.Close()
	dialNet, dialAddr := network, ln.Addr().String()
	if network == "tcp" {
		dialNet, dialAddr = "tcp4", net.JoinHostPort("127.0.0.1", fmt.Sprint(ln.Addr().(*net.TCPAddr).Port))
	}
	d := net.Dialer{Control: boundTo(appDev)}
	c, err := d.Dial(dialNet, dialAddr)
	if err != nil {
		t.Fatal(err)
	}
	app = c.(*net.TCPConn)
	t.Cleanup(func() { _ = app.Close() })
	if err := app.SetWriteBuffer(1 << 20); err != nil {
		t.Fatal(err)
	}
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	proxySide = s.(*net.TCPConn)
	t.Cleanup(func() { _ = proxySide.Close() })
	if network == "tcp" {
		for _, a := range []net.Addr{proxySide.RemoteAddr(), proxySide.LocalAddr()} {
			if !mustAddrPort(t, a).Addr().Is4In6() {
				t.Fatalf("premise: the dual-stack listener's accepted end %s is not v4-mapped", a)
			}
		}
	}
	p.paired.add(proxySide, appSeq)
	tracked := p.inflight.track(proxySide)
	tracked.beforeClose = func() { p.pairClosing(context.Background(), proxySide, tracked) }
	t.Cleanup(func() { p.inflight.forget(tracked) })
	return app, proxySide, tracked
}

// queue writes to the application's socket what its send buffer takes
// without waiting for the proxy to read, and says how much that was.
func queue(t *testing.T, app *net.TCPConn) uint64 {
	t.Helper()
	rc, err := app.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	var serr error
	if err := rc.Control(func(fd uintptr) { n, serr = unix.SendmsgN(int(fd), make([]byte, 4<<20), nil, nil, unix.MSG_DONTWAIT) }); err != nil || serr != nil || n <= 0 {
		t.Fatalf("queue: %d, %v %v", n, err, serr)
	}
	return uint64(n)
}

func received(t *testing.T, c *net.TCPConn) uint64 {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var ti *unix.TCPInfo
	var gerr error
	if err := rc.Control(func(fd uintptr) { ti, gerr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO) }); err != nil || gerr != nil {
		t.Fatalf("TCP_INFO: %v %v", err, gerr)
	}
	return ti.Bytes_received
}

// pairShapes are the paired connections the wait reads, as heldPairBound
// makes them: over IPv4 and IPv6, behind the proxy's own dual-stack
// listener (network "tcp"), each with the application's socket bound to no
// device and to lo.
var pairShapes = []struct{ name, network, addr, dev string }{
	{"tcp4", "tcp4", "127.0.0.1:0", ""},
	{"tcp6", "tcp6", "[::1]:0", ""},
	{"dual-stack listener, tcp4 app", "tcp", ":0", ""},
	{"tcp4 bound to lo", "tcp4", "127.0.0.1:0", "lo"},
	{"tcp6 bound to lo", "tcp6", "[::1]:0", "lo"},
	{"dual-stack listener, tcp4 app bound to lo", "tcp", ":0", "lo"},
}

// A call written to a connection the proxy already holds (a pooled one) is
// waited for until every byte the application had written when the wait
// began has reached the proxy's side, however long the bytes take; an idle
// connection holds nothing back. Over IPv4 and IPv6.
//
// An application socket bound to a device (SO_BINDTODEVICE; a VRF's sockets
// are bound to its device) is waited for alike: sock_diag's exact lookup,
// asked with no device, never finds one, and the wait would read it as gone.
// Also behind the proxy's own dual-stack listener, whose ends for an IPv4
// application are v4-mapped while the application's socket is an IPv4 one.
func TestThePairedBytesAreWaitedForUntilTheyArrive(t *testing.T) {
	for _, tc := range pairShapes {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop()}
			app, proxySide, reader := heldPairBound(t, p, 1, tc.network, tc.addr, "", tc.dev)
			// Idle: nothing written, nothing to wait for.
			if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
				t.Fatalf("WaitPairedBytes on an idle connection: %v", err)
			}
			n := queue(t, app)
			if got := received(t, proxySide); got >= n {
				t.Fatalf("the proxy's side received %d of %d at once: the fixture holds nothing back", got, n)
			}
			done := make(chan error, 1)
			go func() { done <- p.WaitPairedBytes(context.Background(), 1) }()
			buf := make([]byte, 64<<10)
			var read uint64
			for read < n {
				// The wait may end only once everything has arrived:
				// received only grows, so one that ended while it is
				// still short ended early.
				select {
				case err := <-done:
					if got := received(t, proxySide); got < n {
						t.Fatalf("WaitPairedBytes returned %v with %d of %d bytes received", err, got, n)
					}
					done <- err
				default:
				}
				m, err := reader.Read(buf)
				if err != nil {
					t.Fatal(err)
				}
				read += uint64(m)
			}
			if err := <-done; err != nil {
				t.Fatalf("WaitPairedBytes: %v", err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
				t.Fatalf("%d losses, want none", l)
			}
		})
	}
}

// A connection numbered after the wait's count is not the test's: its bytes
// do not hold the wait back.
func TestALaterConnectionsBytesAreNotWaitedFor(t *testing.T) {
	p, app, _, _ := heldPair(t, "tcp4", "127.0.0.1:0")
	p.paired.mu.Lock()
	for _, pc := range p.paired.byConn {
		pc.appSeq = 2
	}
	p.paired.mu.Unlock()
	queue(t, app)
	if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
		t.Fatalf("WaitPairedBytes for connection 1: %v", err)
	}
}

// Bytes that can no longer arrive end the wait as a capture loss: the proxy
// closes its side first, short of what the application wrote.
func TestBytesThatCanNoLongerArriveAreACaptureLoss(t *testing.T) {
	for _, tc := range []string{"the proxy closes it"} {
		t.Run(tc, func(t *testing.T) {
			p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
			queue(t, app)
			// The wait has read what was written before the connection
			// ends: as a test's end that began first.
			targets, err := p.pairTargets(1)
			if err != nil || len(targets) != 1 {
				t.Fatalf("pairTargets = %d, %v; want the connection", len(targets), err)
			}
			done := make(chan error, 1)
			go func() { done <- p.waitPairTargets(context.Background(), targets) }()
			_ = reader.Close()
			p.paired.remove(proxySide)
			if err := <-done; err != nil {
				t.Fatalf("WaitPairedBytes: %v", err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
				t.Fatalf("%d app_bytes_lost, want 1", l)
			}
			// Once: the same target looked at again is not charged again.
			if _, err := p.pairReached(targets[0]); err != nil {
				t.Fatal(err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
				t.Fatalf("%d app_bytes_lost after a second look, want still 1", l)
			}
		})
	}
}

// Everything written, then the application's FIN: once it has all arrived
// there is nothing to wait for, whether the application's socket is still
// there or gone.
func TestAClosedConnectionWhoseBytesArrivedHoldsNothing(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
		t.Fatalf("WaitPairedBytes: %v", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d losses, want none", l)
	}
}

// A wait that holds no paired connection has nothing to tell, whatever
// else is known: a kernel whose sock_diag gives no byte counts fails only the
// waits that would read them, never every begin and end.
func TestABytesWaitWithNoPairedConnectionNeedsNoCounts(t *testing.T) {
	p := &Proxy{logger: zap.NewNop()}
	p.paired.fail(fmt.Errorf("%w: no tcp_diag", models.ErrProxyConnsUnsupported))
	if err := p.WaitPairedBytes(context.Background(), 5); err != nil {
		t.Fatalf("WaitPairedBytes with no paired connection: %v, want nil", err)
	}
}

// The first paired connection proves the application's sockets are seen from
// the proxy's network namespace: its application end is found, bound to a
// device or not, behind a listener of either family or the proxy's own
// dual-stack one.
func TestAPairedConnectionProvesTheApplicationsSocketsSeen(t *testing.T) {
	for _, tc := range pairShapes {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop()}
			_, proxySide, _ := heldPairBound(t, p, 1, tc.network, tc.addr, "", tc.dev)
			if p.paired.visible.Load() {
				t.Fatal("seen before any proof")
			}
			p.proveAppVisible(proxySide)
			if !p.paired.visible.Load() {
				t.Fatal("the application's end of a live paired connection was not found")
			}
		})
	}
}

// closingPair is the paired connection of proxySide and a sock_diag socket
// to read its application end on.
func closingPair(t *testing.T, p *Proxy, proxySide *net.TCPConn) (*pairedConn, *diagSock) {
	t.Helper()
	p.paired.mu.Lock()
	pc := p.paired.byConn[proxySide]
	p.paired.mu.Unlock()
	if pc == nil {
		t.Fatal("no paired connection")
	}
	ds, err := openDiag()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ds.close)
	return pc, ds
}

// wantFINCounted checks that the application's socket is in state, and that
// the wait reads it as there: n bytes written and its FIN, which pairTarget
// counts from the states where the application has closed and its FIN is not
// yet acknowledged.
func wantFINCounted(t *testing.T, p *Proxy, ds *diagSock, pc *pairedConn, state uint8, n uint64) {
	t.Helper()
	if got, err := ds.lookup(pc.app, pc.proxy); err != nil || got.state != state {
		t.Fatalf("lookup of the application's socket %s -> %s = TCP state %d, %v; want %d", pc.app, pc.proxy, got.state, err, state)
	}
	tg, err := p.pairTarget(ds, pc)
	if err != nil || tg == nil || tg.gone || !tg.finCounted || tg.want != n+1 {
		t.Fatalf("pairTarget in TCP state %d = %+v, %v; want %d with its FIN counted, not gone", state, tg, err, n+1)
	}
}

// readAndSettle has the proxy read the n bytes the application left behind,
// and checks that the wait settles on them with no loss.
func readAndSettle(t *testing.T, p *Proxy, reader net.Conn, n uint64) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.WaitPairedBytes(context.Background(), 1) }()
	if _, err := io.ReadFull(reader, make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("WaitPairedBytes: %v", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d losses, want none", l)
	}
}

// movedTo waits until the kernel's own table of sockets lists the
// application's socket of pc in the state want, and checks that the lookup's
// first read after that finds it there. It fails, naming what it saw, if the
// table lists the socket in a state outside passing (the states it moves
// through on its way) or no longer lists it, and if the lookup misses the
// socket or reads it in another state.
//
// The table (/proc/net/tcp and tcp6) is read apart from sock_diag and the
// code under test: the kernel moves the socket on its own, so a lookup that
// never reads want cannot keep the wait going. want is a state the socket
// stays in, so once the table lists it there, the lookup reads it there.
// Before 6.11 the kernel listed a timewait socket a step before it took the
// socket's reference, and a lookup in that step misses the socket, so the
// table must list it with its reference.
//
// The table is read under each chain's lock, and a socket moves to the
// timewait table under that lock, so the table lists it all the way. But a
// read of the table restarts its walk at each page it fills, and a socket
// that moves in its chain at a restart can be left out of that read. So a
// socket is no longer listed when two reads running leave it out: a socket
// the kernel destroyed (with no timewait socket for it, TCPTimeWaitOverflow)
// is left out of every read.
func movedTo(t *testing.T, ds *diagSock, pc *pairedConn, want uint8, passing ...uint8) {
	t.Helper()
	for missed := 0; ; runtime.Gosched() {
		state, refs, listed := kernelTCPState(t, pc.app, pc.proxy)
		if !listed {
			if missed++; missed == 2 {
				t.Fatalf("the kernel's own table no longer lists the application's socket %s -> %s, on its way to TCP state %d", pc.app, pc.proxy, want)
			}
			continue
		}
		missed = 0
		if state == want && refs > 0 {
			break
		}
		if state != want && !slices.Contains(passing, state) {
			t.Fatalf("the kernel's own table lists the application's socket %s -> %s in TCP state %d, on its way to %d; want one of %v first", pc.app, pc.proxy, state, want, passing)
		}
	}
	if got, err := ds.lookup(pc.app, pc.proxy); err != nil || got.state != want {
		t.Fatalf("lookup of the application's socket %s -> %s, once the kernel lists it in TCP state %d = TCP state %d, %v; want %d", pc.app, pc.proxy, want, got.state, err, want)
	}
}

// kernelTCPState is the TCP state the kernel's own table of sockets
// (/proc/net/tcp and tcp6) lists the socket from local to remote in, the
// references held to it, and whether the table lists it. Ends are compared
// unmapped: the table lists an IPv4 socket with IPv4 ends.
func kernelTCPState(t *testing.T, local, remote netip.AddrPort) (uint8, int, bool) {
	t.Helper()
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(table)
		if errors.Is(err, fs.ErrNotExist) {
			continue // no IPv6 here
		}
		if err != nil {
			t.Fatal(err)
		}
		// sl local_address rem_address st tx_queue:rx_queue tr:tm->when
		// retrnsmt uid timeout inode ref pointer ...
		for _, line := range strings.Split(string(b), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) < 11 || procAddrPort(f[1]) != local || procAddrPort(f[2]) != remote {
				continue
			}
			state, serr := strconv.ParseUint(f[3], 16, 8)
			refs, rerr := strconv.Atoi(f[10])
			if serr != nil || rerr != nil {
				t.Fatalf("%s: %q", table, line)
			}
			return uint8(state), refs, true
		}
	}
	return 0, 0, false
}

// procAddrPort is an address:port as /proc/net/tcp and tcp6 list it, both in
// hex, the address in 32-bit words of host byte order (procIP), unmapped.
func procAddrPort(h string) netip.AddrPort {
	ip, port, _ := strings.Cut(h, ":")
	n, err := strconv.ParseUint(port, 16, 16)
	a, ok := netip.AddrFromSlice(procIP(ip))
	if err != nil || !ok {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(a.Unmap(), uint16(n))
}

// An application socket on its way to closed is read by the wait, bound to a
// device or not: how much it wrote, never gone. For a socket bound to a
// device that is the dump's answer, so the dump has to ask for each closing
// state, among them FIN_WAIT1, CLOSING and LAST_ACK, the states the
// application's FIN is counted in.
//
// close(2) sets FIN_WAIT1 and LAST_ACK itself. The FIN goes out behind the
// bytes the proxy has not read, so those bytes hold these states, and
// CLOSING, still.
func TestAClosingApplicationSocketIsReadNotGone(t *testing.T) {
	for _, tc := range pairShapes {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("CLOSE_WAIT", func(t *testing.T) {
				p := &Proxy{logger: zap.NewNop()}
				app, proxySide, _ := heldPairBound(t, p, 1, tc.network, tc.addr, "", tc.dev)
				if _, err := app.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
				if err := proxySide.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				// The application has the FIN once its read ends: its socket
				// is in CLOSE_WAIT from then on.
				if _, err := app.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
					t.Fatalf("the application's read after the proxy's shutdown: %v, want EOF", err)
				}
				pc, ds := closingPair(t, p, proxySide)
				if got, err := ds.lookup(pc.app, pc.proxy); err != nil || got.state != tcpCloseWait {
					t.Fatalf("lookup of the application's socket %s -> %s = TCP state %d, %v; want CLOSE_WAIT", pc.app, pc.proxy, got.state, err)
				}
				tg, err := p.pairTarget(ds, pc)
				if err != nil || tg == nil || tg.gone || tg.want != 1 {
					t.Fatalf("pairTarget = %+v, %v; want 1 byte written, not gone", tg, err)
				}
			})
			t.Run("FIN_WAIT1 then CLOSING", func(t *testing.T) {
				p := &Proxy{logger: zap.NewNop()}
				app, proxySide, reader := heldPairBound(t, p, 1, tc.network, tc.addr, "", tc.dev)
				n := queue(t, app)
				pc, ds := closingPair(t, p, proxySide)
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
				wantFINCounted(t, p, ds, pc, tcpFinWait1, n)
				if err := proxySide.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				// The proxy's FIN moves it to CLOSING when the host gets to
				// deliver the FIN, and the bytes hold it there.
				movedTo(t, ds, pc, tcpClosing, tcpFinWait1)
				wantFINCounted(t, p, ds, pc, tcpClosing, n)
				readAndSettle(t, p, reader, n)
			})
			t.Run("LAST_ACK", func(t *testing.T) {
				p := &Proxy{logger: zap.NewNop()}
				app, proxySide, reader := heldPairBound(t, p, 1, tc.network, tc.addr, "", tc.dev)
				if err := proxySide.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				if _, err := app.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
					t.Fatalf("the application's read after the proxy's shutdown: %v, want EOF", err)
				}
				n := queue(t, app)
				pc, ds := closingPair(t, p, proxySide)
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
				wantFINCounted(t, p, ds, pc, tcpLastAck, n)
				readAndSettle(t, p, reader, n)
			})
			t.Run("TIME_WAIT", func(t *testing.T) {
				p := &Proxy{logger: zap.NewNop()}
				app, proxySide, _ := heldPairBound(t, p, 1, tc.network, tc.addr, "", tc.dev)
				pc, ds := closingPair(t, p, proxySide)
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
				if err := proxySide.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				// Here the socket does move: on its way the kernel swaps it
				// for a timewait socket, in FIN_WAIT2 or TIME_WAIT, which
				// stays in TIME_WAIT for 60 s. The exact lookup, which takes
				// no lock, can catch the swap and miss the socket or read it
				// closed, so the case reads TIME_WAIT once the kernel's own
				// table lists it there.
				movedTo(t, ds, pc, tcpTimeWait, tcpFinWait1, tcpFinWait2, tcpClosing)
				if tg, err := p.pairTarget(ds, pc); err != nil || tg != nil {
					t.Fatalf("pairTarget in TIME_WAIT = %+v, %v; want nothing to wait for", tg, err)
				}
			})
		})
	}
}

// boundTo is a dialler's or listener's Control that binds its socket to the
// device dev (SO_BINDTODEVICE), or leaves it bound to none when dev is "".
// Linux lets an unprivileged process bind a socket that is bound to none.
func boundTo(dev string) func(_, _ string, rc syscall.RawConn) error {
	return func(_, _ string, rc syscall.RawConn) error {
		if dev == "" {
			return nil
		}
		var serr error
		if err := rc.Control(func(fd uintptr) { serr = unix.BindToDevice(int(fd), dev) }); err != nil {
			return err
		}
		return serr
	}
}

// A wait the listener's close ends, or one whose kernel cannot give the
// counts, is unknown, never settled.
func TestABytesWaitThatCannotBeToldIsUnknown(t *testing.T) {
	p, _, _, _ := heldPair(t, "tcp4", "127.0.0.1:0")
	p.paired.fail(errListenerClosed)
	if err := p.WaitPairedBytes(context.Background(), 1); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("WaitPairedBytes after the listener closed: %v, want unknown", err)
	}
}

// Whose application socket is gone when the wait begins: its FIN or reset at
// the proxy's side ends the wait; a proxy's side still alive with no FIN or
// reset from it is waited for once application sockets have been seen (its
// end is on its way), and unknown while none ever was (another network
// namespace); never settled.
func TestAGoneApplicationSocketIsItsFINOrResetOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   uint8
		closed  bool
		visible bool
		reach   bool
		err     bool
	}{
		{name: "its FIN reached the proxy", state: tcpCloseWait, reach: true},
		{name: "its FIN reached the proxy, which closed too", state: tcpLastAck, reach: true},
		{name: "the proxy's socket in TIME_WAIT", state: tcpTimeWait, reach: true},
		{name: "its reset reached the proxy", state: tcpClose, reach: true},
		{name: "its reset reached the proxy, which closed then", state: tcpClose, closed: true, reach: true},
		// Application sockets seen before: its FIN or reset is on its way.
		{name: "alive at the proxy's side, sockets seen", state: tcpEstablished, visible: true, reach: false},
		// None ever seen: it may be in another network namespace.
		{name: "alive at the proxy's side, no socket ever seen", state: tcpEstablished, err: true},
		// Closed by the proxy while the application's end was open, which has
		// gone since, and no orphan of the proxy's in TIME_WAIT says its FIN
		// arrived: a reset, its own or the orphan's refusing bytes written
		// before the close, cannot be told apart.
		{name: "closed by the proxy first, the application's end gone since", state: tcpEstablished, closed: true, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop()}
			p.paired.visible.Store(tc.visible)
			pc := &pairedConn{done: make(chan struct{})}
			reached, err := p.pairEnded(&pairTarget{pc: pc, gone: true}, proxySide{state: tc.state}, tc.closed)
			if tc.err {
				if !errors.Is(err, models.ErrProxyConnsUnsupported) {
					t.Fatalf("pairEnded = %v, %v; want unknown", reached, err)
				}
				return
			}
			if err != nil || reached != tc.reach {
				t.Fatalf("pairEnded = %v, %v; want %v", reached, err, tc.reach)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
				t.Fatalf("%d losses, want none", l)
			}
		})
	}
}

// This kernel's sock_diag gives the counts the wait reads, on a connection
// like the canary's: checkPairedBytes leaves the waits answerable.
func TestTheCanaryProvesTheByteCountsReadable(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := &Proxy{logger: zap.NewNop()}
	p.checkPairedBytes(c, s)
	if err := p.paired.failure(); err != nil {
		t.Fatalf("checkPairedBytes on a plain connection: %v", err)
	}
}
