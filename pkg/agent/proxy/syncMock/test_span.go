package manager

import (
	"slices"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// testSpan is a test case's span, [its request, its answer], in Unix
// nanoseconds, and the pid of the worker that reported it (a test scope's;
// 0 for a window's, or a scope with none).
type testSpan struct {
	start, end int64
	pid        uint32
	// w is the window whose test case it is (Window.Tested), nil for a
	// scope's or one noted as its window ends: while w is still open it is
	// that one test's candidate, never a second one beside it.
	w *Window
}

// TestSpan is the span of the test that owns a mock requested at a time
// (SyncMockManager.TestSpanAt): from its request to its answer, as its test
// case states them, or, when more than one test may own it (Shared), the
// starts of every one of them.
type TestSpan struct {
	// Start is the owner's, or the earliest candidate's while there is no
	// one owner; End is the owner's answer, zero until the span is final and
	// when it is Shared.
	Start, End time.Time
	// starts is when each test that may own the request began, earliest
	// first, each instant once.
	starts []time.Time
	// shared: more than one test may own the request (Shared).
	shared bool
	// changed is closed when the open window the span waits on leaves the
	// open ones; nil once the span is final.
	changed <-chan struct{}
}

// Final reports whether the span's candidates are all test cases, as
// recorded. Until they are, a window or test scope still open may yet be a
// test that owns the request, End is zero, and the owner may still change: ask
// again once Changed fires.
func (s TestSpan) Final() bool { return s.changed == nil }

// Changed is closed when the open window the span waits on leaves the open
// ones: it may have become a test case, or not, and the owner be another. nil
// for a final span: it never fires.
func (s TestSpan) Changed() <-chan struct{} { return s.changed }

// Shared reports whether more than one test may own the request: their
// windows overlap at it, and the connection's process does not tell one of
// them as the owner (models.CallOwner). No owner can be told then: no time
// decides it. Two such tests that began at one instant are Shared, though
// Starts names their instant once.
func (s TestSpan) Shared() bool { return s.shared }

// Starts is when each test that may own the request began, earliest first:
// the owner's alone when there is one.
func (s TestSpan) Starts() []time.Time { return slices.Clone(s.starts) }

// TestSpanAt reports, under the proven switch, the span of the test that owns
// a mock requested at t, by recorded times and the worker that made the call:
// the candidates are the test cases whose span [request, answer] holds t, and
// the windows and test scopes still open that started at or before t, which
// may yet be such a test. chain is the calling connection's process and its
// ancestors (utils.ProcAncestors; nil when unknown). models.CallOwner, the
// rule keploy mock record's mapping asks too, tells the owner among them:
// when it names one, that test alone is the candidate (a worker's test, or of
// one worker's nested tests the innermost, for a call from the worker or a
// process it started; of one worker's nested tests the innermost, too, for a
// call from a process of no candidate's worker). That is the only tie-break: while any candidate is
// still open the span is not final, and Changed fires when the one it waits
// on leaves the open ones; once every candidate is a test case, one is the
// owner, and more than one are Shared, as their windows overlap at t and
// nothing tells which made the call (parallel workers the process names
// none of, concurrent requests). ok is false when there is no candidate: no
// test may own t any more.
//
// It goes by the request time only, so a call sent in the gap before a test
// and answered once that test began (models.WindowSchedule.InFlightInto) is
// no test's here.
//
// A span is noted for a window whose request became a test case before it
// left the open ones (ResolveKept with --sync, Window.Tested without), and for
// a test scope that ended (TestScope.End: keploy mock record's tests, which
// open no window); a window given up, left out, or never answered owns nothing
// here, nor does a scope abandoned, nor a test with no window and no scope
// (gRPC). The spans of the last maxRecentWindows test cases are kept: a mock
// asked about after that many later ones finds none.
func (m *SyncMockManager) TestSpanAt(t time.Time, chain []uint32) (TestSpan, bool) {
	if m == nil || t.IsZero() {
		return TestSpan{}, false
	}
	n := t.UnixNano()
	m.mu.Lock()
	defer m.mu.Unlock()
	type candidate struct {
		start time.Time
		end   int64
		pid   uint32
		// window or scope is the candidate's while it is open.
		window *Window
		scope  *TestScope
	}
	var cands []candidate
	for _, w := range m.open {
		if !w.start.After(t) {
			cands = append(cands, candidate{start: w.start, window: w})
		}
	}
	for _, s := range m.scopes {
		if !s.start.After(t) {
			cands = append(cands, candidate{start: s.start, pid: s.pid, scope: s})
		}
	}
	for _, s := range m.testSpans {
		if s.w != nil && s.w.m == m && s.w.idx >= 0 {
			// Its window is open, and a candidate already.
			continue
		}
		if s.start <= n && n <= s.end {
			cands = append(cands, candidate{start: time.Unix(0, s.start), end: s.end, pid: s.pid})
		}
	}
	if len(cands) == 0 {
		return TestSpan{}, false
	}
	// The process that made the call names its test, when it tells one.
	weigh := make([]models.OwnerCandidate, len(cands))
	for i, c := range cands {
		weigh[i] = models.OwnerCandidate{PID: c.pid, Start: c.start}
	}
	if i := models.CallOwner(weigh, chain); i >= 0 {
		cands = cands[i : i+1]
	}
	// Earliest first; of two at once, an open one (it is waited on), the
	// window before the scope, so the choice does not hang on the order they
	// were noted in.
	rank := func(c candidate) int {
		switch {
		case c.window != nil:
			return 0
		case c.scope != nil:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(cands, func(a, b candidate) int {
		if c := a.start.Compare(b.start); c != 0 {
			return c
		}
		return rank(a) - rank(b)
	})
	span := TestSpan{Start: cands[0].start, shared: len(cands) > 1}
	for _, c := range cands {
		if len(span.starts) == 0 || !span.starts[len(span.starts)-1].Equal(c.start) {
			span.starts = append(span.starts, c.start)
		}
	}
	for _, c := range cands {
		switch {
		case c.window != nil:
			ch, ok := m.spanWatch[c.window]
			if !ok {
				if m.spanWatch == nil {
					m.spanWatch = make(map[*Window]chan struct{})
				}
				ch = make(chan struct{})
				m.spanWatch[c.window] = ch
			}
			span.changed = ch
			return span, true
		case c.scope != nil:
			if c.scope.changed == nil {
				c.scope.changed = make(chan struct{})
			}
			span.changed = c.scope.changed
			return span, true
		}
	}
	if len(cands) == 1 {
		span.End = time.Unix(0, cands[0].end)
	}
	return span, true
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
		m.noteTestSpanLocked(start, end, 0, w)
	}
	m.mu.Unlock()
}

// noteTestSpanLocked keeps, under the proven switch, the span of a test case
// whose window is still open, for TestSpanAt: one that ends before it starts,
// or has no end, ends where it starts, as models.DeriveWindowMappings takes
// it. pid is the worker that reported it (a test scope's), or 0; w the
// window whose test case it is while that window stays open, or nil. Caller
// holds mu.
func (m *SyncMockManager) noteTestSpanLocked(start, end time.Time, pid uint32, w *Window) {
	if !provenswitch.On() || start.IsZero() {
		return
	}
	s := testSpan{start: start.UnixNano(), end: start.UnixNano(), pid: pid, w: w}
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
	// pid is the worker that reported the test (its scope's pid), or 0.
	pid uint32
	// changed is the channel TestSpanAt handed out for the scope, closed as it
	// leaves the open ones; nil until a wait asks for it. ended: it left
	// them. Guarded by m.mu.
	changed chan struct{}
	ended   bool
}

// BeginTestScope opens, under the proven switch, the scope of a test that
// began at start (TestScope), reported by the worker pid (0 when none said
// which): a call whose process is that worker, or descends from it, is that
// test's when models.CallOwner tells it (TestSpanAt). nil with the switch
// off, a zero start, or a nil manager: nothing waits on it, and its End notes
// nothing.
func (m *SyncMockManager) BeginTestScope(start time.Time, pid uint32) *TestScope {
	if m == nil || start.IsZero() || !provenswitch.On() {
		return nil
	}
	s := &TestScope{m: m, start: start, pid: pid}
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
		m.noteTestSpanLocked(s.start, end, s.pid, nil)
	}
	m.scopes = slices.DeleteFunc(m.scopes, func(o *TestScope) bool { return o == s })
	if s.changed != nil {
		close(s.changed)
	}
}
