package generic

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// clocklessConn is a connection on which a deadline still to come is not set
// (none is, instead): a read waits for data, the close, or a deadline already
// passed, never for the clock. Its reads in progress are counted, and each
// read that starts is told on reading, when nothing waits there already.
type clocklessConn struct {
	net.Conn
	inFlight atomic.Int32
	reading  chan struct{}
}

// passedOrNone is t once it has passed, and no deadline before.
func passedOrNone(t time.Time) time.Time {
	if t.IsZero() || t.After(time.Now()) {
		return time.Time{}
	}
	return t
}

func (c *clocklessConn) SetDeadline(t time.Time) error {
	return c.Conn.SetDeadline(passedOrNone(t))
}

func (c *clocklessConn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(passedOrNone(t))
}

func (c *clocklessConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(passedOrNone(t))
}

func (c *clocklessConn) Read(p []byte) (int, error) {
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	select {
	case c.reading <- struct{}{}:
	default:
	}
	return c.Conn.Read(p)
}

// A generic replay whose context ends while the parser reads the client
// returns only once the goroutine that reads it has ended. decodeGeneric
// returned the context's error at once and left that goroutine reading,
// matching and logging after it had returned. Nothing writes to the client or
// closes it here, and no read ends on the clock (clocklessConn), so only the
// context's end can end the read.
func TestTheGenericParserReturnsOnlyOnceItsReaderHasEnded(t *testing.T) {
	_, proxySide := tcpPair(t)
	conn := &clocklessConn{Conn: proxySide, reading: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		returned <- decodeGeneric(ctx, zap.NewNop(), []byte("PING"), conn, nil, bigStore(), models.OutgoingOptions{})
	}()
	// The bounds below only turn a hang into a failure.
	select {
	case <-conn.reading: // reading on for more of the request
	case <-time.After(time.Minute):
		t.Fatal("the parser did not read the client")
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("decodeGeneric = %v, want the context's error", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("decodeGeneric did not return once its context was cancelled")
	}
	if n := conn.inFlight.Load(); n != 0 {
		t.Fatalf("decodeGeneric returned with %d read(s) of the client still in progress: the goroutine it started outlives it", n)
	}
}
