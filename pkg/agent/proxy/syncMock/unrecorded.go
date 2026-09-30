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

const (
	// UnrecordedIdleGrace is how long a stopped connection must carry no bytes
	// before its span is closed. Longer than any plausible gap within one app
	// request (the span must never close mid-request and let a half-covered
	// test case through), short enough that a connection idling between
	// bursts stops suppressing quickly.
	UnrecordedIdleGrace = 1 * time.Second

	// UnrecordedIdleCheck is how often idleness is re-evaluated. A span can
	// therefore run up to this long past the true idle point, which errs
	// toward leaving a test case out: the safe direction.
	UnrecordedIdleCheck = 250 * time.Millisecond
)

// Spans is a set of time spans over which traffic could not be recorded. A test
// case whose window overlaps one must be left out of the recording rather than
// saved without its mocks. The zero value is ready to use; every method is
// nil-safe.
//
// A span is either closed, with both ends known when it is recorded (Record), or
// opened with its end still unknown (Open) and closed later by the func Open
// returns. An open span extends to the moment it is queried.
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
}

// openSpan is a span whose end is not known yet. refs is how many closers must
// still run before it closes: more than one once a span opened past the cap
// joined it (into, on the joined one).
type openSpan struct {
	start time.Time
	refs  int
	into  *openSpan
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
// that ends it. The func is idempotent and safe to call from any goroutine; not
// calling it leaves the span open, which is right for traffic that stays
// unrecorded until the recording ends. A zero start is ignored (a no-op closer).
func (s *Spans) Open(start time.Time) func() {
	if s == nil || start.IsZero() {
		return func() {}
	}
	start = wallTime(start)
	r := &openSpan{start: start, refs: 1}
	s.mu.Lock()
	s.recorded++
	if len(s.open) < maxPressureRanges {
		s.open = append(s.open, r)
	} else {
		// Past the cap: join the open span whose start is nearest. It covers
		// both from the earlier start until both closers have run.
		host := s.open[0]
		for _, o := range s.open[1:] {
			if absDuration(o.start.Sub(start)) < absDuration(host.start.Sub(start)) {
				host = o
			}
		}
		if start.Before(host.start) {
			host.start = start
		}
		host.refs++
		r.into = host
	}
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			root := r
			if r.into != nil {
				root = r.into
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
			s.addClosedLocked(root.start, time.Now())
		})
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// Overlaps reports whether any span overlaps [start, end], and how many do.
// An open span extends to now. Zero bounds make no claim: (false, 0). Spans
// that were joined count as one.
func (s *Spans) Overlaps(start, end time.Time) (bool, int) {
	if s == nil || start.IsZero() || end.IsZero() {
		return false, 0
	}
	start, end = wallTime(start), wallTime(end)
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	i := sort.Search(len(s.closed), func(k int) bool { return !s.closed[k].end.Before(start) })
	for ; i < len(s.closed) && !s.closed[i].start.After(end); i++ {
		count++
	}
	now := wallTime(time.Now())
	for _, r := range s.open { // still open: to now
		if !r.start.After(end) && !now.Before(start) {
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
// keeps a span open (through open) for as long as the connection carries
// traffic afterwards. It closes the span once the connection has been idle for
// idleGrace, and opens a new one, from the first byte, when traffic resumes. So
// a connection that sat idle after it stopped costs nothing, and one that stays
// busy is covered without a gap.
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

	endCh   chan struct{}
	endOnce sync.Once
}

// NewUnrecordedConn prepares to follow a connection whose spans open is given.
// idleGrace and checkEvery are UnrecordedIdleGrace and UnrecordedIdleCheck in
// production; tests pass milliseconds.
func NewUnrecordedConn(open func(start time.Time) func(), idleGrace, checkEvery time.Duration) *UnrecordedConn {
	u := &UnrecordedConn{open: open, idleGrace: idleGrace, checkEvery: checkEvery, endCh: make(chan struct{})}
	u.seen.Store(time.Now().UnixNano())
	return u
}

// Note records that the connection carried bytes, observed now, that were on
// the wire at `at` (zero: now). A capture path may call it for every chunk; until
// Stop it costs two atomic operations.
func (u *UnrecordedConn) Note(at time.Time) {
	if u == nil {
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

// Stop says the connection stopped being recorded at `at` (zero: now): a span
// opens there and follows the connection's traffic from then on. Only the first
// call counts; it reports whether this was it.
func (u *UnrecordedConn) Stop(at time.Time) bool {
	if u == nil || u.ended.Load() || !u.stopped.CompareAndSwap(false, true) {
		return false
	}
	if at.IsZero() {
		at = time.Now()
	}
	if u.running.CompareAndSwap(false, true) {
		u.start(at)
	}
	return true
}

// Stopped reports whether Stop has been called.
func (u *UnrecordedConn) Stopped() bool { return u != nil && u.stopped.Load() }

// End says the connection has ended: an open span is closed now, and none opens
// again. Idempotent.
func (u *UnrecordedConn) End() {
	if u == nil {
		return
	}
	u.endOnce.Do(func() {
		u.ended.Store(true)
		close(u.endCh)
	})
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
