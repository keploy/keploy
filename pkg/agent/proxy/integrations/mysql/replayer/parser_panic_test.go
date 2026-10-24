package replayer

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// writePanicConn is a connection whose writes panic: the parser serving it
// panics as it answers, as a bug in the parser would.
type writePanicConn struct{ net.Conn }

func (writePanicConn) Write([]byte) (int, error) { panic("a parser bug") }

// A replay whose parser panicked as it greeted the app returned nil: its
// goroutine's close read as a clean end, so the proxy filed no error, and
// the test that made the call could pass. It returns the panic.
func TestAReplayWhoseParserPanicsDoesNotEndCleanly(t *testing.T) {
	app, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = agentEnd.Close() })
	done := make(chan error, 1)
	go func() {
		done <- Replay(context.Background(), zap.NewNop(), writePanicConn{agentEnd}, &models.ConditionalDstCfg{Addr: testDBAddr}, &poolDb{fakeMockDb{session: []*models.Mock{nativeHandshake()}}}, models.OutgoingOptions{})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, util.ErrParserPanicked) {
			t.Fatalf("Replay = %v, want ErrParserPanicked", err)
		}
	case <-time.After(30 * time.Second): // the bound only turns a hang into a failure
		t.Fatal("Replay did not return")
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

// A MySQL replay whose goroutine panics as the context's end stops it returns the
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
		done <- Replay(ctx, zap.NewNop(), conn, &models.ConditionalDstCfg{Addr: testDBAddr}, &poolDb{fakeMockDb{session: []*models.Mock{nativeHandshake()}}}, models.OutgoingOptions{})
	}()
	// The bounds below only turn a hang into a failure.
	select {
	case <-conn.writing: // the server's greeting
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
