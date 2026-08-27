// Package proxy — mapping-authoritative window tests.
//
// A per-test pool selected via the recorded test→mock MAPPING must not be
// re-dropped by window containment: test req/res stamps come from the
// ingress path while mock stamps come from the egress proxy path (ordering
// is not guaranteed at millisecond scale), and post-replay write-backs can
// re-stamp test timestamps entirely. The mapping is the authoritative record
// of what THIS test consumed.
package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestMockManager_MappedWindow_KeepsOutOfWindowMocks: with
// SetMappedMocksWithWindow, mocks whose request timestamps fall entirely
// outside [start, end] (the re-stamped-test shape: window hours away from
// the mocks) stay servable at match time. The plain SetMocksWithWindow on
// the same input drops them — the red/green pair for the bug this guards.
func TestMockManager_MappedWindow_KeepsOutOfWindowMocks(t *testing.T) {
	recorded := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	windowStart := recorded.Add(2 * time.Hour)
	windowEnd := windowStart.Add(10 * time.Second)

	mocks := []*models.Mock{
		newMockForTest("mapped-1", recorded, models.LifetimePerTest),
		newMockForTest("mapped-2", recorded.Add(time.Millisecond), models.LifetimePerTest),
	}

	red := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer red.Close()
	red.SetMocksWithWindow(mocks, nil, windowStart, windowEnd)
	got, err := red.GetFilteredMocksInWindow()
	if err != nil {
		t.Fatalf("red GetFilteredMocksInWindow: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("plain window path kept %d out-of-window mocks; expected 0 (drop)", len(got))
	}

	green := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer green.Close()
	green.SetMappedMocksWithWindow(mocks, nil, windowStart, windowEnd)
	got, err = green.GetFilteredMocksInWindow()
	if err != nil {
		t.Fatalf("green GetFilteredMocksInWindow: %v", err)
	}
	if len(got) != 2 || !containsMockNamed(got, "mapped-1") || !containsMockNamed(got, "mapped-2") {
		t.Fatalf("mapped window path served %d mocks (%v); expected both mapped mocks", len(got), mockNamesOf(got))
	}
	if !green.IsTestWindowActive() {
		t.Fatal("mapped set must still publish the test window (tier routing depends on it)")
	}
}

// TestMockManager_MappedWindow_BoundarySkewKept: the ±1ms ingress/egress
// skew shape — a mapped mock 1ms before windowStart is kept.
func TestMockManager_MappedWindow_BoundarySkewKept(t *testing.T) {
	windowStart := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(10 * time.Second)

	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()
	mm.SetMappedMocksWithWindow([]*models.Mock{
		newMockForTest("skewed", windowStart.Add(-time.Millisecond), models.LifetimePerTest),
		newMockForTest("inside", windowStart.Add(time.Second), models.LifetimePerTest),
	}, nil, windowStart, windowEnd)

	got, err := mm.GetFilteredMocksInWindow()
	if err != nil {
		t.Fatalf("GetFilteredMocksInWindow: %v", err)
	}
	if len(got) != 2 || !containsMockNamed(got, "skewed") {
		t.Fatalf("boundary-skewed mapped mock dropped; served %v", mockNamesOf(got))
	}
}

// TestMockManager_MappedWindow_InvalidOrderStillDropped: the res<req sanity
// drop is about corrupt recordings, not window semantics — it stays.
func TestMockManager_MappedWindow_InvalidOrderStillDropped(t *testing.T) {
	windowStart := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(10 * time.Second)

	bad := newMockForTest("invalid-order", windowStart.Add(time.Second), models.LifetimePerTest)
	bad.Spec.ResTimestampMock = bad.Spec.ReqTimestampMock.Add(-time.Second)

	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()
	mm.SetMappedMocksWithWindow([]*models.Mock{bad}, nil, windowStart, windowEnd)

	got, err := mm.GetFilteredMocksInWindow()
	if err != nil {
		t.Fatalf("GetFilteredMocksInWindow: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("invalid-order mock must still be dropped on the mapped path; served %v", mockNamesOf(got))
	}
}

// TestMockManager_MappedWindow_ClearedByPlainSet: a later timestamp-selected
// test on the same manager gets strict containment back.
func TestMockManager_MappedWindow_ClearedByPlainSet(t *testing.T) {
	recorded := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	windowStart := recorded.Add(2 * time.Hour)
	windowEnd := windowStart.Add(10 * time.Second)

	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()

	mm.SetMappedMocksWithWindow([]*models.Mock{newMockForTest("mapped", recorded, models.LifetimePerTest)}, nil, windowStart, windowEnd)
	if got, _ := mm.GetFilteredMocksInWindow(); len(got) != 1 {
		t.Fatalf("mapped set: expected 1 servable mock, got %d", len(got))
	}

	// Next test: plain (timestamp-selected) set on the same manager. Note
	// the same window start feeds firstWindowStart, so use a start AFTER
	// the recorded stamp to make the out-of-window mock a genuine
	// stale-bleed candidate rather than startup-init.
	mm.SetMocksWithWindow([]*models.Mock{newMockForTest("stale", windowStart.Add(-time.Hour), models.LifetimePerTest)}, nil, windowStart, windowEnd)
	if got, _ := mm.GetFilteredMocksInWindow(); len(got) != 0 {
		t.Fatalf("plain set after mapped set must restore strict containment; served %d", len(got))
	}

	mm.SetCurrentTestWindow(windowStart, windowEnd)
	if got, _ := mm.GetFilteredMocksInWindow(); len(got) != 0 {
		t.Fatalf("SetCurrentTestWindow must clear mapping-authoritative mode; served %d", len(got))
	}
}

func mockNamesOf(list []*models.Mock) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		if m != nil {
			out = append(out, m.Name)
		}
	}
	return out
}
