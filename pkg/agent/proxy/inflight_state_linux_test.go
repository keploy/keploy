//go:build linux

package proxy

import (
	"io"
	"net"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A request that sits in the socket while the parser's read is in progress
// (its goroutine parked in the netpoller, not yet run), and that the parser
// takes while a test's end looks at the connection, is seen either untaken or
// taken, never neither: State reads the connection's counts at one instant,
// then the socket's count of what it received, so a read that returns between
// the two counts its bytes untaken. With what the socket holds (SIOCINQ) in
// place of what it received, read after the counts, State called the
// connection settled while its parser held the request, unanswered.

// gatedConn: a Read waits at gate before it reads the socket, as a parser's
// goroutine parked in the netpoller does until it runs.
type gatedConn struct {
	net.Conn
	gate chan struct{}
}

func (g *gatedConn) Read(b []byte) (int, error) {
	<-g.gate
	return g.Conn.Read(b)
}

// hookRaw runs before once, then reads the socket as the real RawConn does:
// the parser's goroutine runs between the reader's view of the counts and its
// read of the socket.
type hookRaw struct {
	syscall.RawConn
	once   sync.Once
	before func()
}

func (h *hookRaw) Control(f func(fd uintptr)) error {
	h.once.Do(h.before)
	return h.RawConn.Control(f)
}

// aRequestWaitingForAParkedRead: the application's request is in the socket
// while the parser's read is in progress, not yet run. The hook it installs
// runs that read to its return; the parser then holds the request, unanswered,
// until the test ends.
func aRequestWaitingForAParkedRead(t *testing.T, tr *inflight) *trackedConn {
	t.Helper()
	app, proxy := acceptedPair(t, tr)
	gate := make(chan struct{})
	var openGate sync.Once
	open := func() { openGate.Do(func() { close(gate) }) }
	proxy.Conn = &gatedConn{Conn: proxy.Conn, gate: gate}
	returned, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(done); open() })
	go func() {
		buf := make([]byte, 64)
		_, _ = proxy.Read(buf)
		close(returned)
		<-done // matching the request: not answered while the test looks
	}()
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if _, err := app.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	for inq(proxy) == 0 {
		runtime.Gosched()
	}
	proxy.raw = &hookRaw{RawConn: proxy.raw, before: func() { open(); <-returned }}
	return proxy
}

func TestStateSeesARequestItsParserTakesWhileItLooks(t *testing.T) {
	tr := &inflight{}
	proxy := aRequestWaitingForAParkedRead(t, tr)
	pending, unknown := tr.State(true)
	now, _ := tr.State(true)
	if pending != 1 {
		t.Fatalf("State: pending %d %v while the parser holds the request it took during the call (reading %d; a State read now says %d): the connection was never settled while State looked",
			pending, unknown, proxy.reading.Load(), now)
	}
}

func TestPendingSeesARequestItsParserTakesWhileItLooks(t *testing.T) {
	tr := &inflight{}
	aRequestWaitingForAParkedRead(t, tr)
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("Pending: pending %d, want 1", n)
	}
}

// A request the parser's read has taken from the socket, and not yet returned
// (the scheduler ran something else between the read's system call and its
// return, as a loaded host's does), is in neither place State looked before:
// the socket holds none of it, and the connection still counts a read in
// progress, which is how a handler waiting for the application looks. State
// called it settled, and a test's end served the pool before the parser
// matched its call (the fire-and-forget lane's held TCP publish, its match
// stopped at no_mocks). A read that has taken bytes it has not returned holds
// work, in a replay and in a record alike.

// lateReturnConn: a Read takes what the socket holds, then waits at gate
// before it returns it.
type lateReturnConn struct {
	net.Conn
	once sync.Once
	took chan struct{}
	gate chan struct{}
}

func (l *lateReturnConn) Read(b []byte) (int, error) {
	n, err := l.Conn.Read(b)
	l.once.Do(func() { close(l.took) })
	<-l.gate
	return n, err
}

func TestARequestAReadHasTakenAndNotReturnedIsWork(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	late := &lateReturnConn{Conn: proxy.Conn, took: make(chan struct{}), gate: make(chan struct{})}
	proxy.Conn = late
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	waitUnread(t, proxy)
	t.Cleanup(sync.OnceFunc(func() { close(late.gate) }))
	returned := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := proxy.Read(buf)
		returned <- buf[:n]
	}()
	<-late.took
	if u := inq(proxy); u != 0 {
		t.Fatalf("the read took the request: the socket still holds %d byte(s)", u)
	}
	if r := proxy.reading.Load(); r != 1 {
		t.Fatalf("the read has not returned: reading %d, want 1", r)
	}
	if pending, unknown := tr.State(true); pending != 1 {
		t.Fatalf("State in a replay: pending %d %v while a read holds the request it took and has not returned, want 1", pending, unknown)
	}
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("Pending in a replay: %d, want 1", n)
	}
	if pending, _ := tr.State(false); pending != 1 {
		t.Fatalf("State in a record: pending %d while a read holds bytes it took and has not returned, want 1", pending)
	}
	if n := tr.Pending(false); n != 1 {
		t.Fatalf("Pending in a record: %d, want 1", n)
	}
	if _, ok := tr.PendingProgress(true)[proxy.id]; !ok {
		t.Fatal("PendingProgress: the connection is not one that holds work")
	}
	late.gate <- struct{}{}
	if got := string(<-returned); got != "publish 1\n" {
		t.Fatalf("read %q", got)
	}
	// Returned: the parser has it (busy in a replay until it blocks reading
	// or holds it), and a record holds nothing more.
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("a request read and not answered: pending %d in a replay, want 1", n)
	}
	if n := tr.Pending(false); n != 0 {
		t.Fatalf("a request read: pending %d in a record, want 0", n)
	}
}

// The kernel's count of what a socket received takes the application's FIN as
// one; it is no byte, so a connection whose every byte its reads returned
// holds none untaken once the FIN is in: before a read returns it (CLOSE_WAIT)
// and after. A socket in TCP_CLOSE after the FIN (its FIN_WAIT2 ended by it,
// after the proxy's half-close; its LAST_ACK ended, the proxy's half-close
// after it) has counts that do not tell the FIN from a byte until a read
// returns the stream's end; until then it counts as one, and a handler
// blocked reading gets it at once.
func TestTheApplicationsFINIsNoUntakenByte(t *testing.T) {
	t.Run("in CLOSE_WAIT", func(t *testing.T) {
		tr := &inflight{}
		app, proxy := acceptedPair(t, tr)
		if _, err := app.Write([]byte("bye")); err != nil {
			t.Fatal(err)
		}
		_ = app.Close()
		buf := make([]byte, 64)
		if n, err := proxy.Read(buf); n != 3 || err != nil {
			t.Fatalf("read %d %v", n, err)
		}
		waitState(t, proxy, tcpCloseWait)
		if n := tr.Pending(false); n != 0 {
			t.Fatalf("every byte read, the FIN in and unread: pending %d in a record, want 0", n)
		}
		if n, err := proxy.Read(buf); n != 0 || err != io.EOF {
			t.Fatalf("read %d %v, want the end", n, err)
		}
		if n := tr.Pending(false); n != 0 {
			t.Fatalf("every byte read and the end: pending %d in a record, want 0", n)
		}
	})
	t.Run("closed by it after a half-close", func(t *testing.T) {
		tr := &inflight{}
		app, proxy := acceptedPair(t, tr)
		if err := proxy.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		if n, err := app.Read(buf); n != 0 || err != io.EOF {
			t.Fatalf("the application read %d %v, want the proxy's end", n, err)
		}
		_ = app.Close()
		waitState(t, proxy, tcpClose)
		if n := tr.Pending(false); n != 1 {
			t.Fatalf("closed by a FIN no read returned: pending %d in a record, want 1 (the kernel's counts alone do not tell it from a byte)", n)
		}
		if n, err := proxy.Read(buf); n != 0 || err != io.EOF {
			t.Fatalf("read %d %v, want the end", n, err)
		}
		if n := tr.Pending(false); n != 0 {
			t.Fatalf("closed by a FIN a read returned: pending %d in a record, want 0", n)
		}
	})
	t.Run("closed as its LAST_ACK ended", func(t *testing.T) {
		tr := &inflight{}
		app, proxy := acceptedPair(t, tr)
		_ = app.Close()
		waitState(t, proxy, tcpCloseWait)
		if err := proxy.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		waitState(t, proxy, tcpClose)
		if n := tr.Pending(false); n != 1 {
			t.Fatalf("closed after its LAST_ACK, the FIN no read returned: pending %d in a record, want 1", n)
		}
		buf := make([]byte, 8)
		if n, err := proxy.Read(buf); n != 0 || err != io.EOF {
			t.Fatalf("read %d %v, want the end", n, err)
		}
		if n := tr.Pending(false); n != 0 {
			t.Fatalf("closed after its LAST_ACK, the end read: pending %d in a record, want 0", n)
		}
	})
}

// A hold on a client's hang-up (HoldHangUp, as the HTTP/1 ingress's watch of
// a live call's client takes it) holds work once the client shows gone, in a
// record as in a replay, until the hang-up is told on (HangUpTold): from then
// on the connection holds no test's work, whatever its socket shows. Here it
// shows the shape a live call's left in a test run: TCP_CLOSE, the client's
// kernel having reset it when one more write came after its FIN, with that
// FIN counted as a byte no read returned, and no read in progress (its
// handler waits on the app). A settle's scope cut before it lets it go, and
// a watchdog has nothing to give up on.
func TestAClientsHangUpToldOnHoldsNoTestsWork(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	release := proxy.HoldHangUp()
	defer release()
	s := tr.scope()
	_ = app.Close()
	waitState(t, proxy, tcpCloseWait)
	if n := tr.Pending(false); n != 1 {
		t.Fatalf("the client gone, its hang-up held and not told: pending %d in a record, want 1", n)
	}
	if _, err := proxy.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	waitState(t, proxy, tcpClose)
	if v := proxy.view(); proxy.untaken(v) != 1 {
		t.Fatalf("precondition: reset after the client's FIN, untaken %d, want the FIN counted as 1", proxy.untaken(v))
	}
	if n, _ := s.State(true); n != 1 {
		t.Fatalf("not told: the scope's pending %d, want 1", n)
	}
	proxy.HangUpTold()
	if n := tr.Pending(false); n != 0 {
		t.Fatalf("told on: pending %d in a record, want 0", n)
	}
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("told on: pending %d in a replay, want 0", n)
	}
	if n, _ := s.State(true); n != 0 {
		t.Fatalf("told on: the scope's pending %d, want 0", n)
	}
	later := tr.scope()
	if p := later.PendingProgress(true); len(p) != 0 {
		t.Fatalf("told on: a later scope waits on %v", p)
	}
	if words := later.GiveUp([]uint64{proxy.id}, true); len(words) != 0 {
		t.Fatalf("told on: a watchdog gave up on %v", words)
	}
}

// waitState waits until c's socket is in TCP state want: the peer's segments
// cross loopback, which no event of the tracker reports.
func waitState(t *testing.T, c *trackedConn, want uint8) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, state, ok := socketReceived(c.raw)
		if !ok {
			t.Fatal("the socket cannot be asked")
		}
		if state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the socket is in TCP state %d, never %d", state, want)
		}
		runtime.Gosched()
	}
}

// A read's return counts what it delivered and stops counting as a read in
// progress at one instant: a view between the two would see the read still
// waiting with its bytes already counted delivered, and none untaken, the
// window a loaded host keeps open.
func TestAReadsReturnIsOneChange(t *testing.T) {
	tr := &inflight{}
	_, proxy := acceptedPair(t, tr)
	proxy.count(&proxy.reading, 1)
	proxy.mu.Lock()
	done := make(chan struct{})
	go func() {
		proxy.returned(7, false)
		close(done)
	}()
	for i := 0; i < 100; i++ {
		runtime.Gosched()
	}
	if d, r := proxy.delivered.Load(), proxy.reading.Load(); d != 0 || r != 1 {
		proxy.mu.Unlock()
		t.Fatalf("a return changed delivered %d, reading %d without the connection's lock", d, r)
	}
	proxy.mu.Unlock()
	<-done
	if v := proxy.view(); v.delivered != 7 || v.reading != 0 {
		t.Fatalf("after the return: delivered %d, reading %d, want 7, 0", v.delivered, v.reading)
	}
}
