package manager

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// scoped_orphans.go: spans of unrecorded traffic that can have stranded only
// some of the recording's test cases, and say why.
//
// An orphan span (Spans, RecordOrphanWindow, OpenOrphanWindow) leaves out every
// test case whose window overlaps it, whichever process served it: the traffic
// cannot tell which test cases used a connection that stopped being recorded.
// Some callers can tell more. The enterprise agent's TLS capture loses a
// process's decrypted TLS when a kernel gate had no room for the process: what
// that process sent and received is lost, and only the test cases whose request
// it, or a process it works with, served can have needed it. A global span for
// such a loss left out every test case recorded while a long-lived worker
// lived, those every other worker served in full included.
//
// A scoped span carries an OrphanScope, which says by a test case's serving
// process (TestCase.ServedBy) whether the test case can have needed what the
// span lost, and a reason, which the record route says for each test case it
// leaves out. A test case whose serving process the capture could not tell
// (ServedBy 0) is left out by every span that overlaps it: a scope cannot clear
// what it cannot name. A nil scope is every test case's, as a global span is.
//
// Its end can be given (the closer OpenScopedOrphanWindow returns takes it):
// the caller can know when the loss ended better than when it learns of it
// (the exit of a process, recorded in the kernel as it happened, is read by the
// agent later), and a span that ends at the loss's end leaves out no test case
// that began after it. An end is clamped to the span's start only: an end
// later than the closer's call is kept (the clock rule below).
//
// Spans are never dropped: a test case still on its way to the record route
// can start before every one it has checked (CheckedBefore says only where the
// ones it holds start), and a dropped span would let it through. Their number
// is bounded (maxScopedOrphanSpans) by joining: past the cap, two adjacent
// closed spans (in the order they start) are joined into one span of every
// test case's (a nil scope) from the earlier start to the later end, as often
// as the cap needs (one join per span past it), never more. A joined span
// covers the time between the spans it joined, for every test case: it leaves
// out more, never fewer, and keeps their reasons (the first maxJoinedReasons,
// and how many more). A join gives up the scope over the width it covers that
// still had one (a span with no scope, a joined one, has none to give up), so
// the pair that gives up the least goes first. Where fewer than two spans are
// closed (every other one open), the new span is kept as a global span
// instead (Spans.OpenUntil, Spans.Record), which says no reason; it is there by
// the time the call that added it returns, and no span a check could find
// before is ever taken out of both sets. ScopedOrphansJoined counts the spans
// that went into joined or global spans, each once, for the record route's
// summary.
//
// A span's end, and the test cases' windows it is checked against, are on one
// clock: the caller gives the end in the terms it stamps the test cases'
// requests and responses in (the enterprise capture converts both a recorded
// exit and a request's first chunk from the kernel's CLOCK_MONOTONIC through
// one anchor). An end is never moved to the wall clock's now, which a step of
// the wall clock can put before it: an end later than now leaves out more, the
// safe direction. An open span covers every test case from its start on.

// OrphanScope says whether a test case whose request the process servedBy
// served (its TGID in the initial PID namespace, as the capture numbers it,
// TestCase.ServedBy), the request starting at req, can need what a span lost.
// It is called with servedBy non-zero only: a test case with no serving
// process is left out by every span that overlaps it. It is asked for as long
// as the span is kept, for every test case that overlaps it, long after the
// process the span is of may have exited and its TGID gone to another: it must
// answer by what it knew of the processes as they ran (servedBy's process at
// req, not whichever holds the TGID now), true where it cannot tell, and be
// cheap. It must be safe to call from any goroutine, and must not call back
// into the manager.
type OrphanScope func(servedBy uint32, req time.Time) bool

// maxScopedOrphanSpans bounds the scoped spans kept (open and closed). A var
// for tests.
var maxScopedOrphanSpans = 4096

// maxJoinedReasons bounds the reasons a joined span keeps by name.
const maxJoinedReasons = 8

// scopedSpan is one scoped span: [start, end], end zero while it is open.
// reasons are the reasons it keeps by name; joined how many of the spans added
// went into it (0 for one that was added as it is).
type scopedSpan struct {
	start, end time.Time
	scope      OrphanScope
	reasons    []string
	joined     int
	// extra holds the reasons past the first maxJoinedReasons, each once (up
	// to maxExtraReasons); uncounted is a floor of how many more distinct
	// ones it could not keep. Their sum is how many it stands for past its
	// named ones (more).
	extra     map[string]struct{}
	uncounted int
}

// maxExtraReasons bounds a joined span's set of reasons past its named ones.
const maxExtraReasons = 4096

// more is how many reasons the span stands for past its named ones, and
// whether that is a floor.
func (sp *scopedSpan) more() (n int, atLeast bool) {
	return len(sp.extra) + sp.uncounted, sp.uncounted > 0
}

// reason is the span's reasons as the record route's log says them.
func (sp *scopedSpan) reason() string {
	r := strings.Join(sp.reasons, "; ")
	if n, atLeast := sp.more(); n > 0 {
		if r != "" {
			r += "; "
		}
		if atLeast {
			r += fmt.Sprintf("and at least %d more", n)
		} else {
			r += fmt.Sprintf("and %d more", n)
		}
	}
	return r
}

// ScopedSpans is a set of scoped orphan spans. The zero value is ready to
// use; every method is nil-safe.
type ScopedSpans struct {
	mu sync.Mutex
	// open are the spans still open, in the order they were added; closed
	// the closed ones, by start (equal starts in the order they closed).
	open, closed []*scopedSpan
	// recorded is how many spans were kept here (one past the cap is a
	// global span's, Spans.Recorded).
	recorded int
	// joined counts the spans added that went into a joined span past the
	// cap, or were kept as global spans there, each once
	// (ScopedOrphansJoined).
	joined int
}

// OrphanSpanHit is a scoped span that overlaps a test case and can have
// stranded it: its reasons by name, and how many more it stands for (a floor
// when MoreAtLeast).
type OrphanSpanHit struct {
	Start, End  time.Time // End zero: still open
	Reasons     []string
	More        int
	MoreAtLeast bool
}

// add keeps sp, joining closed spans past the cap to make room; false where
// none can be made (fewer than two spans closed, sp itself counted), so the
// caller keeps sp as a global span. s.mu not held.
func (s *ScopedSpans) add(sp *scopedSpan) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertLocked(sp)
	if over := len(s.open) + len(s.closed) - maxScopedOrphanSpans; over > 0 {
		s.joinLocked(over)
	}
	if len(s.open)+len(s.closed) > maxScopedOrphanSpans {
		// Fewer than two spans are closed, so nothing was joined: sp goes
		// global (it was never seen here: added and taken out in one step).
		s.removeLocked(sp)
		s.joined++
		return false
	}
	s.recorded++
	return true
}

// insertLocked puts sp among the open spans, or the closed ones in the order
// they start. s.mu held.
func (s *ScopedSpans) insertLocked(sp *scopedSpan) {
	if sp.end.IsZero() {
		s.open = append(s.open, sp)
		return
	}
	i := sort.Search(len(s.closed), func(i int) bool { return s.closed[i].start.After(sp.start) })
	s.closed = append(s.closed, nil)
	copy(s.closed[i+1:], s.closed[i:])
	s.closed[i] = sp
}

// removeLocked takes sp out of whichever set holds it. s.mu held.
func (s *ScopedSpans) removeLocked(sp *scopedSpan) {
	drop := func(list []*scopedSpan) ([]*scopedSpan, bool) {
		for i, o := range list {
			if o == sp {
				copy(list[i:], list[i+1:])
				list[len(list)-1] = nil
				return list[:len(list)-1], true
			}
		}
		return list, false
	}
	var ok bool
	if s.open, ok = drop(s.open); !ok {
		s.closed, _ = drop(s.closed)
	}
}

// joinCost is the width over which joining a and b (a starting no later than
// b) gives up a scope: the union's width, less what of it a span with no
// scope (a joined one, or one added with none) covered already.
func joinCost(a, b *scopedSpan) time.Duration {
	hi := a.end
	if b.end.After(hi) {
		hi = b.end
	}
	cost := hi.Sub(a.start)
	for _, sp := range [...]*scopedSpan{a, b} {
		if sp.scope == nil {
			cost -= sp.end.Sub(sp.start)
		}
	}
	return cost
}

// joinLocked joins two adjacent closed spans (in the order they start), the
// pair that gives up the least scope first (joinCost; the earliest of equal
// ones), need times, or until one closed span is left: each pair into one span
// of every test case's (a nil scope) over both, keeping their reasons. The
// open spans, and the closed ones no join took, stay as they are. s.mu held.
func (s *ScopedSpans) joinLocked(need int) {
	for ; need > 0 && len(s.closed) >= 2; need-- {
		best, bestCost := 0, joinCost(s.closed[0], s.closed[1])
		for k := 1; k+1 < len(s.closed); k++ {
			if c := joinCost(s.closed[k], s.closed[k+1]); c < bestCost {
				best, bestCost = k, c
			}
		}
		// The pair's start is the first's: the order by start holds.
		s.closed[best] = s.joinPair(s.closed[best], s.closed[best+1])
		copy(s.closed[best+1:], s.closed[best+2:])
		s.closed[len(s.closed)-1] = nil
		s.closed = s.closed[:len(s.closed)-1]
	}
}

// joinPair is a and b (a starting no later than b) as one span of every test
// case's over both, with their reasons: a's then b's named ones, then their
// others in order, each once, the first maxJoinedReasons by name. s.mu held.
func (s *ScopedSpans) joinPair(a, b *scopedSpan) *scopedSpan {
	into := &scopedSpan{start: a.start, end: a.end}
	if b.end.After(into.end) {
		into.end = b.end
	}
	overflow := false
	note := func(r string) {
		if r == "" || slices.Contains(into.reasons, r) {
			return
		}
		if len(into.reasons) < maxJoinedReasons {
			into.reasons = append(into.reasons, r)
			return
		}
		if _, ok := into.extra[r]; ok {
			return
		}
		if len(into.extra) >= maxExtraReasons {
			overflow = true // not kept, so not told from the others it could not keep
			return
		}
		if into.extra == nil {
			into.extra = map[string]struct{}{}
		}
		into.extra[r] = struct{}{}
	}
	for _, sp := range [...]*scopedSpan{a, b} {
		if sp.joined == 0 {
			s.joined++ // an added span, joined for the first time
			into.joined++
		} else {
			into.joined += sp.joined
		}
		for _, r := range sp.reasons {
			note(r)
		}
		extra := make([]string, 0, len(sp.extra))
		for r := range sp.extra {
			extra = append(extra, r)
		}
		sort.Strings(extra)
		for _, r := range extra {
			note(r)
		}
		// What either could not keep may be the same reasons: the larger of
		// the two is a floor of what both stand for past what is kept here.
		into.uncounted = max(into.uncounted, sp.uncounted)
	}
	if overflow {
		into.uncounted = max(into.uncounted, 1)
	}
	return into
}

// hits is every span that overlaps [start, end] and can have stranded a test
// case its process servedBy served: an open span covers every time from its
// start; a test case with no serving process (servedBy 0) is stranded by
// every one. The scopes are asked with s.mu not held, at the request's time
// start.
func (s *ScopedSpans) hits(start, end time.Time, servedBy uint32) []OrphanSpanHit {
	if s == nil || start.IsZero() || end.IsZero() {
		return nil
	}
	start, end = wallTime(start), wallTime(end)
	type cand struct {
		hit   OrphanSpanHit
		scope OrphanScope
	}
	var cands []cand
	hit := func(sp *scopedSpan) {
		n, atLeast := sp.more()
		cands = append(cands, cand{OrphanSpanHit{Start: sp.start, End: sp.end, Reasons: sp.reasons, More: n, MoreAtLeast: atLeast}, sp.scope})
	}
	s.mu.Lock()
	for _, sp := range s.open {
		if !sp.start.After(end) {
			hit(sp)
		}
	}
	for _, sp := range s.closed {
		if sp.start.After(end) {
			break // the rest start later still
		}
		if !sp.end.Before(start) {
			hit(sp)
		}
	}
	s.mu.Unlock()
	var out []OrphanSpanHit
	for _, c := range cands {
		if c.scope == nil || servedBy == 0 || c.scope(servedBy, start) {
			out = append(out, c.hit)
		}
	}
	return out
}

// joinedCount is how many of the spans added were joined past the cap, or
// kept as global spans there, each once.
func (s *ScopedSpans) joinedCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.joined
}

// counts is how many spans are closed and open, and how many were added.
func (s *ScopedSpans) counts() (recorded, closed, open int) {
	if s == nil {
		return 0, 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recorded, len(s.closed), len(s.open)
}

// closeAt ends sp at end, clamped to sp.start (a zero end is now, on the wall
// clock). An end later than now is kept: the caller's clock, the test cases',
// can be ahead of a wall clock stepped back since, and a later end leaves out
// more, never fewer. Only the first call ends it.
func (s *ScopedSpans) closeAt(sp *scopedSpan, end time.Time) {
	if end.IsZero() {
		end = time.Now()
	}
	end = wallTime(end)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !sp.end.IsZero() {
		return
	}
	if end.Before(sp.start) {
		end = sp.start
	}
	// From the open spans to the closed ones, in its place by start; open
	// spans are never joined, so it is still among them.
	s.removeLocked(sp)
	sp.end = end
	s.insertLocked(sp)
}

// OpenScopedOrphanWindow marks the start of a span of unrecorded traffic whose
// end is not yet known, which can have stranded only the test cases scope
// names (a nil scope: every test case), for reason, which the record route
// says of each test case it leaves out for it. It returns the closer that
// ends the span at the end it is given (clamped to the span's start; an end
// later than its call is kept, and a zero end is when it runs). The closer is
// idempotent and safe to call
// from any goroutine; not calling it leaves the span open, which is right for a
// loss that lasts to the recording's end. A zero start makes no claim: a no-op
// closer. Past maxScopedOrphanSpans closed spans are joined to make room
// (ScopedSpans); where fewer than two are closed, the span is kept as a global
// one (Spans.OpenUntil), which leaves out every test case that overlaps it and
// says no reason.
func (m *SyncMockManager) OpenScopedOrphanWindow(start time.Time, scope OrphanScope, reason string) func(end time.Time) {
	if m == nil || start.IsZero() {
		return func(time.Time) {}
	}
	sp := &scopedSpan{start: wallTime(start), scope: scope, reasons: reasonList(reason)}
	if !m.scopedOrphans.add(sp) {
		return m.orphans.OpenUntil(start)
	}
	var once sync.Once
	return func(end time.Time) { once.Do(func() { m.scopedOrphans.closeAt(sp, end) }) }
}

// RecordScopedOrphanWindow records the closed span [start, end] of unrecorded
// traffic, which can have stranded only the test cases scope names (a nil
// scope: every test case), for reason. A zero start is ignored; an end before
// start is clamped to start. Past maxScopedOrphanSpans closed spans, this one
// among them, are joined to make room (ScopedSpans); where fewer than two are
// closed, it is kept as a global span (Spans.Record), which says no reason.
func (m *SyncMockManager) RecordScopedOrphanWindow(start, end time.Time, scope OrphanScope, reason string) {
	if m == nil || start.IsZero() {
		return
	}
	sp := &scopedSpan{start: wallTime(start), end: wallTime(end), scope: scope, reasons: reasonList(reason)}
	if sp.end.Before(sp.start) {
		sp.end = sp.start
	}
	if !m.scopedOrphans.add(sp) {
		m.orphans.Record(start, sp.end)
	}
}

// WasMockOrphanedFor says whether a span of unrecorded traffic overlaps
// [start, end], a test case's window, and can have stranded it, servedBy being
// the process that served its request (TestCase.ServedBy; 0 when the capture
// could not tell): every global span (RecordOrphanWindow, OpenOrphanWindow),
// open ones covering every time from their start, and every scoped one whose
// scope names it. It returns how many do, and the scoped ones' reasons by
// name, each once, in their spans' order, then UnnamedOrphanReasons where a
// joined span stands for reasons it does not name. A test case whose window
// the capture left unstamped (a zero bound) is judged as one from the
// earliest time to the latest, and one whose end is before its start over
// both its stamps ([end, start]): no scope can tell such a test case's
// serving process at its request, so every span that overlaps it leaves it
// out. The scoped spans are read before the global ones: a span the scoped
// set has no room for goes global before its add returns, so a check never
// misses one that was in either set before it began.
func (m *SyncMockManager) WasMockOrphanedFor(start, end time.Time, servedBy uint32) (bool, int, []string) {
	if m == nil {
		return false, 0, nil
	}
	switch {
	case start.IsZero() || end.IsZero():
		start, end, servedBy = time.Unix(0, 1), maxSpanTime, 0
	case end.Before(start):
		start, end, servedBy = end, start, 0
	}
	hits := m.scopedOrphans.hits(start, end, servedBy)
	_, n := m.orphans.overlapsFromStart(start, end)
	n += len(hits)
	if len(hits) == 0 {
		return n > 0, n, nil
	}
	var reasons []string
	unnamed := false
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Start.Before(hits[j].Start) })
	for _, h := range hits {
		for _, r := range h.Reasons {
			if !slices.Contains(reasons, r) {
				reasons = append(reasons, r)
			}
		}
		unnamed = unnamed || h.More > 0
	}
	if unnamed {
		reasons = append(reasons, UnnamedOrphanReasons)
	}
	return n > 0, n, reasons
}

// UnnamedOrphanReasons stands, among the reasons WasMockOrphanedFor gives, for
// the ones a span joined past its cap counts but does not name: one entry
// however many, so a tally of the reasons keeps one place for them all.
const UnnamedOrphanReasons = "more reasons that scoped spans joined past their cap stand for but do not name"

// ScopedOrphansJoined is how many scoped orphan spans were joined past their
// cap (into a span of every test case's, keeping their reasons), or kept as a
// global span there (no reason): what the record route's summary says, so a
// recording that left out more than its spans' scopes did says why.
func (m *SyncMockManager) ScopedOrphansJoined() int {
	if m == nil {
		return 0
	}
	return m.scopedOrphans.joinedCount()
}

// reasonList is a span's reasons from the one its caller gives.
func reasonList(reason string) []string {
	if reason == "" {
		return nil
	}
	return []string{reason}
}

// maxSpanTime is the latest time a span check reaches: the window of a test
// case whose end is unknown.
var maxSpanTime = time.Unix(1<<62, 0)
