package proxy

import (
	"net"

	"go.keploy.io/server/v3/pkg/agent"
	"sync"
	"sync/atomic"
	"syscall"
)

// The proxy's share of a test's end count, under the proven switch (design
// v14 §5.2; release1-cut §5.1 item 5). Once every connection the kernel
// completed to the proxy has been accepted (the agent's ProxyConns), the
// test's calls can still be in the proxy's hands in two ways:
//
//   - bytes the application sent that the proxy has not read: they wait in
//     an accepted socket (SIOCINQ);
//   - a request the proxy read and has not answered: the connection's handler
//     is working on it, not waiting for the application's next bytes.
//
// inflight keeps every accepted connection and tells, by events and counts
// only, when none holds either: a connection is settled when its handler is
// blocked reading it and its socket holds no unread byte. Each change of a
// connection's state (a read begins or returns, the connection closes) moves
// the progress count and wakes whoever waits on Changed, so a wait never
// polls a clock.
type inflight struct {
	mu    sync.Mutex
	conns map[*trackedConn]struct{}
	// sig is closed, and replaced, at every change.
	sig chan struct{}
	// progress counts the changes, for the agent's watchdog.
	progress atomic.Uint64
}

// trackedConn is an accepted connection inflight keeps: its reads tell when
// its handler waits for the application.
type trackedConn struct {
	net.Conn
	t *inflight
	// reading counts the reads in progress: one is the handler waiting for
	// the application's next bytes.
	reading atomic.Int32
	// raw reads how many bytes the socket holds unread; nil for a conn
	// with no socket under it.
	raw    syscall.RawConn
	closed atomic.Bool
}

// track keeps conn, and returns it wrapped so its reads are seen.
func (t *inflight) track(conn net.Conn) *trackedConn {
	tc := &trackedConn{Conn: conn, t: t}
	if sc, ok := conn.(syscall.Conn); ok {
		if raw, err := sc.SyscallConn(); err == nil {
			tc.raw = raw
		}
	}
	t.mu.Lock()
	if t.conns == nil {
		t.conns = map[*trackedConn]struct{}{}
	}
	t.conns[tc] = struct{}{}
	t.mu.Unlock()
	t.changed()
	return tc
}

// forget drops tc: its handler is done with it.
func (t *inflight) forget(tc *trackedConn) {
	if tc == nil || !tc.closed.CompareAndSwap(false, true) {
		return
	}
	t.mu.Lock()
	delete(t.conns, tc)
	t.mu.Unlock()
	t.changed()
}

func (t *inflight) changed() {
	t.progress.Add(1)
	t.mu.Lock()
	if t.sig != nil {
		close(t.sig)
		t.sig = nil
	}
	t.mu.Unlock()
}

// Changed returns a channel that is closed at the next change. Take it before
// reading the state it guards.
func (t *inflight) Changed() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sig == nil {
		t.sig = make(chan struct{})
	}
	return t.sig
}

// Pending is how many accepted connections still hold work: unread bytes,
// and, when answers is set (a replay, whose every answer is the proxy's own
// work), a request read and not yet answered. In a record a request waits for
// its real server, which the end never waits on (verify-v14 M2): only unread
// bytes count there.
func (t *inflight) Pending(answers bool) int {
	t.mu.Lock()
	conns := make([]*trackedConn, 0, len(t.conns))
	for tc := range t.conns {
		conns = append(conns, tc)
	}
	t.mu.Unlock()
	n := 0
	for _, tc := range conns {
		if tc.closed.Load() {
			continue
		}
		if tc.unread() > 0 || (answers && tc.reading.Load() == 0) {
			n++
		}
	}
	return n
}

// Progress is the count of changes so far.
func (t *inflight) Progress() uint64 { return t.progress.Load() }

func (c *trackedConn) Read(b []byte) (int, error) {
	c.reading.Add(1)
	c.t.changed()
	n, err := c.Conn.Read(b)
	c.reading.Add(-1)
	c.t.changed()
	return n, err
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.t.forget(c)
	return err
}

// CloseWrite forwards a half-close to the conn underneath.
func (c *trackedConn) CloseWrite() error {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

// NetConn is the conn c reads and writes through (the *tls.Conn convention).
func (c *trackedConn) NetConn() net.Conn { return c.Conn }

// unread is how many bytes the socket holds unread (socketUnread); 0 for a
// conn with no socket under it.
func (c *trackedConn) unread() int {
	if c.raw == nil {
		return 0
	}
	return socketUnread(c.raw)
}

// InFlightPending implements agent.InFlightReader.
func (p *Proxy) InFlightPending(answers bool) int { return p.inflight.Pending(answers) }

// InFlightChanged implements agent.InFlightReader.
func (p *Proxy) InFlightChanged() <-chan struct{} { return p.inflight.Changed() }

// InFlightProgress implements agent.InFlightReader.
func (p *Proxy) InFlightProgress() uint64 { return p.inflight.Progress() }

var _ agent.InFlightReader = (*Proxy)(nil)
