package http

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"
)

// writePanicConn is a connection whose writes panic: the parser serving it
// panics as it answers, as a bug in the parser would.
type writePanicConn struct{ net.Conn }

func (writePanicConn) Write([]byte) (int, error) { panic("a parser bug") }

// A replay whose parser panicked answered nothing, and decodeHTTP returned
// nil: its goroutine's close read as a clean end, so the proxy filed no
// error, and the test that made the call could pass. It returns the panic.
func TestAReplayWhoseParserPanicsDoesNotEndCleanly(t *testing.T) {
	_, proxy := tcpPair(t)
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	done := runDecode(t, proxy, func(ctx context.Context) error {
		return h.decodeHTTP(ctx, []byte("GET /u HTTP/1.1\r\nHost: up\r\n\r\n"), writePanicConn{proxy}, nil, &mockMemDb{}, models.OutgoingOptions{})
	})
	select {
	case err := <-done:
		if !errors.Is(err, util.ErrParserPanicked) {
			t.Fatalf("decodeHTTP = %v, want ErrParserPanicked", err)
		}
	case <-time.After(30 * time.Second): // the bound only turns a hang into a failure
		t.Fatal("decodeHTTP did not return")
	}
}

// keptConn is a connection's request aloft as the proxy's kept connection
// holds it (integrations.FlightCarrier, the proxy's trackedConn): a write
// lands it and leaves none aloft.
type keptConn struct {
	mu sync.Mutex
	f  *learn.Flight
}

func (*keptConn) Hold() func() { return func() {} }
func (k *keptConn) InFlight() *learn.Flight {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.f
}
func (k *keptConn) land() {
	k.mu.Lock()
	f := k.f
	k.f = nil
	k.mu.Unlock()
	f.Land()
}

// landsOnClose is the client connection as the TLS layer the proxy serves a
// kept connection over hands it to a parser: its close writes close_notify,
// which lands the request aloft on k as an answer's first bytes would (the
// proxy's trackedConn.Write).
type landsOnClose struct {
	net.Conn
	k *keptConn
}

func (c landsOnClose) Close() error {
	c.k.land()
	return c.Conn.Close()
}

// A replay whose parser panics with a request aloft ends that request untold
// before it closes the connection it was handed (integrations.RecoverIntoCtx):
// the application was given no answer, so a serve told in the request is in an
// order keploy cannot tell (0,0). Over TLS that close writes close_notify, and
// with pUtil.RecoverInto's close first the request landed on it, so the serve
// read as one the application had its answer to (1,1).
func TestAReplayWhoseParserPanicsEndsItsRequestAloftUntold(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	tr := learn.NewTracker()
	tr.Begin()
	k := &keptConn{f: tr.Lift()}
	told := &models.Mock{Name: "told-before-the-panic"}
	tr.ServedMock(told)
	_, proxy := tcpPair(t)
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	done := runDecode(t, proxy, func(ctx context.Context) error {
		ctx = integrations.WithRequestHolder(learn.WithTracker(ctx, tr), k)
		return h.decodeHTTP(ctx, []byte("GET /u HTTP/1.1\r\nHost: up\r\n\r\n"), writePanicConn{landsOnClose{proxy, k}}, nil, &mockMemDb{}, models.OutgoingOptions{})
	})
	select {
	case err := <-done:
		if !errors.Is(err, util.ErrParserPanicked) {
			t.Fatalf("decodeHTTP = %v, want ErrParserPanicked", err)
		}
	case <-time.After(30 * time.Second): // the bound only turns a hang into a failure
		t.Fatal("decodeHTTP did not return")
	}
	if first, last := tr.Placed().Order.Of(told); first != 0 || last != 0 {
		t.Fatalf("the serve told in the request aloft when the parser panicked as it answered is at %d,%d; want 0,0: the application was given no answer", first, last)
	}
}

// The legacy record path likewise: a parser that panicked as it answered
// the app recorded nothing, and encodeHTTP said it had ended cleanly.
func TestARecordWhoseParserPanicsDoesNotEndCleanly(t *testing.T) {
	_, proxyClient := tcpPair(t)
	up, proxyDest := tcpPair(t)
	go func() {
		_ = readUntil(up, "\r\n\r\n", 30*time.Second)
		_, _ = up.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	}()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, g)
	gctx = context.WithValue(gctx, models.ClientConnectionIDKey, "test-conn")
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	done := make(chan error, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		// The upstream's answer is written to the app, and that write panics.
		done <- h.encodeHTTP(gctx, []byte("GET /a HTTP/1.1\r\nHost: up\r\n\r\n"), writePanicConn{proxyClient}, proxyDest, make(chan *models.Mock, 4), models.OutgoingOptions{}, nil)
	}()
	// As encodeHarness's: registered after cancel, so it runs first.
	t.Cleanup(func() { _ = proxyClient.Close(); _ = proxyDest.Close(); <-returned; _ = g.Wait() })
	select {
	case err := <-done:
		if !errors.Is(err, util.ErrParserPanicked) {
			t.Fatalf("encodeHTTP = %v, want ErrParserPanicked", err)
		}
	case <-time.After(30 * time.Second): // the bound only turns a hang into a failure
		t.Fatal("encodeHTTP did not return")
	}
}

// stoppedWritePanicConn is a connection whose write panics once the context's
// end has stopped it (a deadline already passed: util.InterruptOnDone), as a
// bug in the parser on its way out would. Each write that starts is told on
// writing, when nothing waits there already.
type stoppedWritePanicConn struct {
	net.Conn
	writing chan struct{}
}

func (c *stoppedWritePanicConn) Write(p []byte) (int, error) {
	select {
	case c.writing <- struct{}{}:
	default:
	}
	n, err := c.Conn.Write(p)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		panic("a parser bug on its way out")
	}
	return n, err
}

// A HTTP replay whose goroutine panics as the context's end stops it returns the
// panic, so the proxy counts it: it returns only once that goroutine has, and
// takes what the goroutine reported. It returned the context's error as soon
// as the context ended, before the goroutine could fail, and the panic was
// lost. The app reads nothing: on a pipe the parser's write waits for it,
// until the context's end stops it.
func TestAReplayWhoseParserPanicsAsTheContextEndsReturnsThePanic(t *testing.T) {
	app, proxySide := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = proxySide.Close() })
	conn := &stoppedWritePanicConn{Conn: proxySide, writing: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- newHTTP().decodeHTTP(ctx, []byte("GET /u HTTP/1.1\r\nHost: up\r\n\r\n"), conn, nil, &mockMemDb{}, models.OutgoingOptions{})
	}()
	// The bounds below only turn a hang into a failure.
	select {
	case <-conn.writing: // the 502 to the call no mock answers
	case <-time.After(time.Minute):
		t.Fatal("the parser did not write to the app")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, util.ErrParserPanicked) {
			t.Fatalf("got %v, want ErrParserPanicked", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the parser did not return once its context was cancelled")
	}
}
