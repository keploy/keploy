package manager

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// closed reports whether ch is closed (never blocks).
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Without --sync: an open window started at or before t may be t's test, its
// answer unknown (not final) however it is answered, until it leaves the open
// ones; Changed fires then, and a window noted a test case (Tested) gives its
// test case's span, final; one never noted gives none.
func TestTestSpanAtWaitsForTheWindowToEndATestCase(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	for _, tested := range []bool{true, false} {
		mgr := New(zap.NewNop())
		if _, ok := mgr.TestSpanAt(t0, nil); ok {
			t.Fatal("no window is open, and no test case recorded")
		}
		w := mgr.OpenWindow(t0, nil)
		if _, ok := mgr.TestSpanAt(sec(-1), nil); ok {
			t.Fatal("a window opened after t holds t")
		}
		span, ok := mgr.TestSpanAt(sec(1), nil)
		if !ok || span.Final() || !span.Start.Equal(t0) || !span.End.IsZero() {
			t.Fatalf("an open window: %+v %v, want it, not final", span, ok)
		}
		if again, _ := mgr.TestSpanAt(sec(2), nil); again.Changed() != span.Changed() {
			t.Fatal("two waits on one window got two channels")
		}
		w.Yield(sec(3))
		if tested {
			w.Tested(t0, sec(3))
		}
		if closed(span.Changed()) {
			t.Fatal("Changed fired before the window ended")
		}
		if again, ok := mgr.TestSpanAt(sec(1), nil); !ok || again.Final() {
			t.Fatalf("an open window answered: %+v %v, want not final until it ends", again, ok)
		}
		w.Close()
		if !closed(span.Changed()) {
			t.Fatal("Changed did not fire as the window ended")
		}
		got, ok := mgr.TestSpanAt(sec(1), nil)
		if !tested {
			if ok {
				t.Fatalf("a window that was no test case holds t: %+v", got)
			}
			continue
		}
		if !ok || !got.Final() || !got.Start.Equal(t0) || !got.End.Equal(sec(3)) || got.Changed() != nil {
			t.Fatalf("a test case's span: %+v %v, want [%v, %v], final", got, ok, t0, sec(3))
		}
		if _, ok := mgr.TestSpanAt(sec(4), nil); ok {
			t.Fatal("a test case answered before t holds t")
		}
		if len(mgr.spanWatch) != 0 {
			t.Fatalf("%d waits left after the window ended", len(mgr.spanWatch))
		}
	}
}

// With --sync, ResolveKept notes its request's span as it ends the window, so
// a wait on the window finds the test case's span once Changed fires.
func TestTestSpanAtTakesAResolvedKeptWindowsSpan(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	mgr := New(zap.NewNop())
	w := mgr.OpenWindow(t0, nil)
	span, _ := mgr.TestSpanAt(t0.Add(time.Second), nil)
	if !w.Keep() {
		t.Fatal("the window was given up")
	}
	mgr.ResolveKept(w, t0, t0.Add(3*time.Second), "test-1", false)
	if !closed(span.Changed()) {
		t.Fatal("Changed did not fire as ResolveKept ended the window")
	}
	got, ok := mgr.TestSpanAt(t0.Add(time.Second), nil)
	if !ok || !got.Final() || !got.End.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("a resolved kept window: %+v %v, want its test case's span", got, ok)
	}
	w.Close()
}

// One test case whose span holds t owns it. Of several, none does: they are
// Shared, and the span names every one's start, earliest first, each once, as
// no time tells which made the call. An open window that started no later
// than t is waited for, whenever it started.
func TestTestSpanAtSharedByOverlappingTestsHasNoOwner(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	mgr := New(zap.NewNop())
	mgr.mu.Lock()
	mgr.noteTestSpanLocked(sec(1), sec(5), 0)
	mgr.noteTestSpanLocked(sec(0), sec(10), 0)
	mgr.noteTestSpanLocked(sec(0), sec(8), 0)
	mgr.mu.Unlock()
	got, ok := mgr.TestSpanAt(sec(2), nil)
	if !ok || !got.Final() || !got.Shared() || !got.End.IsZero() || !slices.EqualFunc(got.Starts(), []time.Time{sec(0), sec(1)}, time.Time.Equal) {
		t.Fatalf("owner %+v %v, want shared by the tests begun at 0s and 1s, no owner", got, ok)
	}
	if got, ok := mgr.TestSpanAt(sec(9), nil); !ok || got.Shared() || !got.Start.Equal(sec(0)) || !got.End.Equal(sec(10)) {
		t.Fatalf("owner %+v %v, want [0, 10] alone", got, ok)
	}
	later := mgr.OpenWindow(sec(9), nil)
	if got, _ := mgr.TestSpanAt(sec(9), nil); got.Final() || !slices.EqualFunc(got.Starts(), []time.Time{sec(0), sec(9)}, time.Time.Equal) {
		t.Fatalf("got %+v: a window open since t was not waited for, or not named", got)
	}
	later.Close()
	if got, _ := mgr.TestSpanAt(sec(9), nil); !got.Final() || got.Shared() {
		t.Fatalf("got %+v: the window, ended no test case, is still a candidate", got)
	}
}

// When exactly one candidate is a test scope whose worker the calling process
// is, or descends from, that test alone is the candidate: the others, open or
// not, are not waited on. A worker with two candidates (nested scopes), or a
// chain naming none, leaves them all.
func TestTestSpanAtTakesTheCallingWorkersTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	mgr := New(zap.NewNop())
	a := mgr.BeginTestScope(sec(0), 100)
	b := mgr.BeginTestScope(sec(1), 200)
	w := mgr.OpenWindow(sec(1), nil)
	if got, ok := mgr.TestSpanAt(sec(2), []uint32{201, 200, 1}); !ok || got.Final() || !slices.EqualFunc(got.Starts(), []time.Time{sec(1)}, time.Time.Equal) {
		t.Fatalf("got %+v %v, want worker 200's open scope alone", got, ok)
	}
	b.End(sec(3))
	if got, ok := mgr.TestSpanAt(sec(2), []uint32{201, 200, 1}); !ok || !got.Final() || got.Shared() || !got.Start.Equal(sec(1)) || !got.End.Equal(sec(3)) {
		t.Fatalf("got %+v %v, want worker 200's [1s, 3s], final while the others are open", got, ok)
	}
	// The window and worker 200's test began at once: one start names both.
	if got, _ := mgr.TestSpanAt(sec(2), []uint32{300}); got.Final() || !slices.EqualFunc(got.Starts(), []time.Time{sec(0), sec(1)}, time.Time.Equal) {
		t.Fatalf("got %+v: a call from no candidate's worker is not every candidate's", got)
	}
	nested := mgr.BeginTestScope(sec(0), 200)
	if got, _ := mgr.TestSpanAt(sec(2), []uint32{200}); got.Final() || len(got.Starts()) != 2 {
		t.Fatalf("got %+v: a worker with two candidates named one", got)
	}
	nested.Abandon()
	w.Close()
	a.End(sec(4))
	if got, _ := mgr.TestSpanAt(sec(2), nil); !got.Final() || !got.Shared() || !slices.EqualFunc(got.Starts(), []time.Time{sec(0), sec(1)}, time.Time.Equal) {
		t.Fatalf("got %+v, want shared by 0s and 1s", got)
	}
}

// The spans are kept for the last maxRecentWindows test cases, a ring; one
// that ends before it starts, or has no end, ends where it starts.
func TestTestSpansAreARingOfTheLastTestCases(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	mgr := New(zap.NewNop())
	mgr.mu.Lock()
	mgr.noteTestSpanLocked(t0, time.Time{}, 0)
	mgr.noteTestSpanLocked(t0.Add(time.Second), t0, 0)
	if mgr.testSpans[0].end != mgr.testSpans[0].start || mgr.testSpans[1].end != mgr.testSpans[1].start {
		mgr.mu.Unlock()
		t.Fatalf("spans with no end, or one before the start: %+v", mgr.testSpans)
	}
	for i := 2; i < maxRecentWindows+2; i++ {
		mgr.noteTestSpanLocked(t0.Add(time.Duration(i)*time.Second), t0.Add(time.Duration(i)*time.Second), 0)
	}
	n := len(mgr.testSpans)
	mgr.mu.Unlock()
	if n != maxRecentWindows {
		t.Fatalf("%d spans kept, want %d", n, maxRecentWindows)
	}
	if _, ok := mgr.TestSpanAt(t0, nil); ok {
		t.Fatal("the oldest span was kept past the ring")
	}
	if _, ok := mgr.TestSpanAt(t0.Add(2*time.Second), nil); !ok {
		t.Fatal("a span within the ring was lost")
	}
}

// With the switch off no span is noted: nothing is kept for a recording that
// does not use them.
func TestTestSpansAreNotNotedWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	t0 := time.Unix(5000, 0)
	mgr := New(zap.NewNop())
	w := mgr.OpenWindow(t0, nil)
	w.Tested(t0, t0.Add(time.Second))
	w2 := mgr.OpenWindow(t0, nil)
	w2.Keep()
	mgr.ResolveKept(w2, t0, t0.Add(time.Second), "test-1", false)
	w.Close()
	w2.Close()
	if mgr.testSpans != nil {
		t.Fatalf("spans noted with the switch off: %+v", mgr.testSpans)
	}
	var nilWin *Window
	nilWin.Tested(t0, t0)
	var nilMgr *SyncMockManager
	if _, ok := nilMgr.TestSpanAt(t0, nil); ok {
		t.Fatal("a nil manager has test cases")
	}
}

// A window that has left the open ones is no test case's: noting it after it
// ended (given up, or ended by its ingress) keeps nothing.
func TestAWindowEndedIsNotNotedATestCase(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	mgr := New(zap.NewNop())
	w := mgr.OpenWindow(t0, nil)
	w.Close()
	w.Tested(t0, t0.Add(time.Second))
	if len(mgr.testSpans) != 0 {
		t.Fatalf("an ended window was noted a test case: %+v", mgr.testSpans)
	}
}

// A test scope (keploy mock record's tests, which open no window) is waited
// on as an open window is: a mock requested at or after its begin may be its
// test's, not final, until it leaves the open ones; Changed fires then. Ended,
// its span [begin, end] is a test case's, final; abandoned, it is none.
func TestTestSpanAtWaitsForATestScopeToEnd(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	for _, ended := range []bool{true, false} {
		mgr := New(zap.NewNop())
		s := mgr.BeginTestScope(t0, 0)
		if _, ok := mgr.TestSpanAt(sec(-1), nil); ok {
			t.Fatal("a scope begun after t holds t")
		}
		span, ok := mgr.TestSpanAt(sec(1), nil)
		if !ok || span.Final() || !span.Start.Equal(t0) || !span.End.IsZero() {
			t.Fatalf("an open scope: %+v %v, want it, not final", span, ok)
		}
		if again, _ := mgr.TestSpanAt(sec(2), nil); again.Changed() != span.Changed() {
			t.Fatal("two waits on one scope got two channels")
		}
		if ended {
			s.End(sec(3))
		} else {
			s.Abandon()
		}
		if !closed(span.Changed()) {
			t.Fatal("Changed did not fire as the scope left the open ones")
		}
		s.End(sec(9)) // once: a second leave notes nothing
		got, ok := mgr.TestSpanAt(sec(1), nil)
		if !ended {
			if ok || len(mgr.testSpans) != 0 {
				t.Fatalf("an abandoned scope holds t: %+v, spans %+v", got, mgr.testSpans)
			}
			continue
		}
		if !ok || !got.Final() || !got.Start.Equal(t0) || !got.End.Equal(sec(3)) {
			t.Fatalf("an ended scope's span: %+v %v, want [%v, %v], final", got, ok, t0, sec(3))
		}
		if _, ok := mgr.TestSpanAt(sec(4), nil); ok {
			t.Fatal("a test ended before t holds t")
		}
		if len(mgr.scopes) != 0 || len(mgr.testSpans) != 1 {
			t.Fatalf("after its end: %d scopes open, spans %+v; want none, one", len(mgr.scopes), mgr.testSpans)
		}
	}
}

// An open window and an open scope that both started by t are both
// candidates; the earlier-started is waited on (the window on a tie), and
// once neither is open the test cases left decide: one alone owns t.
func TestTestSpanAtWaitsForEveryOpenWindowAndScope(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	mgr := New(zap.NewNop())
	w := mgr.OpenWindow(sec(1), nil)
	s := mgr.BeginTestScope(sec(2), 0)
	span, ok := mgr.TestSpanAt(sec(3), nil)
	if !ok || !span.Start.Equal(sec(1)) || span.Final() || len(span.Starts()) != 2 {
		t.Fatalf("got %+v %v, want the window's wait (begun first), both named", span, ok)
	}
	w.Close()
	span, ok = mgr.TestSpanAt(sec(3), nil)
	if !ok || !span.Start.Equal(sec(2)) || span.Final() {
		t.Fatalf("got %+v %v, want the scope's wait", span, ok)
	}
	tie := mgr.OpenWindow(sec(2), nil)
	if got, _ := mgr.TestSpanAt(sec(3), nil); got.Changed() == span.Changed() {
		t.Fatal("a window and a scope begun at once: the scope was waited on, not the window")
	}
	tie.Close()
	s.End(sec(5))
	later := mgr.BeginTestScope(sec(4), 0)
	if got, ok := mgr.TestSpanAt(sec(3), nil); !ok || !got.Final() || got.Shared() || !got.Start.Equal(sec(2)) || !got.End.Equal(sec(5)) {
		t.Fatalf("got %+v %v, want the ended test's span [2s, 5s]: the open scope began after t", got, ok)
	}
	if got, _ := mgr.TestSpanAt(sec(4), nil); got.Final() {
		t.Fatalf("got %+v: the scope open since t was not waited for", got)
	}
	later.Abandon()
}

// With the switch off no scope opens, and nothing is noted.
func TestTestScopesAreNotOpenedWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	t0 := time.Unix(5000, 0)
	mgr := New(zap.NewNop())
	s := mgr.BeginTestScope(t0, 0)
	if s != nil {
		t.Fatal("a scope opened with the switch off")
	}
	s.End(t0.Add(time.Second))
	s.Abandon()
	if _, ok := mgr.TestSpanAt(t0, nil); ok || mgr.testSpans != nil || mgr.scopes != nil {
		t.Fatalf("with the switch off: spans %+v, scopes %+v", mgr.testSpans, mgr.scopes)
	}
	var nilMgr *SyncMockManager
	if nilMgr.BeginTestScope(t0, 0) != nil || mgr.BeginTestScope(time.Time{}, 0) != nil {
		t.Fatal("a nil manager, or a zero begin, opened a scope")
	}
}
