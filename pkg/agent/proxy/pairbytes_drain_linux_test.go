//go:build linux

package proxy

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// drainReturns runs PairedBytesDrain on ctx and hands its answer back.
func drainReturns(ctx context.Context, p *Proxy) <-chan error {
	got := make(chan error, 1)
	go func() { got <- p.PairedBytesDrain(ctx) }()
	return got
}

// drainHolds fails t if the drain answered within a while: it still waits for
// a close the proxy has not counted. A drain that does not wait answers at
// once, well within it.
func drainHolds(t *testing.T, got <-chan error, why string) {
	t.Helper()
	select {
	case err := <-got:
		t.Fatalf("the drain answered (%v) %s", err, why)
	case <-time.After(300 * time.Millisecond):
	}
}

// drainAnswers is the drain's answer, which must come once nothing holds it.
func drainAnswers(t *testing.T, got <-chan error, why string) error {
	t.Helper()
	select {
	case err := <-got:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("the drain did not answer %s", why)
		return nil
	}
}

// proxyServed is the accepted socket untoldPair's connection was paired from.
func proxyServed(t *testing.T, p *Proxy) net.Conn {
	t.Helper()
	pc := pairedConnOf(t, p, 1)
	return pc.conn
}

// awaitState waits, bounded, until the proxy's side of conn is in a state ok
// says: the kernel's, which a write or close on loopback reaches at once.
func awaitState(t *testing.T, conn *net.TCPConn, ok func(uint8) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ps := readProxySide(conn)
		if ps.err == nil && ok(ps.state) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("premise: the proxy's side never reached %s (state %d, %v)", what, ps.state, ps.err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A connection that closes as the application stops, with bytes it wrote that
// the proxy never read: the drain the session's last read, and keploy test's
// set end, wait on holds until the proxy has counted that close, so its loss
// is in that read, never in a later one's, and never only a log line. The
// application's FIN is held behind the bytes the proxy's full receive buffer
// will not take, so only the application's socket shows it closed (its FIN
// queued, FIN_WAIT1); the proxy's side is still ESTABLISHED. Once the proxy's
// handler closes the connection (its close counts the bytes it tosses), the
// drain still waits until the handler is done with it (removed), then answers.
func TestADrainWaitsForTheCloseOfAConnectionTheApplicationsStopEnded(t *testing.T) {
	p, app, tracked := untoldPair(t)
	use(t, app, tracked)
	served := proxyServed(t, p).(*net.TCPConn)
	_ = served.SetReadBuffer(4096)
	_ = app.SetWriteBuffer(4096)
	// Written until neither buffer takes more, never read by the proxy.
	_ = app.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	big := bytes.Repeat([]byte("x"), 8<<20)
	if n, err := app.Write(big); err == nil || n == 0 {
		t.Fatalf("premise: the write fills the buffers and times out, wrote %d, %v", n, err)
	}
	// The application stops: its socket closes with the bytes still queued,
	// its FIN behind them.
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if ps := readProxySide(served); ps.err != nil || peerEnded(ps.state) {
		t.Fatalf("premise: the proxy's side has not taken the FIN held behind the bytes (state %d, %v)", ps.state, ps.err)
	}
	got := drainReturns(context.Background(), p)
	drainHolds(t, got, "while the application's socket had closed and the proxy had not counted its close")
	// The proxy's handler closes it: the close counts the bytes it tosses.
	if err := tracked.Close(); err != nil {
		t.Fatal(err)
	}
	if n := lossesOf(p, models.CaptureLossAppBytesLost); n != 1 {
		t.Fatalf("premise: the close counted %d app_bytes_lost, want 1", n)
	}
	drainHolds(t, got, "before the proxy's handler was done with the connection")
	p.paired.remove(served)
	if err := drainAnswers(t, got, "once the proxy was done with the connection"); err != nil {
		t.Fatalf("the drain: %v", err)
	}
}

// What else a drain waits for, and what it does not: a close the proxy began
// (its handler is counting it) holds the drain until the handler is done; the
// application's FIN taken by the proxy's side holds it until the proxy has
// closed and forgotten the connection; a connection the application holds
// open, or one the proxy serves with no session (it counts no loss at its
// close), never holds it. A drain whose context ends answers with why, naming
// what it waited for.
func TestADrainWaitsOnlyForClosesTheProxyHasNotCounted(t *testing.T) {
	t.Run("a close the proxy began", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		served := proxyServed(t, p)
		// The handler's close has begun: what its side had received is
		// noted, and its count of the bytes it tosses is on its way.
		if pc, _, err := p.paired.closing(served, nil); pc == nil || err != nil {
			t.Fatalf("premise: the close began, %v %v", pc, err)
		}
		got := drainReturns(context.Background(), p)
		drainHolds(t, got, "while the proxy's close of the connection was counting it")
		p.paired.remove(served)
		if err := drainAnswers(t, got, "once the close was done"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the application's FIN taken", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		served := proxyServed(t, p).(*net.TCPConn)
		_ = app.Close()
		awaitState(t, served, peerEnded, "the application's FIN")
		got := drainReturns(context.Background(), p)
		drainHolds(t, got, "while the proxy held a connection whose FIN it had taken")
		_ = tracked.Close()
		p.paired.remove(served)
		if err := drainAnswers(t, got, "once the proxy was done with the connection"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a socket closed past its handler's close", func(t *testing.T) {
		// Its side cannot be read: whether its application end ended
		// cannot be told, so the drain waits until it is forgotten.
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		served := proxyServed(t, p)
		_ = served.Close()
		if ps := readProxySide(served.(*net.TCPConn)); ps.err == nil {
			t.Fatalf("premise: a closed socket's side cannot be read, read state %d", ps.state)
		}
		got := drainReturns(context.Background(), p)
		drainHolds(t, got, "while the proxy held a connection whose side it could not read")
		p.paired.remove(served)
		if err := drainAnswers(t, got, "once the proxy forgot the connection"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a connection the application holds open", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		if err := drainAnswers(t, drainReturns(context.Background(), p), "with only a connection the application holds open"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a connection served with no session", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		served := proxyServed(t, p).(*net.TCPConn)
		p.paired.servedWithoutSession(served)
		_ = app.Close()
		awaitState(t, served, peerEnded, "the application's FIN")
		if err := drainAnswers(t, drainReturns(context.Background(), p), "with only a connection served with no session"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("its context ends", func(t *testing.T) {
		p, app, tracked := untoldPair(t)
		use(t, app, tracked)
		served := proxyServed(t, p).(*net.TCPConn)
		_ = app.Close()
		awaitState(t, served, peerEnded, "the application's FIN")
		ctx, cancel := context.WithCancel(context.Background())
		got := drainReturns(ctx, p)
		drainHolds(t, got, "before its context ended")
		cancel()
		err := drainAnswers(t, got, "once its context ended")
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "1 connection(s)") || !strings.Contains(err.Error(), app.LocalAddr().String()) {
			t.Fatalf("the drain: %v; want its context's end, naming the connection it waited for", err)
		}
	})
}
