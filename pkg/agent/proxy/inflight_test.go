//go:build linux

package proxy

import (
	"io"
	"net"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
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
	for inq(c) == 0 {
		runtime.Gosched()
	}
}

// inq is how many bytes c's socket holds unread (SIOCINQ).
func inq(c *trackedConn) int {
	n := 0
	_ = c.raw.Control(func(fd uintptr) {
		if v, err := unix.IoctlGetInt(int(fd), unix.SIOCINQ); err == nil {
			n = v
		}
	})
	return n
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

// A settle's scope holds the connections kept when it began, each until it
// is seen holding no work once: the test's own call still in flight holds
// it; a connection accepted after it began never does, nor a call made on
// one of its connections after that connection was seen settled (an
// application that keeps calling: a timer's call, its miss, a new
// connection).
func TestAScopeIsHeldOnlyByTheWorkItBeganWith(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	sc := tr.scope()
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's call in flight: scope pending %d, want 1", n)
	}
	if got := sc.PendingProgress(true); len(got) != 1 {
		t.Fatalf("the test's call in flight: scope progress %v, want its connection", got)
	}

	read := make(chan []byte, 2)
	reader := func() {
		buf := make([]byte, 64)
		n, _ := proxy.Read(buf)
		read <- buf[:n]
	}
	go reader()
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("its handler waiting on an empty socket: scope pending %d, want 0", n)
	}

	// A connection accepted after the scope began, its handler not reading
	// (a request being handled): all of the proxy's connections hold it,
	// the scope does not.
	_, later := acceptedPair(t, tr)
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("a later connection not reading: pending %d, want 1", n)
	}
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("a connection accepted after the scope began: scope pending %d, want 0", n)
	}

	// A call on the scope's connection after it was seen settled.
	if _, err := io.WriteString(app, "GET /tick HTTP/1.1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	<-read
	if n := tr.Pending(true); n != 2 {
		t.Fatalf("a call read after the connection was seen settled, and the later connection: pending %d, want 2", n)
	}
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("a call made after its connection was seen settled: scope pending %d, want 0", n)
	}
	if got := sc.PendingProgress(true); len(got) != 0 {
		t.Fatalf("scope progress %v, want none", got)
	}
	_ = later
}

// waitInq waits until c's socket holds n bytes unread.
func waitInq(t *testing.T, c *trackedConn, n int) {
	t.Helper()
	for inq(c) < n {
		runtime.Gosched()
	}
}

// readOnce reads c once, as a handler's read for the application's next
// request, and sends what it returned.
func readOnce(c *trackedConn, out chan<- string) {
	buf := make([]byte, 256)
	n, _ := c.Read(buf)
	out <- string(buf[:n])
}

// A pooled connection the application keeps calling on (a timer's next call
// already in the socket each time its handler answers one, so no view ever
// sees it idle) holds a scope only until its handler has answered what the
// socket had received when the scope began: it went back to reading after
// its reads delivered those bytes, holding no request. The test's own call,
// read and not yet answered, holds it until then.
func TestAPooledConnectionKeptBusyByLaterCallsReleasesTheScope(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	const call, tick = "GET /test HTTP/1.1\r\n\r\n", "GET /tick HTTP/1.1\r\n\r\n"
	got := make(chan string, 2)
	go readOnce(proxy, got)
	if _, err := io.WriteString(app, call); err != nil {
		t.Fatal(err)
	}
	if r := <-got; r != call {
		t.Fatalf("read %q", r)
	}
	// The test's call is read, its answer not written: the scope begins.
	sc := tr.scope()
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's call read and not answered: scope pending %d, want 1", n)
	}
	// The application's next call arrives before the test's is answered.
	if _, err := io.WriteString(app, tick); err != nil {
		t.Fatal(err)
	}
	waitInq(t, proxy, len(tick))
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's call not answered, a later call in the socket: scope pending %d, want 1", n)
	}
	// The handler answers the test's call and reads on: the later call is
	// there at once, and it is handling that one.
	go readOnce(proxy, got)
	if r := <-got; r != tick {
		t.Fatalf("read %q", r)
	}
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("the later call read and not answered: pending %d, want 1", n)
	}
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("the test's call answered, a later one being handled: scope pending %d, want 0", n)
	}
	if p := sc.PendingProgress(true); len(p) != 0 {
		t.Fatalf("scope progress %v, want none", p)
	}
}

// Bytes the socket had received when a scope began hold it until the
// handler's reads deliver them, in a record as in a replay, even when one
// read delivers them with bytes that came later; in a replay, until the
// handler has then answered the request they hold.
func TestBytesReceivedBeforeTheScopeHoldItUntilAnswered(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	const call, tick = "GET /test HTTP/1.1\r\n\r\n", "GET /tick HTTP/1.1\r\n\r\n"
	if _, err := io.WriteString(app, call); err != nil {
		t.Fatal(err)
	}
	waitInq(t, proxy, len(call))
	sc := tr.scope()
	if n, _ := sc.State(false); n != 1 {
		t.Fatalf("the test's call not taken: scope pending %d in a record, want 1", n)
	}
	if _, err := io.WriteString(app, tick); err != nil {
		t.Fatal(err)
	}
	waitInq(t, proxy, len(call)+len(tick))
	got := make(chan string, 2)
	go readOnce(proxy, got)
	if r := <-got; r != call+tick {
		t.Fatalf("read %q, want both calls in one read", r)
	}
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's call read with a later one, neither answered: scope pending %d in a replay, want 1", n)
	}
	recordScope := tr.scope()
	if n, _ := recordScope.State(false); n != 0 {
		t.Fatalf("every byte taken: scope pending %d in a record, want 0", n)
	}
	go readOnce(proxy, got)
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("both answered, the handler reading on: scope pending %d, want 0", n)
	}
}

// A handler that reads on while its parser holds a request it read (Hold)
// has not answered it: the scope is held until the hold is released. One
// whose close has begun holds it until the handler is done (forget): the
// close's own reads are no handler waiting for the application.
func TestAHeldRequestOrABegunCloseHoldsTheScope(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	const call = "GET /test HTTP/1.1\r\n\r\n"
	got := make(chan string, 2)
	go readOnce(proxy, got)
	if _, err := io.WriteString(app, call); err != nil {
		t.Fatal(err)
	}
	<-got
	release := proxy.Hold()
	sc := tr.scope()
	go readOnce(proxy, got)
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("a held request, the handler reading on: scope pending %d, want 1", n)
	}
	release()
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("the hold released, the handler reading: scope pending %d, want 0", n)
	}

	app2, closing := acceptedPair(t, tr)
	go readOnce(closing, got)
	if _, err := io.WriteString(app2, call); err != nil {
		t.Fatal(err)
	}
	<-got
	sc = tr.scope()
	if err := closing.Close(); err != nil {
		t.Fatal(err)
	}
	readOnce(closing, got) // the close's own read
	<-got
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("a close begun, its handler not done: scope pending %d, want 1", n)
	}
	tr.forget(closing)
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("the handler done: scope pending %d, want 0", n)
	}
}
