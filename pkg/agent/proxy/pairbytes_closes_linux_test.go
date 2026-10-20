//go:build linux

package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"testing"
	"unsafe"

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
// with nothing said either. The close reset the connection (bytes unread), so
// nothing after it was taken: a loss, said for sure, not an unknown.
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
	if err != nil || l != 1 {
		t.Fatalf("%d of %d bytes written before the proxy's close never reached it: WaitPairedBytes = %v, %d app_bytes_lost; want settled on one loss", n-at.received, n, err, l)
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
	// Most of what was queued waits in the application's socket behind the
	// proxy's small window: sock_diag's tcpi_notsent_bytes says so.
	if tg := targets[0]; tg.want != n || tg.sent >= tg.want || tg.sent == 0 {
		t.Fatalf("the target: want %d, sent %d; want %d written, and less than that sent", tg.want, tg.sent, n)
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
// handler's goroutine removed the connection: the proxy's socket, kept open
// past its close, says the FIN arrived, and nothing else.
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
// but its socket, kept open past the close, took it, and nothing else. A
// drop-all filter on the application's socket holds the proxy's ACK off.
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
		// as root; TestAProxysCloseIsJudgedByWhatCameAfter covers the
		// judgement here.
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
	// The FIN reaches the proxy's kept socket, retransmitted if need be: only
	// a reset (which nothing here sends) would end its read otherwise.
	pc := pairedConnOf(t, p, 1)
	awaitEnd(t, pc)
	if end := pc.end.Load(); end.kind != afterFIN {
		t.Fatalf("the kept socket's read ended with %v (%v), want the application's FIN", end.kind, end.err)
	}
	if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
		t.Fatalf("WaitPairedBytes: %v, want settled: every byte the application wrote, and its FIN, reached the proxy", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none: every byte the application wrote, and its FIN, reached the proxy", l)
	}
}

// What came to the proxy's socket of a connection it closed one short of what
// the application wrote (what appWritten reads once the application has
// closed, its FIN not yet acknowledged: the bytes and the FIN) decides it: the
// proxy keeps the socket open past its close, its write side shut, and reads
// what the application sends after (keepAfterClose).
//   - Only the FIN: no loss, whenever it comes; the wait waits for it, and
//     its arrival, which ends the kept socket's read, wakes it.
//   - The application's reset: what it had sent when the wait began decides
//     (pairReset); with its socket gone, unknown.
//   - A byte of data: a loss, and the application gets the reset an orphan
//     would have sent; over TLS, unknown (the client's close_notify, or a
//     call).
//   - keploy's stop: unknown.
//
// The kept socket is no orphan, so nothing the kernel does to orphans (a
// timewait budget used up, an orphan killed by a reset after it took the FIN,
// TCP_LINGER2) can take away what it read.
func TestAProxysCloseIsJudgedByWhatCameAfter(t *testing.T) {
	// closedPairOn closes the proxy's side of a pair held by p, as
	// application connection appSeq, after it read the 10 bytes the
	// application wrote, with ctx as the proxy's connections' context;
	// linger2, when not 0, is the TCP_LINGER2 (seconds) of the proxy's
	// socket, set before the close.
	closedPairOn := func(t *testing.T, ctx context.Context, p *Proxy, appSeq uint64, linger2 int) (*pairedConn, *net.TCPConn, proxySide) {
		t.Helper()
		app, proxySide, reader := heldPairOn(t, p, appSeq, "tcp4", "127.0.0.1:0")
		reader.(*trackedConn).beforeClose = func() { p.pairClosing(ctx, proxySide, reader.(*trackedConn)) }
		if linger2 != 0 {
			rc, err := proxySide.SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			var serr error
			if err := rc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_LINGER2, linger2) }); err != nil || serr != nil {
				t.Fatalf("TCP_LINGER2 %d on the proxy's socket: %v %v", linger2, err, serr)
			}
		}
		if _, err := app.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
		_ = reader.Close()
		// The proxy's FIN reaches the application at its close, as it did
		// when the close orphaned the socket.
		if _, err := app.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("the application's read after the proxy's close: %v, want EOF", err)
		}
		pc := pairedConnOf(t, p, appSeq)
		ps, closed := pc.side()
		if !closed || ps.err != nil || ps.received != 10 {
			t.Fatalf("the proxy's side: closed %v, received %d (%v), want closed after 10", closed, ps.received, ps.err)
		}
		if !pc.kept.Load() {
			t.Fatal("the proxy's close did not keep its socket open past the close")
		}
		return pc, app, ps
	}
	closedPair := func(t *testing.T, linger2 int) (*Proxy, *pairedConn, *net.TCPConn, proxySide) {
		t.Helper()
		p := &Proxy{logger: zap.NewNop()}
		pc, app, ps := closedPairOn(t, context.Background(), p, 1, linger2)
		return p, pc, app, ps
	}
	oneShort := func(pc *pairedConn, ps proxySide) *pairTarget {
		return &pairTarget{pc: pc, want: ps.received + 1, finCounted: true}
	}

	t.Run("the FIN still to come is waited for", func(t *testing.T) {
		p, pc, app, ps := closedPair(t, 0)
		ok, err := p.pairReached(oneShort(pc, ps))
		if ok || err != nil {
			t.Fatalf("one short, nothing come yet = %v, %v; want waited on: the FIN, or a byte, is still to come, and the kept socket will say which", ok, err)
		}
		done := waitOn(p, oneShort(pc, ps))
		select {
		case err := <-done:
			t.Fatalf("the wait answered %v before the application's FIN was sent", err)
		default:
		}
		_ = app.Close() // the FIN; its arrival ends the kept socket's read
		if err := <-done; err != nil {
			t.Fatalf("the wait = %v; want settled: only the FIN came", err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want none", l)
		}
	})

	// TCP_LINGER2 -1 has the kernel destroy an orphan as it reaches
	// FIN_WAIT2, as a timewait budget used up, or a reset after it took the
	// FIN, does: the kept socket is no orphan.
	t.Run("what the kernel does to orphans takes nothing", func(t *testing.T) {
		p, pc, app, ps := closedPair(t, -1)
		_ = app.Close()
		awaitEnd(t, pc)
		if end := pc.end.Load(); end.kind != afterFIN {
			t.Fatalf("the kept socket's read ended with %v (%v), want the FIN", end.kind, end.err)
		}
		ok, err := p.pairReached(oneShort(pc, ps))
		if !ok || err != nil {
			t.Fatalf("one short, only the FIN come = %v, %v; want settled", ok, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want none", l)
		}
	})

	// A reset says nothing of bytes still on their way (pairReset): what the
	// application had sent when the wait began decides.
	t.Run("the application's reset after the close: what it had sent decides", func(t *testing.T) {
		p, pc, app, ps := closedPair(t, 0)
		if err := app.SetLinger(0); err != nil {
			t.Fatal(err)
		}
		_ = app.Close() // a reset
		awaitEnd(t, pc)
		if end := pc.end.Load(); end.kind != afterReset {
			t.Fatalf("the kept socket's read ended with %v (%v), want the reset", end.kind, end.err)
		}
		for _, c := range []struct {
			name       string
			want, sent uint64
			finCounted bool
			ok, loss   bool
		}{
			{"all it had written in", ps.received, ps.received, false, true, false},
			{"only its FIN missing", ps.received + 1, ps.received + 1, true, true, false},
			{"a byte it had sent missing: a loss", ps.received + 1, ps.received + 1, false, true, true},
			{"a byte it had not sent missing: unknown", ps.received + 1, ps.received, false, false, false},
		} {
			pc.lost.Store(false)
			was := lossesOf(p, models.CaptureLossAppBytesLost)
			ok, err := p.pairReached(&pairTarget{pc: pc, want: c.want, sent: c.sent, finCounted: c.finCounted})
			lost := lossesOf(p, models.CaptureLossAppBytesLost) > was
			if ok != c.ok || lost != c.loss || (c.ok == (err != nil)) || (err != nil && !errors.Is(err, models.ErrProxyConnsUnsupported)) {
				t.Errorf("%s: = %v, %v, loss %v; want %v, loss %v", c.name, ok, err, lost, c.ok, c.loss)
			}
		}
		// Its socket gone when the wait began: nothing says what it had sent.
		ok, err := p.pairReached(&pairTarget{pc: pc, gone: true})
		if ok || !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "was gone") {
			t.Fatalf("a reset, the application's socket gone = %v, %v; want unknown", ok, err)
		}
	})

	// The handler's own reset (SO_LINGER 0: a refused mock.real
	// destination): the application is reset, as before, nothing is kept,
	// nothing after the close was taken, and what the application had sent
	// decides, as for any reset.
	t.Run("the handler's own reset is judged as a reset", func(t *testing.T) {
		p := &Proxy{logger: zap.NewNop()}
		app, proxySide, reader := heldPairOn(t, p, 1, "tcp4", "127.0.0.1:0")
		if _, err := app.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
		if err := proxySide.SetLinger(0); err != nil {
			t.Fatal(err)
		}
		_ = reader.Close()
		if _, err := app.Read(make([]byte, 1)); !errors.Is(err, unix.ECONNRESET) {
			t.Fatalf("the application's read after the handler's reset: %v, want ECONNRESET", err)
		}
		pc := pairedConnOf(t, p, 1)
		if pc.kept.Load() || !pc.lingerReset.Load() {
			t.Fatalf("kept %v, the handler's reset %v; want the reset, not kept", pc.kept.Load(), pc.lingerReset.Load())
		}
		ps, _ := pc.side()
		// A reset like any other: the application's socket gone when the
		// wait began, unknown; a byte it had not sent yet missing, unknown;
		// only its FIN missing, settled; a byte it had sent missing, a loss.
		if ok, err := p.pairReached(&pairTarget{pc: pc, gone: true}); ok || !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("the handler's reset, the application's socket gone = %v, %v; want unknown", ok, err)
		}
		if ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1, sent: ps.received}); ok || !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "not yet sent") {
			t.Fatalf("the handler's reset, a byte not sent yet missing = %v, %v; want unknown", ok, err)
		}
		if ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1, sent: ps.received + 1, finCounted: true}); !ok || err != nil {
			t.Fatalf("the handler's reset, only the FIN missing = %v, %v; want settled", ok, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged before any was lost", l)
		}
		ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1, sent: ps.received + 1})
		if !ok || err != nil {
			t.Fatalf("one byte sent and short, the handler's reset = %v, %v; want resolved as a loss", ok, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("%d app_bytes_lost charged, want 1", l)
		}
	})

	t.Run("a byte of data is a loss, and the application is reset", func(t *testing.T) {
		p, pc, app, ps := closedPair(t, 0)
		if _, err := app.Write([]byte("p")); err != nil {
			t.Fatal(err)
		}
		awaitEnd(t, pc)
		if end := pc.end.Load(); end.kind != afterData {
			t.Fatalf("the kept socket's read ended with %v (%v), want data", end.kind, end.err)
		}
		// The reset an orphan would have sent: the application's socket,
		// in CLOSE_WAIT, is closed by it (its writes fail from then on).
		local, remote := mustAddrPort(t, app.LocalAddr()), mustAddrPort(t, app.RemoteAddr())
		for {
			_, err := lookupTCP(local, remote)
			if errors.Is(err, errNoSocket) {
				break
			}
			if err != nil {
				t.Fatalf("the application's socket after its byte: %v", err)
			}
			runtime.Gosched()
		}
		if _, err := app.Write([]byte("p")); !errors.Is(err, unix.EPIPE) && !errors.Is(err, unix.ECONNRESET) {
			t.Fatalf("the application's write after the reset: %v, want EPIPE or ECONNRESET", err)
		}
		ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1})
		if !ok || err != nil {
			t.Fatalf("one byte short, the byte come after the close = %v, %v; want resolved as a loss", ok, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("%d app_bytes_lost charged, want 1: nothing captured the byte", l)
		}
	})

	t.Run("the data and the FIN are a loss", func(t *testing.T) {
		p, pc, app, ps := closedPair(t, 0)
		if _, err := app.Write([]byte("p")); err != nil {
			t.Fatal(err)
		}
		_ = app.Close()
		awaitEnd(t, pc)
		ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 2, finCounted: true})
		if !ok || err != nil {
			t.Fatalf("two short = %v, %v; want resolved as a loss", ok, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("%d app_bytes_lost charged, want 1", l)
		}
	})

	t.Run("bytes over TLS after the close are unknown", func(t *testing.T) {
		p, pc, app, ps := closedPair(t, 0)
		pc.tls.Store(true)
		if _, err := app.Write([]byte("p")); err != nil {
			t.Fatal(err)
		}
		awaitEnd(t, pc)
		ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1})
		if ok || !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "close_notify") {
			t.Fatalf("bytes after the close over TLS = %v, %v; want unknown: a close_notify, or a call", ok, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want none (unknown)", l)
		}
	})

	t.Run("keploy's stop ends the read, unknown", func(t *testing.T) {
		p := &Proxy{logger: zap.NewNop()}
		ctx, cancel := context.WithCancel(context.Background())
		pc, _, ps := closedPairOn(t, ctx, p, 1, 0)
		cancel()
		awaitEnd(t, pc)
		if end := pc.end.Load(); end.kind != afterStop {
			t.Fatalf("the kept socket's read ended with %v (%v), want keploy's stop", end.kind, end.err)
		}
		ok, err := p.pairReached(oneShort(pc, ps))
		if ok || !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "keploy stopped") {
			t.Fatalf("one short, the read ended by keploy's stop = %v, %v; want unknown, saying keploy stopped", ok, err)
		}
	})

	// Bytes that came as the close began, which the closing handler read
	// itself before the close (so not the kept socket's read): the side the
	// read ends with counts them past what the close began with.
	t.Run("bytes the closing handler read itself are unknown", func(t *testing.T) {
		pc := &pairedConn{app: netip.MustParseAddrPort("127.0.0.1:1")}
		pc.kept.Store(true)
		pc.end.Store(&afterClose{kind: afterFIN, side: proxySide{received: 10 + 3 + 1}})
		p := &Proxy{logger: zap.NewNop()}
		ok, err := p.pairAfterClose(&pairTarget{pc: pc, want: 11, sent: 11}, proxySide{received: 10})
		if ok || !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "3 bytes") {
			t.Fatalf("3 bytes past the close's start, then the FIN = %v, %v; want unknown, saying how many", ok, err)
		}
	})

	// The FIN came before the close began (the side the close began with
	// is past it): the read's end at it is nothing new.
	t.Run("a FIN the close began with is not counted again", func(t *testing.T) {
		pc := &pairedConn{app: netip.MustParseAddrPort("127.0.0.1:1")}
		pc.kept.Store(true)
		pc.end.Store(&afterClose{kind: afterFIN, side: proxySide{received: 11}})
		p := &Proxy{logger: zap.NewNop()}
		ok, err := p.pairAfterClose(&pairTarget{pc: pc, want: 12, sent: 12}, proxySide{received: 11, state: tcpCloseWait})
		if !ok || err != nil {
			t.Fatalf("the FIN in the close's side, nothing after = %v, %v; want settled", ok, err)
		}
	})

	t.Run("a close that could not keep its socket is unknown", func(t *testing.T) {
		pc := &pairedConn{app: netip.MustParseAddrPort("127.0.0.1:1")}
		p := &Proxy{logger: zap.NewNop()}
		pc.keepErr.Store(ptrTo("sock_diag: too many open files"))
		ok, err := p.pairAfterClose(&pairTarget{pc: pc, want: 11, sent: 11}, proxySide{received: 10})
		if ok || !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "could not keep") || !strings.Contains(err.Error(), "too many open files") {
			t.Fatalf("not kept = %v, %v; want unknown", ok, err)
		}
	})

	// A wait that begins once the handler is done with the connection
	// still finds it while its kept socket's read goes on, and so what comes
	// to it; the read's end forgets it.
	t.Run("a kept connection is forgotten once its handler and its read are done", func(t *testing.T) {
		p, pc, app, ps := closedPair(t, 0)
		p.paired.remove(pc.conn) // the handler is done with it
		if !p.paired.holds(pc.conn) {
			t.Fatal("the connection was forgotten while its kept socket's read went on")
		}
		if targets, err := p.pairTargets(1); err != nil || len(targets) != 1 {
			t.Fatalf("pairTargets = %v, %v; want the kept connection", targets, err)
		}
		_ = app.Close()
		awaitEnd(t, pc)
		if p.paired.holds(pc.conn) {
			t.Fatal("the connection is still held after its handler and its kept socket's read were done")
		}
		select {
		case <-pc.done:
		default:
			t.Fatal("the connection's done is still open after it was forgotten")
		}
		if ps, closed := pc.side(); !closed || ps.received != 10 {
			t.Fatalf("the forgotten connection's side = %+v, closed %v; want the close's", ps, closed)
		}
		_ = ps
	})
}

// A clean end the kernel's handling of orphans used to make unknown: the proxy
// closes after reading everything, the application reads its FIN and closes,
// and the wait runs. TCP_LINGER2 -1 on the proxy's socket has the kernel
// destroy an orphan as it reaches FIN_WAIT2, as a timewait budget used up
// does, or a reset that kills a TIME_WAIT once the application's socket is
// gone (tcp_rfc1337 off: replicated under load on .142): what the orphan
// took could not be read after, and the wait answered "cannot be told". The
// socket the proxy keeps open past its close is no orphan, and says the FIN
// came, and nothing else.
func TestACloseTheKernelWouldDestroyAsAnOrphanSettlesOnTheFIN(t *testing.T) {
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	rc, err := proxySide.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_LINGER2, -1) }); err != nil || serr != nil {
		t.Fatalf("TCP_LINGER2 -1 on the proxy's socket: %v %v", err, serr)
	}
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
	_ = app.Close()
	if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
		t.Fatalf("WaitPairedBytes: %v, want settled: every byte the application wrote, and its FIN, reached the proxy", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none", l)
	}
}

// A byte the application sent after the proxy's close, lost on its way, then
// the application's abort: its reset, sequenced past the byte, is not where
// the proxy's kept socket expects the next one, which answers with a challenge
// ACK (RFC 5961); the application's kernel, its socket gone, answers that with
// a reset at exactly the sequence number the kept socket expects, and the
// socket takes it. The byte was sent when the wait began and never arrived: a
// loss, not "the application's own reset". A filter on the proxy's socket
// drops every segment with a payload, as a loaded host's backlog drops one.
// A root test, as the other filter tests.
func TestAByteLostBeforeTheApplicationsResetIsALoss(t *testing.T) {
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	rc, err := proxySide.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	// Accept a segment only when its length is its TCP header's: no payload.
	noPayload := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_B | unix.BPF_ABS, K: 12},    // the data offset byte
		{Code: unix.BPF_ALU | unix.BPF_RSH | unix.BPF_K, K: 2},    // >> 2
		{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: 0x3c}, // & 0x3c: the header's length
		{Code: unix.BPF_MISC | unix.BPF_TAX},                      // X = it
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_LEN},           // A = the segment's length
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_X, Jt: 0, Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: 0xffffffff},
		{Code: unix.BPF_RET | unix.BPF_K, K: 0},
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptSockFprog(int(fd), unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &unix.SockFprog{Len: uint16(len(noPayload)), Filter: &noPayload[0]})
	}); err != nil {
		t.Fatal(err)
	}
	if errors.Is(serr, unix.EPERM) {
		t.Skipf("this kernel refuses an unprivileged socket filter on a TCP socket: %v", serr)
	}
	if serr != nil {
		t.Fatalf("attach the filter to the proxy's socket: %v", serr)
	}
	_ = reader.Close() // kept; its FIN reaches the application
	if _, err := app.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the application's read after the proxy's close: %v, want EOF", err)
	}
	if _, err := app.Write([]byte("p")); err != nil {
		t.Fatal(err)
	}
	local, remote := mustAddrPort(t, app.LocalAddr()), mustAddrPort(t, app.RemoteAddr())
	for { // the byte sent, and not acknowledged: dropped at the proxy
		d, err := lookupTCP(local, remote)
		if err != nil {
			t.Fatalf("the application's socket: %v", err)
		}
		if d.info != nil && d.wqueue == 1 && d.info.Notsent_bytes == 0 {
			break
		}
		runtime.Gosched()
	}
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 || targets[0].want != 11 || targets[0].sent != 11 {
		t.Fatalf("pairTargets = %+v, %v; want one connection, 11 bytes written and sent", targets, err)
	}
	if err := app.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = app.Close() // the abort
	pc := pairedConnOf(t, p, 1)
	awaitEnd(t, pc)
	if end := pc.end.Load(); end.kind != afterReset {
		t.Fatalf("the kept socket's read ended with %v (%v), want the reset", end.kind, end.err)
	}
	ok, err := p.pairReached(targets[0])
	if !ok || err != nil {
		t.Fatalf("a byte sent and lost before the reset = %v, %v; want resolved as a loss", ok, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
		t.Fatalf("%d app_bytes_lost charged, want 1: the byte the application sent never reached the proxy", l)
	}
}

// A sock_diag reply whose tcp_info stops before tcpi_notsent_bytes (a kernel
// before 4.6) carries no tcp_info the wait can read: what was sent would be
// read as all of it.
func TestATCPInfoTooShortForNotsentIsNone(t *testing.T) {
	var ti unix.TCPInfo
	short := int(unsafe.Offsetof(ti.Bytes_received) + unsafe.Sizeof(ti.Bytes_received))
	full := int(unsafe.Offsetof(ti.Notsent_bytes) + unsafe.Sizeof(ti.Notsent_bytes))
	for _, c := range []struct {
		n    int
		info bool
	}{{short, false}, {full - 1, false}, {full, true}} {
		attr := make([]byte, unix.SizeofRtAttr+c.n)
		binary.NativeEndian.PutUint16(attr[0:], uint16(len(attr)))
		binary.NativeEndian.PutUint16(attr[2:], diagInfo)
		msg := make([]byte, diagMsgLen)
		msg[1] = tcpEstablished
		d, err := parseDiagMsg(append(msg, attr...))
		if err != nil || (d.info != nil) != c.info {
			t.Errorf("a tcp_info of %d bytes: info %v, %v; want info %v", c.n, d.info != nil, err, c.info)
		}
	}
}

// appSent is what the application's socket had written less what it had not
// sent, and never less than what the proxy's side acknowledged.
func TestAppSentIsWhatWasWrittenLessWhatWasNotSent(t *testing.T) {
	for _, c := range []struct {
		want           uint64
		acked, notsent uint64
		sent           uint64
	}{
		{100, 41, 0, 100},
		{100, 41, 30, 70},
		{100, 41, 80, 41},  // a write between want's read and tcp_info's
		{100, 41, 120, 41}, // the same, past want
		{40, 41, 0, 40},    // never more than want
	} {
		d := diagTCP{info: &unix.TCPInfo{Bytes_acked: c.acked + 1, Notsent_bytes: uint32(c.notsent)}}
		if got := appSent(c.want, d); got != c.sent {
			t.Errorf("appSent(want %d, acked %d, notsent %d) = %d, want %d", c.want, c.acked, c.notsent, got, c.sent)
		}
	}
}

// A read blocked on a paired connection ends at its close, as it did when the
// close closed the socket: the socket kept open past it is not the handler's.
func TestAReadBlockedOnAKeptConnectionEndsAtItsClose(t *testing.T) {
	_, _, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	read := make(chan error, 1)
	go func() {
		_, err := reader.Read(make([]byte, 1))
		read <- err
	}()
	for reader.(*trackedConn).reading.Load() == 0 {
		runtime.Gosched()
	}
	_ = reader.Close()
	if err := <-read; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the blocked read after the close: %v, want %v", err, net.ErrClosed)
	}
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

// A wait takes its connections (upTo) before their handlers have all said
// which the proxy serves with no session: one the handler marks after the wait
// took it holds the wait no more than one marked before, since nothing on it
// is captured. The handler closes it unanswered with the application's bytes
// unread, and the close resets the application's socket, so the wait may read
// that socket open, read it gone after the close, or read it after the mark.
// None of these is unknown, a loss, or a wait. The wait reads the mark after
// the connection's side (pairReached): the handler marks it before closing it,
// so a side read closed carries a mark that is seen. The hooks lane's
// TestTheApplicationsConnectionIsWaitedForBeforeTheProxySeesIt lost this race
// about 1 run in 15: its wait read the socket gone, after the proxy's close,
// and ended unknown.
func TestAConnectionServedWithoutASessionAfterTheWaitTookItHoldsNothing(t *testing.T) {
	for _, read := range []string{"before the mark", "gone after the close", "after the mark"} {
		t.Run("the application's socket read "+read, func(t *testing.T) {
			p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
			if _, err := app.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			for received(t, proxySide) < 1 {
				runtime.Gosched()
			}
			pcs, err := p.paired.upTo(1) // the wait takes it: not yet marked
			if err != nil || len(pcs) != 1 {
				t.Fatalf("upTo = %v, %v; want the connection", pcs, err)
			}
			pc := pcs[0]
			ds, err := openDiag()
			if err != nil {
				t.Fatal(err)
			}
			defer ds.close()
			var targets []*pairTarget
			if read == "before the mark" {
				target, err := p.pairTarget(ds, pc)
				if err != nil || target == nil || target.gone {
					t.Fatalf("pairTarget = %+v, %v; want the open connection's", target, err)
				}
				targets = append(targets, target)
			}
			p.paired.servedWithoutSession(proxySide)
			_ = reader.Close() // unanswered: the byte unread, the close resets
			switch read {
			case "gone after the close":
				// pairTarget's read raced the mark: it found the socket gone.
				for {
					if _, _, err := appWritten(ds, pc.app, pc.proxy); errors.Is(err, errNoSocket) {
						break
					}
					runtime.Gosched()
				}
				targets = append(targets, &pairTarget{pc: pc, gone: true})
			case "after the mark":
				if target, err := p.pairTarget(ds, pc); err != nil || target != nil {
					t.Fatalf("pairTarget = %+v, %v; want nothing to wait for", target, err)
				}
			}
			if err := p.waitPairTargets(context.Background(), targets); err != nil {
				t.Fatalf("the wait: %v; want nothing held by a connection served without a session", err)
			}
			if l := p.losses.Snapshot(); len(l) != 0 {
				t.Fatalf("capture losses %+v, want none for a connection served without a session", l)
			}
		})
	}
}

// A connection the proxy passes through with no session stays open, and no
// read or close of its wakes a wait that took it before the mark: the mark
// itself does, and releases it.
func TestTheMarkOfAConnectionServedWithoutASessionReleasesAWaitOnIt(t *testing.T) {
	p, app, _, _ := heldPair(t, "tcp4", "127.0.0.1:0")
	// More than the proxy's side can receive before the proxy reads, which
	// it never does here: the wait cannot be reached by bytes.
	queue(t, app)
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 {
		t.Fatalf("pairTargets = %v, %v; want the connection", targets, err)
	}
	done := make(chan error, 1)
	go func() { done <- p.waitPairTargets(context.Background(), targets) }()
	p.paired.mu.Lock()
	var conn net.Conn
	for c := range p.paired.byConn {
		conn = c
	}
	p.paired.mu.Unlock()
	p.paired.servedWithoutSession(conn)
	// No deadline but the test binary's -timeout: without the wake the
	// wait never ends.
	if err := <-done; err != nil {
		t.Fatalf("the wait: %v; want released by the mark", err)
	}
	if l := p.losses.Snapshot(); len(l) != 0 {
		t.Fatalf("capture losses %+v, want none", l)
	}
}

// waitOn runs the bytes wait on targets in the background: its answer comes
// on the channel.
func waitOn(p *Proxy, targets ...*pairTarget) <-chan error {
	done := make(chan error, 1)
	go func() { done <- p.waitPairTargets(context.Background(), targets) }()
	return done
}

// awaitEnd waits for the end of the read of pc's socket kept open past its
// close (pairedConns.ended).
func awaitEnd(t *testing.T, pc *pairedConn) {
	t.Helper()
	if !pc.kept.Load() {
		t.Fatal("the proxy's socket was not kept past its close")
	}
	for pc.end.Load() == nil {
		runtime.Gosched()
	}
}

func ptrTo[T any](v T) *T { return &v }

// pairedConnOf is p's paired connection appSeq.
func pairedConnOf(t *testing.T, p *Proxy, appSeq uint64) *pairedConn {
	t.Helper()
	p.paired.mu.Lock()
	defer p.paired.mu.Unlock()
	for _, pc := range p.paired.byConn {
		if pc.appSeq == appSeq {
			return pc
		}
	}
	t.Fatalf("no paired connection %d", appSeq)
	return nil
}

// Close to the CI's shape (E-C6 12078), through WaitPairedBytes: the
// application read the proxy's FIN and closed, its socket in LAST_ACK, its FIN
// on its way to the proxy's closed side, which a drop-all filter on the
// proxy's socket holds off (a loaded host's softirq, a held packet). The wait
// reads the FIN counted in what was written and waits instead of answering at
// once; once the filter goes, the FIN reaches the socket the proxy kept open,
// its read ends, and the wait settles with no loss. The test keeps a
// descriptor of the socket of its own to take the filter off. A root test, as
// the other filter tests.
func TestAFINStillOnItsWayToAClosedConnectionIsWaitedFor(t *testing.T) {
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	rc, err := proxySide.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	dfd, serr := -1, error(nil)
	if err := rc.Control(func(fd uintptr) {
		drop := []unix.SockFilter{{Code: unix.BPF_RET | unix.BPF_K, K: 0}}
		if serr = unix.SetsockoptSockFprog(int(fd), unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &unix.SockFprog{Len: 1, Filter: &drop[0]}); serr == nil {
			dfd, serr = unix.Dup(int(fd))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if errors.Is(serr, unix.EPERM) {
		t.Skipf("this kernel refuses an unprivileged socket filter on a TCP socket: %v", serr)
	}
	if serr != nil {
		t.Fatalf("attach a drop-all filter to the proxy's socket, and dup it: %v", serr)
	}
	t.Cleanup(func() { _ = unix.Close(dfd) })
	_ = reader.Close() // the handler's close: the proxy's FIN goes out, its socket kept
	if _, err := app.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the application's read after the proxy's FIN: %v, want EOF", err)
	}
	local, remote := mustAddrPort(t, app.LocalAddr()), mustAddrPort(t, app.RemoteAddr())
	_ = app.Close() // its FIN, which the proxy's socket drops
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
	pc := pairedConnOf(t, p, 1)
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 || !targets[0].finCounted || targets[0].want != 11 {
		t.Fatalf("pairTargets = %v, %v; want one connection, its 10 bytes and its FIN written", targets, err)
	}
	if ok, err := p.pairReached(targets[0]); ok || err != nil {
		t.Fatalf("while the FIN is held = %v, %v; want waited on", ok, err)
	}
	if pc.end.Load() != nil {
		t.Fatalf("the kept socket's read ended (%v) while the filter held everything off", pc.end.Load().kind)
	}
	done := make(chan error, 1)
	go func() { done <- p.WaitPairedBytes(context.Background(), 1) }()
	if err := unix.SetsockoptInt(dfd, unix.SOL_SOCKET, unix.SO_DETACH_FILTER, 0); err != nil {
		t.Fatalf("detach the proxy's filter: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the wait = %v; want settled: the application's FIN reached the proxy's kept socket", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none", l)
	}
}
