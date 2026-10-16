package manager

import (
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
		if _, ok := mgr.TestSpanAt(t0); ok {
			t.Fatal("no window is open, and no test case recorded")
		}
		w := mgr.OpenWindow(t0, nil)
		if _, ok := mgr.TestSpanAt(sec(-1)); ok {
			t.Fatal("a window opened after t holds t")
		}
		span, ok := mgr.TestSpanAt(sec(1))
		if !ok || span.Final() || !span.Start.Equal(t0) || !span.End.IsZero() {
			t.Fatalf("an open window: %+v %v, want it, not final", span, ok)
		}
		if again, _ := mgr.TestSpanAt(sec(2)); again.Changed() != span.Changed() {
			t.Fatal("two waits on one window got two channels")
		}
		w.Yield(sec(3))
		if tested {
			w.Tested(t0, sec(3))
		}
		if closed(span.Changed()) {
			t.Fatal("Changed fired before the window ended")
		}
		if again, ok := mgr.TestSpanAt(sec(1)); !ok || again.Final() {
			t.Fatalf("an open window answered: %+v %v, want not final until it ends", again, ok)
		}
		w.Close()
		if !closed(span.Changed()) {
			t.Fatal("Changed did not fire as the window ended")
		}
		got, ok := mgr.TestSpanAt(sec(1))
		if !tested {
			if ok {
				t.Fatalf("a window that was no test case holds t: %+v", got)
			}
			continue
		}
		if !ok || !got.Final() || !got.Start.Equal(t0) || !got.End.Equal(sec(3)) || got.Changed() != nil {
			t.Fatalf("a test case's span: %+v %v, want [%v, %v], final", got, ok, t0, sec(3))
		}
		if _, ok := mgr.TestSpanAt(sec(4)); ok {
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
	span, _ := mgr.TestSpanAt(t0.Add(time.Second))
	if !w.Keep() {
		t.Fatal("the window was given up")
	}
	mgr.ResolveKept(w, t0, t0.Add(3*time.Second), "test-1", false)
	if !closed(span.Changed()) {
		t.Fatal("Changed did not fire as ResolveKept ended the window")
	}
	got, ok := mgr.TestSpanAt(t0.Add(time.Second))
	if !ok || !got.Final() || !got.End.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("a resolved kept window: %+v %v, want its test case's span", got, ok)
	}
	w.Close()
}

// Of the test cases whose span holds t, the one whose request came first owns
// it, as keploy record maps it; of two at once the longer; an open window that
// started no later than that one is waited for.
func TestTestSpanAtTakesTheEarliestRequest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	mgr := New(zap.NewNop())
	mgr.mu.Lock()
	mgr.noteTestSpanLocked(sec(1), sec(5))
	mgr.noteTestSpanLocked(sec(0), sec(10))
	mgr.noteTestSpanLocked(sec(0), sec(8))
	mgr.mu.Unlock()
	if got, ok := mgr.TestSpanAt(sec(2)); !ok || !got.Start.Equal(sec(0)) || !got.End.Equal(sec(10)) {
		t.Fatalf("owner %+v %v, want the earliest request, the longer of two at once", got, ok)
	}
	if got, ok := mgr.TestSpanAt(sec(9)); !ok || !got.End.Equal(sec(10)) {
		t.Fatalf("owner %+v %v, want [0, 10]", got, ok)
	}
	later := mgr.OpenWindow(sec(1), nil)
	if got, _ := mgr.TestSpanAt(sec(2)); !got.Final() {
		t.Fatal("a window open since after the owner's request was waited for")
	}
	tie := mgr.OpenWindow(sec(0), nil)
	if got, _ := mgr.TestSpanAt(sec(2)); got.Final() {
		t.Fatal("a window open since the owner's request was not waited for")
	}
	tie.Close()
	if got, _ := mgr.TestSpanAt(sec(2)); !got.Final() {
		t.Fatal("the tie, ended no test case, is still waited for")
	}
	later.Close()
}

// The spans are kept for the last maxRecentWindows test cases, a ring; one
// that ends before it starts, or has no end, ends where it starts.
func TestTestSpansAreARingOfTheLastTestCases(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	mgr := New(zap.NewNop())
	mgr.mu.Lock()
	mgr.noteTestSpanLocked(t0, time.Time{})
	mgr.noteTestSpanLocked(t0.Add(time.Second), t0)
	if mgr.testSpans[0].end != mgr.testSpans[0].start || mgr.testSpans[1].end != mgr.testSpans[1].start {
		mgr.mu.Unlock()
		t.Fatalf("spans with no end, or one before the start: %+v", mgr.testSpans)
	}
	for i := 2; i < maxRecentWindows+2; i++ {
		mgr.noteTestSpanLocked(t0.Add(time.Duration(i)*time.Second), t0.Add(time.Duration(i)*time.Second))
	}
	n := len(mgr.testSpans)
	mgr.mu.Unlock()
	if n != maxRecentWindows {
		t.Fatalf("%d spans kept, want %d", n, maxRecentWindows)
	}
	if _, ok := mgr.TestSpanAt(t0); ok {
		t.Fatal("the oldest span was kept past the ring")
	}
	if _, ok := mgr.TestSpanAt(t0.Add(2 * time.Second)); !ok {
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
	if _, ok := nilMgr.TestSpanAt(t0); ok {
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
