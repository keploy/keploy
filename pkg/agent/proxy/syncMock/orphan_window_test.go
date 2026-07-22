package manager

import (
	"testing"
	"time"
)

// TestWasMockOrphanedInWindowSuppressesOverlappingTC verifies the keploy side of
// the enterprise mongo parser's resync-orphan suppression: a [start,end] hole
// recorded via RecordOrphanWindow is reported by WasMockOrphanedInWindow, which
// record.go queries alongside WasPressureActiveInWindow so a TC whose HTTP window
// overlaps the hole is suppressed (rather than shipped mock-less → replay
// match_phase=no_mocks). Kept on a separate orphanRanges slice so it can't
// corrupt the pressureRanges open/close state machine.
func TestWasMockOrphanedInWindowSuppressesOverlappingTC(t *testing.T) {
	t.Parallel()

	m := &SyncMockManager{}
	base := time.Now()
	holeStart := base.Add(1 * time.Second)
	holeEnd := base.Add(3 * time.Second)
	m.RecordOrphanWindow(holeStart, holeEnd)

	// A TC whose window overlaps the hole must be flagged (→ suppressed).
	if ok, n := m.WasMockOrphanedInWindow(base.Add(2*time.Second), base.Add(2500*time.Millisecond)); !ok || n < 1 {
		t.Fatalf("expected the resync hole to flag an overlapping TC window; got ok=%v n=%d", ok, n)
	}
	// The parser probes single instants (WasMockOrphanedInWindow(t, t)): a point
	// inside the hole overlaps.
	if ok, _ := m.WasMockOrphanedInWindow(base.Add(2*time.Second), base.Add(2*time.Second)); !ok {
		t.Fatalf("an instant inside the hole must overlap")
	}
	// A TC touching the hole's edge overlaps too (inclusive interval test).
	if ok, _ := m.WasMockOrphanedInWindow(base.Add(3*time.Second), base.Add(4*time.Second)); !ok {
		t.Fatalf("a TC window touching the hole's end must overlap")
	}
	// TCs entirely before/after the hole must NOT be suppressed.
	if ok, _ := m.WasMockOrphanedInWindow(base, base.Add(500*time.Millisecond)); ok {
		t.Fatalf("a TC window entirely before the hole must not be suppressed")
	}
	if ok, _ := m.WasMockOrphanedInWindow(base.Add(5*time.Second), base.Add(6*time.Second)); ok {
		t.Fatalf("a TC window entirely after the hole must not be suppressed")
	}
}

// TestWasMockOrphanedInWindowIndependentOfPressure verifies the two suppressor
// queries scan disjoint slices: a pressure range is invisible to
// WasMockOrphanedInWindow AND an orphan window is invisible to
// WasPressureActiveInWindow. Uses DISJOINT intervals so each direction is proven
// directly, so record.go attributes a suppression to its real cause instead of
// mislabeling an orphan as pressure.
func TestWasMockOrphanedInWindowIndependentOfPressure(t *testing.T) {
	t.Parallel()

	base := time.Now()
	// Pressure [0s,2s] and orphan [3s,4s] are DISJOINT.
	m := &SyncMockManager{
		pressureRanges: []pressureRange{{start: base, end: base.Add(2 * time.Second)}},
	}
	m.RecordOrphanWindow(base.Add(3*time.Second), base.Add(4*time.Second))

	win := func(off int) (time.Time, time.Time) {
		s := base.Add(time.Duration(off) * time.Millisecond)
		return s, s.Add(300 * time.Millisecond)
	}

	// A window over the orphan-only region [3s,4s]: orphaned yes, pressure NO.
	s, e := win(3500)
	if ok, n := m.WasMockOrphanedInWindow(s, e); !ok || n != 1 {
		t.Fatalf("orphan-only window: WasMockOrphanedInWindow got ok=%v n=%d, want true 1", ok, n)
	}
	if ok, _ := m.WasPressureActiveInWindow(s, e); ok {
		t.Fatalf("orphan-only window must NOT be reported by WasPressureActiveInWindow (it must not see orphanRanges)")
	}

	// A window over the pressure-only region [0s,2s]: pressure yes, orphaned NO.
	s, e = win(500)
	if ok, n := m.WasMockOrphanedInWindow(s, e); ok || n != 0 {
		t.Fatalf("pressure-only window must NOT be reported orphaned (WasMockOrphanedInWindow must not see pressureRanges); got ok=%v n=%d", ok, n)
	}
	if ok, _ := m.WasPressureActiveInWindow(s, e); !ok {
		t.Fatalf("sanity: pressure-only window must still be reported by WasPressureActiveInWindow")
	}
}

// TestRecordOrphanWindowDegenerateInputs pins the no-op / safety contracts.
func TestRecordOrphanWindowDegenerateInputs(t *testing.T) {
	t.Parallel()

	m := &SyncMockManager{}
	base := time.Now()

	// Zero start is dropped (no wire ts to attribute).
	m.RecordOrphanWindow(time.Time{}, base.Add(time.Second))
	if ok, _ := m.WasMockOrphanedInWindow(base, base.Add(time.Second)); ok {
		t.Fatalf("a zero-start orphan window must be dropped, not recorded")
	}

	// end < start is clamped to a point interval (start,start); it overlaps only
	// a TC window that contains that instant, never blankets everything.
	m.RecordOrphanWindow(base.Add(2*time.Second), base.Add(1*time.Second))
	if ok, _ := m.WasMockOrphanedInWindow(base.Add(2*time.Second), base.Add(2*time.Second)); !ok {
		t.Fatalf("clamped point interval must overlap a TC window at that instant")
	}
	if ok, _ := m.WasMockOrphanedInWindow(base.Add(1500*time.Millisecond), base.Add(1800*time.Millisecond)); ok {
		t.Fatalf("clamped point interval must NOT overlap a window before it (no inverted range)")
	}

	// A zero END (with non-zero start) is < start, so it clamps to a point too —
	// never a zero-end interval that the (no open-interval handling) overlap scan
	// would mistreat. Point at base+5s overlaps only that instant.
	m.RecordOrphanWindow(base.Add(5*time.Second), time.Time{})
	if ok, _ := m.WasMockOrphanedInWindow(base.Add(5*time.Second), base.Add(5*time.Second)); !ok {
		t.Fatalf("zero-end orphan window must clamp to a point at start, overlapping that instant")
	}
	if ok, _ := m.WasMockOrphanedInWindow(base.Add(6*time.Second), base.Add(10*time.Second)); ok {
		t.Fatalf("zero-end clamp must NOT create an open interval matching everything after start")
	}

	// Degenerate query inputs (zero start/end) and nil receiver must be safe.
	if ok, n := m.WasMockOrphanedInWindow(time.Time{}, base); ok || n != 0 {
		t.Fatalf("zero-start query must return (false,0); got (%v,%d)", ok, n)
	}
	var nilM *SyncMockManager
	nilM.RecordOrphanWindow(base, base.Add(time.Second)) // must not panic
	if ok, n := nilM.WasMockOrphanedInWindow(base, base.Add(time.Second)); ok || n != 0 {
		t.Fatalf("nil receiver query must return (false,0); got (%v,%d)", ok, n)
	}
}

// TestRecordOrphanWindowCountCap verifies the ring caps at maxPressureRanges,
// evicting the OLDEST intervals and retaining the newest — so continuous
// recording can't grow orphanRanges unbounded and slow every overlap scan.
func TestRecordOrphanWindowCountCap(t *testing.T) {
	t.Parallel()

	m := &SyncMockManager{}
	base := time.Now()
	// Record one more than the cap; interval i lives at [base+i, base+i+1] ms.
	total := maxPressureRanges + 100
	for i := 0; i < total; i++ {
		s := base.Add(time.Duration(i) * time.Millisecond)
		m.RecordOrphanWindow(s, s.Add(time.Millisecond))
	}

	m.mu.Lock()
	got := len(m.orphanRanges)
	oldest := m.orphanRanges[0].start
	newest := m.orphanRanges[len(m.orphanRanges)-1].start
	m.mu.Unlock()

	if got != maxPressureRanges {
		t.Fatalf("orphanRanges not capped: got %d, want %d", got, maxPressureRanges)
	}
	// The first (total-maxPressureRanges) intervals must have been evicted, so
	// the oldest surviving start is interval index (total-maxPressureRanges).
	wantOldest := base.Add(time.Duration(total-maxPressureRanges) * time.Millisecond)
	if !oldest.Equal(wantOldest) {
		t.Fatalf("cap evicted the wrong end: oldest survivor=%v, want %v (newest should be kept)", oldest, wantOldest)
	}
	wantNewest := base.Add(time.Duration(total-1) * time.Millisecond)
	if !newest.Equal(wantNewest) {
		t.Fatalf("cap dropped the newest interval: newest survivor=%v, want %v", newest, wantNewest)
	}
}

// TestRecordOrphanWindowDoesNotTouchPressureRanges guards the isolation
// invariant: orphan windows must NOT be appended to pressureRanges, whose
// open/close state machine assumes the last element is the still-open interval.
func TestRecordOrphanWindowDoesNotTouchPressureRanges(t *testing.T) {
	t.Parallel()

	m := &SyncMockManager{}
	// Open a real pressure interval (last element, end==zero).
	m.SetMemoryPressure(true)
	base := time.Now()
	m.RecordOrphanWindow(base, base.Add(time.Second))
	// Closing pressure must still find and close the open interval — i.e. the
	// orphan window did not become the last pressureRanges element.
	m.SetMemoryPressure(false)

	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.pressureRanges {
		if r.end.IsZero() {
			t.Fatalf("pressureRanges[%d] left open after close — RecordOrphanWindow corrupted the open/close state machine", i)
		}
	}
	if len(m.orphanRanges) != 1 {
		t.Fatalf("expected the orphan window in orphanRanges (got %d), not pressureRanges", len(m.orphanRanges))
	}
}
