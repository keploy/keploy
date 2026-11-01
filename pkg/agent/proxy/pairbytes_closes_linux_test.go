//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/stall"
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
		d, err := lookupTCPOrdered(remote, local)
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

// closeBegunNotDone begins the proxy's close of a pair held by a new Proxy,
// after it read the 10 bytes the application wrote, and leaves it short of the
// kernel: the close is read (atClose), the descriptor is marked closing, so
// the wait reads the proxy's side as closed, but the close(2) has not run. A
// reference a stalled goroutine holds is what holds it here, as one more step
// of Close's own on a loaded host does: an epoll removal, a thread not given a
// CPU. let lets the close finish.
func closeBegunNotDone(t *testing.T) (p *Proxy, pc *pairedConn, app *net.TCPConn, at proxySide, let func()) {
	t.Helper()
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	pc = pairedConnOf(t, p, 1)
	rc, err := proxySide.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	refDone := make(chan struct{})
	go func() {
		defer close(refDone)
		_ = rc.Read(func(uintptr) bool {
			close(held)
			<-release
			return true
		})
	}()
	<-held
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		_ = reader.Close()
	}()
	for !errors.Is(readProxySide(proxySide).err, net.ErrClosed) {
		runtime.Gosched()
	}
	var once sync.Once
	let = func() {
		once.Do(func() {
			close(release)
			<-refDone
			<-closeDone
		})
	}
	t.Cleanup(let)
	at, closed := pc.side()
	if !closed || at.state != tcpEstablished {
		t.Fatalf("the proxy's side as its close began: %+v closed %v, want the close's own read of an established socket", at, closed)
	}
	if d, err := lookupTCP(pc.proxy, pc.app); err != nil || d.state != tcpEstablished {
		t.Fatalf("the proxy's socket before its close reached the kernel: %+v %v, want it established", d, err)
	}
	return p, pc, app, at, let
}

// wroteBeforeTheClose makes pc's record of what the application had written
// as the proxy began closing it (noteWroteAtClose) n bytes past what its side
// had received then (at), its FIN the last of them when fin: what that read
// finds when they were written before the close and were still on their way,
// which a fixture on loopback cannot hold back on demand. Without it the
// record is the read's own, and bytes written after closeBegunNotDone came
// after the close began: no loss, and nothing to wait for
// (TestBytesTheApplicationWritesAfterTheProxysCloseBeganAreNoLoss).
func wroteBeforeTheClose(pc *pairedConn, at proxySide, n uint64, fin bool) {
	pc.wroteAtClose.Store(&appWrote{want: at.received + n, fin: fin})
}

// proxySocketIn waits until the proxy's socket of pc shows state.
func proxySocketIn(t *testing.T, pc *pairedConn, state uint8) {
	t.Helper()
	for {
		d, err := lookupTCP(pc.proxy, pc.app)
		if err != nil {
			t.Fatalf("the proxy's socket: %v", err)
		}
		if d.state == state {
			return
		}
		runtime.Gosched()
	}
}

// The proxy answered, began closing the connection, and the application's FIN
// came in before the close reached the kernel. What was written counts that
// FIN, which the close's own read of the proxy's side, made before it, does
// not, so the wait is one FIN short, and the socket it finds is no orphan: it
// took the FIN in sequence (CLOSE_WAIT), so nothing was refused. No test ends
// up charged with bytes nobody lost (a 502 Connection: close that the
// application closed on at once, under load: mock-pins-e2e's first-use row).
func TestAFINTakenBeforeTheProxysCloseReachedTheKernelIsNoLoss(t *testing.T) {
	p, pc, app, at, let := closeBegunNotDone(t)
	wroteBeforeTheClose(pc, at, 1, true)
	_ = app.Close()
	proxySocketIn(t, pc, tcpCloseWait)
	target := &pairTarget{pc: pc, want: at.received + 1, finCounted: true}
	reached, err := p.pairReached(target)
	if err != nil || !reached {
		t.Fatalf("pairReached = %v, %v, want settled: the proxy's socket took the application's FIN in sequence", reached, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none: every byte the application wrote, and its FIN, reached the proxy", l)
	}
	let()
}

// The same close, read only once it reached the kernel: the socket that took
// the FIN closed from CLOSE_WAIT to LAST_ACK and went as the application
// acknowledged its own FIN, so no orphan is left to tell. The application's
// socket, in TIME_WAIT, tells: its FIN was acknowledged, so nothing was refused.
func TestAnOrphanGoneAfterItTookTheFINIsToldByTheApplicationsSocket(t *testing.T) {
	p, pc, app, at, let := closeBegunNotDone(t)
	wroteBeforeTheClose(pc, at, 1, true)
	_ = app.Close()
	proxySocketIn(t, pc, tcpCloseWait)
	let()
	if gone := orphanIn(t, pc, 0); !gone {
		t.Fatal("the proxy's socket stayed after it took the application's FIN and closed")
	}
	target := &pairTarget{pc: pc, want: at.received + 1, finCounted: true}
	d, err := lookupTCPOrdered(pc.app, pc.proxy)
	if errors.Is(err, errNoSocket) {
		// TIME_WAIT takes a bucket from the network namespace's budget,
		// which a loaded runner's earlier connections can have used up:
		// with neither socket left nothing tells, and the wait says so.
		if _, err := p.pairReached(target); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("pairReached with neither socket left: %v, want unknown", err)
		}
		return
	}
	if err != nil || d.state != tcpTimeWait {
		t.Fatalf("the application's socket: %+v %v, want TIME_WAIT", d, err)
	}
	if reached, err := p.pairReached(target); err != nil || !reached {
		t.Fatalf("pairReached = %v, %v, want settled: the application's FIN was acknowledged", reached, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none", l)
	}
	// That FIN speaks for itself alone: bytes short of it stay a loss.
	wroteBeforeTheClose(pc, at, 2, true)
	target = &pairTarget{pc: pc, want: at.received + 2, finCounted: true}
	if reached, err := p.pairReached(target); err != nil || !reached {
		t.Fatalf("pairReached two short = %v, %v, want settled as a loss", reached, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
		t.Fatalf("two short: %d app_bytes_lost charged, want 1", l)
	}
}

// An orphan gone with the application's socket showing no acknowledged FIN
// says nothing: the kernel destroys one it has no room for as it destroys one
// that refused a byte. Only an acknowledged FIN speaks for the bytes before it.
func TestAnOrphanGoneWithTheApplicationsFINUnacknowledgedIsUnknown(t *testing.T) {
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
	pc := pairedConnOf(t, p, 1)
	ps, closed := pc.side()
	if !closed {
		t.Fatal("the proxy's side is not closed")
	}
	was := lookupOrphan
	lookupOrphan = func(local, remote netip.AddrPort) (diagTCP, error) {
		if local == pc.proxy {
			return diagTCP{}, errNoSocket
		}
		return was(local, remote)
	}
	t.Cleanup(func() { lookupOrphan = was })
	wroteBeforeTheClose(pc, ps, 1, true)
	_, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1, finCounted: true})
	if !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("pairReached: %v, want unknown: the application's socket, in CLOSE_WAIT, says nothing of its FIN", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none", l)
	}
}

// The same close with the application's FIN still on its way: the socket, not
// yet an orphan, still ESTABLISHED, will take it or refuse a byte in its turn.
// The wait waits, as it does for an orphan one FIN short, and settles when the
// FIN comes in. It charges nothing on the way.
func TestAFINOnItsWayToACloseNotYetInTheKernelIsWaitedFor(t *testing.T) {
	p, pc, app, at, let := closeBegunNotDone(t)
	wroteBeforeTheClose(pc, at, 1, true)
	target := &pairTarget{pc: pc, want: at.received + 1, finCounted: true}
	reached, err := p.pairReached(target)
	if err != nil || reached {
		t.Fatalf("pairReached = %v, %v, want a wait: the FIN is on its way", reached, err)
	}
	if target.orphan != tcpEstablished {
		t.Fatalf("the wait looks for state %d, want the socket's own, ESTABLISHED", target.orphan)
	}
	if got := orphansUnmoved([]*pairTarget{target}).Error(); !strings.Contains(got, "ESTABLISHED") {
		t.Fatalf("a wait that ends on it says %q, want the socket's state named", got)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged while the FIN was on its way, want none", l)
	}
	_ = app.Close()
	proxySocketIn(t, pc, tcpCloseWait)
	if reached, err := p.pairReached(target); err != nil || !reached {
		t.Fatalf("pairReached after the FIN = %v, %v, want settled", reached, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none", l)
	}
	let()
}

// A close that has not reached the kernel loses bytes all the same: nothing
// reads the descriptor again, so bytes written before the close began that
// come in now go with the close (a real loss of the application's request:
// it stays one). The
// FIN taken after them says nothing for them, one byte short with no FIN
// counted (a byte of data, the wait's count of what was written having no FIN
// in it) as two short with one: each is a loss.
func TestBytesThatMissedTheCloseAreALossBeforeItReachesTheKernel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		short      uint64
		finCounted bool
	}{
		{"two short with the FIN", 2, true},
		{"one short with no FIN counted", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, pc, app, at, let := closeBegunNotDone(t)
			wroteBeforeTheClose(pc, at, tc.short, tc.finCounted)
			if _, err := app.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			_ = app.Close()
			proxySocketIn(t, pc, tcpCloseWait)
			target := &pairTarget{pc: pc, want: at.received + tc.short, finCounted: tc.finCounted}
			if reached, err := p.pairReached(target); err != nil || !reached {
				t.Fatalf("pairReached = %v, %v, want settled as a loss", reached, err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
				t.Fatalf("%d app_bytes_lost charged, want 1: the byte went with the close", l)
			}
			let()
		})
	}
	// A byte on its way as the close began, and the FIN written after it:
	// what the close's read counts (no FIN) is what is judged, and the byte
	// short of it is data, a loss, whatever the FIN the wait reads later.
	t.Run("a byte short of the close's count, the FIN after it", func(t *testing.T) {
		p, pc, app, at, let := closeBegunNotDone(t)
		wroteBeforeTheClose(pc, at, 1, false)
		_ = app.Close()
		proxySocketIn(t, pc, tcpCloseWait)
		target := &pairTarget{pc: pc, want: at.received + 2, finCounted: true}
		if reached, err := p.pairReached(target); err != nil || !reached {
			t.Fatalf("pairReached = %v, %v, want settled as a loss", reached, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("%d app_bytes_lost charged, want 1: the byte written before the close never reached the proxy", l)
		}
		let()
	})
	// Bytes short with the socket still ESTABLISHED are no FIN on its way.
	t.Run("two short before anything came in", func(t *testing.T) {
		p, pc, _, at, let := closeBegunNotDone(t)
		wroteBeforeTheClose(pc, at, 2, true)
		target := &pairTarget{pc: pc, want: at.received + 2, finCounted: true}
		if reached, err := p.pairReached(target); err != nil || !reached {
			t.Fatalf("pairReached = %v, %v, want settled as a loss", reached, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("%d app_bytes_lost charged, want 1", l)
		}
		let()
	})
}

// pairReached on a connection the proxy closed one short of what the
// application wrote (what appWritten reads once the application has closed,
// its FIN not yet acknowledged: the bytes and the FIN), on the kernel's own
// sockets, with no socket filter: an orphan that took the FIN (TIME_WAIT)
// refused nothing and settles with no loss; one still waiting for a FIN
// (FIN_WAIT2) is waited on, as that FIN, or a byte it will refuse, is on its
// way: the wait settles once it takes the FIN, and answers unknown only once
// the agent could run for orphanFINBudget with nothing it waits on moving;
// one gone (the kernel destroyed it: it had no room for it, or it refused a
// byte) cannot tell the FIN from a refused byte, so the wait answers unknown;
// two short is a loss, whatever the orphan holds.
//
// The orphan's shape is the kernel's to choose. TIME_WAIT takes a timewait
// bucket from the network namespace's budget (tcp_max_tw_buckets), which a
// loaded runner's earlier connections can have used up: the kernel then
// destroys the orphan (TCPTimeWaitOverflow), and the test judges the shape it
// finds. The orphan that waits for a FIN is kept a full socket (TCP_LINGER2
// past TCP_TIMEWAIT_LEN), which takes no bucket; the gone one is made gone
// (TCP_LINGER2 -1: the kernel destroys it as it reaches FIN_WAIT2).
func TestAProxysCloseOneFINShortIsJudgedByItsOrphan(t *testing.T) {
	// closedPairWith closes the proxy's side of a pair held by p, as
	// application connection appSeq, after it read the 10 bytes the
	// application wrote, with closeIt, given the proxy's socket and its
	// tracked conn; linger2, when not 0, is the TCP_LINGER2 (seconds) of the
	// proxy's socket, set before the close.
	closedPairWith := func(t *testing.T, p *Proxy, appSeq uint64, linger2 int, closeIt func(t *testing.T, proxySide *net.TCPConn, reader net.Conn)) (*pairedConn, *net.TCPConn, proxySide) {
		t.Helper()
		app, proxySide, reader := heldPairOn(t, p, appSeq, "tcp4", "127.0.0.1:0")
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
		closeIt(t, proxySide, reader)
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
		// The rows judge the targets they give the wait, as written
		// before the close began: no record of the close's own read
		// caps them, as none does when that read fails
		// (noteWroteAtClose).
		pc.wroteAtClose.Store(nil)
		return pc, app, ps
	}
	// closedPairOn is closedPairWith closing the proxy's side as the proxy
	// does: its tracked conn's close.
	closedPairOn := func(t *testing.T, p *Proxy, appSeq uint64, linger2 int) (*pairedConn, *net.TCPConn, proxySide) {
		t.Helper()
		return closedPairWith(t, p, appSeq, linger2, func(_ *testing.T, _ *net.TCPConn, reader net.Conn) { _ = reader.Close() })
	}
	// closedPair is closedPairOn on a proxy of its own.
	closedPair := func(t *testing.T, linger2 int) (*Proxy, *pairedConn, *net.TCPConn, proxySide) {
		t.Helper()
		p := &Proxy{logger: zap.NewNop()}
		pc, app, ps := closedPairOn(t, p, 1, linger2)
		return p, pc, app, ps
	}
	// closedPairInTimewait is closedPair with the proxy's orphan a timewait
	// socket in FIN_WAIT2 as its tracked conn's close returns, or gone with no
	// timewait bucket left (closeIntoTimewait): moved on no timer, so a wait
	// that needs it there never waits on the kernel's fin timer.
	closedPairInTimewait := func(t *testing.T) (*Proxy, *pairedConn, *net.TCPConn, proxySide) {
		t.Helper()
		p := &Proxy{logger: zap.NewNop()}
		pc, app, ps := closedPairWith(t, p, 1, 0, func(t *testing.T, proxySide *net.TCPConn, reader net.Conn) {
			closeIntoTimewait(t, proxySide, reader.Close)
		})
		return p, pc, app, ps
	}
	// oneShortUnknown: one short, the FIN counted, is unknown, and charges
	// nothing.
	oneShortUnknown := func(t *testing.T, p *Proxy, pc *pairedConn, ps proxySide, why string) {
		t.Helper()
		ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1, finCounted: true})
		if ok || !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("one short = %v, %v; want unknown (%v): %s", ok, err, models.ErrProxyConnsUnsupported, why)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("one short: %d app_bytes_lost charged, want none (unknown)", l)
		}
	}
	// twoShortLoss: two short, the FIN counted, is a loss, charged once.
	twoShortLoss := func(t *testing.T, p *Proxy, pc *pairedConn, ps proxySide) {
		t.Helper()
		ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 2, finCounted: true})
		if !ok || err != nil {
			t.Fatalf("two short = %v, %v; want resolved as a loss", ok, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
			t.Fatalf("two short: %d app_bytes_lost charged, want 1: at least one byte the application wrote will be refused", l)
		}
	}

	t.Run("its orphan took the FIN", func(t *testing.T) {
		p, pc, app, ps := closedPair(t, 0)
		_ = app.Close()
		gone := orphanIn(t, pc, tcpTimeWait)
		tg := &pairTarget{pc: pc, want: ps.received + 1, finCounted: true}
		ok, err := p.pairReached(tg)
		if gone {
			// Read again, after the wait's answer (goneStill). Gone
			// still: no timewait bucket was left for it, the kernel
			// destroyed it, and a gone orphan tells the FIN from a refused
			// byte no more than any other gone one does.
			d, rerr := lookupTCPOrdered(pc.proxy, pc.app)
			if errors.Is(rerr, errNoSocket) {
				if ok || !errors.Is(err, models.ErrProxyConnsUnsupported) {
					t.Fatalf("one short = %v, %v; want unknown (%v): the orphan is gone, which says nothing of the FIN", ok, err, models.ErrProxyConnsUnsupported)
				}
				if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
					t.Fatalf("one short: %d app_bytes_lost charged, want none (unknown)", l)
				}
				return
			}
			if rerr != nil || (d.state != tcpFinWait2 && d.state != tcpTimeWait) {
				t.Fatalf("the proxy's orphaned socket, read again after the wait = state %d, %v; want FIN_WAIT2 or TIME_WAIT, or gone as orphanIn read it", d.state, rerr)
			}
			// Found: before 6.11 orphanIn read it in the step of its move
			// to the timewait table, and the wait, which read after, found
			// it there. The orphan moves as it is acknowledged, so the FIN
			// can still be on its way: the wait then waits on it, and is
			// asked again once the orphan has it.
			if !ok && err == nil && tg.orphan == tcpFinWait2 {
				t.Log("the wait waited on the orphan in FIN_WAIT2, the FIN still on its way: asked again once the orphan took it")
				if orphanIn(t, pc, tcpTimeWait) {
					t.Fatal("the proxy's orphaned socket, a timewait socket in FIN_WAIT2, went as it took the FIN; want TIME_WAIT")
				}
				ok, err = p.pairReached(tg)
			}
		}
		if !ok || err != nil {
			t.Fatalf("pairReached = %v, %v; want settled", ok, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want none: the orphan took the FIN, so it refused nothing", l)
		}
	})

	t.Run("its orphan still waits for a FIN", func(t *testing.T) {
		p, pc, _, ps := closedPair(t, 120)
		// A full socket: gone only if the kernel's orphan or memory
		// limits took it at the close, which is judged as gone.
		if orphanIn(t, pc, tcpFinWait2) {
			t.Log("the kernel destroyed the orphan at its close (its orphan or memory limits): judged gone, not in FIN_WAIT2")
			oneShortUnknown(t, p, pc, ps, "an orphan the kernel had no room for is gone as one that refused a byte is")
			twoShortLoss(t, p, pc, ps)
			return
		}
		tg := &pairTarget{pc: pc, want: ps.received + 1, finCounted: true}
		ok, err := p.pairReached(tg)
		if ok || err != nil || tg.orphan != tcpFinWait2 {
			t.Fatalf("one short = %v, %v, orphan %d; want waited on (not reached, no error), its orphan in FIN_WAIT2: the FIN, or a byte the orphan will refuse, is still to come, and the orphan will say which", ok, err, tg.orphan)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("one short: %d app_bytes_lost charged, want none", l)
		}
		twoShortLoss(t, p, pc, ps)
	})

	// An exact lookup made as the orphan moves to the timewait table (as it
	// is acknowledged in FIN_WAIT1, or takes the FIN) can miss it, or find
	// it closed, while it is there: neither is the orphan gone, nor a byte
	// refused. The wait reads it again, ordered against the move.
	for _, m := range []struct {
		name string
		s    diagTCP
		err  error
	}{{"missed", diagTCP{}, errNoSocket}, {"found closed", diagTCP{state: tcpClose}, nil}} {
		t.Run("its orphan took the FIN, an exact lookup "+m.name+" as it moved", func(t *testing.T) {
			p, pc, app, ps := closedPair(t, 0)
			_ = app.Close()
			if orphanIn(t, pc, tcpTimeWait) {
				t.Skip(orphanGoneAtItsMove)
			}
			asked := exactLookupSays(t, m.s, m.err)
			ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1, finCounted: true})
			if asked() == 0 {
				t.Fatal("the wait read the orphan without the exact lookup this row makes miss: its read is not the ordered one (lookupOrphan)")
			}
			if !ok || err != nil {
				t.Fatalf("pairReached = %v, %v; want settled: the orphan took the FIN, in TIME_WAIT", ok, err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
				t.Fatalf("%d app_bytes_lost charged, want none: the orphan took the FIN, so it refused nothing", l)
			}
		})
		t.Run("its orphan still waits for a FIN, an exact lookup "+m.name+" as it moved", func(t *testing.T) {
			// The orphan is a timewait socket in FIN_WAIT2, moved there by
			// the proxy's close, where the wait's ordered read must find
			// it.
			p, pc, _, ps := closedPairInTimewait(t)
			if orphanInTimewait(t, pc, tcpFinWait2) {
				t.Skip(noTimewaitBucket)
			}
			asked := exactLookupSays(t, m.s, m.err)
			tg := &pairTarget{pc: pc, want: ps.received + 1, finCounted: true}
			ok, err := p.pairReached(tg)
			if asked() == 0 {
				t.Fatal("the wait read the orphan without the exact lookup this row makes miss: its read is not the ordered one (lookupOrphan)")
			}
			if ok || err != nil || tg.orphan != tcpFinWait2 {
				t.Fatalf("one short = %v, %v, orphan %d; want waited on (not reached, no error), its orphan in FIN_WAIT2", ok, err, tg.orphan)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
				t.Fatalf("%d app_bytes_lost charged, want none", l)
			}
		})
	}

	t.Run("its orphan unreadable, one short is unknown", func(t *testing.T) {
		p, pc, _, ps := closedPair(t, 120)
		was := lookupOrphan
		lookupOrphan = func(netip.AddrPort, netip.AddrPort) (diagTCP, error) {
			return diagTCP{}, fmt.Errorf("sock_diag socket: %w", unix.EMFILE)
		}
		t.Cleanup(func() { lookupOrphan = was })
		ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1, finCounted: true})
		if ok || !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "cannot be read") {
			t.Fatalf("one short, the orphan unreadable = %v, %v; want unknown: the orphan alone tells the FIN from a refused byte", ok, err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want none (unknown)", l)
		}
		twoShortLoss(t, p, pc, ps)
	})

	// The wait on an orphan that waits for the application's FIN: the FIN
	// is in flight (here, not yet sent) when the wait looks first, as on a
	// loaded host whose softirq has not delivered it (E-C6 12078). The wait
	// runs on a clock the test moves (stall.ManualClock): Pass is wall time
	// in which the agent could not run, Advance time it could.
	t.Run("the wait settles once its orphan takes the FIN", func(t *testing.T) {
		p, pc, app, ps := closedPair(t, 120)
		clk := &stall.ManualClock{}
		p.paired.clk = clk
		if orphanIn(t, pc, tcpFinWait2) {
			t.Log("the kernel destroyed the orphan at its close (its orphan or memory limits): judged gone")
			oneShortUnknown(t, p, pc, ps, "an orphan the kernel had no room for is gone as one that refused a byte is")
			return
		}
		done := waitOn(p, &pairTarget{pc: pc, want: ps.received + 1, finCounted: true})
		awaitLook(t, clk, done, "before the application's FIN, still to come, could reach the orphan")
		// More looks than a budget of wall time takes, at their slowest:
		// a starved agent, for which wall time passes and it cannot run.
		for i := 0; i < 2*int(orphanFINBudget/stall.CheckEvery(orphanFINBudget))+10; i++ {
			clk.Pass()
			awaitLook(t, clk, done, "while the agent was starved")
		}
		clk.Advance(orphanFINBudget - stall.Tick)
		awaitLook(t, clk, done, "short of its budget")
		_ = app.Close() // the FIN
		gone := orphanIn(t, pc, tcpTimeWait)
		clk.Pass()
		err := <-done
		if gone && goneStill(t, pc) {
			// No timewait bucket was left for it (TCPTimeWaitOverflow).
			if !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "is gone") {
				t.Fatalf("the wait = %v; want unknown: the kernel destroyed the orphan as it took the FIN, which says nothing of the FIN", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("the wait = %v; want settled: the orphan took the FIN, so it refused nothing", err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want none", l)
		}
	})

	t.Run("the wait answers unknown once it could run its budget with nothing moving", func(t *testing.T) {
		p, pc, _, ps := closedPair(t, 120)
		clk := &stall.ManualClock{}
		p.paired.clk = clk
		if orphanIn(t, pc, tcpFinWait2) {
			t.Log("the kernel destroyed the orphan at its close (its orphan or memory limits): judged gone")
			oneShortUnknown(t, p, pc, ps, "an orphan the kernel had no room for is gone as one that refused a byte is")
			return
		}
		done := waitOn(p, &pairTarget{pc: pc, want: ps.received + 1, finCounted: true})
		awaitLook(t, clk, done, "before the application's FIN could reach the orphan")
		clk.Advance(orphanFINBudget - stall.Tick)
		awaitLook(t, clk, done, "short of its budget")
		clk.Advance(stall.Tick)
		err := <-done
		if !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("the wait = %v; want unknown (%v): the orphan took no FIN while the agent could run its budget", err, models.ErrProxyConnsUnsupported)
		}
		for _, want := range []string{pc.app.String(), "FIN_WAIT2", "waited for the application's FIN", orphanFINBudget.String()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the wait's answer %q does not say %q: it must say what it waited on", err, want)
			}
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want none (unknown)", l)
		}
	})

	// What else the wait waits on moving restarts its budget: another
	// connection's bytes arriving, another orphan taking its FIN.
	t.Run("another connection's bytes restart the budget", func(t *testing.T) {
		p, pc, _, ps := closedPair(t, 120)
		clk := &stall.ManualClock{}
		p.paired.clk = clk
		if orphanIn(t, pc, tcpFinWait2) {
			t.Log("the kernel destroyed the orphan at its close (its orphan or memory limits): judged gone")
			oneShortUnknown(t, p, pc, ps, "an orphan the kernel had no room for is gone as one that refused a byte is")
			return
		}
		// A pooled connection of the same wait whose bytes are still
		// arriving: the wait wants 20, 10 are in.
		app2, side2, _ := heldPairOn(t, p, 2, "tcp4", "127.0.0.1:0")
		if _, err := app2.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		for received(t, side2) < 10 {
			runtime.Gosched()
		}
		done := waitOn(p, &pairTarget{pc: pc, want: ps.received + 1, finCounted: true}, &pairTarget{pc: pairedConnOf(t, p, 2), want: 20})
		awaitLook(t, clk, done, "before the application's FIN could reach the orphan")
		clk.Advance(orphanFINBudget - stall.Tick)
		awaitLook(t, clk, done, "short of its budget")
		// Five more arrive, unread: the wait moved, and its budget starts
		// again here.
		if _, err := app2.Write([]byte("pub 2")); err != nil {
			t.Fatal(err)
		}
		for received(t, side2) < 15 {
			runtime.Gosched()
		}
		clk.Pass()
		awaitLook(t, clk, done, "as the other connection's bytes arrived")
		clk.Advance(orphanFINBudget - stall.Tick)
		awaitLook(t, clk, done, "short of the budget that began at the other connection's bytes")
		clk.Advance(stall.Tick)
		if err := <-done; !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), pc.app.String()) {
			t.Fatalf("the wait = %v; want unknown, for the orphan of the connection from %s", err, pc.app)
		}
	})

	t.Run("another orphan's FIN restarts the budget", func(t *testing.T) {
		p := &Proxy{logger: zap.NewNop()}
		clk := &stall.ManualClock{}
		p.paired.clk = clk
		pc, _, ps := closedPairOn(t, p, 1, 120)
		pc2, app2, ps2 := closedPairOn(t, p, 2, 120)
		if orphanIn(t, pc, tcpFinWait2) || orphanIn(t, pc2, tcpFinWait2) {
			t.Skip("the kernel destroyed an orphan at its close (its orphan or memory limits): no orphan waits for a FIN")
		}
		done := waitOn(p, &pairTarget{pc: pc, want: ps.received + 1, finCounted: true}, &pairTarget{pc: pc2, want: ps2.received + 1, finCounted: true})
		awaitLook(t, clk, done, "before the applications' FINs could reach the orphans")
		clk.Advance(orphanFINBudget - stall.Tick)
		awaitLook(t, clk, done, "short of its budget")
		_ = app2.Close() // the second orphan's FIN
		gone := orphanIn(t, pc2, tcpTimeWait)
		clk.Pass()
		// The second orphan is a full socket in FIN_WAIT2 (its TCP_LINGER2
		// is over the timewait length), and it moves to the timewait table
		// as it takes the FIN: the wait's look finds it in TIME_WAIT, or
		// gone. Before 6.11, orphanIn can read it gone in the step of that
		// move, and the wait, which looks after, finds it there: the wait
		// looking again, having answered nothing, says the orphan was
		// there.
		if answered, err := answerOrLook(clk, done); answered {
			if !gone {
				t.Fatalf("the wait answered (%v) as the second orphan took its FIN", err)
			}
			if !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "is gone") {
				t.Fatalf("the wait = %v; want unknown: the kernel destroyed the second orphan as it took the FIN (no timewait bucket)", err)
			}
			return
		}
		clk.Advance(orphanFINBudget - stall.Tick)
		awaitLook(t, clk, done, "short of the budget that began at the second orphan's FIN")
		clk.Advance(stall.Tick)
		err := <-done
		if !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), pc.app.String()) || strings.Contains(err.Error(), pc2.app.String()+" (") {
			t.Fatalf("the wait = %v; want unknown, for the first orphan (%s) alone", err, pc.app)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("%d app_bytes_lost charged, want none", l)
		}
	})

	// One short with the FIN not counted (the application has not closed)
	// is a byte of data, which the orphan refuses: a loss, in FIN_WAIT2 or
	// gone alike.
	for _, c := range []struct {
		name    string
		linger2 int
	}{{"one byte short, its orphan waiting for a FIN", 120}, {"one byte short, its orphan gone", -1}} {
		t.Run(c.name, func(t *testing.T) {
			p, pc, _, ps := closedPair(t, c.linger2)
			orphanIn(t, pc, tcpFinWait2) // in FIN_WAIT2, or gone
			ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1})
			if !ok || err != nil {
				t.Fatalf("one byte short = %v, %v; want resolved as a loss", ok, err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
				t.Fatalf("one byte short: %d app_bytes_lost charged, want 1: the orphan refuses the byte", l)
			}
		})
	}

	t.Run("its orphan is gone", func(t *testing.T) {
		p, pc, _, ps := closedPair(t, -1)
		orphanIn(t, pc, 0) // the kernel destroys it as it reaches FIN_WAIT2
		oneShortUnknown(t, p, pc, ps, "an orphan the kernel had no room for is gone as one that refused a byte is")
		twoShortLoss(t, p, pc, ps)
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
					if _, _, _, err := appWritten(ds, pc.app, pc.proxy); errors.Is(err, errNoSocket) {
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

// awaitLook returns once the wait has armed a look of its own on clk: it
// waits on, having answered nothing. An answer first fails the test (why
// says when it came).
func awaitLook(t *testing.T, clk *stall.ManualClock, done <-chan error, why string) {
	t.Helper()
	for clk.Pending() == 0 {
		select {
		case err := <-done:
			t.Fatalf("the wait answered (%v) %s", err, why)
		default:
			runtime.Gosched()
		}
	}
}

// answerOrLook waits until the wait answers, and returns true and its
// answer, or until it waits on clk again, having looked, and returns false.
func answerOrLook(clk *stall.ManualClock, done <-chan error) (bool, error) {
	for clk.Pending() == 0 {
		select {
		case err := <-done:
			return true, err
		default:
			runtime.Gosched()
		}
	}
	return false, nil
}

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

// orphanIn waits until the proxy's orphaned socket of pc is in state, and
// reports false, or until the kernel has destroyed it, and reports true. With
// state 0 it waits for the latter only. It reads the orphan as the wait does
// (lookupOrdered): a lookup that misses it as it moves to the timewait table
// is not it gone.
func orphanIn(t *testing.T, pc *pairedConn, state uint8) (gone bool) {
	t.Helper()
	return orphanUntil(t, pc, func(d diagTCP) bool { return state != 0 && d.state == state })
}

// orphanInTimewait is orphanIn for the orphan as a timewait socket in state,
// one with no tcp_info (timewaitIn): a read of the full socket in FIN_WAIT2,
// made before the kernel ran the close that moves it (closedPairInTimewait),
// is not it.
func orphanInTimewait(t *testing.T, pc *pairedConn, state uint8) (gone bool) {
	t.Helper()
	return orphanUntil(t, pc, func(d diagTCP) bool { return d.state == state && d.info == nil })
}

// orphanUntil is orphanIn with the orphan's read matched by want.
func orphanUntil(t *testing.T, pc *pairedConn, want func(diagTCP) bool) (gone bool) {
	t.Helper()
	for {
		d, err := lookupTCPOrdered(pc.proxy, pc.app)
		if errors.Is(err, errNoSocket) {
			return true
		}
		if err != nil {
			t.Fatalf("the proxy's orphaned socket: %v", err)
		}
		if want(d) {
			return false
		}
		runtime.Gosched()
	}
}

// goneStill reads the proxy's orphaned socket of pc once more, after the
// wait has answered, when orphanIn found it gone, and reports whether it is
// gone still. Before 6.11 the kernel let go of a chain's lock a step before
// a timewait socket in it could be taken, and a read in that step, ordered
// or not, misses it (lookupOrdered). A socket the kernel destroyed never
// comes back, and a timewait socket stays for 60 s, so a read after the
// wait's answer (the wait read after orphanIn) tells which orphanIn's miss
// was: the timewait socket found, the orphan took the FIN, and the wait must
// have settled; gone again, the kernel destroyed it. (Only a step that lasts
// from orphanIn's read through the wait's own read, which then answers
// unknown, as the wait does in it, fails here.)
func goneStill(t *testing.T, pc *pairedConn) bool {
	t.Helper()
	d, err := lookupTCPOrdered(pc.proxy, pc.app)
	if errors.Is(err, errNoSocket) {
		return true
	}
	if err != nil || d.state != tcpTimeWait {
		t.Fatalf("the proxy's orphaned socket, read again after the wait = state %d, %v; want TIME_WAIT, or gone as orphanIn read it", d.state, err)
	}
	return false
}

// An orphan's own move is the wait moving: the proxy's orphan in FIN_WAIT1
// (its FIN not acknowledged: a drop-all filter on the application's socket
// holds it off) that reaches FIN_WAIT2 once the filter is gone restarts the
// wait's budget, and the wait then settles on the FIN it takes. A root test:
// Linux 7 refuses an unprivileged filter on a TCP socket, and CI's root step
// runs it, failing on a skip.
func TestAnOrphansMoveRestartsTheBytesWaitsBudget(t *testing.T) {
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	clk := &stall.ManualClock{}
	p.paired.clk = clk
	rc, err := proxySide.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var serr error
	// A full socket in FIN_WAIT2 (TCP_LINGER2 past TCP_TIMEWAIT_LEN): no
	// timewait bucket.
	if err := rc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_LINGER2, 120) }); err != nil || serr != nil {
		t.Fatalf("TCP_LINGER2 on the proxy's socket: %v %v", err, serr)
	}
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	arc, err := app.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := arc.Control(func(fd uintptr) {
		drop := []unix.SockFilter{{Code: unix.BPF_RET | unix.BPF_K, K: 0}}
		serr = unix.SetsockoptSockFprog(int(fd), unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &unix.SockFprog{Len: 1, Filter: &drop[0]})
	}); err != nil {
		t.Fatal(err)
	}
	if errors.Is(serr, unix.EPERM) {
		t.Skipf("this kernel refuses an unprivileged socket filter on a TCP socket: %v", serr)
	}
	if serr != nil {
		t.Fatalf("attach a drop-all filter to the application's socket: %v", serr)
	}
	_ = reader.Close() // its FIN, which the application's socket drops
	pc := pairedConnOf(t, p, 1)
	ps, closed := pc.side()
	if !closed || ps.err != nil || ps.received != 10 {
		t.Fatalf("the proxy's side: closed %v, received %d (%v), want closed after 10", closed, ps.received, ps.err)
	}
	if orphanIn(t, pc, tcpFinWait1) {
		// Its orphan or memory limits took it at the close: judged gone.
		ok, err := p.pairReached(&pairTarget{pc: pc, want: ps.received + 1, finCounted: true})
		if ok || !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "is gone") {
			t.Fatalf("one short, the orphan gone = %v, %v; want unknown", ok, err)
		}
		return
	}
	done := waitOn(p, &pairTarget{pc: pc, want: ps.received + 1, finCounted: true})
	awaitLook(t, clk, done, "before the application's FIN could reach the orphan")
	clk.Advance(orphanFINBudget - stall.Tick)
	awaitLook(t, clk, done, "short of its budget")
	before := p.PairedBytesProgress()
	// The filter goes: the orphan's FIN, sent again, is taken and
	// acknowledged, and the orphan moves to FIN_WAIT2.
	if err := arc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_DETACH_FILTER, 0) }); err != nil || serr != nil {
		t.Fatalf("detach the application's filter: %v %v", err, serr)
	}
	gone := orphanIn(t, pc, tcpFinWait2)
	clk.Pass()
	if gone {
		// Its orphan or memory limits took it as it moved: judged gone. Or,
		// before 6.11, orphanIn read it in the step after its chain's unlock
		// (goneStill), and the wait, which reads after, finds it in
		// FIN_WAIT2 and looks again: the orphan was not gone.
		if answered, err := answerOrLook(clk, done); answered {
			if !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "is gone") {
				t.Fatalf("the wait = %v; want unknown: the kernel destroyed the orphan", err)
			}
			return
		}
	}
	awaitLook(t, clk, done, "as the orphan moved to FIN_WAIT2")
	if p.PairedBytesProgress() == before {
		t.Error("PairedBytesProgress did not move with the orphan: the agent's watchdog would see the wait stalled")
	}
	clk.Advance(orphanFINBudget - stall.Tick)
	awaitLook(t, clk, done, "short of the budget that began at the orphan's move")
	_ = app.Close() // the FIN
	gone = orphanIn(t, pc, tcpTimeWait)
	clk.Pass()
	err = <-done
	if gone && goneStill(t, pc) {
		if !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "is gone") {
			t.Fatalf("the wait = %v; want unknown: the kernel destroyed the orphan as it took the FIN (no timewait bucket)", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("the wait = %v; want settled: the orphan took the FIN", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none", l)
	}
}

// Close to the CI's shape (E-C6 12078), through WaitPairedBytes: the
// application read the proxy's FIN and closed, its socket in LAST_ACK, its FIN
// on its way to the proxy's closed side, which a drop-all filter on the
// proxy's socket holds off. The wait reads the FIN counted in what was
// written, and waits on the proxy's socket instead of answering at once; once
// the filter goes, the FIN is taken and the wait settles with no loss. The
// handler's close is the proxy's (read first, as the accept loop's), but a dup
// of the socket's descriptor keeps it open so the filter can be taken off, and
// the proxy's FIN is sent by shutting its write side. So the proxy's socket is
// a full one in FIN_WAIT1 (the filter drops the application's ACK too), not
// the timewait socket in FIN_WAIT2 that a real orphan most likely is; the wait
// reads only its state, which is the same. A root test, as the other filter
// tests.
func TestAFINStillOnItsWayToAClosedConnectionIsWaitedFor(t *testing.T) {
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	clk := &stall.ManualClock{}
	p.paired.clk = clk
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
	_ = reader.Close() // the handler's close, read first
	if err := unix.Shutdown(dfd, unix.SHUT_WR); err != nil {
		t.Fatalf("the proxy's FIN: %v", err)
	}
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
	done := make(chan error, 1)
	go func() { done <- p.WaitPairedBytes(context.Background(), 1) }()
	awaitLook(t, clk, done, "while the application's FIN was still on its way")
	clk.Advance(orphanFINBudget - stall.Tick)
	awaitLook(t, clk, done, "short of its budget")
	if err := unix.SetsockoptInt(dfd, unix.SOL_SOCKET, unix.SO_DETACH_FILTER, 0); err != nil {
		t.Fatalf("detach the proxy's filter: %v", err)
	}
	// The FIN, sent again, is taken. The kernel moves the proxy's socket to
	// its timewait socket (or, with no timewait bucket left, destroys it),
	// and only then closes the socket the dup holds: once that reads CLOSE,
	// the move is done, and a read of the pair finds the timewait socket or
	// nothing (before 6.11 even an ordered read could miss a timewait
	// socket in the step after its chain's unlock, see lookupOrdered).
	for {
		info, err := unix.GetsockoptTCPInfo(dfd, unix.IPPROTO_TCP, unix.TCP_INFO)
		if err != nil {
			t.Fatalf("the proxy's socket after the filter went: %v", err)
		}
		if info.State == tcpClose {
			break
		}
		runtime.Gosched()
	}
	gone := orphanIn(t, pc, tcpTimeWait)
	clk.Pass()
	err = <-done
	if gone {
		if !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "is gone") {
			t.Fatalf("the wait = %v; want unknown: the kernel destroyed the proxy's socket as it took the FIN (no timewait bucket)", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("the wait = %v; want settled: the application's FIN reached the proxy's closed side", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none", l)
	}
}

// Bytes the application writes once the proxy has begun closing the
// connection are no capture loss: keploy's close, not a capture it missed,
// ended the connection under them, as a server's close would. The close's own
// read of the application's socket (noteWroteAtClose) bounds what the wait
// waits for, so a byte that comes in before the close reaches the kernel, and
// goes with it, is none; nor is the reset the close answers it with, which
// leaves the application's socket gone when a later wait begins.
func TestBytesTheApplicationWritesAfterTheProxysCloseBeganAreNoLoss(t *testing.T) {
	p, pc, app, at, let := closeBegunNotDone(t)
	if w := pc.wroteAtClose.Load(); w == nil || w.want != at.received || w.fin {
		t.Fatalf("the close's read of the application's socket: %+v, want the %d bytes written before it and no FIN", w, at.received)
	}
	if _, err := app.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	proxySocketIn(t, pc, tcpCloseWait)
	// What a wait reads of the application's socket now: the byte and the FIN.
	target := &pairTarget{pc: pc, want: at.received + 2, finCounted: true}
	if reached, err := p.pairReached(target); err != nil || !reached {
		t.Fatalf("pairReached = %v, %v, want settled: nothing written before the close was missed", reached, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged for a byte written after the proxy's close began, want none", l)
	}
	let() // the close tosses the byte unread: a reset to the application
	for {
		if _, err := lookupTCP(pc.app, pc.proxy); errors.Is(err, errNoSocket) {
			break
		}
		runtime.Gosched()
	}
	if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
		t.Fatalf("WaitPairedBytes with the application's socket reset after the close: %v, want settled: the reset refused only a byte written after the close", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none", l)
	}
}

// The close-gap of TestAFINTakenBeforeTheProxysCloseReachedTheKernelIsNoLoss
// with the application's half-close (shutdown of its write side) before the
// close began: its socket stays a full socket in FIN_WAIT2 once the FIN is
// acknowledged, and what it wrote counts that FIN. The wait reads it through
// the product's pairTarget, which counts a FIN there as it does in FIN_WAIT1.
func TestAHalfCloseAcknowledgedBeforeTheProxysCloseReachedTheKernelIsNoLoss(t *testing.T) {
	p, pc, app, at, let := closeBegunNotDone(t)
	wroteBeforeTheClose(pc, at, 1, true)
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	proxySocketIn(t, pc, tcpCloseWait)
	for {
		d, err := lookupTCP(pc.app, pc.proxy)
		if err != nil {
			t.Fatalf("the application's socket: %v", err)
		}
		if d.state == tcpFinWait2 && d.info != nil {
			break
		}
		runtime.Gosched()
	}
	ds, err := openDiag()
	if err != nil {
		t.Fatal(err)
	}
	defer ds.close()
	target, err := p.pairTarget(ds, pc)
	if err != nil || target == nil {
		t.Fatalf("pairTarget = %+v, %v", target, err)
	}
	if target.want != at.received+1 || !target.finCounted {
		t.Fatalf("pairTarget: want %d, finCounted %v; want %d with the FIN counted: the application's socket in FIN_WAIT2 has sent it", target.want, target.finCounted, at.received+1)
	}
	if reached, err := p.pairReached(target); err != nil || !reached {
		t.Fatalf("pairReached = %v, %v, want settled", reached, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none: the application wrote 10 bytes and its FIN was acknowledged in sequence", l)
	}
	let()
}

// appFINAcknowledged reads the application's socket as orphanTookFIN reads the
// orphan, by the read ordered against its move to the timewait table
// (lookupOrphan): an exact lookup made as it moves can miss it, or find the
// full socket it leaves. Here that read finds it in TIME_WAIT, its FIN
// acknowledged, while the socket an exact lookup finds is still the full one,
// in FIN_WAIT2.
func TestTheApplicationsAcknowledgedFINIsReadAsTheOrphanIs(t *testing.T) {
	p, pc, app, at, let := closeBegunNotDone(t)
	wroteBeforeTheClose(pc, at, 1, true)
	_ = app.Close()
	proxySocketIn(t, pc, tcpCloseWait)
	for {
		d, err := lookupTCP(pc.app, pc.proxy)
		if err != nil {
			t.Fatalf("the application's socket: %v", err)
		}
		if d.state == tcpFinWait2 {
			break
		}
		runtime.Gosched() // its FIN's ACK can be a delayed one
	}
	was := lookupOrphan
	lookupOrphan = func(local, remote netip.AddrPort) (diagTCP, error) {
		if local == pc.proxy {
			return diagTCP{}, errNoSocket
		}
		return diagTCP{state: tcpTimeWait}, nil
	}
	t.Cleanup(func() { lookupOrphan = was })
	if reached, err := p.pairReached(&pairTarget{pc: pc, want: at.received + 1, finCounted: true}); err != nil || !reached {
		t.Fatalf("pairReached = %v, %v, want settled: the ordered read finds the application's FIN acknowledged", reached, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost charged, want none", l)
	}
	lookupOrphan = was
	let()
}
