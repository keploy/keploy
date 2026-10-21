//go:build linux

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The application aborts a held connection (a reset: SO_LINGER 0, or a close
// with an unread reply) after every byte it wrote has reached the proxy,
// while the proxy's handler still holds the connection: nothing was lost, so
// nothing is charged and the wait settles.
func TestAResetAfterTheBytesArrivedIsNoLoss(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := io.ReadFull(reader, buf[:10]); err != nil || n != 10 {
		t.Fatalf("the proxy read %d, %v; want the 10 bytes", n, err)
	}
	if err := app.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	// The proxy's handler sees the reset; it has not returned yet.
	if _, err := reader.Read(buf); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("the proxy's read after the reset: %v, want ECONNRESET", err)
	}
	if err := p.WaitPairedBytes(context.Background(), 1, models.MODE_TEST); err != nil {
		t.Fatalf("WaitPairedBytes: %v, want settled", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost for a connection whose every byte reached the proxy before its reset, want 0", l)
	}
}

// The application's reset reaches the proxy's held side with its call still
// unsent, and then the handler closes with the bytes that did arrive unread:
// the close charges the loss (they were never captured), and the wait settles
// on it, rather than leaving the reset's unsent rest unknown.
func TestAResetThenACloseWithBytesUnreadSettlesOnTheLoss(t *testing.T) {
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	queue(t, app)
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 || targets[0].sent >= targets[0].want {
		t.Fatalf("pairTargets = %+v, %v; want one connection with bytes not sent yet", targets, err)
	}
	// Everything it sent acknowledged first, so its reset cannot overtake a
	// byte on its way: what is missing was never sent.
	awaitAcked(t, app)
	if err := app.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	for {
		ps := readProxySide(proxySide)
		if ps.err != nil {
			t.Fatal(ps.err)
		}
		if ps.state == tcpClose {
			break
		}
		runtime.Gosched()
	}
	_ = reader.Close() // bytes unread: a loss, charged at the close
	if err := p.waitPairTargets(context.Background(), targets, false); err != nil {
		t.Fatalf("waitPairTargets: %v, want settled on the loss its close charged", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
		t.Fatalf("%d app_bytes_lost, want 1", l)
	}
}

// A loss being charged on a connection (its close tossing bytes unread) and a
// wait that finds a reset there with bytes it had sent missing: the wait
// charges the same loss, finds it being charged, and waits for its count, so
// it settles only once the test's read of its losses would see it. Without
// that, the wait would settle with the loss not yet counted: a test judged
// without it.
func TestAWaitSettlesOnALossOnlyOnceItIsCounted(t *testing.T) {
	p := &Proxy{logger: zap.NewNop()}
	pc := &pairedConn{done: make(chan struct{})}
	counting, release, waits, waited := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1), make(chan struct{}, 1)
	var countingOnce sync.Once
	// Idempotent: a second charge of the same loss (a once-only check
	// lost) is counted again, not a panic, and the count says it.
	pairLostCounting = func() { countingOnce.Do(func() { close(counting); <-release }) }
	pairLostWaits = func() { waits <- struct{}{} }
	pairLostWaited = func() { waited <- struct{}{} }
	t.Cleanup(func() { pairLostCounting, pairLostWaits, pairLostWaited = nil, nil, nil })
	closed := make(chan struct{})
	go func() { p.pairLost(pc, "the close tossed bytes unread"); close(closed) }()
	<-counting // the close's charge, not yet counted
	type answer struct {
		ok  bool
		err error
		n   uint64
	}
	done := make(chan answer, 1)
	go func() {
		ok, err := p.pairReset(&pairTarget{pc: pc, want: 20, sent: 20}, 10)
		done <- answer{ok, err, lossesOf(p, models.CaptureLossAppBytesLost)}
	}()
	select {
	case a := <-done:
		close(release)
		t.Fatalf("the wait answered %v, %v with %d losses counted while the close's charge was still being counted", a.ok, a.err, a.n)
	case <-waits:
	}
	close(release)
	a := <-done
	<-closed
	select {
	case <-waited:
	default:
		t.Fatal("the wait returned without having waited for the close's charge to be counted")
	}
	if !a.ok || a.err != nil || a.n != 1 {
		t.Fatalf("the wait = %v, %v with %d losses counted as it returned; want settled with the 1 loss counted", a.ok, a.err, a.n)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
		t.Fatalf("%d app_bytes_lost, want 1: once per connection", l)
	}
}

// The same reset, after the wait read its target (all of it already
// received): a race of a few syscalls in production.
func TestAResetAfterTheTargetWasReadIsNoLoss(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := io.ReadFull(reader, buf[:10]); err != nil || n != 10 {
		t.Fatalf("the proxy read %d, %v; want the 10 bytes", n, err)
	}
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 || targets[0].want != 10 {
		t.Fatalf("pairTargets = %v, %v; want one of 10 bytes", targets, err)
	}
	if err := app.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	if _, err := reader.Read(buf); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("the proxy's read after the reset: %v, want ECONNRESET", err)
	}
	if err := p.waitPairTargets(context.Background(), targets, false); err != nil {
		t.Fatalf("waitPairTargets: %v", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost for 10 of 10 bytes received before the reset, want 0", l)
	}
}

// The proxy resets the connection itself (resetOnClose, a refused mock.real
// destination) after every byte arrived: nothing the application wrote was
// lost.
func TestTheProxysResetAfterTheBytesArrivedIsNoLoss(t *testing.T) {
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := io.ReadFull(reader, buf[:10]); err != nil || n != 10 {
		t.Fatalf("the proxy read %d, %v; want the 10 bytes", n, err)
	}
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 {
		t.Fatalf("pairTargets = %v, %v", targets, err)
	}
	_ = proxySide.SetLinger(0)
	_ = reader.Close()
	p.paired.remove(proxySide)
	if err := p.waitPairTargets(context.Background(), targets, false); err != nil {
		t.Fatalf("waitPairTargets: %v", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost for 10 of 10 bytes received before the proxy's reset, want 0", l)
	}
}

// The accept loop forgets every paired connection once its handler is done
// with it (the registry is keyed by the accepted *net.TCPConn, not the
// tracked wrapper): a stale entry would be looked up at every end, forever,
// and charged as lost once its sockets are gone.
func TestTheAcceptLoopForgetsAPairedConnectionOnceItsHandlerIsDone(t *testing.T) {
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
	cfg.Agent.Mode = models.MODE_RECORD // no session: the call passes through
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
	// Accepted (the canary is 1): countAccept has kept it.
	if err := p.WaitAccepted(ctx, 2); err != nil {
		t.Fatal(err)
	}
	p.paired.mu.Lock()
	n := len(p.paired.byConn)
	p.paired.mu.Unlock()
	if n != 1 {
		t.Fatalf("the registry holds %d connections while the paired one is open, want 1", n)
	}
	_ = c.Close()
	// No deadline but the test binary's -timeout.
	for {
		ch := p.paired.changes()
		p.paired.mu.Lock()
		n = len(p.paired.byConn)
		p.paired.mu.Unlock()
		if n == 0 {
			return
		}
		<-ch
	}
}

// The wait is parked on the bytes still on their way, never ends before the
// last of them arrives, and the read that takes them in is what ends it: the
// target counts what the application has not sent yet (most of it here), and
// the wait has taken its channels before the proxy reads a byte.
func TestTheWaitIsParkedUntilTheLastByteArrives(t *testing.T) {
	for _, tc := range []struct{ network, addr string }{{"tcp4", "127.0.0.1:0"}, {"tcp6", "[::1]:0"}} {
		t.Run(tc.network, func(t *testing.T) {
			p, app, proxySide, reader := heldPair(t, tc.network, tc.addr)
			n := queue(t, app)
			targets, err := p.pairTargets(1)
			if err != nil || len(targets) != 1 {
				t.Fatalf("pairTargets = %v, %v; want the connection", targets, err)
			}
			want := targets[0].want
			if want < n {
				t.Fatalf("the target is %d bytes, fewer than the %d the application wrote", want, n)
			}
			if got := received(t, proxySide); got >= want {
				t.Fatalf("the proxy's side received %d of %d at once: the fixture holds nothing back", got, want)
			}
			done := make(chan error, 1)
			go func() { done <- p.waitPairTargets(context.Background(), targets, false) }()
			// Parked: it took the registry's change channel (only it takes
			// one here) before the first read below.
			for {
				p.paired.mu.Lock()
				parked := p.paired.changed != nil
				p.paired.mu.Unlock()
				if parked {
					break
				}
				runtime.Gosched()
			}
			buf := make([]byte, 64<<10)
			var read uint64
			for read < n {
				select {
				case err := <-done:
					if got := received(t, proxySide); got < want {
						t.Fatalf("the wait returned %v with %d of %d bytes received", err, got, want)
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
			// No deadline but the test binary's -timeout: the last read ended it.
			if err := <-done; err != nil {
				t.Fatalf("waitPairTargets: %v", err)
			}
		})
	}
}

// The application resets the connection with its call still unsent behind the
// proxy's window. Its abort may have dropped those bytes unsent, as it would
// have without keploy, or sent them later and lost them: a reset the kernel
// takes says nothing of bytes still on their way (pairReset), so it is
// unknown, and charges nothing.
func TestTheApplicationsResetWithItsCallUnsentIsUnknown(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	queue(t, app)
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 || targets[0].sent >= targets[0].want {
		t.Fatalf("pairTargets = %+v, %v; want one connection with bytes not sent yet", targets, err)
	}
	done := make(chan error, 1)
	go func() { done <- p.waitPairTargets(context.Background(), targets, false) }()
	// Everything it sent acknowledged first, so its reset cannot overtake
	// a byte on its way: what is missing was never sent.
	awaitAcked(t, app)
	if err := app.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	go func() { _, _ = io.Copy(io.Discard, reader) }()
	if err := <-done; !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "not yet sent") {
		t.Fatalf("waitPairTargets: %v; want unknown: the abort may have dropped the call unsent, or it was sent and lost", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost for an unknown, want 0", l)
	}
}

// awaitAcked returns once everything the application's socket has sent is
// acknowledged (tcpi_unacked 0).
func awaitAcked(t *testing.T, app *net.TCPConn) {
	t.Helper()
	local, remote := mustAddrPort(t, app.LocalAddr()), mustAddrPort(t, app.RemoteAddr())
	for {
		d, err := lookupTCP(local, remote)
		if err != nil {
			t.Fatalf("the application's socket: %v", err)
		}
		if d.info != nil && d.info.Unacked == 0 {
			return
		}
		runtime.Gosched()
	}
}
