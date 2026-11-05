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
	mm.ServeExactPool("k", nil)
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
