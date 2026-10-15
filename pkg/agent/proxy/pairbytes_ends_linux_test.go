//go:build linux

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
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
	if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
		t.Fatalf("WaitPairedBytes: %v, want settled", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost for a connection whose every byte reached the proxy before its reset, want 0", l)
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
	if err := p.waitPairTargets(context.Background(), targets); err != nil {
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
	if err := p.waitPairTargets(context.Background(), targets); err != nil {
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
			go func() { done <- p.waitPairTargets(context.Background(), targets) }()
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

// The application resets the connection with its call still unsent: its own
// abort dropped those bytes, as it would have without keploy (the kernel took
// the reset only at the sequence number it expected next, so every byte it
// had sent had arrived): settled, no capture loss.
func TestTheApplicationsOwnResetIsNoLoss(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	queue(t, app)
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 {
		t.Fatalf("pairTargets = %v, %v", targets, err)
	}
	done := make(chan error, 1)
	go func() { done <- p.waitPairTargets(context.Background(), targets) }()
	if err := app.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	go func() { _, _ = io.Copy(io.Discard, reader) }()
	if err := <-done; err != nil {
		t.Fatalf("waitPairTargets: %v", err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Fatalf("%d app_bytes_lost for the application's own abort, want 0", l)
	}
}
