//go:build linux

package proxy

import (
	"io"
	"net"
	"runtime"
	"testing"
)

// acceptedPair is a loopback TCP connection: the app's end, and the proxy's
// accepted end, kept by t.
func acceptedPair(tb testing.TB, t *inflight) (app net.Conn, proxy *trackedConn) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	app, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	c := <-accepted
	if c == nil {
		tb.Fatal("accept failed")
	}
	proxy = t.track(c)
	tb.Cleanup(func() { _ = app.Close(); _ = proxy.Close() })
	return app, proxy
}

// waitSettled waits, by the tracker's own change events, until cond holds.
func waitSettled(tb testing.TB, t *inflight, cond func() bool) {
	tb.Helper()
	for {
		changed := t.Changed()
		if cond() {
			return
		}
		<-changed
	}
}

// A connection whose handler has not started reading holds work, in a
// replay: a request may be being handled. A handler blocked reading an empty
// socket holds none. Bytes the application sent that the handler has not
// read are work in a record too.
func TestAConnectionIsSettledWhenItsHandlerWaitsOnAnEmptySocket(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("a connection whose handler is not reading: pending %d in a replay, want 1", n)
	}
	if n := tr.Pending(false); n != 0 {
		t.Fatalf("a connection with no unread byte: pending %d in a record, want 0", n)
	}

	read := make(chan []byte)
	go func() {
		buf := make([]byte, 64)
		n, _ := proxy.Read(buf)
		read <- buf[:n]
	}()
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("a handler waiting on an empty socket: pending %d, want 0", n)
	}

	if _, err := io.WriteString(app, "GET / HTTP/1.1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if got := string(<-read); got != "GET / HTTP/1.1\r\n\r\n" {
		t.Fatalf("read %q", got)
	}
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("a request read and not answered: pending %d in a replay, want 1", n)
	}
	if n := tr.Pending(false); n != 0 {
		t.Fatalf("a request read: pending %d in a record, want 0 (a record never waits on an answer)", n)
	}

	// Bytes the handler has not read are the application's call in hand.
	if _, err := io.WriteString(app, "more"); err != nil {
		t.Fatal(err)
	}
	waitUnread(t, proxy)
	if n := tr.Pending(false); n != 1 {
		t.Fatalf("unread bytes: pending %d in a record, want 1", n)
	}
}

// waitUnread waits until the socket holds the bytes the application wrote:
// they cross loopback, which no event of the tracker reports.
func waitUnread(t *testing.T, c *trackedConn) {
	t.Helper()
	for c.unread() == 0 {
		runtime.Gosched()
	}
}

// A closed connection holds the test's work, in a replay, until its handler
// is done with it: a parser that closes it and then returns its miss (SQS's)
// has not filed the miss yet. Once the handler is done (forget) it holds
// nothing. A close, and a forget, are changes.
func TestAClosedConnectionHoldsNothingOnceItsHandlerIsDone(t *testing.T) {
	tr := &inflight{}
	_, proxy := acceptedPair(t, tr)
	changed := tr.Changed()
	before := tr.Progress()
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	<-changed
	if tr.Progress() == before {
		t.Fatal("a close moved no progress")
	}
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("a closed connection whose handler is not done: pending %d in a replay, want 1", n)
	}
	if n := tr.Pending(false); n != 0 {
		t.Fatalf("a closed connection: pending %d in a record, want 0", n)
	}
	changed = tr.Changed()
	tr.forget(proxy)
	<-changed
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("a connection its handler is done with: pending %d, want 0", n)
	}
	tr.forget(proxy) // a second forget is a no-op
}
