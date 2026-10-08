package models

import (
	"sort"
	"time"
)

// TestWindow is one recorded test case's [request, response] span: the window
// the replayer opens on the agent while that test runs (MockFilterParams
// AfterTime/BeforeTime).
type TestWindow struct {
	TestCase string    `json:"testCase,omitempty"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
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
