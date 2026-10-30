package manager

import (
	"slices"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// testSpan is a test case's span, [its request, its answer], in Unix
// nanoseconds.
type testSpan struct{ start, end int64 }

// TestSpan is the span of the test that owns a mock requested at a time
// (SyncMockManager.TestSpanAt): from its request to its answer, as its test
// case states them.
type TestSpan struct {
	Start, End time.Time
	// changed is closed when the open window the span waits on leaves the
	// open ones; nil once the span is final.
	changed <-chan struct{}
}

// Final reports whether the span is a test case's, as recorded. Until it is,
// the earliest-started candidate is a window still open, End is zero, and the
// owner may still change: ask again once Changed fires.
func (s TestSpan) Final() bool { return s.changed == nil }

// Changed is closed when the open window the span waits on leaves the open
// ones: it may have become a test case, or not, and the owner be another. nil
// for a final span: it never fires.
func (s TestSpan) Changed() <-chan struct{} { return s.changed }

// TestSpanAt reports, under the proven switch, the span of the test that owns
// a mock requested at t, by recorded times alone, as keploy record maps a
// recording made without --sync (models.DeriveWindowMappings): of the test
// cases whose span [request, answer] holds t, the one whose request came first.
// It goes by the request time only, so a call sent in the gap before a test
// and answered once that test began, which DeriveWindowMappings gives that
// test (models.WindowSchedule.InFlightInto), is no test's here.
// A window still open that started at or before t may yet be that test, and
// its answer is not known until it ends: when the earliest-started candidate
// is such a window, the span is not final, and Changed fires when it leaves
// the open ones; a tie waits for the open one. ok is false when no window open
// now started at or before t and no test case's span holds t: no test may own
// t any more.
//
// A span is noted for a window whose request became a test case before it
// left the open ones (ResolveKept with --sync, Window.Tested without), and for
// a test scope that ended (TestScope.End: keploy mock record's tests, which
// open no window); a window given up, left out, or never answered owns nothing
// here, nor does a scope abandoned, nor a test with no window and no scope
// (gRPC). A test scope still open is waited on as an open window is. The spans
// of the last maxRecentWindows test cases are kept: a mock asked about after
// that many later ones finds none.
func (m *SyncMockManager) TestSpanAt(t time.Time) (TestSpan, bool) {
	if m == nil || t.IsZero() {
		return TestSpan{}, false
	}
	n := t.UnixNano()
	m.mu.Lock()
	defer m.mu.Unlock()
	var open *Window
	for _, w := range m.open {
		if !w.start.After(t) && (open == nil || w.start.Before(open.start)) {
			open = w
		}
	}
	// An open test scope is waited on as an open window is; of a window and
	// a scope opened at the same instant, the window.
	var scope *TestScope
	for _, s := range m.scopes {
		if !s.start.After(t) && (scope == nil || s.start.Before(scope.start)) {
			scope = s
		}
	}
	if scope != nil && open != nil && !scope.start.Before(open.start) {
		scope = nil
	}
	var tested testSpan
	found := false
	for _, s := range m.testSpans {
		if s.start > n || n > s.end {
			continue
		}
		// The earliest request; of two at once, the longer, so the choice
		// does not hang on the order they were noted in.
		if !found || s.start < tested.start || (s.start == tested.start && s.end > tested.end) {
			tested, found = s, true
		}
	}
	if scope != nil && (!found || scope.start.UnixNano() <= tested.start) {
		if scope.changed == nil {
			scope.changed = make(chan struct{})
		}
		return TestSpan{Start: scope.start, changed: scope.changed}, true
	}
	if open != nil && (!found || open.start.UnixNano() <= tested.start) {
		ch, ok := m.spanWatch[open]
		if !ok {
			if m.spanWatch == nil {
				m.spanWatch = make(map[*Window]chan struct{})
			}
			ch = make(chan struct{})
			m.spanWatch[open] = ch
		}
		return TestSpan{Start: open.start, changed: ch}, true
	}
	if !found {
		return TestSpan{}, false
	}
	return TestSpan{Start: time.Unix(0, tested.start), End: time.Unix(0, tested.end)}, true
}

// Tested notes, under the proven switch, that w's request became a test case
// spanning [start, end] (its request and answer, as the test case states
// them), in a recording whose agent does not resolve its windows (made
// without --sync): called as the test case is handed on, before its window
// ends. A window that has left the open ones (given up, or ended) is no
// test's here (TestSpanAt). Nil-safe.
func (w *Window) Tested(start, end time.Time) {
	if w == nil || !provenswitch.On() {
		return
	}
	m := w.m
	m.mu.Lock()
	if w.idx >= 0 {
		m.noteTestSpanLocked(start, end)
	}
	m.mu.Unlock()
}

// noteTestSpanLocked keeps, under the proven switch, the span of a test case
// whose window is still open, for TestSpanAt: one that ends before it starts,
// or has no end, ends where it starts, as models.DeriveWindowMappings takes
// it. Caller holds mu.
func (m *SyncMockManager) noteTestSpanLocked(start, end time.Time) {
	if !provenswitch.On() || start.IsZero() {
		return
	}
	s := testSpan{start: start.UnixNano(), end: start.UnixNano()}
	if end.After(start) {
		s.end = end.UnixNano()
	}
	if len(m.testSpans) < maxRecentWindows {
		m.testSpans = append(m.testSpans, s)
		return
	}
	m.testSpans[m.testSpansNext] = s
	m.testSpansNext = (m.testSpansNext + 1) % maxRecentWindows
}

// windowLeftLocked tells the waiter on w's span (TestSpanAt) that w has left
// the open ones. Caller holds mu.
func (m *SyncMockManager) windowLeftLocked(w *Window) {
	if ch, ok := m.spanWatch[w]; ok {
		close(ch)
		delete(m.spanWatch, w)
	}
}

// TestScope is, under the proven switch, a test the run's test harness said
// began (the scope API: keploy mock record), in a recording whose agent opens
// no window for it, as the test answers no request of keploy's: its calls are
// all it makes. Until it ends, a mock requested at or after its begin may be
// its, and TestSpanAt waits on it as on an open window; ended, its span [its
// begin, its end] is a test case's, as a window's noted test case is.
type TestScope struct {
	m     *SyncMockManager
	start time.Time
	// changed is the channel TestSpanAt handed out for the scope, closed as it
	// leaves the open ones; nil until a wait asks for it. ended: it left
	// them. Guarded by m.mu.
	changed chan struct{}
	ended   bool
}

// BeginTestScope opens, under the proven switch, the scope of a test that
// began at start (TestScope). nil with the switch off, a zero start, or a nil
// manager: nothing waits on it, and its End notes nothing.
func (m *SyncMockManager) BeginTestScope(start time.Time) *TestScope {
	if m == nil || start.IsZero() || !provenswitch.On() {
		return nil
	}
	s := &TestScope{m: m, start: start}
	m.mu.Lock()
	m.scopes = append(m.scopes, s)
	m.mu.Unlock()
	return s
}

// End notes that the scope's test ended at end: its span [begin, end] is a
// test case's (noteTestSpanLocked), and the scope leaves the open ones, which
// tells its waiters. Once, and nil-safe: an End after End or Abandon does
// nothing.
func (s *TestScope) End(end time.Time) { s.leave(end, true) }

// Abandon takes the scope out of the open ones with no span noted: its test
// is no test case (its begin failed, or the recording it was opened in was
// reset). Its waiters are told, and find no test of its. Once, and nil-safe.
func (s *TestScope) Abandon() { s.leave(time.Time{}, false) }

func (s *TestScope) leave(end time.Time, tested bool) {
	if s == nil {
		return
	}
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	// Noted before it leaves the open ones, so a waiter told finds it.
	if tested {
		m.noteTestSpanLocked(s.start, end)
	}
	m.scopes = slices.DeleteFunc(m.scopes, func(o *TestScope) bool { return o == s })
	if s.changed != nil {
		close(s.changed)
	}
}
