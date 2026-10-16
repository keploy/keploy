//go:build linux

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The same events (the application writes, the proxy closes its side short
// of what was written, the bytes still on their way are refused with a reset)
// give a capture loss when the wait began before the reset
// (TestBytesThatCanNoLongerArriveAreACaptureLoss); a wait that begins after
// it, before the handler's goroutine removed the connection, must not settle
// with nothing said either.
func TestBytesTheProxysCloseRefusedAreNeverSettledSilently(t *testing.T) {
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	n := queue(t, app)
	if got := received(t, proxySide); got >= n {
		t.Fatalf("fixture holds nothing back: %d of %d", got, n)
	}
	_ = reader.Close() // the proxy closes short; unread data: a reset to the app
	for {
		_, err := lookupTCP(mustAddrPort(t, app.LocalAddr()), mustAddrPort(t, app.RemoteAddr()))
		if errors.Is(err, errNoSocket) {
			break
		}
		runtime.Gosched()
	}
	at := p.paired.byConn[proxySide].atClose.Load()
	err := p.WaitPairedBytes(context.Background(), 1)
	l := lossesOf(p, models.CaptureLossAppBytesLost)
	t.Logf("written %d, the proxy's side had received %d at its close; WaitPairedBytes = %v, app_bytes_lost = %d", n, at.received, err, l)
	if err == nil && l == 0 {
		t.Fatalf("%d of %d bytes written before the proxy's close never reached it, and the wait settled with no loss and no unknown", n-at.received, n)
	}
}

// The real proxy's PairedBytesProgress moves with what the wait sees.
func TestTheProxysBytesProgressMovesWithWhatTheWaitSees(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	n := queue(t, app)
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 {
		t.Fatalf("pairTargets = %v, %v", targets, err)
	}
	before := p.PairedBytesProgress()
	if _, err := io.ReadFull(reader, make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	if err := p.waitPairTargets(context.Background(), targets); err != nil {
		t.Fatal(err)
	}
	if got := p.PairedBytesProgress() - before; got < 2 {
		t.Fatalf("PairedBytesProgress moved %d for a connection that received more and was resolved, want 2", got)
	}
}

// Through the real accept loop, a paired connection proves the
// application's sockets seen, and its handler's close is read before it
// happens (beforeClose wired at the accept).
func TestTheAcceptLoopReadsAPairedConnectionsCloseFirst(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	up, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = up.Close() })
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	f := &fakeCounter{dst: ipv4Dest(t, up.Addr().String())}
	cfg := config.New()
	cfg.ProxyPort = 0
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, up.Addr().String())}, cfg)
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.SetListenerCounter(f)
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp4", p.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: dep\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := p.WaitAccepted(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if !p.paired.visible.Load() {
		t.Error("the first paired connection did not prove the application's sockets seen")
	}
	p.paired.mu.Lock()
	var pc *pairedConn
	for _, v := range p.paired.byConn {
		pc = v
	}
	p.paired.mu.Unlock()
	if pc == nil {
		t.Fatal("not paired")
	}
	_ = c.Close()
	for {
		ch := p.paired.changes()
		p.paired.mu.Lock()
		n := len(p.paired.byConn)
		p.paired.mu.Unlock()
		if n == 0 {
			break
		}
		<-ch
	}
	if pc.atClose.Load() == nil {
		t.Fatal("the handler closed the paired connection without its close being read first: a wait holding it would answer unknown")
	}
}

// The clean end settles: every byte arrived, the proxy closes, the
// application closes after it (its FIN), and its socket is gone before the
// handler's goroutine removed the connection: the proxy's orphan in TIME_WAIT
// says the FIN arrived.
func TestTheProxysCloseThenTheApplicationsFINSettles(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	if _, err := app.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the application's read after the proxy's close: %v, want EOF", err)
	}
	local, remote := mustAddrPort(t, app.LocalAddr()), mustAddrPort(t, app.RemoteAddr())
	_ = app.Close()
	for {
		_, err := lookupTCP(local, remote)
		if errors.Is(err, errNoSocket) {
			break
		}
		runtime.Gosched()
	}
	if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
		t.Fatalf("WaitPairedBytes: %v, want settled", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d losses, want none", l)
	}
}

// The clean end with the wait beginning while the application's FIN is not yet
// acknowledged (its socket in LAST_ACK: a loaded host deferring the ACK's
// softirq, a held packet) settles with no loss: what the application wrote
// counts that FIN, which the proxy's side, read as it closed, never received,
// but its orphan took it (TIME_WAIT), so nothing was refused. A drop-all
// filter on the application's socket holds the proxy's ACK off.
func TestTheProxysCloseThenTheApplicationsUnacknowledgedFINIsNoLoss(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	if _, err := app.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the application's read after the proxy's close: %v, want EOF", err)
	}
	rc, err := app.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		drop := []unix.SockFilter{{Code: unix.BPF_RET | unix.BPF_K, K: 0}}
		serr = unix.SetsockoptSockFprog(int(fd), unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &unix.SockFprog{Len: 1, Filter: &drop[0]})
	}); err != nil {
		t.Fatal(err)
	}
	if errors.Is(serr, unix.EPERM) {
		// Linux 7 refuses an unprivileged filter on a TCP socket (a UDP
		// one, or root, is let through). CI's root step runs this test
		// as root; TestAProxysCloseOneFINShortIsJudgedByItsOrphan covers
		// the judgement here.
		t.Skipf("this kernel refuses an unprivileged socket filter on a TCP socket: %v", serr)
	}
	if serr != nil {
		t.Fatalf("attach a drop-all filter to the application's socket: %v", serr)
	}
	local, remote := mustAddrPort(t, app.LocalAddr()), mustAddrPort(t, app.RemoteAddr())
	_ = app.Close()
	for {
		d, err := lookupTCP(local, remote)
		if err != nil {
			t.Fatalf("the application's socket after its close: %v", err)
		}
		if d.state == tcpLastAck {
			break
		}
		runtime.Gosched()
	}
	// The FIN reaches the orphan, retransmitted if need be: only a reset
	// (which nothing here sends) would end it first.
	for {
		d, err := lookupTCP(remote, local)
		if err != nil {
			t.Fatalf("the proxy's orphaned socket went before the application's FIN reached it: %v", err)
		}
		if d.state == tcpTimeWait {
			break
		}
		runtime.Gosched()
	}
	if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
		t.Fatalf("WaitPairedBytes: %v, want settled: every byte the application wrote, and its FIN, reached the proxy", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none: every byte the application wrote, and its FIN, reached the proxy", l)
	}
}

// pairReached on a connection the proxy closed one short of what the
// application wrote (what appWritten reads once the application has closed,
// its FIN not yet acknowledged: the bytes and the FIN), on the kernel's own
// sockets, with no socket filter: an orphan that took the FIN (TIME_WAIT)
// refused nothing and settles with no loss; one still waiting for a FIN
// (FIN_WAIT2) cannot tell it from a byte it will refuse, so the wait answers
// unknown; two short is a loss, whatever the orphan holds.
func TestAProxysCloseOneFINShortIsJudgedByItsOrphan(t *testing.T) {
	closedPair := func(t *testing.T) (*Proxy, *pairedConn, *net.TCPConn, proxySide) {
		t.Helper()
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		if _, err := app.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
		_ = reader.Close()
		if _, err := app.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("the application's read after the proxy's close: %v, want EOF", err)
		}
		p.paired.mu.Lock()
		pc := p.paired.byConn[proxySide]
		p.paired.mu.Unlock()
		if pc == nil {
			t.Fatal("the paired connection is not kept")
		}
		ps, closed := pc.side()
		if !closed || ps.err != nil || ps.received != 10 {
			t.Fatalf("the proxy's side: closed %v, received %d (%v), want closed after 10", closed, ps.received, ps.err)
		}
		return p, pc, app, ps
	}
	orphanIn := func(t *testing.T, pc *pairedConn, state uint8) {
		t.Helper()
		for {
			d, err := lookupTCP(pc.proxy, pc.app)
			if err != nil {
				t.Fatalf("the proxy's orphaned socket: %v", err)
			}
			if d.state == state {
				return
			}
			runtime.Gosched()
		}
	}

	t.Run("its orphan took the FIN", func(t *testing.T) {
		p, pc, app, ps := closedPair(t)
		_ = app.Close()
		orphanIn(t, pc, tcpTimeWait)
		ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1})
		if !ok || err != nil {
			t.Fatalf("pairReached = %v, %v; want settled", ok, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want none: the orphan took the FIN, so it refused nothing", l)
		}
	})

	t.Run("its orphan still waits for a FIN", func(t *testing.T) {
		p, pc, _, ps := closedPair(t)
		orphanIn(t, pc, tcpFinWait2)
		ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1})
		if ok || !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("one short = %v, %v; want unknown (%v): the FIN, or a byte the orphan will refuse, is still to come", ok, err, models.ErrProxyConnsUnsupported)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("one short: %d app_bytes_lost charged, want none (unknown)", l)
		}
		ok, err = p.pairReached(&pairTarget{pc: pc, want: ps.received + 2})
		if !ok || err != nil {
			t.Fatalf("two short = %v, %v; want resolved as a loss", ok, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("two short: %d app_bytes_lost charged, want 1: at least one byte the application wrote will be refused", l)
		}
	})
}

// The proxy's handler closes a connection with bytes it never read: its side
// counts them received, and the close tosses them with a reset (tcp_close).
// They were never captured, so the close charges a loss, whether a wait holds
// the connection or not; a wait holding it settles on it.
func TestAProxysCloseWithBytesUnreadIsALoss(t *testing.T) {
	for _, waiting := range []bool{true, false} {
		name := "no wait holds it"
		if waiting {
			name = "a wait holds it"
		}
		t.Run(name, func(t *testing.T) {
			p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
			if _, err := app.Write([]byte("publish 1\npublish 2\n")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil { // the handler takes the first call only
				t.Fatal(err)
			}
			for received(t, proxySide) < 20 {
				runtime.Gosched()
			}
			var targets []*pairTarget
			if waiting {
				var err error
				if targets, err = p.pairTargets(1); err != nil || len(targets) != 1 || targets[0].want != 20 {
					t.Fatalf("pairTargets = %v, %v; want one connection, 20 bytes written", targets, err)
				}
			}
			_ = reader.Close() // 10 bytes unread: the close resets
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
				t.Fatalf("%d app_bytes_lost charged at the close, want 1: 10 bytes the application wrote were never read, and the close tossed them", l)
			}
			if waiting {
				if err := p.waitPairTargets(context.Background(), targets); err != nil {
					t.Fatalf("the wait: %v, want settled on the loss", err)
				}
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
				t.Fatalf("%d app_bytes_lost charged, want 1: once per connection", l)
			}
		})
	}
}

// A connection the proxy serves with no session captures nothing by design:
// an agent not started in record or test mode closes it unanswered, with the
// application's bytes unread, and an agent in record mode passes it through.
// Neither is a capture loss, and no bytes wait holds one: no test is open on
// it. The close of the unanswered one was charged app_bytes_lost (the hooks
// lane's TestTheApplicationsConnectionIsWaitedForBeforeTheProxySeesIt).
func TestAConnectionServedWithoutASessionIsNoCaptureLoss(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode models.Mode
	}{{"no mode", ""}, {"record mode", models.MODE_RECORD}} {
		mode := tc.mode
		t.Run(tc.name, func(t *testing.T) {
			up, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = up.Close() })
			reached := make(chan struct{}, 1)
			go func() {
				for {
					c, err := up.Accept()
					if err != nil {
						return
					}
					reached <- struct{}{}
					go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
				}
			}()
			f := &fakeCounter{dst: ipv4Dest(t, up.Addr().String())}
			cfg := config.New()
			cfg.ProxyPort = 0
			cfg.Agent.Mode = mode
			p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, up.Addr().String())}, cfg)
			p.SetListenerCounter(f)
			ctx, cancel := context.WithCancel(context.Background())
			ready, done := make(chan error, 1), make(chan error, 1)
			go func() { done <- p.Serve(ctx, ready) }()
			t.Cleanup(func() { cancel(); <-done })
			if err := <-ready; err != nil {
				t.Fatal(err)
			}
			// The application's bytes are in the proxy's socket when it
			// accepts the connection, as the hooks lane holds them, so the
			// close of an unanswered connection finds them unread whatever
			// the scheduler does (30 s is only the kernel's cap with no data,
			// which is never waited on).
			rc, err := p.Listener.(*net.TCPListener).SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			var serr error
			if err := rc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT, 30) }); err != nil || serr != nil {
				t.Fatalf("TCP_DEFER_ACCEPT: %v %v", err, serr)
			}
			c, err := net.Dial("tcp4", p.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: dep\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			if err := p.WaitAccepted(ctx, 2); err != nil {
				t.Fatal(err)
			}
			if mode == models.MODE_RECORD {
				// Passed through (its destination reached), and open: no
				// bytes wait holds it.
				<-reached
				if targets, err := p.pairTargets(2); err != nil || len(targets) != 0 {
					t.Fatalf("pairTargets = %v, %v; want none: nothing on a connection served without a session is captured", targets, err)
				}
				_ = c.Close()
			}
			// The proxy closes its side: unanswered, or once the
			// passthrough ends.
			for {
				ch := p.paired.changes()
				p.paired.mu.Lock()
				n := len(p.paired.byConn)
				p.paired.mu.Unlock()
				if n == 0 {
					break
				}
				<-ch
			}
			if l := p.losses.Snapshot(); len(l) != 0 {
				t.Fatalf("capture losses %+v, want none for a connection served without a session", l)
			}
		})
	}
}
