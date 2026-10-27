package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The proxy's share of a test's end count, under the proven switch, and of a
// settled read of a test's misses for an agent started settling reads (design
// v14 §5.2; release1-cut §5.1 item 5). Once every connection the kernel
// completed to the proxy has been accepted (the agent's ProxyConns), the
// test's calls can still be in the proxy's hands in two ways:
//
//   - bytes the application sent that the proxy has not read: they wait in
//     an accepted socket, or a read has taken them from it and not returned
//     them yet (untaken);
//   - a request the proxy read and has not answered: the connection's handler
//     is working on it, not waiting for the application's next bytes.
//
// inflight keeps every accepted connection and tells, by events and counts
// only, when none holds either: a connection is settled when its handler is
// blocked reading it, or waits out an answer the dependency held idle
// (integrations.MarkIdle: a long poll that has caught up with the
// recording), its parser holds no request it read (integrations.HoldRequest:
// a parser that reads ahead is blocked reading while it still holds one), and
// no byte the application sent on it is untaken. Each change of a
// connection's state (a read begins or returns, a request is held or
// released, a mark that it cannot tell, or that it waits idle, is set or
// released, the connection closes) wakes whoever waits on Changed, so a wait
// never polls a clock. Progress, for
// the agent's watchdog, counts only the changes that move work: a read that
// returns bytes or ends the connection, a hold, a release, a mark or its
// release, an accept, a close. A read that waited out its deadline with
// nothing (a parser polling an idle connection) is no progress.
type inflight struct {
	mu    sync.Mutex
	conns map[*trackedConn]struct{}
	// sig is closed, and replaced, at every change.
	sig chan struct{}
	// progress counts the changes that move work, anywhere.
	progress atomic.Uint64
	// ids numbers the connections kept, for PendingProgress.
	ids atomic.Uint64
}

// trackedConn is an accepted connection inflight keeps: its reads tell when
// its handler waits for the application.
type trackedConn struct {
	net.Conn
	t *inflight
	// id names it in PendingProgress.
	id uint64
	// progress counts what moved its own work: bytes read, a request held
	// or released, its close.
	progress atomic.Uint64
	// mu orders each change of reading, held, unknownFor, idle, delivered
	// and eof against the reads of them (view): a parser hands a request
	// from one to another (taken, then returned; busy, then held, then
	// reading on; busy, then marked, then reading on; marked, then busy;
	// busy, then idle, then busy), so no order of separate reads sees every
	// hand-off.
	mu sync.Mutex
	// reading counts the reads in progress: one is the handler waiting for
	// the application's next bytes.
	reading atomic.Int32
	// held counts the requests its parser read and holds, not yet answered
	// (Hold).
	held atomic.Int32
	// delivered counts the bytes its reads have returned, and eof says one
	// returned the end of the stream: changed with reading as a read
	// returns, under mu (returned), so a view never sees a read done whose
	// bytes it does not count, or bytes counted whose read is still in
	// progress.
	delivered atomic.Uint64
	eof       atomic.Bool
	// raw reads how many bytes the socket has received (untaken); nil for
	// a conn with no socket under it.
	raw syscall.RawConn
	// forgotten: its handler is done with it (forget).
	forgotten atomic.Bool
	// beforeClose, when set (by the accept, before the handler runs), runs
	// once as Close begins, while the socket can still be read
	// (pairbytes.go).
	beforeClose func()
	closeOnce   sync.Once
	// closing: its Close has begun. A read from then on is the close's own
	// (pairClosing reads through the TLS layer what the socket holds) or one
	// the close is ending, never its handler waiting for the application:
	// until its handler is done (forget), and its miss filed, it holds work.
	closing atomic.Bool
	// tlsLayer is the TLS layer the proxy serves the connection over, once
	// its handshake is done (HoldTLSLayer); nil for a plain one. Its close
	// reads through it what the socket still holds (pairClosing).
	tlsLayer atomic.Pointer[tls.Conn]
	// unknown names the protocol of a parser serving it that reads ahead
	// and does not mark what it holds (integrations.HoldsUnknown): whether
	// it holds a request cannot be told.
	unknown atomic.Pointer[string]
	// unknownFor counts the marks its parser holds that what it holds cannot
	// be told for now (MarkUnknown), and unknownWhat names the protocol of
	// the latest.
	unknownFor  atomic.Int32
	unknownWhat atomic.Pointer[string]
	// idle counts the marks its parser holds that it waits out an answer the
	// dependency held idle (MarkIdle): while one stands, a handler that is
	// not reading is no work of keploy's.
	idle atomic.Int32
	// settled is what its reads had delivered the last time its handler
	// held no request it read (as a read began, or a mark changed, with no
	// request held, its close not begun, and a read in progress or an idle
	// mark standing: busy's rule with nothing untaken), settledMarks
	// whether an unknown mark stood then, and hasSettled whether that ever
	// happened; under mu. A settle's scope reads them (scope): a handler
	// that went back to reading after its reads delivered every byte the
	// scope waits for had answered them, even when the application's next
	// request is already in the socket and no later view sees it idle.
	settled      uint64
	settledMarks bool
	hasSettled   bool
	// lost is the cut a settle's watchdog gave up at (scope.GiveUp): the
	// work up to it was charged to that settle, and no later scope waits
	// for it while its handler has not passed it (scope.leftOut); hasLost
	// says one stands. charged is how far its received bytes have been
	// charged to a test since (scope.Behind): from lost on, the calls the
	// application queued behind the lost work. Under mu.
	lost, charged uint64
	hasLost       bool
	// flights, under the proven switch, is the learn step's tracker its
	// requests are told to (learn.Flight): a read that returns bytes while
	// none is aloft lifts one (flight), and the first write after it lands
	// it, or ends it untold (EndUntold) if that write fails; its half-close
	// or its close, with it still aloft, ends it untold. Set when it is kept
	// (trackTold), before anything reads it; nil, nothing is told. fmu
	// orders the lift against the landing: a handler may read and write
	// from two goroutines. grounded: its close began, and no read lifts one
	// again (the close's own reads, pairClosing, are no request).
	flights  *learn.Tracker
	fmu      sync.Mutex
	flight   *learn.Flight
	grounded bool
	// hangUps counts the holds on a client's hang-up on a TLS handshake
	// keploy makes over it, not yet told (HoldHangUp).
	hangUps atomic.Int32
}

// track keeps conn, and returns it wrapped so its reads are seen.
func (t *inflight) track(conn net.Conn) *trackedConn { return t.trackTold(conn, nil) }

// trackTold is track of a connection whose requests are told to flights
// (trackedConn.flights; nil, to none).
func (t *inflight) trackTold(conn net.Conn, flights *learn.Tracker) *trackedConn {
	tc := &trackedConn{Conn: conn, t: t, id: t.ids.Add(1), flights: flights}
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

// Pending is how many accepted connections still hold work: untaken bytes,
// and, when answers is set (a replay, whose every answer is the proxy's own
// work), a request read and not yet answered: its handler is not blocked
// reading, or its parser holds one (Hold). In a record a request waits for
// its real server, which the end never waits on (verify-v14 M2): only untaken
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
		if tc.pending(answers) {
			n++
		}
	}
	return n
}

// pending reports whether c still holds work (Pending): its counts at one
// instant (view), then its socket (untaken), as State reads it.
func (c *trackedConn) pending(answers bool) bool {
	return c.busyAt(c.view(), answers)
}

// busyAt is busy for c's counts at one instant (v), read before its socket
// (untaken, hungUp).
func (c *trackedConn) busyAt(v connView, answers bool) bool {
	return busy(c.untaken(v), answers, v.reading, v.held, v.idle, c.closing.Load()) || c.hungUp(v)
}

// busy is the rule Pending, State and PendingProgress read a connection by:
// it holds work when bytes the application sent on it are untaken, or, in a
// replay (answers), when its parser holds a request it read, its close has
// begun (closing: until its handler is done its miss may still be filed), or
// its handler is neither blocked reading nor waiting out an idle answer
// (MarkIdle). An idle mark never covers a close.
func busy(untaken uint64, answers bool, reading, held, idle int32, closing bool) bool {
	return untaken > 0 || (answers && (held > 0 || closing || (reading == 0 && idle == 0)))
}

// State is Pending and Unknown read as one view: how many accepted
// connections still hold work, and the protocols of those whose parser does
// not say, or says it cannot tell for now, what they hold (Unknown). A wait
// that read the two apart could see a connection unknown and not pending, then
// known and not pending, while between the reads its parser ended the state
// it could not tell (a moreToCome group's last frame read) and went on
// answering: neither read sees it pending, and the wait would call it
// settled. Within a connection, separate reads in any order miss a hand-off
// (busy to held to reading on, busy to marked, marked to busy): its counts
// are read at one instant (view), then its socket (untaken).
func (t *inflight) State(answers bool) (pending int, unknown []string) {
	t.mu.Lock()
	conns := make([]*trackedConn, 0, len(t.conns))
	for tc := range t.conns {
		conns = append(conns, tc)
	}
	t.mu.Unlock()
	seen := map[string]bool{}
	for _, tc := range conns {
		if tc.forgotten.Load() {
			continue
		}
		// The counts at one instant (view), then the socket (untaken): a
		// read that has taken a request from the socket counts as reading
		// until it returns, so the socket alone, or the counts alone, miss
		// it. Then the parser the proxy noted, which it notes before that
		// parser's first read (servedBy).
		v := tc.view()
		if tc.busyAt(v, answers) {
			pending++
		}
		if answers {
			if p := tc.unknown.Load(); p != nil {
				seen[*p] = true
			}
			if v.marks > 0 {
				if p := tc.unknownWhat.Load(); p != nil {
					seen[*p] = true
				}
			}
		}
	}
	if len(seen) > 0 {
		unknown = make([]string, 0, len(seen))
		for p := range seen {
			unknown = append(unknown, p)
		}
		sort.Strings(unknown)
	}
	return pending, unknown
}

// PendingProgress is, for each connection that holds work now (Pending), the
// count of what moved its own work: a wait whose pending connections' counts
// do not move is stuck, whatever moves elsewhere.
func (t *inflight) PendingProgress(answers bool) map[uint64]uint64 {
	t.mu.Lock()
	conns := make([]*trackedConn, 0, len(t.conns))
	for tc := range t.conns {
		conns = append(conns, tc)
	}
	t.mu.Unlock()
	out := map[uint64]uint64{}
	for _, tc := range conns {
		if tc.forgotten.Load() {
			continue
		}
		if tc.pending(answers) {
			out[tc.id] = tc.progress.Load()
		}
	}
	return out
}

// Unknown are the protocols of the connections, still kept, whose parser
// reads ahead and does not say which requests it holds, or says that for now
// it cannot tell (MarkUnknown): whether they hold one cannot be told, in a
// replay (answers). Sorted, each once.
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
		if tc.unknownFor.Load() > 0 {
			if p := tc.unknownWhat.Load(); p != nil {
				seen[*p] = true
			}
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

// flights is the learn step's tracker an accepted connection tells its
// requests to (learn.Flight), under the proven switch; nil without it.
func (p *Proxy) flights() *learn.Tracker {
	if !provenswitch.On() {
		return nil
	}
	return p.learnTracker
}

// Progress is the count of changes so far.
func (t *inflight) Progress() uint64 { return t.progress.Load() }

func (c *trackedConn) Read(b []byte) (int, error) {
	c.count(&c.reading, 1)
	c.t.wake()
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.lift()
	}
	c.returned(n, errors.Is(err, io.EOF))
	if n == 0 && (err == nil || isTimeout(err) || errors.Is(err, io.EOF)) {
		// Nothing came: a deadline waited out by a parser polling an idle
		// connection, or the end of one it reads again. No work moved.
		c.t.wake()
	} else {
		c.moved()
	}
	return n, err
}

// Write lands the request in flight (learn.Flight) before the answer's
// first bytes go out: the application cannot send the next request on
// what it read before the landing, on this connection or another, so a
// request that waited for the answer is never read as one aloft with it.
// A landing write that fails ends that request untold instead
// (learn.Flight.EndUntold): the application was not given its answer, so no
// serve in it is read as one it had before a later request. A later write that fails ends nothing: c cannot tell the rest of
// an answer from a write after it (the next request's, TLS's close_notify
// once the application read its answer and hung up), and a request whose
// answer is cut after its first write stays landed.
func (c *trackedConn) Write(b []byte) (int, error) {
	var f *learn.Flight
	if len(b) > 0 {
		f = c.land()
	}
	n, err := c.Conn.Write(b)
	if err != nil {
		f.EndUntold()
	}
	return n, err
}

// lift is a read of c that returned bytes: the start of a request in
// flight, unless one is aloft (learn.Tracker.Lift).
func (c *trackedConn) lift() {
	if c.flights == nil {
		return
	}
	c.fmu.Lock()
	if c.flight == nil && !c.grounded {
		c.flight = c.flights.Lift()
	}
	c.fmu.Unlock()
}

// InFlight implements integrations.FlightCarrier: c's request aloft now, nil
// with none, or told to no tracker.
func (c *trackedConn) InFlight() *learn.Flight {
	if c.flights == nil {
		return nil
	}
	c.fmu.Lock()
	defer c.fmu.Unlock()
	return c.flight
}

var _ integrations.FlightCarrier = (*trackedConn)(nil)

// land ends c's request in flight, if one is aloft, as its answer starts
// to go out (learn.Flight.Land), and returns it; nil with none aloft.
func (c *trackedConn) land() *learn.Flight {
	if c.flights == nil {
		return nil
	}
	c.fmu.Lock()
	f := c.flight
	c.flight = nil
	c.fmu.Unlock()
	f.Land()
	return f
}

// endUntold ends c's request in flight, if one is aloft, as one keploy
// writes no answer to (learn.Flight.EndUntold): c closes, or half-closes,
// before its answer starts. ground too stops any later read lifting one.
func (c *trackedConn) endUntold(ground bool) {
	if c.flights == nil {
		return
	}
	c.fmu.Lock()
	f := c.flight
	c.flight = nil
	c.grounded = c.grounded || ground
	c.fmu.Unlock()
	f.EndUntold()
}

// count changes one of c's counts under c.mu (view).
func (c *trackedConn) count(n *atomic.Int32, delta int32) {
	c.mu.Lock()
	n.Add(delta)
	c.noteSettled()
	c.mu.Unlock()
}

// noteSettled notes, under c.mu, what c's reads have delivered when its
// handler holds no request it read (settled).
func (c *trackedConn) noteSettled() {
	if c.held.Load() == 0 && !c.closing.Load() && (c.reading.Load() > 0 || c.idle.Load() > 0) {
		c.settled, c.settledMarks, c.hasSettled = c.delivered.Load(), c.unknownFor.Load() > 0, true
	}
}

// returned is a read of c returning n bytes, and the end of the stream when
// eof: what it delivered is counted as it stops counting as a read in
// progress, at one instant (view).
func (c *trackedConn) returned(n int, eof bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > 0 {
		c.delivered.Add(uint64(n))
	}
	if eof {
		c.eof.Store(true)
	}
	c.reading.Add(-1)
}

// connView is c's counts at one instant (view).
type connView struct {
	// reading, held, marks and idle are its reads in progress, held
	// requests, unknown marks and idle marks.
	reading, held, marks, idle int32
	// delivered is what its reads have returned, and eof whether one
	// returned the end of the stream.
	delivered uint64
	eof       bool
	// hangUps are its holds on a client's hang-up not yet told (HoldHangUp).
	hangUps int32
	// settled, settledMarks and hasSettled are its last note of holding
	// no request (trackedConn.settled).
	settled      uint64
	settledMarks bool
	hasSettled   bool
}

// view is c's counts, read at one instant.
func (c *trackedConn) view() connView {
	c.mu.Lock()
	defer c.mu.Unlock()
	return connView{
		reading: c.reading.Load(), held: c.held.Load(), marks: c.unknownFor.Load(), idle: c.idle.Load(),
		delivered: c.delivered.Load(), eof: c.eof.Load(), hangUps: c.hangUps.Load(),
		settled: c.settled, settledMarks: c.settledMarks, hasSettled: c.hasSettled,
	}
}

// HoldHangUp implements pTls.HangUpHolder: until release, c holds work
// whenever its client is gone and the hang-up is not yet told (hungUp), in a
// record as in a replay. A test's end waits for it by events: release, as
// every change of c's, wakes whoever waits on Changed.
func (c *trackedConn) HoldHangUp() (release func()) {
	c.count(&c.hangUps, 1)
	c.t.changed()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.count(&c.hangUps, -1)
			c.moved()
		})
	}
}

// hungUp reports whether c, holding a client's hang-up not yet told (v's
// hangUps), shows its client gone: a read returned the end of the stream
// (v's eof), or its socket, read after v, has received the client's FIN or
// a reset (clientGone) -- one a read in progress may already have taken,
// its goroutine not run since. The socket not to be asked (its handler has
// closed it) shows nothing: that close follows the hang-up's telling, or is
// keploy's own.
func (c *trackedConn) hungUp(v connView) bool {
	if v.hangUps == 0 {
		return false
	}
	if v.eof {
		return true
	}
	if c.raw == nil {
		return false
	}
	_, state, ok := socketReceived(c.raw)
	return ok && clientGone(state)
}

// moved is a change that moved c's own work: progress for c, and for all.
func (c *trackedConn) moved() {
	c.progress.Add(1)
	c.t.changed()
}

// Hold marks a request c's parser read and holds, not yet answered
// (integrations.RequestHolder); release unmarks it, once.
func (c *trackedConn) Hold() (release func()) {
	c.count(&c.held, 1)
	c.moved()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.count(&c.held, -1)
			c.moved()
		})
	}
}

// MarkUnknown marks that, until release, whether c's parser holds a request
// cannot be told (integrations.UnknownMarker): a test's end that finds the
// mark leaves the test not verified, and does not wait for it. release
// unmarks it, once.
func (c *trackedConn) MarkUnknown(protocol string) (release func()) {
	c.unknownWhat.Store(&protocol)
	c.count(&c.unknownFor, 1)
	c.moved()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.count(&c.unknownFor, -1)
			c.moved()
		})
	}
}

// MarkIdle marks that, until release, c's parser waits out an answer the
// dependency held idle (integrations.IdleMarker): its handler, not reading,
// is no work of keploy's, and a test's end does not wait for it. release
// unmarks it, once.
func (c *trackedConn) MarkIdle() (release func()) {
	c.count(&c.idle, 1)
	c.moved()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.count(&c.idle, -1)
			c.moved()
		})
	}
}

// SyscallConn is the socket under c (syscall.Conn), for what reads the
// kernel's state of it (util.ReceivedNothing) or peeks at it (MSG_PEEK): c
// reads and writes through its own methods only. Nothing may take bytes from
// the socket through it: what c's reads do not return stays untaken, and c
// holds work, until the connection closes.
func (c *trackedConn) SyscallConn() (syscall.RawConn, error) {
	if c.raw == nil {
		return nil, errors.New("no socket under this connection")
	}
	return c.raw, nil
}

// Close closes the connection; it is kept until its handler is done
// (forget). A request still aloft is given no answer (endUntold), and no
// read lifts one again. A parser that closes the connection and then
// returns its miss (SQS's) has not filed the miss yet: until the proxy
// files it, the connection holds the test's work, as a handler not blocked
// reading does.
func (c *trackedConn) Close() error {
	c.endUntold(true)
	c.closing.Store(true)
	if c.beforeClose != nil {
		c.closeOnce.Do(c.beforeClose)
	}
	err := c.Conn.Close()
	c.moved()
	return err
}

// HoldTLSLayer keeps the TLS layer the proxy serves c over
// (pTls.TLSLayerHolder), for its close.
func (c *trackedConn) HoldTLSLayer(l *tls.Conn) { c.tlsLayer.Store(l) }

// CloseWrite forwards a half-close to the conn underneath: a request
// still aloft is given no answer on c (endUntold).
func (c *trackedConn) CloseWrite() error {
	c.endUntold(false)
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

// NetConn is the conn c reads and writes through (the *tls.Conn convention),
// for what sets its socket's options or half-closes it. Nothing may read
// through it: a byte read past c's own Read stays untaken, and c holds work,
// until the connection closes.
func (c *trackedConn) NetConn() net.Conn { return c.Conn }

// untaken is how many bytes the application has sent on c that none of
// c's reads had returned by v (c's counts at one instant, read before it):
// those its socket holds, and those a read in progress has taken from it and
// not returned (its system call done, its goroutine not run since, which on a
// loaded host is as long as the scheduler makes it). The socket alone does not
// show those, and the counts show their read as one waiting for the
// application. They are what the socket has received in all (the kernel's
// count, tcpi_bytes_received, read after v, so a read that returns between
// the two counts as untaken, never as neither) less what v says the reads
// returned. That count takes the application's FIN as one: it is taken off
// once the socket is in a state only a FIN it received leads to, or in
// TCP_CLOSE (reset, or closed as its FIN_WAIT2, CLOSING or LAST_ACK ended)
// after a read returned the stream's end (finReceived). In
// TCP_CLOSE with no end returned the kernel's counts do not tell the FIN from
// a byte (a TLS layer stops reading at the close_notify before it), so it
// counts as one until a read returns it or the handler closes the
// connection. An urgent byte (MSG_OOB), which no read returns, counts so too,
// until the connection closes. 0 for a conn with no socket under it, or whose
// socket its handler has closed (it cannot be asked).
func (c *trackedConn) untaken(v connView) uint64 {
	return c.received(v) - v.delivered
}

// received is how many bytes of the application's c's socket has received,
// its FIN not counted, and never fewer than c's reads delivered (v); what
// they delivered for a conn with no socket under it, or whose socket its
// handler has closed (untaken).
func (c *trackedConn) received(v connView) uint64 {
	if c.raw == nil {
		return v.delivered
	}
	received, state, ok := socketReceived(c.raw)
	if !ok {
		return v.delivered
	}
	if finReceived(state, v.eof) && received > 0 {
		received--
	}
	return max(received, v.delivered)
}

// scope is a settle's share of the connections: those kept when it began
// (inflight.scope), each with the bytes its socket had received then (its
// cut), until its handler has taken them and, in a replay, answered them.
// It began once every byte the settle waits for was received (the agent's
// first wait), so a connection's cut covers them. A connection accepted
// after it began is not in it, and bytes one receives after it began are
// not the settle's: a handler whose reads delivered its cut and that then
// held no request (trackedConn.settled) had answered all of them, however
// soon the application's next request arrived. So a settle waits for work
// that only shrinks, never for an application that keeps calling, on new
// connections or on the ones it kept.
type scope struct {
	t  *inflight
	mu sync.Mutex
	// left maps each connection not yet seen done to its cut.
	left map[*trackedConn]uint64
	// unknown names the protocols of the connections seen done while their
	// parser could not say what it held (State's unknown).
	unknown map[string]bool
	// behind maps each connection it leaves out (leftOut) that has received
	// bytes past what was charged to a test (trackedConn.charged) to its
	// cut (Behind).
	behind map[*trackedConn]uint64
}

// scope begins a scope with every connection kept now, each cut at what its
// socket has received.
func (t *inflight) scope() *scope {
	t.mu.Lock()
	conns := make([]*trackedConn, 0, len(t.conns))
	for tc := range t.conns {
		conns = append(conns, tc)
	}
	t.mu.Unlock()
	left := make(map[*trackedConn]uint64, len(conns))
	for _, tc := range conns {
		left[tc] = tc.received(tc.view())
	}
	return &scope{t: t, left: left, unknown: map[string]bool{}, behind: map[*trackedConn]uint64{}}
}

// leftOut is whether the scope leaves tc out, under s.mu: a settle's
// watchdog gave up on it (GiveUp) and its handler has not passed what it
// held then, so no work on it can settle; that work was charged to the
// settle that gave up. One that has received bytes past what was charged
// (calls the application queued behind the lost work) is noted in behind.
// One whose handler has passed it is waited for again from here, cut at the
// scope's cut.
func (s *scope) leftOut(tc *trackedConn, cut uint64, answers bool) bool {
	tc.mu.Lock()
	lost, has, charged := tc.lost, tc.hasLost, tc.charged
	tc.mu.Unlock()
	if !has {
		return false
	}
	if held, _ := s.holds(tc, lost, answers); !held {
		tc.mu.Lock()
		if tc.hasLost && tc.lost == lost {
			tc.hasLost = false
		}
		tc.mu.Unlock()
		delete(s.behind, tc)
		return false
	}
	if cut > charged {
		s.behind[tc] = cut
	}
	return true
}

// GiveUp gives up, for the settle whose watchdog found them stuck, on each
// connection named (PendingProgress's ids) that still holds the scope's
// work: that work, up to its cut, is charged to the settle (its test is not
// verified, with the words returned, one per connection, saying what it
// held), and no later scope waits for it while its handler has not passed
// it (leftOut).
func (s *scope) GiveUp(ids []uint64, answers bool) []string {
	want := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for tc, cut := range s.left {
		if !want[tc.id] || tc.forgotten.Load() || s.leftOut(tc, cut, answers) {
			continue
		}
		if held, _ := s.holds(tc, cut, answers); !held {
			continue
		}
		out = append(out, tc.name()+", which held "+heldWords(tc, tc.view(), cut, answers))
		tc.mu.Lock()
		tc.lost, tc.charged, tc.hasLost = cut, max(tc.charged, cut), true
		tc.mu.Unlock()
	}
	sort.Strings(out)
	return out
}

// Behind is, for each connection the scope left out (leftOut) that has
// received bytes past what was charged to a test, words saying so: calls
// the application queued behind the lost work, which keploy cannot tell
// were answered. charge charges them, up to the scope's cut: a test's end
// calls it, as the test is not verified with them; a begin leaves them to
// the next end.
func (s *scope) Behind() (conns []string, charge func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.behind) == 0 {
		return nil, func() {}
	}
	cuts := make(map[*trackedConn]uint64, len(s.behind))
	for tc, cut := range s.behind {
		cuts[tc] = cut
		tc.mu.Lock()
		charged := tc.charged
		tc.mu.Unlock()
		conns = append(conns, fmt.Sprintf("%s received %d byte(s) after keploy gave up on it, queued behind what it held then: keploy cannot tell whether the calls in them were answered", tc.name(), cut-min(cut, charged)))
	}
	sort.Strings(conns)
	return conns, func() {
		for tc, cut := range cuts {
			tc.mu.Lock()
			tc.charged = max(tc.charged, cut)
			tc.mu.Unlock()
		}
	}
}

// name names tc in a settle's words: its number and the application's end.
func (c *trackedConn) name() string {
	if c.Conn == nil {
		return fmt.Sprintf("connection %d", c.id)
	}
	return fmt.Sprintf("connection %d from %s", c.id, c.RemoteAddr())
}

// heldWords says what tc, seen in v, holds of a scope's work cut at cut.
func heldWords(tc *trackedConn, v connView, cut uint64, answers bool) string {
	if v.delivered < cut {
		return fmt.Sprintf("%d byte(s) the application had sent that its handler had not read", cut-v.delivered)
	}
	var what []string
	if v.held > 0 {
		what = append(what, fmt.Sprintf("%d request(s) its parser had read and not answered", v.held))
	}
	if tc.closing.Load() {
		what = append(what, "a close its handler was not done with")
	}
	if answers && v.reading == 0 && v.idle == 0 {
		what = append(what, "a request its handler had read and not answered (it had not gone back to reading)")
	}
	if len(what) == 0 {
		return "work keploy could not name"
	}
	return strings.Join(what, ", and ")
}

// holds is whether tc still holds the scope's work, cut its cut: bytes up
// to it its reads have not delivered, or, in a replay, a request read from
// them not answered. Its handler answered them once, after its reads
// delivered them, it held no request (settled); else its view now is read
// by busy's rule with the bytes after cut left out. marked: an unknown mark
// stood when it was found holding none.
func (s *scope) holds(tc *trackedConn, cut uint64, answers bool) (held, marked bool) {
	v := tc.view()
	if v.delivered < cut {
		return true, false
	}
	if !answers {
		return false, false
	}
	if v.hasSettled && v.settled >= cut {
		return false, v.settledMarks
	}
	return busy(0, answers, v.reading, v.held, v.idle, tc.closing.Load()), v.marks > 0
}

// State is inflight.State over the scope's connections not yet seen done;
// it drops each it sees done now (or forgotten), noting, in a replay, the
// protocol of one whose parser could not say what it held. unknown is every
// protocol noted so far. One it leaves out (leftOut) is not pending.
func (s *scope) State(answers bool) (pending int, unknown []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tc, cut := range s.left {
		if tc.forgotten.Load() {
			delete(s.left, tc)
			delete(s.behind, tc)
			continue
		}
		if s.leftOut(tc, cut, answers) {
			continue
		}
		held, marked := s.holds(tc, cut, answers)
		if held {
			pending++
			continue
		}
		if answers {
			if p := tc.unknown.Load(); p != nil {
				s.unknown[*p] = true
			}
			if marked {
				if p := tc.unknownWhat.Load(); p != nil {
					s.unknown[*p] = true
				}
			}
		}
		delete(s.left, tc)
	}
	if len(s.unknown) > 0 {
		unknown = make([]string, 0, len(s.unknown))
		for p := range s.unknown {
			unknown = append(unknown, p)
		}
		sort.Strings(unknown)
	}
	return pending, unknown
}

// PendingProgress is inflight.PendingProgress over the scope's connections
// that still hold its work, those it leaves out (leftOut) not among them.
func (s *scope) PendingProgress(answers bool) map[uint64]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uint64]uint64{}
	for tc, cut := range s.left {
		if tc.forgotten.Load() || s.leftOut(tc, cut, answers) {
			continue
		}
		if held, _ := s.holds(tc, cut, answers); held {
			out[tc.id] = tc.progress.Load()
		}
	}
	return out
}

// InFlightScope implements agent.InFlightScoper.
func (p *Proxy) InFlightScope() agent.InFlightScope { return p.inflight.scope() }

// InFlightChanged implements agent.InFlightReader.
func (p *Proxy) InFlightChanged() <-chan struct{} { return p.inflight.Changed() }

// InFlightState implements agent.InFlightReader.
func (p *Proxy) InFlightState(answers bool) (pending int, unknown []string) {
	return p.inflight.State(answers)
}

// InFlightPendingProgress implements agent.InFlightReader.
func (p *Proxy) InFlightPendingProgress(answers bool) map[uint64]uint64 {
	return p.inflight.PendingProgress(answers)
}

var (
	_ agent.InFlightReader       = (*Proxy)(nil)
	_ agent.InFlightScoper       = (*Proxy)(nil)
	_ integrations.RequestHolder = (*trackedConn)(nil)
	_ integrations.UnknownMarker = (*trackedConn)(nil)
	_ integrations.IdleMarker    = (*trackedConn)(nil)
	_ syscall.Conn               = (*trackedConn)(nil)
)

// keepsConnections reports whether the accept keeps each connection it hands
// over (inflight.track), so a test's end, or a read of its misses, can tell
// when none holds its calls: under the proven switch; and with it off, for an
// agent started for a run that learns noise (settleReads) whose calls are
// counted from the proxy's start (CountedFromStart). One whose calls cannot be
// counted settles no read, so it keeps none, as before.
func (p *Proxy) keepsConnections() bool {
	return provenswitch.On() || p.settleReads && p.countedFromStart.Load()
}
