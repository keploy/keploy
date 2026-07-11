package manager

import (
	"testing"
	"time"
)

// TestOrphanWindowOverlapSemantics pins the join contract record.go relies on:
// a recorded non-pressure mock-void window suppresses exactly the TCs whose
// HTTP [req, resp] window overlaps it, and no others.
func TestOrphanWindowOverlapSemantics(t *testing.T) {
	t.Parallel()
	mgr := &SyncMockManager{}
	base := time.Now()
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }

	// A mock voided over the wire window [10ms, 20ms].
	mgr.RecordOrphanWindow(ms(10), ms(20))

	// A TC whose HTTP window [0, 15ms] straddles the void → suppressed.
	if ok, cnt := mgr.WasMockOrphanedInWindow(ms(0), ms(15)); !ok || cnt != 1 {
		t.Fatalf("straddling TC window must be suppressed; got ok=%v cnt=%d", ok, cnt)
	}
	// A TC fully inside the void → suppressed.
	if ok, _ := mgr.WasMockOrphanedInWindow(ms(12), ms(18)); !ok {
		t.Fatalf("TC window inside the void must be suppressed")
	}
	// A TC that touches the boundary (end == void.start) → overlaps (closed
	// intervals), suppressed.
	if ok, _ := mgr.WasMockOrphanedInWindow(ms(5), ms(10)); !ok {
		t.Fatalf("boundary-touching TC window must be suppressed")
	}
	// A TC entirely after the void → NOT suppressed (no over-suppression of a
	// concurrent TC that kept all its mocks).
	if ok, _ := mgr.WasMockOrphanedInWindow(ms(30), ms(40)); ok {
		t.Fatalf("non-overlapping TC window must NOT be suppressed")
	}
	// A TC entirely before the void ([0, 5ms] ends before void start 10ms) →
	// NOT suppressed.
	if ok, _ := mgr.WasMockOrphanedInWindow(ms(0), ms(5)); ok {
		t.Fatalf("TC window ending before the void start must NOT be suppressed")
	}
}

// TestOrphanWindowDegenerateInputs pins the guards: a zero start is ignored
// (never poison the suppressor into over-claiming), a zero/inverted end is
// clamped to a valid point interval, and a degenerate TC query is refused
// rather than matching everything.
func TestOrphanWindowDegenerateInputs(t *testing.T) {
	t.Parallel()
	mgr := &SyncMockManager{}
	base := time.Now()

	// Zero start → ignored, records nothing.
	mgr.RecordOrphanWindow(time.Time{}, base)
	if got := mgr.OrphanRangeCount(); got != 0 {
		t.Fatalf("zero-start orphan window must be ignored; count=%d", got)
	}

	// Zero end → clamped to start (a point interval at `base`).
	mgr.RecordOrphanWindow(base, time.Time{})
	if got := mgr.OrphanRangeCount(); got != 1 {
		t.Fatalf("zero-end window must still record as a point; count=%d", got)
	}
	if ok, _ := mgr.WasMockOrphanedInWindow(base.Add(-time.Millisecond), base.Add(time.Millisecond)); !ok {
		t.Fatalf("a TC window spanning the point void must be suppressed")
	}

	// Inverted end (before start) → clamped to start (point interval).
	mgr.RecordOrphanWindow(base.Add(100*time.Millisecond), base.Add(50*time.Millisecond))
	if ok, _ := mgr.WasMockOrphanedInWindow(base.Add(99*time.Millisecond), base.Add(101*time.Millisecond)); !ok {
		t.Fatalf("inverted-end window must clamp to a point at start and still suppress")
	}

	// Degenerate TC query (zero start or end) → refused, never over-suppresses.
	if ok, _ := mgr.WasMockOrphanedInWindow(time.Time{}, base); ok {
		t.Fatalf("zero-start TC query must be refused")
	}
	if ok, _ := mgr.WasMockOrphanedInWindow(base, time.Time{}); ok {
		t.Fatalf("zero-end TC query must be refused")
	}
}

// TestOrphanRangeCountBoundedAndEvictsOldest pins that orphanRanges stays
// count-bounded under a continuous stream of voids: it grows to at most 2×cap
// then compacts IN PLACE back toward cap (the amortized-O(1) scheme that avoids
// a per-call realloc on the hot per-void path), and compaction evicts the
// OLDEST windows first — so a lagging record.go still sees the most recent
// voids it might need to suppress.
func TestOrphanRangeCountBoundedAndEvictsOldest(t *testing.T) {
	t.Parallel()
	mgr := &SyncMockManager{}
	base := time.Now()
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }

	// Insert past the 2×cap compaction trigger so eviction fires at least once.
	total := 2*maxPressureRanges + 100
	for i := 0; i < total; i++ {
		mgr.RecordOrphanWindow(ms(i), ms(i))
	}
	// Count must stay within [cap, 2×cap] — never unbounded.
	got := mgr.OrphanRangeCount()
	if got < maxPressureRanges || got > 2*maxPressureRanges {
		t.Fatalf("orphanRanges count must stay in [%d, %d]; got %d", maxPressureRanges, 2*maxPressureRanges, got)
	}
	// The oldest window was evicted by compaction → no longer suppressible.
	if ok, _ := mgr.WasMockOrphanedInWindow(ms(0), ms(0)); ok {
		t.Fatalf("oldest orphan window must be evicted after compaction")
	}
	// The newest window is retained.
	last := ms(total - 1)
	if ok, _ := mgr.WasMockOrphanedInWindow(last, last); !ok {
		t.Fatalf("newest orphan window must be retained")
	}
}

// TestOrphanWindowIndependentOfPressureRanges pins that the two suppressors are
// separate: recording an orphan window does not create a pressure range and
// vice-versa, so the session-summary telemetry (pressure_ranges_total vs
// orphan_ranges_total) stays accurate.
func TestOrphanWindowIndependentOfPressureRanges(t *testing.T) {
	t.Parallel()
	mgr := &SyncMockManager{}
	base := time.Now()

	mgr.RecordOrphanWindow(base, base.Add(time.Millisecond))
	if mgr.PressureRangeCount() != 0 {
		t.Fatalf("orphan window must not create a pressure range")
	}
	if got := mgr.OrphanRangeCount(); got != 1 {
		t.Fatalf("expected 1 orphan range, got %d", got)
	}
	// A pressure-only window is not seen by the orphan query.
	if ok, _ := mgr.WasPressureActiveInWindow(base, base.Add(time.Millisecond)); ok {
		t.Fatalf("no pressure range should exist")
	}
}
