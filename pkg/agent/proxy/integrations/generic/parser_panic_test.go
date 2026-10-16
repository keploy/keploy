package generic

import (
	"context"
	"errors"
	"net"
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

// A replay whose parser panicked as it answered a recorded exchange returned
// nil: its goroutine's close read as a clean end, so the proxy filed no
// error, and the test that made the call could pass. It returns the panic.
func TestAReplayWhoseParserPanicsDoesNotEndCleanly(t *testing.T) {
	app, proxySide := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = proxySide.Close() })
	done := make(chan error, 1)
	go func() {
		done <- decodeGeneric(context.Background(), zap.NewNop(), []byte("PING"), writePanicConn{proxySide}, nil, bigStore(), models.OutgoingOptions{})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, util.ErrParserPanicked) {
			t.Fatalf("decodeGeneric = %v, want ErrParserPanicked", err)
		}
	case <-time.After(30 * time.Second): // the bound only turns a hang into a failure
		t.Fatal("decodeGeneric did not return")
	}
}
