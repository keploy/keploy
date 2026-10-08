//go:build linux

package util

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// serve accepts one connection and hands it to handle.
func serve(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		handle(c)
	}()
	return l.Addr().String()
}

// TestWatchPropagatesADestinationThatClosesFirst: a destination that closes
// the pre-dialled connection while the application has not yet sent anything
// closes the application's end, as it would have without keploy.
func TestWatchPropagatesADestinationThatClosesFirst(t *testing.T) {
	addr := serve(t, func(c net.Conn) {
		time.Sleep(100 * time.Millisecond) // an idle timeout
		_ = c.Close()
	})
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(addr, pre)
	defer p.CloseIfUnused()
	closed := make(chan struct{})
	p.WatchUntilTaken(func() { close(closed) })
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the destination closed the connection and the application's end was not closed")
	}
}

// TestWatchLeavesAGreetingForTheDial: bytes the destination sends first stay
// for the dial that takes the connection, and do not count as a close.
func TestWatchLeavesAGreetingForTheDial(t *testing.T) {
	addr := serve(t, func(c net.Conn) {
		_, _ = c.Write([]byte("HELLO"))
		time.Sleep(time.Second)
		_ = c.Close()
	})
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(addr, pre)
	var fired atomic.Bool
	p.WatchUntilTaken(func() { fired.Store(true) })
	time.Sleep(100 * time.Millisecond) // the greeting has arrived
	c, err := DialRaw(context.Background(), nil, "tcp", addr, p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := make([]byte, 5)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "HELLO" {
		t.Fatalf("read %q, %v; the greeting must reach whoever takes the connection", b, err)
	}
	if fired.Load() {
		t.Fatal("a greeting was taken for a close")
	}
}

// TestTakeEndsTheWatch: a connection taken while watched is handed over
// usable, with no deadline left on it, and its later close is the taker's
// business, not the watch's.
func TestTakeEndsTheWatch(t *testing.T) {
	addr := serve(t, func(c net.Conn) {
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err == nil {
			_, _ = c.Write(buf)
		}
		_ = c.Close()
	})
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(addr, pre)
	var fired atomic.Bool
	p.WatchUntilTaken(func() { fired.Store(true) })
	time.Sleep(50 * time.Millisecond)
	c, err := DialRaw(context.Background(), nil, "tcp", addr, p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
		t.Fatalf("read %q, %v from a taken connection", b, err)
	}
	time.Sleep(100 * time.Millisecond) // the destination has closed by now
	if fired.Load() {
		t.Fatal("the watch fired for a connection a dial had taken")
	}
}

// TestCloseIfUnusedEndsTheWatchQuietly: closing an untaken connection is not
// the destination closing it.
func TestCloseIfUnusedEndsTheWatchQuietly(t *testing.T) {
	addr := serve(t, func(c net.Conn) { time.Sleep(time.Second); _ = c.Close() })
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(addr, pre)
	var fired atomic.Bool
	p.WatchUntilTaken(func() { fired.Store(true) })
	p.CloseIfUnused()
	time.Sleep(100 * time.Millisecond)
	if fired.Load() {
		t.Fatal("CloseIfUnused was reported as the destination's close")
	}
}

// TestReturnRearmsTheConnection: the MySQL probe takes the
// pre-dialled connection for a look; a negative verdict hands it back, with
// any greeting it read, for the next dial — and it is watched again.
func TestReturnRearmsTheConnection(t *testing.T) {
	addr := serve(t, func(c net.Conn) {
		_, _ = c.Write([]byte("HI"))
		time.Sleep(300 * time.Millisecond)
		_ = c.Close()
	})
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(addr, pre)
	defer p.CloseIfUnused()
	ctx := context.Background()
	closed := make(chan struct{})
	p.WatchUntilTaken(func() { close(closed) })

	look, err := DialRaw(ctx, nil, "tcp", addr, p)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Is(look) {
		t.Fatal("the taken connection is not recognised as the pre-dialled one")
	}
	other, err := net.Dial("tcp", addr)
	if err == nil {
		defer other.Close()
		if p.Is(other) || p.Return(other, nil) {
			t.Fatal("an unrelated connection was taken for the pre-dialled one")
		}
	}
	b := make([]byte, 2)
	if _, err := io.ReadFull(look, b); err != nil {
		t.Fatal(err)
	}
	// Handed back with what was read, unwrapped: no watch on a wrapper, so
	// hand it back bare here to prove the re-armed watch.
	if !p.Return(look, nil) {
		t.Fatal("Return refused the pre-dialled connection")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the handed-back connection is not watched for the destination closing it")
	}
	// The destination has closed it, so the next dial dials afresh.
	again, err := DialRaw(ctx, nil, "tcp", addr, p)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again == pre {
		t.Fatal("the next dial got the handed-back connection the destination had closed")
	}
}

// TestReturnHandsTheConnectionToTheNextDial: a handed-back
// connection the destination keeps open is the next dial's.
func TestReturnHandsTheConnectionToTheNextDial(t *testing.T) {
	addr := serve(t, func(c net.Conn) { time.Sleep(time.Second); _ = c.Close() })
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(addr, pre)
	defer p.CloseIfUnused()
	ctx := context.Background()
	p.WatchUntilTaken(func() {})
	look, _ := DialRaw(ctx, nil, "tcp", addr, p)
	if !p.Return(look, nil) {
		t.Fatal("Return refused the pre-dialled connection")
	}
	if again, _ := DialRaw(ctx, nil, "tcp", addr, p); again != pre {
		t.Fatal("the next dial did not get the handed-back connection")
	}
}

// TestReturnReplaysWhatWasRead: bytes the look consumed reach the
// next dial through the replay wrapper it hands back.
func TestReturnReplaysWhatWasRead(t *testing.T) {
	addr := serve(t, func(c net.Conn) { time.Sleep(time.Second); _ = c.Close() })
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(addr, pre)
	defer p.CloseIfUnused()
	ctx := context.Background()
	look, _ := DialRaw(ctx, nil, "tcp", addr, p)
	wrapped := &replayed{Conn: look}
	if !p.Return(look, wrapped) {
		t.Fatal("Return refused the pre-dialled connection")
	}
	if again, _ := DialRaw(ctx, nil, "tcp", addr, p); again != net.Conn(wrapped) {
		t.Fatal("the next dial did not get the replay wrapper")
	}
}

type replayed struct{ net.Conn }

// TestTakeDiscardsAConnectionTheDestinationClosed: a dial never gets a
// pre-dialled connection the destination has already closed; it dials
// afresh, as it did before connections were pre-dialled.
func TestTakeDiscardsAConnectionTheDestinationClosed(t *testing.T) {
	var accepted atomic.Int32
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if accepted.Add(1) == 1 {
				_ = c.Close() // the first connection: closed for idling
				continue
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	addr := l.Addr().String()
	pre, err := DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPredialed(addr, pre)
	p.WatchUntilTaken(func() {}) // the application had spoken: nothing to propagate
	for deadline := time.Now().Add(2 * time.Second); !closedByPeer(pre) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	c, err := DialRaw(context.Background(), nil, "tcp", addr, p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c == pre {
		t.Fatal("handed over a connection the destination had closed")
	}
	waitCount(t, &accepted, 2) // the closed one and a fresh one
}

// TestReceivedNothingCountsWhatTheProxyAlreadyRead: the proxy's own reads
// (a TLS ClientHello it answered itself) count as the application having
// spoken.
func TestReceivedNothingCountsWhatTheProxyAlreadyRead(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	app, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	proxyEnd, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer proxyEnd.Close()
	if !ReceivedNothing(proxyEnd) {
		t.Fatal("a connection the application has not written to reads as having received bytes")
	}
	if _, err := app.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	if _, err := io.ReadFull(proxyEnd, b); err != nil {
		t.Fatal(err)
	}
	if ReceivedNothing(proxyEnd) {
		t.Fatal("bytes the proxy has already read do not count")
	}
	if ReceivedNothing(&replayed{Conn: proxyEnd}) {
		t.Fatal("a connection it cannot inspect must not read as silent")
	}
}
