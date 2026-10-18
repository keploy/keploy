package recorder

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// writePanicConn is a connection whose writes panic: the parser serving it
// panics as it relays, as a bug in the parser would.
type writePanicConn struct{ net.Conn }

func (writePanicConn) Write([]byte) (int, error) { panic("a parser bug") }

// The legacy record path: a parser that panicked as it relayed the server's
// greeting to the app recorded nothing, and Record said it had ended
// cleanly. It returns the panic.
func TestARecordWhoseParserPanicsDoesNotEndCleanly(t *testing.T) {
	app, clientConn := net.Pipe()
	server, destConn := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = clientConn.Close(); _ = server.Close(); _ = destConn.Close() })
	greeting := cannedServerGreeting(t, 7)
	go func() { _, _ = server.Write(greeting) }()
	g, gctx := errgroup.WithContext(context.Background())
	gctx = context.WithValue(gctx, models.ErrGroupKey, g)
	done := make(chan error, 1)
	go func() {
		done <- Record(gctx, zap.NewNop(), writePanicConn{clientConn}, destConn, make(chan *models.Mock, 4), models.OutgoingOptions{}, nil)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, util.ErrParserPanicked) {
			t.Fatalf("Record = %v, want ErrParserPanicked", err)
		}
	case <-time.After(30 * time.Second): // the bound only turns a hang into a failure
		t.Fatal("Record did not return")
	}
	_ = g.Wait()
}
