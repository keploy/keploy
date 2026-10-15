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
