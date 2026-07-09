package manager

import (
	"testing"
	"time"
)

// TestPressureRangesRetainedBeyondFormerStaleness is the regression guard for
// the #4336 orphan-TC fix. The pressure-range history must be retained by
// COUNT, not by wall-clock age: routes/record.go's test-case stream can lag the
// recorder by far more than the former 7s staleness horizon, so a range that
// caused a mock drop must still be queryable when the lagging TC is finally
// checked. Under the old age-based prune the range was reaped and the orphan
// slipped through (replay: match_phase=no_mocks); under the count cap it
// survives. This test fails against the pre-fix time-prune and passes after.
func TestPressureRangesRetainedBeyondFormerStaleness(t *testing.T) {
	t.Parallel()

	// A pressure interval that opened and closed a full minute ago — well
	// beyond the former 7s staleness horizon that used to prune it.
	old := time.Now().Add(-time.Minute)
	mgr := &SyncMockManager{
		pressureRanges: []pressureRange{{start: old.Add(-time.Second), end: old}},
	}

	// memoryguard keeps ticking SetMemoryPressure long after that interval
	// closed. Every such call ran the old age-based prune, which would have
	// dropped the minute-old range. The count cap must not.
	mgr.SetMemoryPressure(true)
	mgr.SetMemoryPressure(false)
	mgr.SetMemoryPressure(true)
	mgr.SetMemoryPressure(false)

	// The exact query routes/record.go makes when it belatedly processes a TC
	// whose HTTP window overlaps that old pressure interval. It must still see
	// the pressure so it suppresses the orphan.
	has, count := mgr.WasPressureActiveInWindow(old.Add(-time.Second), old)
	if !has || count == 0 {
		t.Fatalf("pressure range older than the former 7s staleness was lost (has=%v count=%d); a lagging record.go would fail to suppress the orphan TC and replay would report match_phase=no_mocks", has, count)
	}
}

// TestPressureRangeCountCapEvictsOldest verifies the memory bound: past
// maxPressureRanges intervals the slice is capped (oldest evicted, newest kept),
// so retention-by-count cannot leak unbounded over a long recording.
func TestPressureRangeCountCapEvictsOldest(t *testing.T) {
	t.Parallel()

	mgr := &SyncMockManager{}
	// Each true→false pair appends exactly one closed range.
	for i := 0; i < maxPressureRanges+64; i++ {
		mgr.SetMemoryPressure(true)
		mgr.SetMemoryPressure(false)
	}

	mgr.mu.Lock()
	n := len(mgr.pressureRanges)
	mgr.mu.Unlock()
	if n > maxPressureRanges {
		t.Fatalf("pressureRanges exceeded the count cap: got %d, want <= %d", n, maxPressureRanges)
	}
}
