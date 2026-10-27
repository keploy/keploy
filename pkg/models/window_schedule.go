package models

import (
	"sort"
	"sync"
	"time"
)

// TestWindow is one recorded test case's [request, response] span: the window
// the replayer opens on the agent while that test runs (MockFilterParams
// AfterTime/BeforeTime).
type TestWindow struct {
	TestCase string    `json:"testCase,omitempty"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	// Clock names, under the proven switch, the clock Start is on: the test
	// case's Origin.Clock (RecordedWindow). "" is untrusted.
	Clock string `json:"clock,omitempty"`
}

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
	// byTest is each test case's window, by its name (Of): built at the
	// first Of, once.
	byTestOnce sync.Once
	byTest     map[string]int
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
// name recorded twice gives its first window in Start order.
func (s *WindowSchedule) Of(testCase string) (TestWindow, bool) {
	if s.Len() == 0 || testCase == "" {
		return TestWindow{}, false
	}
	s.byTestOnce.Do(func() {
		s.byTest = make(map[string]int, len(s.windows))
		for i, w := range s.windows {
			if _, seen := s.byTest[w.TestCase]; !seen && w.TestCase != "" {
				s.byTest[w.TestCase] = i
			}
		}
	})
	i, ok := s.byTest[testCase]
	if !ok {
		return TestWindow{}, false
	}
	return s.windows[i], true
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
