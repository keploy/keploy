package manager

import (
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
// A window still open that started at or before t may yet be that test, and
// its answer is not known until it ends: when the earliest-started candidate
// is such a window, the span is not final, and Changed fires when it leaves
// the open ones; a tie waits for the open one. ok is false when no window open
// now started at or before t and no test case's span holds t: no test may own
// t any more.
//
// A span is noted for a window whose request became a test case before it
// left the open ones (ResolveKept with --sync, Window.Tested without); a
// window given up, left out, or never answered owns nothing here, nor does a
// test with no window (gRPC). The spans of the last maxRecentWindows test
// cases are kept: a mock asked about after that many later ones finds none.
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
