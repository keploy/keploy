//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
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
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) {
			// A segment far smaller than the buffer, so the window
			// reopens a segment at a time as the proxy reads (loopback's
			// own, 64 KiB, would hold it shut until the persist timer).
			if serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG, 1024); serr == nil {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 16384)
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
	c, err := net.Dial(network, ln.Addr().String())
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

func lossesOf(p *Proxy, kind models.CaptureLossKind) uint64 {
	for _, l := range p.losses.Snapshot() {
		if l.Kind == kind {
			return l.Count
		}
	}
	return 0
}

// A call written to a connection the proxy already holds (a pooled one) is
// waited for until every byte the application had written when the wait
// began has reached the proxy's side, however long the bytes take; an idle
// connection holds nothing back. Over IPv4 and IPv6.
func TestThePairedBytesAreWaitedForUntilTheyArrive(t *testing.T) {
	for _, tc := range []struct{ network, addr string }{{"tcp4", "127.0.0.1:0"}, {"tcp6", "[::1]:0"}} {
		t.Run(tc.network, func(t *testing.T) {
			p, app, proxySide, reader := heldPair(t, tc.network, tc.addr)
			// Idle: nothing written, nothing to wait for.
			if err := p.WaitPairedBytes(context.Background(), 1, models.MODE_TEST); err != nil {
				t.Fatalf("WaitPairedBytes on an idle connection: %v", err)
			}
			n := queue(t, app)
			if got := received(t, proxySide); got >= n {
				t.Fatalf("the proxy's side received %d of %d at once: the fixture holds nothing back", got, n)
			}
			done := make(chan error, 1)
			go func() { done <- p.WaitPairedBytes(context.Background(), 1, models.MODE_TEST) }()
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
	if err := p.WaitPairedBytes(context.Background(), 1, models.MODE_TEST); err != nil {
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
			go func() { done <- p.waitPairTargets(context.Background(), targets, false) }()
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
	if err := p.WaitPairedBytes(context.Background(), 1, models.MODE_TEST); err != nil {
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
	if err := p.WaitPairedBytes(context.Background(), 5, models.MODE_TEST); err != nil {
		t.Fatalf("WaitPairedBytes with no paired connection: %v, want nil", err)
	}
}

// The first paired connection proves the application's sockets are seen from
// the proxy's network namespace: its application end is found.
func TestAPairedConnectionProvesTheApplicationsSocketsSeen(t *testing.T) {
	p, _, proxySide, _ := heldPair(t, "tcp4", "127.0.0.1:0")
	if p.paired.visible.Load() {
		t.Fatal("seen before any proof")
	}
	p.proveAppVisible(proxySide)
	if !p.paired.visible.Load() {
		t.Fatal("the application's end of a live paired connection was not found")
	}
}

// A wait the listener's close ends, or one whose kernel cannot give the
// counts, is unknown, never settled.
func TestABytesWaitThatCannotBeToldIsUnknown(t *testing.T) {
	p, _, _, _ := heldPair(t, "tcp4", "127.0.0.1:0")
	p.paired.fail(errListenerClosed)
	if err := p.WaitPairedBytes(context.Background(), 1, models.MODE_TEST); !errors.Is(err, models.ErrProxyConnsUnsupported) {
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

// A record's wait for the paired bytes goes on past a connection it cannot
// tell, until every other one has received what the application had written
// to it, and then answers what it could not tell: a call on one of the others
// is the test's, seen by its end, not left to land in the next test. A
// replay's ends at the connection it cannot tell.
func TestARecordsBytesWaitGoesOnPastAConnectionItCannotTell(t *testing.T) {
	for _, record := range []bool{true, false} {
		t.Run(fmt.Sprintf("record %v", record), func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop()}
			app1, _, _ := heldPairOn(t, p, 1, "tcp4", "127.0.0.1:0")
			app2, proxySide2, reader2 := heldPairOn(t, p, 2, "tcp4", "127.0.0.1:0")
			queue(t, app1)
			n := queue(t, app2)
			targets, err := p.pairTargets(2)
			if err != nil || len(targets) != 2 {
				t.Fatalf("pairTargets = %d, %v; want both connections", len(targets), err)
			}
			// The first cannot be told: its application socket gone while
			// its connection is alive at the proxy's side, and no
			// application socket ever seen (pairEnded).
			for _, tg := range targets {
				tg.gone = tg.pc.appSeq == 1
			}
			p.paired.visible.Store(false)
			done := make(chan error, 1)
			go func() { done <- p.waitPairTargets(context.Background(), targets, record) }()
			if !record {
				if err := <-done; !errors.Is(err, models.ErrProxyConnsUnsupported) {
					t.Fatalf("a replay's wait = %v, want the unknown", err)
				}
				return
			}
			buf := make([]byte, 64<<10)
			var read uint64
			for read < n {
				select {
				case err := <-done:
					if got := received(t, proxySide2); got < n {
						t.Fatalf("the record's wait returned %v with %d of %d bytes of the other connection received", err, got, n)
					}
					done <- err
				default:
				}
				m, err := reader2.Read(buf)
				if err != nil {
					t.Fatal(err)
				}
				read += uint64(m)
			}
			if err := <-done; !errors.Is(err, models.ErrProxyConnsUnsupported) {
				t.Fatalf("the record's wait = %v, want the unknown once the other connection's bytes arrived", err)
			}
		})
	}
}

// Only a record's wait (models.MODE_RECORD) goes on past a connection it
// cannot tell (WaitPairedBytes): a replay's, or one in no mode, ends at it.
// The one it cannot tell is a connection whose socket the proxy's side lost
// before the wait could read it.
func TestOnlyARecordsBytesWaitGoesOnPastWhatItCannotTell(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST, models.MODE_OFF} {
		t.Run(string(mode), func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop()}
			app1, proxySide1, _ := heldPairOn(t, p, 1, "tcp4", "127.0.0.1:0")
			app2, proxySide2, reader2 := heldPairOn(t, p, 2, "tcp4", "127.0.0.1:0")
			queue(t, app1)
			n := queue(t, app2)
			_ = proxySide1.Close() // no close of the proxy's: nothing read it
			done := make(chan error, 1)
			go func() { done <- p.WaitPairedBytes(context.Background(), 2, mode) }()
			if mode != models.MODE_RECORD {
				// Nothing reads the other connection: a wait that went on
				// for it would never end (the run's hang guard).
				if err := <-done; !errors.Is(err, models.ErrProxyConnsUnsupported) {
					t.Fatalf("WaitPairedBytes = %v, want the unknown", err)
				}
				return
			}
			buf := make([]byte, 64<<10)
			var read uint64
			for read < n {
				select {
				case err := <-done:
					if got := received(t, proxySide2); got < n {
						t.Fatalf("the record's wait returned %v with %d of %d bytes of the other connection received", err, got, n)
					}
					done <- err
				default:
				}
				m, err := reader2.Read(buf)
				if err != nil {
					t.Fatal(err)
				}
				read += uint64(m)
			}
			if err := <-done; !errors.Is(err, models.ErrProxyConnsUnsupported) {
				t.Fatalf("the record's wait = %v, want the unknown", err)
			}
		})
	}
}

// A record's wait goes on past a connection whose application socket it
// cannot read as the wait begins (a sock_diag lookup that fails), as past
// one it cannot tell later: it waits for the other connection's bytes, then
// answers the unknown. A replay's ends at it.
func TestARecordsBytesWaitGoesOnPastAConnectionItCannotRead(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		t.Run(string(mode), func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop()}
			app1, _, _ := heldPairOn(t, p, 1, "tcp4", "127.0.0.1:0")
			app2, proxySide2, reader2 := heldPairOn(t, p, 2, "tcp4", "127.0.0.1:0")
			queue(t, app1)
			n := queue(t, app2)
			// The first's lookup fails: an IPv6 end on an IPv4 connection.
			p.paired.mu.Lock()
			for _, pc := range p.paired.byConn {
				if pc.appSeq == 1 {
					pc.app = netip.MustParseAddrPort("[2001:db8::1]:1")
				}
			}
			p.paired.mu.Unlock()
			if _, err := p.pairTargets(2); err == nil {
				t.Fatal("the fixture's lookup did not fail")
			}
			done := make(chan error, 1)
			go func() { done <- p.WaitPairedBytes(context.Background(), 2, mode) }()
			if mode != models.MODE_RECORD {
				if err := <-done; err == nil {
					t.Fatal("a replay's wait settled past a connection it could not read")
				}
				return
			}
			buf := make([]byte, 64<<10)
			var read uint64
			for read < n {
				select {
				case err := <-done:
					if got := received(t, proxySide2); got < n {
						t.Fatalf("the record's wait returned %v with %d of %d bytes of the other connection received", err, got, n)
					}
					done <- err
				default:
				}
				m, err := reader2.Read(buf)
				if err != nil {
					t.Fatal(err)
				}
				read += uint64(m)
			}
			if err := <-done; err == nil {
				t.Fatal("the record's wait settled a connection it could not read")
			}
		})
	}
}

// A connection a record's wait can no longer tell is resolved, as far as the
// wait goes: it moves the wait's progress (PairedBytesProgress), which the
// settle's watchdog reads, as one that is reached does.
func TestAConnectionARecordCannotTellMovesTheWaitsProgress(t *testing.T) {
	p := &Proxy{logger: zap.NewNop()}
	app, _, _ := heldPairOn(t, p, 1, "tcp4", "127.0.0.1:0")
	queue(t, app)
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 {
		t.Fatalf("pairTargets = %d, %v", len(targets), err)
	}
	targets[0].gone = true
	p.paired.visible.Store(false)
	was := p.PairedBytesProgress()
	if err := p.waitPairTargets(context.Background(), targets, true); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("the record's wait = %v, want the unknown", err)
	}
	if got := p.PairedBytesProgress(); got != was+1 {
		t.Fatalf("progress %d after the unknown, want %d", got, was+1)
	}
}
