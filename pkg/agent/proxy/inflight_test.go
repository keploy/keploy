//go:build linux

package proxy

import (
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
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

// A connection a settle's watchdog gave up on (GiveUp: its handler read the
// test's call and never answered it) is the settle's loss, named with what
// it held, and holds no later scope: the next test's begin goes on. Calls
// the application queues on it behind the lost work are named (Behind) until
// a test's end charges them. Once its handler passes the lost work, later
// scopes wait for it again.
func TestAConnectionASettleGaveUpOnHoldsNoLaterScopeUntilItsHandlerPassesIt(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	const call, tick = "GET /test HTTP/1.1\r\n\r\n", "GET /tick HTTP/1.1\r\n\r\n"
	got := make(chan string, 2)
	go readOnce(proxy, got)
	if _, err := io.WriteString(app, call); err != nil {
		t.Fatal(err)
	}
	<-got
	// The handler never answers it: the end's scope holds it, and its
	// watchdog gives up on it.
	sc := tr.scope()
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's call read and not answered: scope pending %d, want 1", n)
	}
	var ids []uint64
	for id := range sc.PendingProgress(true) {
		ids = append(ids, id)
	}
	words := sc.GiveUp(ids, true)
	if len(words) != 1 || !strings.Contains(words[0], fmt.Sprintf("connection %d from ", proxy.id)) ||
		!strings.Contains(words[0], "a request its handler had read and not answered") {
		t.Fatalf("give-up words %q: want the connection named with what it held", words)
	}

	// The next test's begin.
	next := tr.scope()
	if n, _ := next.State(true); n != 0 {
		t.Fatalf("a connection given up on: next scope pending %d, want 0", n)
	}
	if p := next.PendingProgress(true); len(p) != 0 {
		t.Fatalf("a connection given up on: next scope progress %v, want none (its watchdog would weigh it)", p)
	}
	if b, _ := next.Behind(); len(b) != 0 {
		t.Fatalf("nothing queued behind the lost work: behind %q", b)
	}

	// The application queues its next call on it, behind the lost work.
	if _, err := io.WriteString(app, tick); err != nil {
		t.Fatal(err)
	}
	waitInq(t, proxy, len(tick))
	end := tr.scope()
	if n, _ := end.State(true); n != 0 {
		t.Fatalf("a call queued behind the lost work: scope pending %d, want 0", n)
	}
	b, charge := end.Behind()
	if len(b) != 1 || !strings.Contains(b[0], fmt.Sprintf("received %d byte(s) after keploy gave up on it", len(tick))) {
		t.Fatalf("a call queued behind the lost work: behind %q", b)
	}
	charge()
	after := tr.scope()
	_, _ = after.State(true)
	if b, _ := after.Behind(); len(b) != 0 {
		t.Fatalf("charged once: behind %q", b)
	}

	// Its handler passes the lost work and is handling the queued call: a
	// scope waits for it again, until it has answered that.
	go readOnce(proxy, got)
	if r := <-got; r != tick {
		t.Fatalf("read %q", r)
	}
	again := tr.scope()
	if n, _ := again.State(true); n != 1 {
		t.Fatalf("its handler past the lost work, a call read and not answered: scope pending %d, want 1", n)
	}
	go readOnce(proxy, got)
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if n, _ := again.State(true); n != 0 {
		t.Fatalf("its handler back reading: scope pending %d, want 0", n)
	}
}

// streamParser stands for a parser that answers several requests on one
// connection at once (HTTP/2's streams): its frame reader reads the
// application's next request, holds it, and reads on at once, and each
// request's hold is released as that request is answered, in any order.
type streamParser struct {
	t     *testing.T
	tr    *inflight
	app   net.Conn
	proxy *trackedConn
	got   chan string
}

func newStreamParser(t *testing.T) *streamParser {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	p := &streamParser{t: t, tr: tr, app: app, proxy: proxy, got: make(chan string, 4)}
	go readOnce(proxy, p.got)
	return p
}

// send has the application send req, with the frame reader waiting for it.
func (p *streamParser) send(req string) {
	p.t.Helper()
	if _, err := io.WriteString(p.app, req); err != nil {
		p.t.Fatal(err)
	}
}

// open sends req (unless sent) and has the frame reader read it, hold it and
// read on.
func (p *streamParser) open(req string, sent bool) (release func()) {
	p.t.Helper()
	if !sent {
		p.send(req)
	}
	if r := <-p.got; r != req {
		p.t.Fatalf("read %q, want %q", r, req)
	}
	release = p.proxy.Hold()
	go readOnce(p.proxy, p.got)
	waitSettled(p.t, p.tr, func() bool { return p.proxy.reading.Load() == 1 })
	return release
}

// A connection that answers several requests at once (HTTP/2's streams)
// holds a scope only while it holds a request the scope's cut covers: the
// test's own request, held, holds it however many later ones the
// application opens and has answered, and once that one is answered, the
// later ones, held at every instant, do not. Nor do they move its work for
// the watchdog: what it weighs moves only as the scope's own holds go.
func TestALaterStreamOnAMultiplexedConnectionDoesNotHoldTheScope(t *testing.T) {
	const call = "HEADERS stream 1"
	p := newStreamParser(t)
	test := p.open(call, false)
	sc := p.tr.scope()
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's request held: scope pending %d, want 1", n)
	}
	moved := sc.PendingProgress(true)[p.proxy.id]
	// Background requests overlap at every instant, each answered while
	// the next is held: the test's request is still held.
	release := p.open("HEADERS stream 3", false)
	for i := 5; i < 11; i += 2 {
		next := p.open(fmt.Sprintf("HEADERS stream %d", i), false)
		release()
		release = next
		if n, _ := sc.State(true); n != 1 {
			t.Fatalf("later requests answered, the test's held (stream %d open): scope pending %d, want 1", i, n)
		}
		if got := sc.PendingProgress(true)[p.proxy.id]; got != moved {
			t.Fatalf("later requests moved the scope's work for the watchdog: %d, was %d", got, moved)
		}
	}
	// The frame reader takes a later request and is not reading as the
	// test's request is answered: what it handles now is not the scope's.
	p.send("HEADERS stream 11")
	if r := <-p.got; r != "HEADERS stream 11" {
		t.Fatalf("read %q", r)
	}
	test()
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("the test's request answered, a later one held, the frame reader busy: scope pending %d, want 0", n)
	}
	go readOnce(p.proxy, p.got)
	waitSettled(t, p.tr, func() bool { return p.proxy.reading.Load() == 1 })
	// A scope that begins now waits for the request held now, and only
	// until it is answered.
	next := p.tr.scope()
	if n, _ := next.State(true); n != 1 {
		t.Fatalf("a request held as a scope begins: next scope pending %d, want 1", n)
	}
	later := p.open("HEADERS stream 13", false)
	release()
	if n, _ := next.State(true); n != 0 {
		t.Fatalf("the request held as it began answered, a later one held: next scope pending %d, want 0", n)
	}
	later()
}

// The test's request still in the socket when the scope begins is the
// scope's, though the frame reader holds it only after the cut: the scope
// waits for it until it is answered, not for one the application sends
// after the cut, held while the test's is.
func TestARequestReceivedBeforeTheCutAndHeldAfterItHoldsTheScope(t *testing.T) {
	const call, tick = "HEADERS stream 1", "HEADERS stream 3"
	p := newStreamParser(t)
	// The frame reader is busy (not reading) as the test's request arrives.
	if r := func() string { p.send("PING"); return <-p.got }(); r != "PING" {
		t.Fatalf("read %q", r)
	}
	p.send(call)
	waitInq(t, p.proxy, len(call))
	sc := p.tr.scope()
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's request in the socket: scope pending %d, want 1", n)
	}
	go readOnce(p.proxy, p.got)
	test := p.open(call, true)
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's request held after the cut: scope pending %d, want 1", n)
	}
	later := p.open(tick, false)
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's and a later request held: scope pending %d, want 1", n)
	}
	test()
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("the test's request answered, a later one held: scope pending %d, want 0", n)
	}
	later()
}

// A stream opened before a test's window began is not the test's, whether it
// is still held (a long-lived one that keeps sending) or not: the test's end
// waits for the streams opened since, still sending or not, and only until
// they are answered, and its watchdog weighs and names only those. A
// connection accepted after the window began holds the end with each of its
// requests.
func TestAStreamOpenedBeforeTheWindowBeganDoesNotHoldTheTestsEnd(t *testing.T) {
	p := newStreamParser(t)
	before := p.open("HEADERS stream 1", false)
	p.tr.beginWindow()
	test := p.open("HEADERS stream 3", false)
	// The stream opened before keeps sending.
	for i := 0; i < 3; i++ {
		p.send("DATA stream 1")
		if r := <-p.got; r != "DATA stream 1" {
			t.Fatalf("read %q", r)
		}
		go readOnce(p.proxy, p.got)
		waitSettled(t, p.tr, func() bool { return p.proxy.reading.Load() == 1 })
	}
	sc := p.tr.scope()
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's stream held: scope pending %d, want 1", n)
	}
	if got := sc.PendingProgress(true)[p.proxy.id]; got != 1 {
		t.Fatalf("the watchdog weighs %d of the connection's holds, want 1 (the test's)", got)
	}
	if words := sc.GiveUp([]uint64{p.proxy.id}, true); len(words) != 1 || !strings.Contains(words[0], "which held 1 request(s)") {
		t.Fatalf("give-up words %q: want the test's 1 request", words)
	}
	test()
	// The give-up ends with the test's stream: a scope cut now waits for
	// nothing, the stream opened before the window still held.
	if n, _ := p.tr.scope().State(true); n != 0 {
		t.Fatalf("the test's stream answered, one opened before its window still held: scope pending %d, want 0", n)
	}
	// One opened in the window and still sending holds a scope cut now.
	next := p.open("HEADERS stream 5", false)
	again := p.tr.scope()
	if n, _ := again.State(true); n != 1 {
		t.Fatalf("a stream opened in the window: scope pending %d, want 1", n)
	}
	if b, _ := again.Behind(); len(b) != 0 {
		t.Fatalf("nothing left out: behind %q", b)
	}
	next()
	if n, _ := again.State(true); n != 0 {
		t.Fatalf("the window's stream answered: scope pending %d, want 0", n)
	}
	before()
	// A connection accepted after the window began: each request it holds
	// is of the window.
	app, proxy := acceptedPair(t, p.tr)
	got := make(chan string, 1)
	go readOnce(proxy, got)
	if _, err := io.WriteString(app, "HEADERS stream 1"); err != nil {
		t.Fatal(err)
	}
	<-got
	release := proxy.Hold()
	go readOnce(proxy, got)
	waitSettled(t, p.tr, func() bool { return proxy.reading.Load() == 1 })
	late := p.tr.scope()
	if n, _ := late.State(true); n != 1 {
		t.Fatalf("a request on a connection accepted in the window: scope pending %d, want 1", n)
	}
	release()
	if n, _ := late.State(true); n != 0 {
		t.Fatalf("it answered: scope pending %d, want 0", n)
	}
}

// A stream whose opening frame was in the socket as the test's window began
// is opened before it, though the frame reader holds it only after: the
// test's end does not wait for it, only for the stream opened after, and
// neither does an end cut before the frame reader read it.
func TestAStreamInTheSocketAsTheWindowBeganIsNotTheTests(t *testing.T) {
	const before, call = "HEADERS stream 1", "HEADERS stream 3"
	p := newStreamParser(t)
	// The frame reader is busy (not reading) as the stream arrives.
	if r := func() string { p.send("PING"); return <-p.got }(); r != "PING" {
		t.Fatalf("read %q", r)
	}
	p.send(before)
	waitInq(t, p.proxy, len(before))
	p.tr.beginWindow()
	early := p.tr.scope()
	go readOnce(p.proxy, p.got)
	held := p.open(before, true)
	if n, _ := early.State(true); n != 0 {
		t.Fatalf("an end cut as the window began, the stream in the socket then held: scope pending %d, want 0", n)
	}
	test := p.open(call, false)
	sc := p.tr.scope()
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's stream held: scope pending %d, want 1", n)
	}
	test()
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("the test's stream answered, the one in the socket as its window began held: scope pending %d, want 0", n)
	}
	held()
}

// A settle that gives up on a multiplexed connection names the requests its
// cut covers that its parser holds, not the later request its frame reader
// is busy with.
func TestAGiveUpOnAMultiplexedConnectionNamesTheRequestsItCovered(t *testing.T) {
	p := newStreamParser(t)
	p.open("HEADERS stream 1", false)
	sc := p.tr.scope()
	p.send("HEADERS stream 3")
	if r := <-p.got; r != "HEADERS stream 3" {
		t.Fatalf("read %q", r)
	}
	words := sc.GiveUp([]uint64{p.proxy.id}, true)
	want := fmt.Sprintf("connection %d from %s, which held 1 request(s) its parser had read and not answered", p.proxy.id, p.proxy.RemoteAddr())
	if len(words) != 1 || words[0] != want {
		t.Fatalf("give-up words %q, want %q", words, want)
	}
}

// A settle that gave up on a multiplexed connection gave up on the requests
// its cut covered, the test's own still in the socket then: once those are
// answered, no later scope leaves it out, though the application keeps a
// later request held.
func TestAGiveUpOnAMultiplexedConnectionEndsWithTheRequestsItCovered(t *testing.T) {
	const call = "HEADERS stream 1"
	p := newStreamParser(t)
	// The frame reader is busy (not reading) as the test's request arrives.
	if r := func() string { p.send("PING"); return <-p.got }(); r != "PING" {
		t.Fatalf("read %q", r)
	}
	p.send(call)
	waitInq(t, p.proxy, len(call))
	sc := p.tr.scope()
	var ids []uint64
	for id := range sc.PendingProgress(true) {
		ids = append(ids, id)
	}
	want := fmt.Sprintf("%d byte(s) the application had sent that its handler had not read", len(call))
	if words := sc.GiveUp(ids, true); len(words) != 1 || !strings.Contains(words[0], want) {
		t.Fatalf("give-up words %q: want %q", words, want)
	}
	go readOnce(p.proxy, p.got)
	test := p.open(call, true)
	later := p.open("HEADERS stream 3", false)
	if n, _ := p.tr.scope().State(true); n != 0 {
		t.Fatalf("a connection given up on: next scope pending %d, want 0", n)
	}
	test()
	// The given-up request answered: a scope that begins now waits for the
	// request held now.
	again := p.tr.scope()
	if n, _ := again.State(true); n != 1 {
		t.Fatalf("the given-up request answered, a later one held: scope pending %d, want 1", n)
	}
	if b, _ := again.Behind(); len(b) != 0 {
		t.Fatalf("nothing left out: behind %q", b)
	}
	later()
	if n, _ := again.State(true); n != 0 {
		t.Fatalf("every request it covered answered: scope pending %d, want 0", n)
	}
}

// A scope names, in a replay, each connection it sees done while keploy
// cannot tell whether its parser still holds a request, so the test's end is
// not verified with it: one served by a parser that reads ahead and does not
// say what it holds (servedBy), and one whose parser's mark that it cannot
// tell for now (MarkUnknown) stood when its handler was found holding none,
// whether that was noted as its handler went back to reading past the cut
// (settled) or is read from its view now (a handler waiting out an idle
// answer, whose read past the cut no change has noted since). It stays named
// once the connection is dropped as done. A record names none.
func TestAScopeNamesTheConnectionsWhoseHeldRequestsCannotBeTold(t *testing.T) {
	const call = "GET /test HTTP/1.1\r\n\r\n"
	got := make(chan string, 8)
	named := func(sc *scope, what, want string) {
		t.Helper()
		for i := 0; i < 2; i++ {
			n, u := sc.State(true)
			if n != 0 || len(u) != 1 || u[0] != want {
				t.Fatalf("%s (look %d): scope pending %d, unknown %q; want 0 and %q", what, i+1, n, u, want)
			}
		}
	}

	tr := &inflight{}
	_, ahead := acceptedPair(t, tr)
	ahead.servedBy(integrations.MONGO_V2, readAheadParser{holds: false})
	go readOnce(ahead, got)
	waitSettled(t, tr, func() bool { return ahead.reading.Load() == 1 })
	named(tr.scope(), "a read-ahead parser that does not say what it holds", string(integrations.MONGO_V2))
	if _, u := tr.scope().State(false); len(u) != 0 {
		t.Fatalf("a record: unknown %q, want none", u)
	}

	// Marked while its handler answered the test's call, then back to
	// reading past the cut: the note it went back with says so.
	tr = &inflight{}
	app, marked := acceptedPair(t, tr)
	go readOnce(marked, got)
	if _, err := io.WriteString(app, call); err != nil {
		t.Fatal(err)
	}
	<-got
	sc := tr.scope()
	release := marked.MarkUnknown("mysql (a statement's result sets open)")
	go readOnce(marked, got)
	waitSettled(t, tr, func() bool { return marked.reading.Load() == 1 })
	named(sc, "a mark standing as its handler went back to reading", "mysql (a statement's result sets open)")
	release()

	// Marked and waiting out an idle answer, then a read past the cut while
	// both stand: its view now says so.
	tr = &inflight{}
	app, idle := acceptedPair(t, tr)
	releaseIdle := idle.MarkIdle()
	releaseMark := idle.MarkUnknown("mongo_v2 (a moreToCome group open)")
	go readOnce(idle, got)
	if _, err := io.WriteString(app, call); err != nil {
		t.Fatal(err)
	}
	<-got
	named(tr.scope(), "a mark standing while its handler waits out an idle answer", "mongo_v2 (a moreToCome group open)")
	releaseMark()
	releaseIdle()
}

// A parser that holds, marks, or releases a hold or a mark while its handler
// answers the test's call (neither reading nor waiting out an idle answer)
// has not answered it: the scope holds the call until the handler goes back
// to reading.
func TestAHoldOrMarkChangeWhileTheHandlerAnswersDoesNotSettleTheScope(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	const call = "GET /test HTTP/1.1\r\n\r\n"
	got := make(chan string, 2)
	go readOnce(proxy, got)
	if _, err := io.WriteString(app, call); err != nil {
		t.Fatal(err)
	}
	<-got
	sc := tr.scope()
	proxy.Hold()()
	proxy.MarkUnknown("mongo_v2 (a moreToCome group open)")()
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("a hold and a mark changed while the handler answers: scope pending %d, want 1", n)
	}
	go readOnce(proxy, got)
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if n, u := sc.State(true); n != 0 || len(u) != 0 {
		t.Fatalf("its handler back reading: scope pending %d, unknown %q; want 0 and none", n, u)
	}
}

// A settle's watchdog gives up only on the connections it names (those that
// did not move): another that still holds the scope's work is kept, and the
// scope waits for it until its handler has answered.
func TestAGiveUpKeepsTheConnectionsItDoesNotName(t *testing.T) {
	tr := &inflight{}
	const call = "GET /test HTTP/1.1\r\n\r\n"
	got := make(chan string, 4)
	app1, stuck := acceptedPair(t, tr)
	app2, moved := acceptedPair(t, tr)
	for _, c := range []struct {
		app   net.Conn
		proxy *trackedConn
	}{{app1, stuck}, {app2, moved}} {
		go readOnce(c.proxy, got)
		if _, err := io.WriteString(c.app, call); err != nil {
			t.Fatal(err)
		}
		<-got
	}
	sc := tr.scope()
	if n, _ := sc.State(true); n != 2 {
		t.Fatalf("two calls read and not answered: scope pending %d, want 2", n)
	}
	words := sc.GiveUp([]uint64{stuck.id}, true)
	if len(words) != 1 || !strings.Contains(words[0], fmt.Sprintf("connection %d from ", stuck.id)) {
		t.Fatalf("give-up words %q: want the named connection only", words)
	}
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the connection not named still holds its call: scope pending %d, want 1", n)
	}
	if p := sc.PendingProgress(true); len(p) != 1 {
		t.Fatalf("scope progress %v, want connection %d only", p, moved.id)
	} else if _, ok := p[moved.id]; !ok {
		t.Fatalf("scope progress %v, want connection %d only", p, moved.id)
	}
	go readOnce(moved, got)
	waitSettled(t, tr, func() bool { return moved.reading.Load() == 1 })
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("its handler back reading: scope pending %d, want 0", n)
	}
}
