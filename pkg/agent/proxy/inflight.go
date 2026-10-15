package proxy

import (
	"errors"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
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
// blocked reading it, its parser holds no request it read
// (integrations.HoldRequest: a parser that reads ahead is blocked reading
// while it still holds one), and its socket holds no unread byte. Each change
// of a connection's state (a read begins or returns, a request is held or
// released, the connection closes) wakes whoever waits on Changed, so a wait
// never polls a clock. Progress, for the agent's watchdog, counts only the
// changes that move work: a read that returns bytes or ends the connection, a
// hold, a release, an accept, a close. A read that waited out its deadline
// with nothing (a parser polling an idle connection) is no progress.
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
	// held counts the requests its parser read and holds, not yet answered
	// (Hold).
	held atomic.Int32
	// raw reads how many bytes the socket holds unread; nil for a conn
	// with no socket under it.
	raw syscall.RawConn
	// forgotten: its handler is done with it (forget).
	forgotten atomic.Bool
	// unknown names the protocol of a parser serving it that reads ahead
	// and does not mark what it holds (integrations.HoldsUnknown): whether
	// it holds a request cannot be told.
	unknown atomic.Pointer[string]
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

// forget drops tc: its handler is done with it, and has filed whatever miss
// it returned (the proxy files it before the handler is done).
func (t *inflight) forget(tc *trackedConn) {
	if tc == nil || !tc.forgotten.CompareAndSwap(false, true) {
		return
	}
	t.mu.Lock()
	delete(t.conns, tc)
	t.mu.Unlock()
	t.changed()
}

// changed is a change that moves work: progress, and a wake-up.
func (t *inflight) changed() {
	t.progress.Add(1)
	t.wake()
}

// wake wakes whoever waits on Changed, with no progress.
func (t *inflight) wake() {
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
// work), a request read and not yet answered: its handler is not blocked
// reading, or its parser holds one (Hold). In a record a request waits for
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
		if tc.forgotten.Load() {
			continue
		}
		if tc.unread() > 0 || (answers && (tc.reading.Load() == 0 || tc.held.Load() > 0)) {
			n++
		}
	}
	return n
}

// Unknown are the protocols of the connections, still kept, whose parser
// reads ahead and does not say which requests it holds: whether they hold
// one cannot be told, in a replay (answers). Sorted, each once.
func (t *inflight) Unknown(answers bool) []string {
	if !answers {
		return nil
	}
	t.mu.Lock()
	seen := map[string]bool{}
	for tc := range t.conns {
		if tc.forgotten.Load() {
			continue
		}
		if p := tc.unknown.Load(); p != nil {
			seen[*p] = true
		}
	}
	t.mu.Unlock()
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// servedBy notes the parser that serves c in a replay: one that reads ahead
// and does not mark what it holds leaves c's requests unknown.
func (c *trackedConn) servedBy(t integrations.IntegrationType, parser any) {
	if !integrations.HoldsUnknown(t, parser) {
		return
	}
	name := string(t)
	c.unknown.Store(&name)
	c.t.changed()
}

// Progress is the count of changes so far.
func (t *inflight) Progress() uint64 { return t.progress.Load() }

func (c *trackedConn) Read(b []byte) (int, error) {
	c.reading.Add(1)
	c.t.wake()
	n, err := c.Conn.Read(b)
	c.reading.Add(-1)
	if n == 0 && isTimeout(err) {
		// Waited out its deadline with nothing: no work moved.
		c.t.wake()
	} else {
		c.t.changed()
	}
	return n, err
}

// Hold marks a request c's parser read and holds, not yet answered
// (integrations.RequestHolder); release unmarks it, once.
func (c *trackedConn) Hold() (release func()) {
	c.held.Add(1)
	c.t.changed()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.held.Add(-1)
			c.t.changed()
		})
	}
}

// SyscallConn is the socket under c (syscall.Conn), for what reads the
// kernel's state of it (util.ReceivedNothing): c reads and writes through
// its own methods only.
func (c *trackedConn) SyscallConn() (syscall.RawConn, error) {
	if c.raw == nil {
		return nil, errors.New("no socket under this connection")
	}
	return c.raw, nil
}

// Close closes the connection; it is kept until its handler is done
// (forget). A parser that closes the connection and then returns its miss
// (SQS's) has not filed the miss yet: until the proxy files it, the
// connection holds the test's work, as a handler not blocked reading does.
func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.t.changed()
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

// InFlightUnknown implements agent.InFlightReader.
func (p *Proxy) InFlightUnknown(answers bool) []string { return p.inflight.Unknown(answers) }

// InFlightProgress implements agent.InFlightReader.
func (p *Proxy) InFlightProgress() uint64 { return p.inflight.Progress() }

var (
	_ agent.InFlightReader       = (*Proxy)(nil)
	_ integrations.RequestHolder = (*trackedConn)(nil)
	_ syscall.Conn               = (*trackedConn)(nil)
)
