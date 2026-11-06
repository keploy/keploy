package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

var placeT0 = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

func placeAt(s int) time.Time { return placeT0.Add(time.Duration(s) * time.Second) }

// placeWindows: test-1 [0s,10s], test-2 [20s,30s], test-3 [40s,50s].
func placeWindows() []models.TestWindow {
	return []models.TestWindow{
		{TestCase: "test-1", Start: placeAt(0), End: placeAt(10)},
		{TestCase: "test-2", Start: placeAt(20), End: placeAt(30)},
		{TestCase: "test-3", Start: placeAt(40), End: placeAt(50)},
	}
}

// The run's place follows the events the replayer drives, with the switch
// off: a set's staging begins it at the start-up, each test's window opens
// its window, a test's end its end, the set's end passes all; it never goes
// back, and every move wakes a waiter.
func TestRunPlace_FollowsTheReplayWithTheSwitchOff(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SeedRecordedWindows(placeWindows())
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	p := mm.RunPlace()
	if p.Reached != 0 || p.SetEnd || p.Windows.Len() != 3 {
		t.Fatalf("at the staging: %+v", p)
	}
	staging := p.Staging

	ch := mm.RunPlaceChanged()
	mm.SetMocksWithWindow(nil, nil, placeAt(20), placeAt(30))
	if !fired(ch) {
		t.Fatal("test-2's open woke no waiter")
	}
	if got := mm.RunPlace().Reached; got != models.RunOpen(1) {
		t.Fatalf("at test-2's open: %d, want %d", got, models.RunOpen(1))
	}
	mm.EndRunTest("test-2")
	if got := mm.RunPlace().Reached; got != models.RunEnd(1) {
		t.Fatalf("at test-2's end: %d, want %d", got, models.RunEnd(1))
	}

	// A test run again, or out of its recorded order, moves nothing back,
	// and wakes no waiter.
	ch = mm.RunPlaceChanged()
	mm.SetMocksWithWindow(nil, nil, placeAt(0), placeAt(10))
	mm.EndRunTest("test-1")
	mm.EndRunTest("no-such-test")
	if got := mm.RunPlace().Reached; got != models.RunEnd(1) || fired(ch) {
		t.Fatalf("went back to %d (woke: %v)", got, fired(ch))
	}

	mm.SetMocksWithWindowThreeTier(nil, nil, nil, placeAt(40), placeAt(50))
	if got := mm.RunPlace().Reached; got != models.RunOpen(2) {
		t.Fatalf("at test-3's open (three tiers): %d", got)
	}
	mm.EndRunSet()
	if !mm.RunPlace().SetEnd || !fired(ch) {
		t.Fatal("the set's end is not reached")
	}

	// The next set's staging begins a new place.
	ch = mm.RunPlaceChanged()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	p = mm.RunPlace()
	if p.Staging == staging || p.Reached != 0 || p.SetEnd || !fired(ch) {
		t.Fatalf("after the next staging: %+v (woke: %v)", p, fired(ch))
	}
}

// Under exact serving, a test's view opens its window, and the pool's view
// after it ends that test.
func TestRunPlace_FollowsTheExactViews(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SeedRecordedWindows(placeWindows())
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	// The set's first pool view comes with no view in force before it.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("the set's first pool view panicked: %v", r)
			}
		}()
		mm.ServeExactPool("k", nil)
	}()
	if got := mm.RunPlace().Reached; got != 0 {
		t.Fatalf("the set's start's pool moved the place to %d", got)
	}
	mm.ServeExactTest("test-2", nil)
	if got := mm.RunPlace().Reached; got != models.RunOpen(1) {
		t.Fatalf("at test-2's view: %d", got)
	}
	mm.ServeExactPool("k", nil)
	if got := mm.RunPlace().Reached; got != models.RunEnd(1) {
		t.Fatalf("at the pool after test-2: %d", got)
	}
	mm.OpenExactTest("test-3", nil, nil)
	if got := mm.RunPlace().Reached; got != models.RunOpen(2) {
		t.Fatalf("at test-3's view: %d", got)
	}
}

// A closed manager is at the set's end, and its close wakes every waiter.
func TestRunPlace_ACloseEndsTheSet(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SeedRecordedWindows(placeWindows())
	ch := mm.RunPlaceChanged()
	mm.Close()
	if !fired(ch) || !mm.RunPlace().SetEnd {
		t.Fatal("a close did not end the set")
	}
}

// A window several tests run at once were merged into (models.TestWindow
// Tests, keploy mock replay's recordedTestWindows) opens at the first of
// them to begin, and its end is reached only once each has ended: an
// answer recorded in the gap after it is not written while one of them
// still runs, and is written at the last one's end, not held to a later
// window.
func TestRunPlace_AMergedWindowEndsAtItsLastTestsEnd(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.SeedRecordedWindows([]models.TestWindow{
		{Start: placeAt(0), End: placeAt(12), Tests: []string{"test-a", "test-b"}},
		{TestCase: "test-c", Start: placeAt(20), End: placeAt(30)},
	})
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.ReachRunTest("test-b", true)
	if got := mm.RunPlace().Reached; got != models.RunOpen(0) {
		t.Fatalf("at test-b's begin: %d, want the merged window's open %d", got, models.RunOpen(0))
	}
	mm.ReachRunTest("test-a", true)
	mm.ReachRunTest("test-a", false)
	if got := mm.RunPlace().Reached; got != models.RunOpen(0) {
		t.Fatalf("test-a ended while test-b runs: %d, want %d", got, models.RunOpen(0))
	}
	mm.ReachRunTest("test-b", false)
	if got := mm.RunPlace().Reached; got != models.RunEnd(0) {
		t.Fatalf("at the last merged test's end: %d, want %d", got, models.RunEnd(0))
	}

	// The next set's staging forgets which merged tests ended.
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.ReachRunTest("test-b", false)
	if got := mm.RunPlace().Reached; got != models.RunOpen(0) {
		t.Fatalf("a staging kept the previous set's ends: %d", got)
	}
}

// keploy mock replay with the switch off seeds the windows for the run's
// place alone: they place the run, and nothing else reads them (the learn
// step's testWindows stay unset). Seeded at a set boundary they wait for
// the set's staging, and a set that seeds none has none, not the previous
// set's. A set's recorded windows (SeedRecordedWindows) come first.
func TestRunPlace_WindowsSeededForThePlaceAlone(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	mm.ResetForReplaySession()
	ch := mm.RunPlaceChanged()
	mm.SeedRunPlaceWindows(placeWindows())
	if !fired(ch) {
		t.Fatal("a seed woke no waiter")
	}
	if mm.RunPlace().Windows.Len() != 0 {
		t.Fatal("a seed at a set boundary was installed before the set's staging")
	}
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if p := mm.RunPlace(); p.Windows.Len() != 3 {
		t.Fatalf("the set's staging did not install the place's windows: %+v", p)
	}
	if _, known := mm.InRecordedTestWindow(placeAt(5)); known {
		t.Fatal("the place's windows reached the learn step's")
	}
	mm.ReachRunTest("test-2", true)
	if got := mm.RunPlace().Reached; got != models.RunOpen(1) {
		t.Fatalf("at test-2's begin: %d", got)
	}
	mm.ReachRunTest("test-2", false)
	if got := mm.RunPlace().Reached; got != models.RunEnd(1) {
		t.Fatalf("at test-2's end: %d", got)
	}

	// Mid-set, a seed is installed at once.
	mm.SeedRunPlaceWindows(placeWindows()[:1])
	if got := mm.RunPlace().Windows.Len(); got != 1 {
		t.Fatalf("a mid-set seed: %d windows, want 1", got)
	}

	// The next set seeds none: it has none.
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if p := mm.RunPlace(); p.Windows != nil {
		t.Fatalf("a set that seeded none kept the previous set's windows: %d", p.Windows.Len())
	}

	// A seed parked for a set whose staging never came is not the next
	// set's.
	mm.ResetForReplaySession()
	mm.SeedRunPlaceWindows(placeWindows())
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if p := mm.RunPlace(); p.Windows != nil {
		t.Fatalf("an aborted set's parked windows reached the next set: %d", p.Windows.Len())
	}

	// A set's recorded windows are the place's when it has both.
	mm.SeedRunPlaceWindows(placeWindows())
	mm.SeedRecordedWindows(placeWindows()[:2])
	if got := mm.RunPlace().Windows.Len(); got != 2 {
		t.Fatalf("the place reads %d windows, want the recorded 2", got)
	}
}

// A parser that found nothing due waits idle until the place's next change,
// and that change ends the wait's idle mark before the call that makes it
// returns: a read that settles the application's calls right after it (the
// set's end read) finds the connection busy until its parser has written
// the part it made due. Each move ends the marks of the waits on it: a
// test's window opening, a test's end, the set's end, a new staging, new
// windows and the close. A move to where the place is already ends none. A
// wait on a channel the place has since changed marks nothing: the parser
// reads the place again.
func TestRunPlace_AChangeEndsItsWaitsIdleMarksBeforeItReturns(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(mm *MockManager)
		ends bool
	}{
		{"a window opens", func(mm *MockManager) { mm.SetMocksWithWindow(nil, nil, placeAt(20), placeAt(30)) }, true},
		{"a test ends", func(mm *MockManager) { mm.EndRunTest("test-1") }, true},
		{"the set ends", func(mm *MockManager) { mm.EndRunSet() }, true},
		{"a new staging", func(mm *MockManager) { mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now()) }, true},
		{"new windows", func(mm *MockManager) { mm.SeedRunPlaceWindows(placeWindows()) }, true},
		{"the close", func(mm *MockManager) { mm.Close() }, true},
		{"the window that is open opens", func(mm *MockManager) { mm.SetMocksWithWindow(nil, nil, placeAt(0), placeAt(10)) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SeedRecordedWindows(placeWindows())
			mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
			mm.SetMocksWithWindow(nil, nil, placeAt(0), placeAt(10))
			marked, released := 0, 0
			mark := func() func() {
				marked++
				return func() { released++ }
			}
			ch := mm.RunPlaceChanged()
			release, ok := mm.IdleUntilRunPlaceChanges(ch, mark)
			if !ok || marked != 1 || released != 0 {
				t.Fatalf("a wait on the place now: ok %v, marked %d, released %d", ok, marked, released)
			}
			tc.move(mm)
			if want := map[bool]int{true: 1, false: 0}[tc.ends]; released != want {
				t.Fatalf("released %d times when the move returned, want %d", released, want)
			}
			release()
			release()
			if released != 1 {
				t.Fatalf("released %d times, want once", released)
			}
			if !tc.ends {
				return
			}
			if _, ok := mm.IdleUntilRunPlaceChanges(ch, mark); ok || marked != 1 {
				t.Fatalf("a wait on a channel the place has changed since: ok %v, marked %d", ok, marked)
			}
		})
	}
}

// The proxy ends the set on the run's manager (Proxy.EndRunSet).
func TestRunPlace_TheProxyEndsTheRunsSet(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	p := &Proxy{mockManager: mm}
	p.EndRunSet()
	if !mm.RunPlace().SetEnd {
		t.Fatal("the run's manager is not at the set's end")
	}
	(&Proxy{}).EndRunSet() // no manager: nothing to end
}

// The proxy hands the windows the replay sends to its manager's place.
func TestRunPlace_TheProxySeedsTheRunsWindows(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	p := &Proxy{mockManager: mm}
	p.SeedRunPlaceWindows(placeWindows())
	if got, want := mm.RunPlace().Windows.Len(), len(placeWindows()); got != want {
		t.Fatalf("the run's place has %d windows, want %d", got, want)
	}
}

// The proxy hands the run's manager a test's begin as its window's open and
// its end as its end.
func TestRunPlace_TheProxyReachesATestsOpenThenItsEnd(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	p := &Proxy{mockManager: mm}
	p.SeedRunPlaceWindows(placeWindows())
	p.ReachRunTest("test-2", true)
	if got := mm.RunPlace().Reached; got != models.RunOpen(1) {
		t.Fatalf("at test-2's begin: %d, want its open %d", got, models.RunOpen(1))
	}
	p.ReachRunTest("test-2", false)
	if got := mm.RunPlace().Reached; got != models.RunEnd(1) {
		t.Fatalf("at test-2's end: %d, want its end %d", got, models.RunEnd(1))
	}
}

// A change runs each wait's release and keeps none: the waits of a long set
// do not pile up on its place.
func TestRunPlace_AChangeKeepsNoReleasedWait(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows(placeWindows())
	released := 0
	if _, ok := mm.IdleUntilRunPlaceChanges(mm.RunPlaceChanged(), func() func() { return func() { released++ } }); !ok {
		t.Fatal("a wait on the place as it is was not marked")
	}
	mm.reachRunPlace(models.RunOpen(0), false)
	mm.place.mu.Lock()
	kept := len(mm.place.atChange)
	mm.place.mu.Unlock()
	if released != 1 || kept != 0 {
		t.Fatalf("after a change: %d releases run, %d waits kept; want 1 and none", released, kept)
	}
}

// The first window's open is reached as any other's.
func TestRunPlace_TheFirstWindowsOpenIsReached(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows(placeWindows())
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	ch := mm.RunPlaceChanged()
	mm.SetMocksWithWindow(nil, nil, placeAt(0), placeAt(10))
	if got := mm.RunPlace().Reached; got != models.RunOpen(0) || !fired(ch) {
		t.Fatalf("at test-1's open: %d (woke: %v), want %d", got, fired(ch), models.RunOpen(0))
	}
}

// A second set's end moves nothing, and wakes no waiter.
func TestRunPlace_ASecondSetEndChangesNothing(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows(placeWindows())
	mm.EndRunSet()
	ch := mm.RunPlaceChanged()
	mm.EndRunSet()
	if p := mm.RunPlace(); !p.SetEnd || fired(ch) {
		t.Fatalf("after a second set's end: %+v (woke: %v), want the set's end and no wake", p, fired(ch))
	}
}

// A wait on no channel is no wait, even when no channel is handed out now:
// nothing is marked idle.
func TestRunPlace_AWaitOnNoChannelMarksNothing(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows(placeWindows())
	mm.EndRunSet()
	marked := 0
	if _, waits := mm.IdleUntilRunPlaceChanges(nil, func() func() { marked++; return func() { marked-- } }); waits || marked != 0 {
		t.Fatalf("waits %v, marked %d; want no wait and nothing marked", waits, marked)
	}
}
