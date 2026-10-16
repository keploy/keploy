//go:build linux

package proxy

import (
	"net"
	"runtime"
	"sync"
	"syscall"
	"testing"
)

// A request that sits in the socket while the parser's read is in progress
// (its goroutine parked in the netpoller, not yet run), and that the parser
// takes while a test's end looks at the connection, is seen either unread or
// taken, never neither: State reads the socket first, then the connection's
// counts at one instant. Read in another order, State called the connection
// settled while its parser held the request, unanswered.

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
// the parser's goroutine runs between the reader's last atomic load and its
// SIOCINQ.
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
	for proxy.unread() == 0 {
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
