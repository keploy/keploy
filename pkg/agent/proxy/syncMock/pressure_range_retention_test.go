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
	mgr := withPressure(pressureRange{start: old.Add(-time.Second), end: old})

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

// TestPressureRangeCountCapNeverUncoversARange verifies the memory bound:
// past maxPressureRanges intervals the spans are capped, so retention by count
// cannot leak unbounded over a long recording, and the cap never uncovers an
// interval: past it the oldest are joined, not evicted. A test case over an
// evicted interval, checked by a record.go that lags the recorder, was saved
// without the mocks the pressure dropped.
func TestPressureRangeCountCapNeverUncoversARange(t *testing.T) {
	t.Parallel()

	mgr := &SyncMockManager{}
	before := time.Now()
	mgr.SetMemoryPressure(true)
	mgr.SetMemoryPressure(false)
	afterFirst := time.Now()
	// Each true→false pair records exactly one closed range.
	const total = maxPressureRanges + maxPressureRanges/4 + 64
	for i := 1; i < total; i++ {
		mgr.SetMemoryPressure(true)
		mgr.SetMemoryPressure(false)
	}

	recorded, spans := mgr.PressureRangeCount()
	if recorded != total {
		t.Fatalf("PressureRangeCount recorded %d ranges, want %d: every one is counted", recorded, total)
	}
	if spans == 0 || spans > maxPressureRanges {
		t.Fatalf("pressure spans exceeded the count cap: got %d, want 1..%d", spans, maxPressureRanges)
	}
	if ok, _ := mgr.WasPressureActiveInWindow(before, afterFirst); !ok {
		t.Fatal("the first pressure range is no longer covered: the cap uncovered it")
	}
}
