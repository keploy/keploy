package util

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

// closeFlagConn records that it was closed.
type closeFlagConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeFlagConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// A parser goroutine that panics sends no result of its own. RecoverInto
// sends ErrParserPanicked as its result, once it has closed the parser's
// connections, and before the goroutine's own close of the channel: the
// caller reads the panic, never the close (a clean end).
func TestRecoverIntoMakesAPanicTheGoroutinesResult(t *testing.T) {
	app, proxy := net.Pipe()
	defer app.Close()
	conn := &closeFlagConn{Conn: proxy}
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		defer RecoverInto(zap.NewNop(), conn, nil, errCh)
		panic("a parser bug")
	}()
	err, ok := <-errCh
	if !ok || !errors.Is(err, ErrParserPanicked) {
		t.Fatalf("the caller read %v (open %v), want ErrParserPanicked", err, ok)
	}
	if !conn.closed.Load() {
		t.Fatal("the panic was the result before the parser's connection was closed")
	}
	if _, ok := <-errCh; ok {
		t.Fatal("a second result after the panic")
	}

	// A goroutine that ends without a panic keeps its own result, or none.
	clean := make(chan error, 1)
	go func() {
		defer close(clean)
		defer RecoverInto(zap.NewNop(), nil, nil, clean)
	}()
	if err, ok := <-clean; ok {
		t.Fatalf("a clean end read %v, want only the close", err)
	}
}

// RecoverWith runs then for a recovered panic, before it closes the
// connections, and not on a clean end.
func TestRecoverWithCountsAPanicBeforeItClosesTheConnections(t *testing.T) {
	app, proxy := net.Pipe()
	defer app.Close()
	conn := &closeFlagConn{Conn: proxy}
	var closedFirst, ran atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer RecoverWith(zap.NewNop(), conn, nil, func() { ran.Store(true); closedFirst.Store(conn.closed.Load()) })
		panic("a handler bug")
	}()
	<-done
	if !ran.Load() || closedFirst.Load() || !conn.closed.Load() {
		t.Fatalf("then ran %v, after the close %v, closed %v: want it run before the close", ran.Load(), closedFirst.Load(), conn.closed.Load())
	}
	ran.Store(false)
	clean := make(chan struct{})
	go func() {
		defer close(clean)
		defer RecoverWith(zap.NewNop(), nil, nil, func() { ran.Store(true) })
	}()
	<-clean
	if ran.Load() {
		t.Fatal("then ran on a clean end")
	}
}
