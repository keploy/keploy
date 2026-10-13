package manager

import (
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// unrecorded.go is the one rule for traffic a recording could not capture.
//
// A connection stops being recorded when its parser can no longer follow it: a
// hole in its captured bytes that its protocol cannot re-align across (MySQL,
// Postgres, HTTP), or a parser that failed or was retired partway. The app keeps
// using the connection, and nothing it carries from then on becomes a mock. A
// test case recorded meanwhile would be saved without its mocks and fail replay,
// so it is left out of the recording instead: every test case whose window
// overlaps a span in which the connection carried traffic after it stopped.
// Which test cases used the connection cannot be told from the traffic, so the
// spans follow the connection's activity, not the test cases.
//
// A hole that the parser does re-align across (mongo v2) is followed the same
// way. The parser re-aligns at some later message, but when is known only once
// it gets there, and it can be a full queue behind the traffic: the test cases
// in between would already have been streamed. Such a parser also records the
// hole's own span once it knows its width (Spans.Record).
//
// Proxy mode applies the rule to the manager of the recording
// (pkg/agent/proxy/proxy_v2.go). A DaemonSet agent records many pods, whose
// test cases can only ride their own pod's connections, so it keeps one Spans
// per pod. Both follow a stopped connection with an UnrecordedConn.
//
// Both ends of a span are on the capture's clock: the stamps of the chunks the
// connection carried (when the relay, or the kernel, read them), never the
// agent's own clock. A parser runs behind its connection's traffic, by as much
// as the agent is starved; a span dated by when the agent got to an event is
// that much too wide, and on a connection-unscoped rule it leaves out every
// test case of every healthy connection inside the lag. So a stop is dated at
// the stamp of the newest chunk the connection had carried by then
// (supervisor.Session.StoppedAt); what its parser had not recorded of what
// came before is left out apart, by what it read and accounted for
// (pkg/agent/proxy's parserReads); the span the stop opens reaches the newest
// chunk noted before it, and what the connection carries after the stop
// extends it to each chunk's stamp; it falls idle
// when the capture has passed its last chunk by the idle grace with no chunk
// in between, and its span then ends at that last chunk. An open span extends
// no further than the capture has got (the horizon), and no further than the
// idle grace past its last chunk (UnrecordedConn, Spans.OpenAt).

const (
	// UnrecordedIdleGrace is how long a stopped connection must carry no bytes
	// before its span is closed: a gap between the stamps of its chunks, on
	// the capture's clock. Longer than any plausible gap within one app
	// request, short enough that a connection idling between bursts stops
	// suppressing quickly. A span on the capture's clock ends at the last
	// chunk before the gap, so the grace decides only where one span ends and
	// the next begins, and how far an open span may still be joined.
	UnrecordedIdleGrace = 1 * time.Second

	// UnrecordedIdleCheck is how often idleness is re-evaluated. It decides
	// only how soon a span is closed, not where: idleness is read off the
	// capture's stamps, and the span ends at its last chunk however late the
	// check runs.
	UnrecordedIdleCheck = 250 * time.Millisecond
)

// Spans is a set of time spans over which traffic could not be recorded. A test
// case whose window overlaps one must be left out of the recording rather than
// saved without its mocks. The zero value is ready to use; every method is
// nil-safe.
//
// A span is either closed, with both ends known when it is recorded (Record), or
// opened with its end still unknown and closed later. A span opened on the
// capture's clock (OpenAt) is extended to each chunk of the traffic it follows
// and closed at the last one; while open it extends no further than the
// capture has got (SetHorizon) nor than its idle grace past its last chunk,
// and never ends short of its last chunk. A span opened with Open is on the
// agent's clock: it extends to now, and its closer ends it then. Without a
// horizon, the horizon is the clock.
//
// Closed spans are kept sorted and disjoint: a span that overlaps or touches
// another joins it, so Overlaps is a binary search. That needs every span to
// start no later than it ends, and every time in one order: a span that would
// end before it starts (Open's closer reads the clock, and the start it was
// given can be a little ahead of it) ends at its start, and every time is kept
// on the wall clock alone (a comparison of two times that both carry a
// monotonic reading uses that, and one with a time that does not uses the wall
// clock, so a mix can be ordered two ways once the wall clock is stepped).
//
// Their number is bounded (maxPressureRanges), and spans are never evicted: a
// DaemonSet agent records a span per mock its stream drops, thousands a second
// under load, and the test cases they cover are checked up to half a minute
// later (their verdict is held, settle.go); with the oldest evicted to make
// room, those test cases were saved without their mocks. Past the cap, spans
// are joined until a quarter of the room is free again, which only widens what
// is covered: it can leave out a test case that fell in a joined gap, never
// let one through that overlaps a span. What is joined is chosen to cover the
// least of the test cases still to be checked:
//   - first, the spans that end before every test case the consumer still
//     holds (CheckedBefore), into one: no test case it holds overlaps them;
//   - then, the spans with the smallest gaps between them, which covers the
//     least time.
//
// Open spans are bounded the same way: past the cap, a new one joins the open
// span whose start is nearest, which then stays open from the earlier start
// until every closer of the pair has run.
//
// A span's WIDTH is not bounded here: keeping it to the traffic that really
// went unrecorded is the caller's job.
type Spans struct {
	mu sync.Mutex
	// closed is sorted by start, and no two overlap or touch.
	closed []pressureRange
	// open holds the spans Open returned a closer for and that are still
	// open, as pointers so the closer can find its own. Closing one moves it
	// to closed.
	open []*openSpan
	// recorded is how many spans Record and Open were given, before any was
	// joined to another.
	recorded int
	// checked is where the consumer's test cases still to be checked start
	// (CheckedBefore): none before it.
	checked time.Time
	// horizon is how far the capture has got (SetHorizon); nil: the clock.
	horizon atomic.Pointer[func() time.Time]
}

// openSpan is a span whose end is not known yet. refs is how many closers must
// still run before it closes: more than one once a span opened past the cap
// joined it (into, on the joined one).
type openSpan struct {
	start time.Time
	refs  int
	into  *openSpan
	// onCapture: opened by OpenAt, and every span joined to it too. last is
	// then the stamp of the newest chunk of the traffic it follows (Extend),
	// and grace how far past it more traffic may still join it. A span that
	// is not on the capture (Open: OpenOrphanWindow, NewUnrecordedConn) is on
	// the agent's clock, as it always was: it extends to now, and its closer
	// ends it then.
	onCapture bool
	last      time.Time
	grace     time.Duration
	// endAt is the latest end its closers have given, as each ran.
	endAt time.Time
}

// SetHorizon says how far the capture has got: every chunk stamped up to it
// has been read and noted. An open span on the capture (OpenAt) extends no
// further. Spans Open opens are on the agent's clock and do not use it: their
// traffic is not stamped, so the horizon, which can lag far behind the clock
// while a connection's chunk is being stamped, would only cut them short. Nil,
// or never set: the clock.
func (s *Spans) SetHorizon(h func() time.Time) {
	if s == nil {
		return
	}
	if h == nil {
		s.horizon.Store(nil)
		return
	}
	s.horizon.Store(&h)
}

// horizonNow is how far the capture has got, on the wall clock alone.
func (s *Spans) horizonNow() time.Time {
	if h := s.horizon.Load(); h != nil {
		if t := (*h)(); !t.IsZero() {
			return wallTime(t)
		}
	}
	return wallTime(time.Now())
}

// queryEnd is where an open span ends for a query made at now with the
// capture at horizon: a span on the capture extends to its last chunk, and on
// to the horizon no further than its grace past it; any other (Open, on the
// agent's clock) to now.
func (o *openSpan) queryEnd(horizon, now time.Time) time.Time {
	if !o.onCapture {
		return now
	}
	end := o.last.Add(o.grace)
	if horizon.Before(end) {
		end = horizon
	}
	if end.Before(o.last) {
		end = o.last
	}
	return end
}

// spansKeepOnJoin is how many closed spans are left once the cap forces a join:
// three quarters of it, so a join comes once per quarter of the cap's spans
// added, not once per span.
const spansKeepOnJoin = maxPressureRanges * 3 / 4

// wallTime is t on the wall clock alone (see Spans).
func wallTime(t time.Time) time.Time { return t.Round(0) }

// Record adds the closed span [start, end]. A zero start makes no claim and is
// ignored; an end before start is clamped to start.
func (s *Spans) Record(start, end time.Time) {
	if s == nil || start.IsZero() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recorded++
	s.addClosedLocked(start, end)
}

// addClosedLocked adds the closed span [start, end], on the wall clock, ending
// no earlier than it starts.
func (s *Spans) addClosedLocked(start, end time.Time) {
	r := pressureRange{start: wallTime(start), end: wallTime(end)}
	if r.end.Before(r.start) {
		r.end = r.start
	}
	// Every span before i ends before r starts; from i on, those that start
	// no later than r ends overlap or touch it, and join it.
	i := sort.Search(len(s.closed), func(k int) bool { return !s.closed[k].end.Before(r.start) })
	j := i
	for ; j < len(s.closed) && !s.closed[j].start.After(r.end); j++ {
		if s.closed[j].start.Before(r.start) {
			r.start = s.closed[j].start
		}
		if s.closed[j].end.After(r.end) {
			r.end = s.closed[j].end
		}
	}
	s.closed = slices.Replace(s.closed, i, j, r)
	if len(s.closed) > maxPressureRanges {
		s.closed = joinForRoom(s.closed, spansKeepOnJoin, s.checked)
	}
}

// CheckedBefore says that every test case still to be checked against these
// spans starts at or after t: its consumer has checked those that started
// before it (a TestCaseHold's earliest held start, once it has released the
// rest). Past the cap, the spans that end before it are joined first (Spans).
// It only moves forward. A test case that comes later with an earlier start
// (it was still on its way to the consumer) is checked against the joined
// span: it can be left out for a gap, never let through over a span.
func (s *Spans) CheckedBefore(t time.Time) {
	if s == nil || t.IsZero() {
		return
	}
	t = wallTime(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.After(s.checked) {
		s.checked = t
	}
}

// joinForRoom joins spans (sorted, disjoint) until keep remain: first those
// that end before checked, into one, then those with the smallest gaps between
// them (joinNearest). It returns them in a new slice, so the old array is not
// kept.
func joinForRoom(spans []pressureRange, keep int, checked time.Time) []pressureRange {
	if keep < 1 || len(spans) <= keep {
		return spans
	}
	// spans[:done] end before checked: no test case still to be checked
	// overlaps them.
	done := sort.Search(len(spans), func(k int) bool { return !spans[k].end.Before(checked) })
	if done >= 2 {
		out := make([]pressureRange, 0, len(spans)-done+1)
		out = append(out, pressureRange{start: spans[0].start, end: spans[done-1].end})
		spans = append(out, spans[done:]...)
	}
	return joinNearest(spans, keep)
}

// joinNearest joins the spans (sorted, disjoint) with the smallest gaps between
// them until keep remain, and returns them in a new slice, so the old array is
// not kept.
func joinNearest(spans []pressureRange, keep int) []pressureRange {
	n := len(spans)
	if keep < 1 || n <= keep {
		return spans
	}
	gaps := make([]time.Duration, n-1)
	for k := range gaps {
		gaps[k] = spans[k+1].start.Sub(spans[k].end)
	}
	sorted := slices.Clone(gaps)
	slices.Sort(sorted)
	joins := n - keep
	limit := sorted[joins-1]
	// Every gap below limit is closed, and as many at it as it takes.
	atLimit := joins - sort.Search(len(sorted), func(k int) bool { return sorted[k] >= limit })
	out := make([]pressureRange, 0, keep)
	cur := spans[0]
	for k, g := range gaps {
		if g < limit || (g == limit && atLimit > 0) {
			if g == limit {
				atLimit--
			}
			cur.end = spans[k+1].end
			continue
		}
		out = append(out, cur)
		cur = spans[k+1]
	}
	return append(out, cur)
}

// Open adds a span from start whose end is not yet known, and returns the func
// that ends it, at the clock as it runs (the agent's clock, as before the
// capture's clock existed: not the horizon). The func is idempotent
// and safe to call from any goroutine; not calling it leaves the span open,
// which is right for traffic that stays unrecorded until the recording ends. A
// zero start is ignored (a no-op closer). A span whose traffic the capture
// stamps is better opened with OpenAt, which ends it at its last chunk.
func (s *Spans) Open(start time.Time) func() {
	if s == nil || start.IsZero() {
		return func() {}
	}
	r := s.openLocked(&openSpan{start: wallTime(start), refs: 1})
	var once sync.Once
	return func() { once.Do(func() { s.closeOpen(r, time.Time{}) }) }
}

// OpenSpan is a span on the capture's clock whose end is not known yet
// (Spans.OpenAt): the traffic it follows extends it (Extend), and it ends at
// the stamp its closer gives (Close). Every method is nil-safe and safe to call
// from any goroutine.
type OpenSpan struct {
	s    *Spans
	r    *openSpan
	once sync.Once
}

// OpenAt adds a span from start, on the capture's clock, that follows traffic
// whose chunks the capture stamps: Extend moves its end to each chunk's stamp,
// and Close ends it at the stamp of its last chunk. While open it extends no
// further than grace past its last chunk, nor than the horizon (SetHorizon).
// A zero start is ignored (a nil OpenSpan, whose methods do nothing).
func (s *Spans) OpenAt(start time.Time, grace time.Duration) *OpenSpan {
	if s == nil || start.IsZero() {
		return nil
	}
	start = wallTime(start)
	r := s.openLocked(&openSpan{start: start, refs: 1, onCapture: true, last: start, grace: grace})
	return &OpenSpan{s: s, r: r}
}

// openLocked adds r to the open spans, or, past the cap, joins it to the open
// span whose start is nearest, which then covers both until both closers have
// run. It returns r.
func (s *Spans) openLocked(r *openSpan) *openSpan {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recorded++
	if len(s.open) < maxPressureRanges {
		s.open = append(s.open, r)
		return r
	}
	host := s.open[0]
	for _, o := range s.open[1:] {
		if absDuration(o.start.Sub(r.start)) < absDuration(host.start.Sub(r.start)) {
			host = o
		}
	}
	if r.start.Before(host.start) {
		host.start = r.start
	}
	// The joined span is on the capture only when both are: one on the
	// agent's clock (Open) keeps the host there, extending to now.
	if host.onCapture && r.onCapture {
		if r.last.After(host.last) {
			host.last = r.last
		}
		host.grace = max(host.grace, r.grace)
	} else {
		host.onCapture = false
	}
	host.refs++
	r.into = host
	return r
}

// root is the span r's closer ends: r, or the one it joined past the cap.
func (r *openSpan) root() *openSpan {
	if r.into != nil {
		return r.into
	}
	return r
}

// closeOpen runs one closer of r: end is where it ends the span; zero ends a
// span on the capture at the horizon, and any other (Open's) at the clock. A
// span on the capture ends no earlier than its last chunk. The span closes
// once its last closer has run, at the latest end any of them gave.
func (s *Spans) closeOpen(r *openSpan, end time.Time) {
	var horizon, now time.Time
	if end.IsZero() {
		// Read before the lock: the horizon asks the capture, which must
		// never wait on these spans.
		horizon, now = s.horizonNow(), wallTime(time.Now())
	}
	end = wallTime(end)
	s.mu.Lock()
	defer s.mu.Unlock()
	root := r.root()
	if end.IsZero() {
		end = now
		if root.onCapture {
			end = horizon
		}
	}
	if root.onCapture && end.Before(root.last) {
		// A span on the capture reaches at least its last chunk (Extend),
		// however far the horizon lags behind it as it closes: it does
		// while a chunk is being stamped (relay.Relay.CapturedTo).
		end = root.last
	}
	if end.After(root.endAt) {
		root.endAt = end
	}
	if root.refs--; root.refs > 0 {
		return
	}
	for i, o := range s.open {
		if o == root {
			last := len(s.open) - 1
			s.open[i], s.open[last] = s.open[last], nil
			s.open = s.open[:last]
			break
		}
	}
	s.addClosedLocked(root.start, root.endAt)
}

// Extend says the traffic the span follows carried a chunk stamped at: the
// span reaches at least that far.
func (o *OpenSpan) Extend(at time.Time) {
	if o == nil || at.IsZero() {
		return
	}
	at = wallTime(at)
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if root := o.r.root(); root.onCapture && at.After(root.last) {
		root.last = at
	}
}

// Close ends the span at end, the stamp of the last chunk of the traffic it
// followed (zero: at the horizon). Idempotent.
func (o *OpenSpan) Close(end time.Time) {
	if o == nil {
		return
	}
	o.once.Do(func() { o.s.closeOpen(o.r, end) })
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// Overlaps reports whether any span overlaps [start, end], and how many do.
// An open span extends as far as the capture tells (Spans). Zero bounds make
// no claim: (false, 0). Spans that were joined count as one.
func (s *Spans) Overlaps(start, end time.Time) (bool, int) {
	if s == nil || start.IsZero() || end.IsZero() {
		return false, 0
	}
	start, end = wallTime(start), wallTime(end)
	// Read before the lock: the horizon asks the capture, which must never
	// wait on these spans.
	horizon, now := s.horizonNow(), wallTime(time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	i := sort.Search(len(s.closed), func(k int) bool { return !s.closed[k].end.Before(start) })
	for ; i < len(s.closed) && !s.closed[i].start.After(end); i++ {
		count++
	}
	for _, r := range s.open { // still open: as far as the capture tells
		if !r.start.After(end) && !r.queryEnd(horizon, now).Before(start) {
			count++
		}
	}
	return count > 0, count
}

// Counts reports how many spans are closed and how many are still open, after
// joining. A span Open returned has been closed once its closer ran (every
// closer, for spans joined past the cap), so it counts as closed.
func (s *Spans) Counts() (closed, open int) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.closed), len(s.open)
}

// Recorded reports how many spans Record and Open have been given, however
// many of them were joined since: how often what they stand for happened.
func (s *Spans) Recorded() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recorded
}

// UnrecordedConn follows one connection that has stopped being recorded, and
// keeps a span open for as long as the connection carries traffic afterwards.
// It closes the span once the connection has been idle for idleGrace, and opens
// a new one, from the first byte, when traffic resumes. So a connection that
// sat idle after it stopped costs nothing, and one that stays busy is covered
// without a gap.
//
// On the capture's clock (NewUnrecordedConnOnCapture) every time is a chunk's
// stamp: the span opens at the stop's stamp, each chunk carried after it
// extends it to its stamp, idleness is a gap of idleGrace between the stamp of
// the last chunk and how far the capture has got (capturedTo), and the span
// ends at that last chunk, however late the agent gets to look. One made by
// NewUnrecordedConn judges idleness on the agent's clock instead, and ends its
// spans where the closer it is given ends them.
//
// Nothing runs until Stop. While a span is open one goroutine follows the
// connection; while it is idle none does. End ends it for good.
type UnrecordedConn struct {
	open                  func(start time.Time) func()
	idleGrace, checkEvery time.Duration

	stopped atomic.Bool
	ended   atomic.Bool
	// running: a goroutine is following the connection, with a span open.
	running atomic.Bool
	// armed: the span was closed for idleness, so the next Note starts a new
	// active spell, and records where (resume).
	armed atomic.Bool
	// seen is when bytes were last observed on this process's clock (UnixNano),
	// the clock idleness is judged on. resume is when the first bytes after an
	// idle close were on the wire, where the next span starts.
	seen, resume atomic.Int64

	// On the capture's clock (NewUnrecordedConnOnCapture): openAt opens a
	// span, capturedTo says how far the capture of the connection has got,
	// last is the stamp of the newest chunk noted (UnixNano), and cur is the
	// open span, nil while none is (guarded by mu).
	openAt     func(start time.Time) CaptureSpan
	capturedTo func() time.Time
	last       atomic.Int64
	mu         sync.Mutex
	cur        CaptureSpan

	endCh   chan struct{}
	endOnce sync.Once

	// beforeIdleClose, when set (tests only), runs as the follower has
	// judged the span idle and is about to close it, under mu.
	beforeIdleClose func()
}

// CaptureSpan is an open span on the capture's clock: the traffic it follows
// extends it, and it ends at the stamp of the last chunk. *OpenSpan is one, and
// NoSpan one that covers nothing.
type CaptureSpan interface {
	Extend(at time.Time)
	Close(end time.Time)
}

// NoSpan is a CaptureSpan that covers nothing: what an opener returns when no
// span is to open.
var NoSpan CaptureSpan = noSpan{}

type noSpan struct{}

func (noSpan) Extend(time.Time) {}
func (noSpan) Close(time.Time)  {}

// NewUnrecordedConn prepares to follow a connection whose spans open is given,
// judging idleness on the agent's clock. idleGrace and checkEvery are
// UnrecordedIdleGrace and UnrecordedIdleCheck in production; tests pass
// milliseconds. A connection whose chunks the capture stamps is followed on
// the capture's clock with NewUnrecordedConnOnCapture instead.
func NewUnrecordedConn(open func(start time.Time) func(), idleGrace, checkEvery time.Duration) *UnrecordedConn {
	u := &UnrecordedConn{open: open, idleGrace: idleGrace, checkEvery: checkEvery, endCh: make(chan struct{})}
	u.seen.Store(time.Now().UnixNano())
	return u
}

// NewUnrecordedConnOnCapture prepares to follow a connection on the capture's
// clock: open opens its spans (Spans.OpenAt, through the session's manager or
// its own spans), and capturedTo says how far the capture of the connection
// has got: every chunk it carried stamped up to then has been noted. nil
// capturedTo is the clock. idleGrace and checkEvery are UnrecordedIdleGrace
// and UnrecordedIdleCheck in production.
func NewUnrecordedConnOnCapture(open func(start time.Time) CaptureSpan, capturedTo func() time.Time, idleGrace, checkEvery time.Duration) *UnrecordedConn {
	if capturedTo == nil {
		capturedTo = time.Now
	}
	return &UnrecordedConn{openAt: open, capturedTo: capturedTo, idleGrace: idleGrace, checkEvery: checkEvery, endCh: make(chan struct{})}
}

// onCapture reports whether u follows its connection on the capture's clock.
func (u *UnrecordedConn) onCapture() bool { return u.openAt != nil }

// Note records that the connection carried bytes, observed now, that were on
// the wire at `at` (zero: now): the stamp of the chunk, on the capture's
// clock. A capture path may call it for every chunk; until Stop it costs two
// atomic operations.
func (u *UnrecordedConn) Note(at time.Time) {
	if u == nil {
		return
	}
	if u.onCapture() {
		u.noteOnCapture(at)
		return
	}
	u.seen.Store(time.Now().UnixNano())
	if !u.stopped.Load() {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	if u.armed.Load() && u.armed.CompareAndSwap(true, false) {
		u.resume.Store(at.UnixNano())
	}
	if !u.running.Load() && !u.ended.Load() && u.running.CompareAndSwap(false, true) {
		u.start(at)
	}
}

// noteLast moves last to at, if at is newer.
func (u *UnrecordedConn) noteLast(at time.Time) {
	ns := at.UnixNano()
	for {
		old := u.last.Load()
		if ns <= old || u.last.CompareAndSwap(old, ns) {
			return
		}
	}
}

func (u *UnrecordedConn) noteOnCapture(at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	u.noteLast(at)
	if !u.stopped.Load() {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.ended.Load() {
		return
	}
	if u.cur == nil {
		// The first chunk after the stop (from the next bytes), or after an
		// idle close: a span from its own stamp.
		u.openSpanLocked(at)
		return
	}
	u.cur.Extend(at)
}

// openSpanLocked opens a span from start, reaching at least the newest chunk
// noted, and starts a follower if none runs. Caller holds mu.
func (u *UnrecordedConn) openSpanLocked(start time.Time) {
	sp := u.openAt(start)
	if sp == nil || sp == NoSpan {
		return // no span opens (the recording is stopping, say), and nothing follows it
	}
	u.cur = sp
	if last := u.last.Load(); last != 0 {
		u.cur.Extend(time.Unix(0, last))
	}
	if u.running.CompareAndSwap(false, true) {
		go u.followOnCapture()
	}
}

// Stop says the connection stopped being recorded at `at` (zero: now; on the
// capture's clock, the stamp of the last chunk it had carried): a span opens
// there and follows the connection's traffic from then on. Only the first call
// counts; it reports whether this was it.
func (u *UnrecordedConn) Stop(at time.Time) bool {
	if u == nil || u.ended.Load() || !u.stopped.CompareAndSwap(false, true) {
		return false
	}
	if at.IsZero() {
		at = time.Now()
	}
	if u.onCapture() {
		// The stop is a stamp on the capture's clock: the connection had
		// carried everything up to it, so idleness counts from no earlier.
		u.noteLast(at)
		u.mu.Lock()
		if !u.ended.Load() && u.cur == nil {
			u.openSpanLocked(at)
		}
		u.mu.Unlock()
		return true
	}
	if u.running.CompareAndSwap(false, true) {
		u.start(at)
	}
	return true
}

// StopFromNextBytes says the connection is no longer recorded, with nothing it
// carried so far lost: its parser returned, having recorded what it read. No
// span opens now; the next bytes the connection carries (Note) open one, from
// where they were, and it follows the traffic from then on as after Stop. A
// connection that carries nothing more, the usual end of a parser that
// returned at its connection's end, leaves no span. Only the first Stop or
// StopFromNextBytes counts; it reports whether this was it.
func (u *UnrecordedConn) StopFromNextBytes() bool {
	return u != nil && !u.ended.Load() && u.stopped.CompareAndSwap(false, true)
}

// Stopped reports whether Stop (or StopFromNextBytes) has been called.
func (u *UnrecordedConn) Stopped() bool { return u != nil && u.stopped.Load() }

// End says the connection has ended: an open span is closed, and none opens
// again. On the capture's clock it ends at the last chunk the connection
// carried: it carried nothing after. Idempotent.
func (u *UnrecordedConn) End() {
	if u == nil {
		return
	}
	u.endOnce.Do(func() {
		u.ended.Store(true)
		close(u.endCh)
		if u.onCapture() {
			u.mu.Lock()
			u.closeSpanLocked()
			u.mu.Unlock()
		}
	})
}

// closeSpanLocked closes the open span at the newest chunk noted. Caller
// holds mu.
func (u *UnrecordedConn) closeSpanLocked() { u.closeSpanAtLocked(u.last.Load()) }

// closeSpanAtLocked ends the open span at last, the stamp of a chunk the
// connection carried (0: at the horizon, for a span that noted nothing).
// Caller holds mu.
func (u *UnrecordedConn) closeSpanAtLocked(last int64) {
	if u.cur == nil {
		return
	}
	var end time.Time // zero: at the horizon, for a span that noted nothing
	if last != 0 {
		end = time.Unix(0, last)
	}
	u.cur.Close(end)
	u.cur = nil
}

// followOnCapture keeps the span open until the connection ends, or the
// capture has got idleGrace past its last chunk with no chunk in between: the
// span then ends at that chunk. The check runs every checkEvery; when it runs
// decides only how soon the span closes, not where.
func (u *UnrecordedConn) followOnCapture() {
	t := time.NewTicker(u.checkEvery)
	defer t.Stop()
	for {
		select {
		case <-u.endCh:
			u.running.Store(false)
			return
		case <-t.C:
		}
		// How far the capture has got, read before the last chunk: a chunk
		// noted after this read is stamped no earlier than about now.
		passed := u.capturedTo()
		u.mu.Lock()
		if u.cur == nil {
			u.running.Store(false)
			u.mu.Unlock()
			return
		}
		// Nothing passed (a reader that has got nowhere yet) is no idleness.
		last := u.last.Load()
		if passed.IsZero() || passed.UnixNano()-last < int64(u.idleGrace) {
			u.mu.Unlock()
			continue
		}
		if u.beforeIdleClose != nil {
			u.beforeIdleClose()
		}
		// At the chunk the check judged idle past, not at one noted since
		// (Note moves last outside mu): closed there, the span covered the
		// whole idle stretch before it.
		u.closeSpanAtLocked(last)
		// The next chunk opens a span of its own (noteOnCapture), and a
		// follower with it.
		u.running.Store(false)
		u.mu.Unlock()
		return
	}
}

// start opens a span from `from` and follows the connection with it. The span
// opens before start returns, so a test case checked as soon as the stop or
// the traffic has been seen is already covered; only closing it waits for the
// follower.
func (u *UnrecordedConn) start(from time.Time) {
	go u.follow(u.open(from))
}

// follow keeps closeSpan's span open until the connection ends or falls idle.
func (u *UnrecordedConn) follow(closeSpan func()) {
	t := time.NewTicker(u.checkEvery)
	defer t.Stop()
	for {
		select {
		case <-u.endCh:
			closeSpan()
			return
		case now := <-t.C:
			if now.UnixNano()-u.seen.Load() <= int64(u.idleGrace) {
				continue
			}
			// Idle: the span ends here, and the next Note starts a new one
			// from its own bytes.
			u.resume.Store(0)
			u.armed.Store(true)
			closeSpan()
			u.running.Store(false)
			// A Note that ran after the arming but before running was cleared
			// saw a follower still running and left the restart to it: this
			// one. (One after the clear restarts on its own; the CAS keeps it
			// to one follower.)
			if r := u.resume.Load(); r != 0 && !u.ended.Load() && u.running.CompareAndSwap(false, true) {
				closeSpan = u.open(time.Unix(0, r))
				continue
			}
			return
		}
	}
}
