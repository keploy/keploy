package models

import (
	"slices"
	"sort"
	"sync"
	"time"
)

// TestWindow is one recorded test case's [request, response] span: the window
// the replayer opens on the agent while that test runs (MockFilterParams
// AfterTime/BeforeTime).
type TestWindow struct {
	TestCase string    `json:"testCase,omitempty" yaml:"test_case,omitempty"`
	Start    time.Time `json:"start" yaml:"start"`
	End      time.Time `json:"end" yaml:"end"`
	// Clock names, under the proven switch, the clock Start is on: the test
	// case's Origin.Clock (RecordedWindow). "" is untrusted.
	Clock string `json:"clock,omitempty" yaml:"clock,omitempty"`
	// Tests are, for a window several tests' windows were merged into (tests
	// run at once; TestCase ""), the tests merged: the run reaches it when
	// any of them begins, and passes its end once each of them has ended
	// (models.RunPlace). Empty for one test's window.
	Tests []string `json:"tests,omitempty" yaml:"tests,omitempty"`
}

// RunWindows is ws, the windows keploy mock replay derives from a set's
// mappings, as the run's place reads them (RunPlace): each runs up to the
// next one's start, and the last has no end.
//
// A derived window ends at its test's last recording's place, a request, so
// its End is not when its test ended, which the mappings do not say. Read as
// that, the answer to the test's last call lies after its window, in a gap
// released by the test's end, and the test's end waits for that answer. So
// here a moment belongs to the test running when it was recorded, and no
// release waits for a test's end: an answer recorded after a test ended and
// before the next began is due while that test runs.
func RunWindows(ws []TestWindow) []TestWindow {
	out := slices.Clone(ws)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	for i := range out {
		end := noWindowEnd
		if i+1 < len(out) {
			end = out[i+1].Start.Add(-time.Nanosecond)
		}
		if end.After(out[i].End) {
			out[i].End = end
		}
	}
	return out
}

// noWindowEnd is the End of a window that has none: later than any recording.
var noWindowEnd = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)

// MockWindowEdgeSkew is the maximum distance outside every recorded test window
// at which an unmapped mock is still attributed as edge skew to the window
// whose nearer edge is closest. Small enough to absorb capture/handler
// timestamp skew at a window boundary (e.g. tens of microseconds) without
// claiming genuine startup or between-tests traffic.
const MockWindowEdgeSkew = 100 * time.Microsecond

// WindowSchedule is every recorded test window of one test set, sorted by
// Start — the unselected and ignored tests included, because traffic the
// application produced during a test that is not replayed still belongs to that
// test's place in the recording.
//
// It answers one question for traffic that the replay's test windows do not
// pace by themselves (a broker's server push, a publish the application makes
// while handling one): which recorded window does a recorded moment belong to,
// and has the replay reached it yet. The rule is:
//
//   - the moment's RELEASE window is the first recorded window (in Start order)
//     whose [Start, End] contains it;
//   - failing that, the latest window that started before it (a moment in the
//     gap after a test belongs to that test);
//   - a moment before every window is startup traffic (index -1).
//
// The first CONTAINING window, not the latest-started one, on purpose: a long
// early window that later tests overlap owns what was recorded inside it, so
// releasing that traffic only at a later test would make the early test's own
// handler wait on it.
//
// The release index is monotone in time, so "released by the current window"
// is a prefix of the recording; ReleaseHorizon returns its end.
//
// Immutable after NewWindowSchedule and safe for concurrent readers. Every
// method is nil-safe: a nil or empty schedule means the replayer sent no
// windows, and then everything counts as released.
type WindowSchedule struct {
	windows []TestWindow
	// maxEnd[i] is the latest End among windows[0..i]. Monotone, so the first
	// window containing t is a binary search over it.
	maxEnd []time.Time
	// byTest is each test case's window, by its name (Of), and byMember
	// each test merged into a window no test names (IndexOfTest): built at
	// the first lookup, once.
	byTestOnce sync.Once
	byTest     map[string]int
	byMember   map[string]int
}

// NewWindowSchedule sorts a copy of ws by Start. Windows with a zero Start are
// dropped; a zero End, or one before Start, is clamped to Start.
func NewWindowSchedule(ws []TestWindow) *WindowSchedule {
	s := &WindowSchedule{windows: make([]TestWindow, 0, len(ws))}
	for _, w := range ws {
		if w.Start.IsZero() {
			continue
		}
		if w.End.IsZero() || w.End.Before(w.Start) {
			w.End = w.Start
		}
		s.windows = append(s.windows, w)
	}
	sort.SliceStable(s.windows, func(i, j int) bool { return s.windows[i].Start.Before(s.windows[j].Start) })
	s.maxEnd = make([]time.Time, len(s.windows))
	for i, w := range s.windows {
		s.maxEnd[i] = w.End
		if i > 0 && s.maxEnd[i-1].After(w.End) {
			s.maxEnd[i] = s.maxEnd[i-1]
		}
	}
	return s
}

// Len is the number of windows.
func (s *WindowSchedule) Len() int {
	if s == nil {
		return 0
	}
	return len(s.windows)
}

// Of returns the window recorded for the test case named testCase: false
// when the schedule holds none (a case with no request time opens none). A
// name recorded twice gives its first window in Start order. A test merged
// into a window no test names (TestWindow.Tests) has none of its own.
func (s *WindowSchedule) Of(testCase string) (TestWindow, bool) {
	if s.Len() == 0 || testCase == "" {
		return TestWindow{}, false
	}
	s.indexTests()
	i, ok := s.byTest[testCase]
	if !ok {
		return TestWindow{}, false
	}
	return s.windows[i], true
}

// IndexOfTest is the index, in Start order (Window), of the window testCase
// runs in: the one Of returns, else the merged window that lists it
// (TestWindow.Tests); false where there is neither.
func (s *WindowSchedule) IndexOfTest(testCase string) (int, bool) {
	if s.Len() == 0 || testCase == "" {
		return 0, false
	}
	s.indexTests()
	if i, ok := s.byTest[testCase]; ok {
		return i, true
	}
	i, ok := s.byMember[testCase]
	return i, ok
}

// indexTests builds byTest and byMember, once: a name's first window in
// Start order.
func (s *WindowSchedule) indexTests() {
	s.byTestOnce.Do(func() {
		s.byTest = make(map[string]int, len(s.windows))
		for i, w := range s.windows {
			if _, seen := s.byTest[w.TestCase]; !seen && w.TestCase != "" {
				s.byTest[w.TestCase] = i
			}
			for _, t := range w.Tests {
				if _, seen := s.byMember[t]; !seen {
					if s.byMember == nil {
						s.byMember = map[string]int{}
					}
					s.byMember[t] = i
				}
			}
		}
	})
}

// After reports whether t is later than every window's End: recorded after
// the last recorded test ended (a set's teardown). False for an empty
// schedule: nothing tells a teardown there.
func (s *WindowSchedule) After(t time.Time) bool {
	n := s.Len()
	if n == 0 {
		return false
	}
	return t.After(s.maxEnd[n-1])
}

// Window returns window i in Start order.
func (s *WindowSchedule) Window(i int) TestWindow {
	return s.windows[i]
}

// CurrentIndex returns the index of the last window that starts at or before
// start — the window the replay is in when the agent's current window starts
// at start — or -1 when start precedes every window.
func (s *WindowSchedule) CurrentIndex(start time.Time) int {
	if s.Len() == 0 {
		return -1
	}
	return sort.Search(len(s.windows), func(i int) bool { return s.windows[i].Start.After(start) }) - 1
}

// ReleaseIndex returns the index of t's release window (see WindowSchedule),
// or -1 when t is startup traffic.
func (s *WindowSchedule) ReleaseIndex(t time.Time) int {
	p := s.CurrentIndex(t) + 1 // windows[0:p] started at or before t
	if p == 0 {
		return -1
	}
	if i := sort.Search(p, func(i int) bool { return !s.maxEnd[i].Before(t) }); i < p {
		return i
	}
	return p - 1
}

// GapBefore is the gap before the window that starts at start: the recorded
// time after every window that starts earlier has ended and before start.
// from is its exclusive lower bound, the latest end of those windows. ok is
// false when no window starts at start, for the first window (what precedes
// it is the app's startup), when an earlier window reaches start (no gap), and
// when two windows start at start: a window is known by its start alone (the
// agent's current window, MockFilterParams.AfterTime), so which of their two
// tests a call in flight then belongs to cannot be told, and neither gets it.
//
// A request recorded in it and answered at or after start was in flight when
// that window's test began, and is that test's (InFlightInto).
func (s *WindowSchedule) GapBefore(start time.Time) (from time.Time, ok bool) {
	n := s.Len()
	if n == 0 || start.IsZero() {
		return time.Time{}, false
	}
	i := sort.Search(n, func(i int) bool { return !s.windows[i].Start.Before(start) })
	if i == n || !s.windows[i].Start.Equal(start) || i == 0 {
		return time.Time{}, false
	}
	if i+1 < n && s.windows[i+1].Start.Equal(start) {
		return time.Time{}, false
	}
	from = s.maxEnd[i-1]
	if !from.Before(start) {
		return time.Time{}, false
	}
	return from, true
}

// InFlightInto reports whether a request recorded at req and answered at res
// was in flight when the window that starts at start began: sent in the gap
// before it (GapBefore), answered at or after its start (SentInGapAnsweredBy).
// Such a call is that window's test's, whenever its answer landed; one sent and
// answered within the gap is no test's.
func (s *WindowSchedule) InFlightInto(req, res, start time.Time) bool {
	from, ok := s.GapBefore(start)
	return ok && SentInGapAnsweredBy(req, res, from, start)
}

// SentInGapAnsweredBy is the in-flight rule for a gap that runs from from
// (exclusive, GapBefore) to start: a call sent inside it and answered at or
// after start. Every site that applies the rule calls this one predicate,
// directly or through InFlightInto: the agent's on-disk load and its filter,
// the mock manager's staging and its windowed read, the mapping write and the
// window ownership (DeriveWindowMappings).
func SentInGapAnsweredBy(req, res, from, start time.Time) bool {
	return req.After(from) && req.Before(start) && !res.IsZero() && !res.Before(start)
}

// InFlightEligible reports whether m may be attributed by the in-flight rule:
// a per-test mock (DeriveLifetime; a mock not derived yet is looked at through
// a copy) that is neither carry-over nor window-bound.
//   - A carry-over mock (IsCarryOver, a Pulsar SEND's receipt) is released at
//     the window its recorded time belongs to, and a moment in the gap after a
//     test belongs to that test (WindowSchedule); the carry-over tier keeps it
//     reachable while the next test runs. Its receipt time says nothing about
//     which test made the publish.
//   - A window-bound mock (a connection failure) is served from its own window
//     or the startup band only.
//   - A session or connection mock is every test's already.
//   - A mock placed at its answer (IsAnswerPlaced) is its answer's window's
//     test's, wherever it was sent (PlaceOf): the in-flight rule would give
//     one sent in a gap to the next test whenever its answer came.
func InFlightEligible(m *Mock) bool {
	return m != nil && !WindowBound(m.Kind) && lifetimeOf(m) == LifetimePerTest && !IsCarryOver(m) && !IsAnswerPlaced(m)
}

// EdgeSkewEligible reports whether the edge rule may give m to a window: a
// mock placed (PlaceOf) outside every recorded window, within
// MockWindowEdgeSkew of one's edge, is that window's test's (OwnerOf). Only a
// plain per-test timed mock is: the shape the in-flight rule applies to
// (InFlightEligible: per-test, neither carry-over, window-bound nor placed at
// its answer), not one a StagingWithStartup need stages with the set's
// startup (IsStagedAtStaging), and not one whose consume outside its window
// is flagged for the replay to attribute by the release rule
// (MarksOutOfWindow: every per-test mock of a carry-over kind, whatever its
// own predicate says). Every other shape has its own rule for where it is
// served (a carry-over mock its release window, a window-bound one its own
// window, one placed at its answer the window its answer is in, a declared
// one the set's staging, a carry-over kind's the release rule), and keeps it:
// the edge rule never routes one.
func EdgeSkewEligible(m *Mock) bool {
	return InFlightEligible(m) && !IsStagedAtStaging(m) && !MarksOutOfWindow(m)
}

// Contains reports whether t is inside one of the windows, ends included.
func (s *WindowSchedule) Contains(t time.Time) bool {
	return s.IndexOf(t) >= 0
}

// IndexOf is the index of the earliest-starting window t is inside (ends
// included), -1 when it is inside none.
func (s *WindowSchedule) IndexOf(t time.Time) int {
	p := s.CurrentIndex(t) + 1 // windows[0:p] started at or before t
	if p <= 0 {
		return -1
	}
	// maxEnd[i] >= t: some window j <= i ends at or after t, and starts no
	// later than window i, so at or before t.
	i := sort.Search(p, func(i int) bool { return !s.maxEnd[i].Before(t) })
	if i >= p {
		return -1
	}
	for j := 0; j <= i; j++ {
		if !s.windows[j].End.Before(t) {
			return j
		}
	}
	return i
}

// Released reports whether traffic recorded at t is due once the current
// window starts at currentStart: its release window starts at or before it.
// With no schedule everything is released.
func (s *WindowSchedule) Released(t, currentStart time.Time) bool {
	if s.Len() == 0 {
		return true
	}
	return s.ReleaseIndex(t) <= s.CurrentIndex(currentStart)
}

// ReleasedFirstAt reports whether the window that starts at start is the
// first at which traffic recorded at t is released: Released(t, start) holds,
// and held at no window that starts earlier. A window is known by its start,
// as the agent's current window is, so two windows that start together both
// release it. False for startup traffic, which no window releases first, and
// with no schedule.
func (s *WindowSchedule) ReleasedFirstAt(t, start time.Time) bool {
	if s.Len() == 0 {
		return false
	}
	i := s.ReleaseIndex(t)
	return i >= 0 && s.windows[i].Start.Equal(start)
}

// ReleaseHorizon returns the latest recorded moment that is released once the
// current window starts at currentStart: Released(t, currentStart) holds
// exactly for t <= horizon. ok is false when the horizon is unbounded (the
// current window is the last one, or there is no schedule).
func (s *WindowSchedule) ReleaseHorizon(currentStart time.Time) (horizon time.Time, ok bool) {
	n := s.Len()
	if n == 0 {
		return time.Time{}, false
	}
	c := s.CurrentIndex(currentStart)
	if c == n-1 {
		return time.Time{}, false
	}
	// Before window c+1 starts only windows 0..c have started, so nothing
	// there can release later than c. From that start on, a moment is still
	// c's (or earlier) exactly while some window 0..c contains it.
	horizon = s.windows[c+1].Start.Add(-time.Nanosecond)
	if c >= 0 && s.maxEnd[c].After(horizon) {
		horizon = s.maxEnd[c]
	}
	return horizon, true
}

// OwnerOf returns the recorded test window index that owns timestamp t:
//
//  1. When one or more windows contain t (Start <= t <= End), the smallest
//     containing window owns it (the innermost request in a nested pair).
//     Partly overlapping (non-nested) windows that both contain t are ranked
//     by closest request start (smallest t - Start, i.e. latest Start), then
//     smallest End (shortest duration on equal Start), then stable ID
//     (TestCase lexicographically, then schedule index).
//  2. When t is outside every recorded window and maxSkew > 0, the window
//     whose nearer edge is closest within maxSkew (dist <= maxSkew) owns it
//     (edge skew), tie-breaking by closer request start, shorter duration,
//     then stable ID.
//
// inside reports whether t was contained in the returned window (true for
// rule 1, false for edge-skew rule 2). ok is false when the schedule is empty,
// t is zero, or t lies outside every window beyond maxSkew.
func (s *WindowSchedule) OwnerOf(t time.Time, maxSkew time.Duration) (index int, inside, ok bool) {
	n := s.Len()
	if n == 0 || t.IsZero() {
		return -1, false, false
	}
	p := s.CurrentIndex(t) + 1 // windows[0:p] started at or before t
	if p > 0 {
		first := sort.Search(p, func(i int) bool { return !s.maxEnd[i].Before(t) })
		best := -1
		for i := first; i < p; i++ {
			w := s.windows[i]
			if w.End.Before(t) {
				continue
			}
			if best < 0 || betterContainingOwner(w, i, s.windows[best], best) {
				best = i
			}
		}
		if best >= 0 {
			return best, true, true
		}
	}
	if maxSkew <= 0 {
		return -1, false, false
	}
	best := -1
	var bestDist time.Duration
	if p > 0 {
		cutoff := t.Add(-maxSkew)
		first := sort.Search(p, func(i int) bool { return !s.maxEnd[i].Before(cutoff) })
		for i := first; i < p; i++ {
			w := s.windows[i]
			if w.End.Before(cutoff) {
				continue
			}
			dist := t.Sub(w.End)
			if best < 0 || betterEdgeOwner(t, w, i, dist, s.windows[best], best, bestDist) {
				best, bestDist = i, dist
			}
		}
	}
	limit := t.Add(maxSkew)
	for i := p; i < n && !s.windows[i].Start.After(limit); i++ {
		w := s.windows[i]
		dist := w.Start.Sub(t)
		if best < 0 || betterEdgeOwner(t, w, i, dist, s.windows[best], best, bestDist) {
			best, bestDist = i, dist
		}
	}
	if best >= 0 {
		return best, false, true
	}
	return -1, false, false
}

// EdgeOutcome is what the edge rule decides for a mock (EdgeDecision).
type EdgeOutcome uint8

const (
	// EdgeNone: the edge rule gives the mock to no window.
	EdgeNone EdgeOutcome = iota
	// EdgeOwned: the edge rule gives the mock to the window.
	EdgeOwned
	// EdgeLeftInFlight: the edge rule would give the mock to the window, but
	// leaves it to the in-flight rule: it is in flight into the next window,
	// which that rule gives it.
	EdgeLeftInFlight
)

// EdgeDecision is the edge rule for m: a plain per-test timed mock
// (EdgeSkewEligible) placed (PlaceOf) outside every window, within
// MockWindowEdgeSkew of a window's edge, belongs to the window whose edge is
// nearest (OwnerOf). It gives no window a mock another rule places: one
// placed before the first window began is startup traffic (EdgeNone), and one
// in flight into the window after the gap it is in (sent in the gap, answered
// at or after that window's start: InFlightInto) is that window's
// (EdgeLeftInFlight, with the window the rule would have given it to). Every
// site that applies the edge rule reads it, through EdgeOwner: the mock
// manager's staging, its windowed read and its reclaim, the MySQL matcher's
// ownership rank, the agent's disk read, and the replay's mapping of what a
// test consumed (MockState.EdgeOwned).
func (s *WindowSchedule) EdgeDecision(m *Mock) (index int, outcome EdgeOutcome) {
	if m == nil || s.Len() == 0 {
		return -1, EdgeNone
	}
	place := PlaceOf(m)
	if place.IsZero() || place.Before(s.windows[0].Start) {
		return -1, EdgeNone
	}
	idx, inside, owned := s.OwnerOf(place, MockWindowEdgeSkew)
	if !owned || inside || !EdgeSkewEligible(m) {
		return -1, EdgeNone
	}
	if next := s.CurrentIndex(place) + 1; next < s.Len() && s.InFlightInto(m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock, s.windows[next].Start) {
		return idx, EdgeLeftInFlight
	}
	return idx, EdgeOwned
}

// EdgeOwnedBy reports whether the edge rule gives m to the window
// [start, end] (EdgeOwner). A mock not placed within MockWindowEdgeSkew
// outside one of that window's edges is no edge mock of it, which is told
// before the schedule is searched. False on a nil schedule.
func (s *WindowSchedule) EdgeOwnedBy(m *Mock, start, end time.Time) bool {
	if m == nil || s.Len() == 0 || start.IsZero() || end.IsZero() {
		return false
	}
	place := PlaceOf(m)
	nearStart := place.Before(start) && !place.Before(start.Add(-MockWindowEdgeSkew))
	nearEnd := place.After(end) && !place.After(end.Add(MockWindowEdgeSkew))
	if place.IsZero() || (!nearStart && !nearEnd) {
		return false
	}
	idx, ok := s.EdgeOwner(m)
	if !ok {
		return false
	}
	w := s.windows[idx]
	return w.Start.Equal(start) && w.End.Equal(end)
}

// EdgeOwner is the window the edge rule gives m to (EdgeDecision's EdgeOwned).
func (s *WindowSchedule) EdgeOwner(m *Mock) (index int, ok bool) {
	idx, outcome := s.EdgeDecision(m)
	if outcome != EdgeOwned {
		return -1, false
	}
	return idx, true
}

// OwnerOfSpan returns the recorded test window index that owns the recorded
// interval [start, end] (w.Start <= start && end <= w.End), using the same
// smallest-containing-window and partly-overlapping ranking as OwnerOf. When
// end is zero or before start, it reduces to the containing-window rule at
// start. Returns (-1, false) when no recorded window contains the full span.
func (s *WindowSchedule) OwnerOfSpan(start, end time.Time) (index int, ok bool) {
	n := s.Len()
	if n == 0 || start.IsZero() {
		return -1, false
	}
	if end.IsZero() || end.Before(start) {
		end = start
	}
	p := s.CurrentIndex(start) + 1 // windows[0:p] started at or before start
	if p <= 0 {
		return -1, false
	}
	first := sort.Search(p, func(i int) bool { return !s.maxEnd[i].Before(end) })
	best := -1
	for i := first; i < p; i++ {
		w := s.windows[i]
		if w.End.Before(end) {
			continue
		}
		if best < 0 || betterContainingOwner(w, i, s.windows[best], best) {
			best = i
		}
	}
	if best >= 0 {
		return best, true
	}
	return -1, false
}

// betterContainingOwner reports whether candidate a (at schedule index ia)
// outranks b (at ib) among windows containing a timestamp t:
//   - If one strictly encloses the other, the nested (inner) window wins.
//   - For partly overlapping (non-nested) windows, the closer request start
//     (larger Start, since Start <= t) wins, then shorter duration (earlier
//     End on equal Start), then stable TestCase ID, then schedule index.
//
// Because any window enclosing another has Start <= inner.Start (and strictly
// larger End when starts are equal), comparing (Start desc, End asc, TestCase
// asc, index asc) yields the exact winner of dropping enclosing windows first
// and ranking the remaining non-nested windows by closest Start then stable ID.
func betterContainingOwner(a TestWindow, ia int, b TestWindow, ib int) bool {
	if !a.Start.Equal(b.Start) {
		return a.Start.After(b.Start)
	}
	if !a.End.Equal(b.End) {
		return a.End.Before(b.End)
	}
	if a.TestCase != b.TestCase {
		return a.TestCase < b.TestCase
	}
	return ia < ib
}

// betterEdgeOwner reports whether candidate a (at schedule index ia, distance
// da to nearer edge) outranks b (at ib, distance db) for an edge-skewed
// timestamp t outside every recorded window.
func betterEdgeOwner(t time.Time, a TestWindow, ia int, da time.Duration, b TestWindow, ib int, db time.Duration) bool {
	if da != db {
		return da < db
	}
	sa, sb := absDuration(t.Sub(a.Start)), absDuration(t.Sub(b.Start))
	if sa != sb {
		return sa < sb
	}
	dura, durb := a.End.Sub(a.Start), b.End.Sub(b.Start)
	if dura != durb {
		return dura < durb
	}
	if a.TestCase != b.TestCase {
		return a.TestCase < b.TestCase
	}
	return ia < ib
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
