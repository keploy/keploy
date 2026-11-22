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
// match_phase=no_mocks). Kept in spans of their own, apart from the pressure
// spans.
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
	m := withPressure(pressureRange{start: base, end: base.Add(2 * time.Second)})
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
		t.Fatalf("orphan-only window must NOT be reported by WasPressureActiveInWindow (it must not see the orphan spans)")
	}

	// A window over the pressure-only region [0s,2s]: pressure yes, orphaned NO.
	s, e = win(500)
	if ok, n := m.WasMockOrphanedInWindow(s, e); ok || n != 0 {
		t.Fatalf("pressure-only window must NOT be reported orphaned (WasMockOrphanedInWindow must not see the pressure spans); got ok=%v n=%d", ok, n)
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

// TestRecordOrphanWindowCountCap verifies the orphan spans stay bounded
// (maxPressureRanges), so continuous recording can't grow them without limit,
// and that the bound never uncovers a span: past it the oldest spans are
// joined, never evicted. A test case over an evicted span, checked
// by a record.go that lags the recorder, was saved without its mocks.
func TestRecordOrphanWindowCountCap(t *testing.T) {
	t.Parallel()

	m := &SyncMockManager{}
	base := time.Now()
	// Record more than the cap of disjoint intervals; interval i lives at
	// [base+2i, base+2i+1] ms.
	total := maxPressureRanges + 100
	at := func(i int) time.Time { return base.Add(time.Duration(2*i) * time.Millisecond) }
	for i := 0; i < total; i++ {
		m.RecordOrphanWindow(at(i), at(i).Add(time.Millisecond))
	}

	if recorded, closed, _ := m.OrphanRangeCount(); recorded != total || closed == 0 || closed > maxPressureRanges {
		t.Fatalf("OrphanRangeCount = (%d recorded, %d closed), want (%d, 1..%d): the spans are capped, and every interval is counted", recorded, closed, total, maxPressureRanges)
	}
	for _, i := range []int{0, 1, total / 2, total - 1} {
		mid := at(i).Add(500 * time.Microsecond)
		if ok, _ := m.WasMockOrphanedInWindow(mid, mid); !ok {
			t.Fatalf("interval %d of %d is no longer covered: the cap uncovered a span", i, total)
		}
	}
	after := at(total).Add(time.Second)
	if ok, _ := m.WasMockOrphanedInWindow(after, after.Add(time.Millisecond)); ok {
		t.Fatal("a window after the last interval is covered")
	}
	// The newest intervals are as they were: a window between two is clear.
	gap := at(total - 2).Add(1500 * time.Microsecond)
	if ok, _ := m.WasMockOrphanedInWindow(gap, gap); ok {
		t.Fatal("a window between the two newest intervals is covered: the cap joined recent history")
	}
}

// TestRecordOrphanWindowDoesNotTouchPressureRanges guards the isolation
// invariant: orphan windows must NOT be added to the pressure spans, and an
// open pressure span still closes.
func TestRecordOrphanWindowDoesNotTouchPressureRanges(t *testing.T) {
	t.Parallel()

	m := &SyncMockManager{}
	// Open a real pressure interval (last element, end==zero).
	m.SetMemoryPressure(true)
	base := time.Now()
	m.RecordOrphanWindow(base, base.Add(time.Second))
	// Closing pressure must still close the open pressure span.
	m.SetMemoryPressure(false)

	if closed, open := m.pressure.Counts(); closed != 1 || open != 0 {
		t.Fatalf("pressure spans = (%d closed, %d open), want (1, 0): the orphan window touched them", closed, open)
	}
	if closed, _ := m.orphans.Counts(); closed != 1 {
		t.Fatalf("expected the orphan window in the orphan spans (got %d), not the pressure spans", closed)
	}
}

// TestOpenOrphanWindowSuppressesWhileStillOpen covers the hole whose end is not
// yet known: a connection whose parser was retired, leaving the relay to raw-
// forward the rest of its life.
//
// The open case is the load-bearing one. record.go queries these ranges as each
// test case is STREAMED, so a hole that is only recorded once its width is known
// — i.e. when the connection finally closes, which for a pooled connection means
// at shutdown — arrives long after the mock-less test cases it should have
// suppressed were already written to disk. This is the exact shape that made a
// real recording ship 18 unreplayable tests.
func TestOpenOrphanWindowSuppressesWhileStillOpen(t *testing.T) {
	t.Parallel()

	m := &SyncMockManager{}
	// The hole opened in the past: an OPEN interval extends to now, so the
	// span it covers is [base, now]. Timestamps must sit inside that, the way
	// a real TC's recorded window does — a future timestamp is outside an
	// open hole by definition.
	base := time.Now().Add(-2 * time.Second)

	// A test case recorded BEFORE the hole opens must not be suppressed.
	before := base.Add(-1 * time.Second)
	if orphaned, _ := m.WasMockOrphanedInWindow(before, before.Add(time.Millisecond)); orphaned {
		t.Fatal("a TC recorded before any hole was opened must not be suppressed")
	}

	closeHole := m.OpenOrphanWindow(base)

	// Still open: a TC recorded since then must be suppressed even though
	// nothing has told the manager when the hole ends.
	during := time.Now().Add(-1 * time.Second)
	if orphaned, n := m.WasMockOrphanedInWindow(during, during.Add(time.Millisecond)); !orphaned || n != 1 {
		t.Fatalf("orphaned=%v count=%d during an OPEN hole, want true/1: the connection is "+
			"un-capturable right now, so this TC would be shipped mock-less and fail replay "+
			"with match_phase=no_mocks", orphaned, n)
	}

	closeHole()

	// The hole is bounded now: traffic after it is capturable again.
	//
	// The sleep is load-bearing, not padding. An OPEN hole's end is "now at
	// query time", so a timestamp taken only microseconds after the close is
	// still inside an open interval and this assertion would pass whether or
	// not the closer did anything — a closer that silently did nothing was a
	// surviving mutant until this sleep put real distance between the close
	// instant and the timestamp under test.
	time.Sleep(20 * time.Millisecond)
	after := time.Now()
	if orphaned, _ := m.WasMockOrphanedInWindow(after, after.Add(time.Millisecond)); orphaned {
		t.Fatal("a TC recorded after the hole closed must not be suppressed — closing must " +
			"bound the window, or one retired parser silently suppresses the rest of the session")
	}
	// And what happened inside it stays suppressed.
	if orphaned, _ := m.WasMockOrphanedInWindow(during, during.Add(time.Millisecond)); !orphaned {
		t.Fatal("a TC inside the closed hole must still be suppressed")
	}

	// Idempotent: a double close must not move the end.
	closeHole()
	if orphaned, _ := m.WasMockOrphanedInWindow(during, during.Add(time.Millisecond)); !orphaned {
		t.Fatal("closing twice changed the recorded window")
	}
}

// TestOrphanRangeCountSplitsClosedFromOpen pins the operator-facing counter.
//
// The closer stamps r.end in place instead of removing the entry, so counting
// the slice lengths reports every cleanly-closed window as still open. That
// number is the ONLY signal that this suppressor fired at all, and a
// still-open window is the one that keeps suppressing to the end of the
// session — so getting the split backwards actively misleads.
func TestOrphanRangeCountSplitsClosedFromOpen(t *testing.T) {
	t.Parallel()

	m := &SyncMockManager{}
	base := time.Now().Add(-time.Minute)

	closeA := m.OpenOrphanWindow(base)
	m.OpenOrphanWindow(base.Add(time.Second)) // deliberately left open
	closeA()

	// A conventional bounded hole (the mongo resync path) lands elsewhere and
	// must still be counted as closed.
	// (Before A: a span that overlapped A would join it.)
	m.RecordOrphanWindow(base.Add(-3*time.Second), base.Add(-2*time.Second))

	if recorded, closed, open := m.OrphanRangeCount(); recorded != 3 || closed != 2 || open != 1 {
		t.Fatalf("OrphanRangeCount() = (recorded=%d, closed=%d, open=%d), want (3, 2, 1)", recorded, closed, open)
	}
}

// TestOpenOrphanWindowIgnoresZeroStart pins the degenerate input, matching
// RecordOrphanWindow's refusal to make a claim on one. Suppressing from a zero
// start would match every TC ever recorded.
func TestOpenOrphanWindowIgnoresZeroStart(t *testing.T) {
	t.Parallel()

	m := &SyncMockManager{}
	closeHole := m.OpenOrphanWindow(time.Time{})

	// Queried while still OPEN and against a LONG-PAST window, deliberately.
	// A zero start means "the beginning of time", so the range it would
	// create covers every test case ever recorded — and closing first, or
	// probing only the present, hides that: the closed end is later than a
	// present-day timestamp, so the overlap test says no. Probing the past
	// with the hole still open is what actually catches an accepted zero.
	past := time.Now().Add(-365 * 24 * time.Hour)
	if orphaned, _ := m.WasMockOrphanedInWindow(past, past.Add(time.Millisecond)); orphaned {
		t.Fatal("a zero-start hole must suppress nothing; it would otherwise match every TC ever recorded")
	}

	closeHole() // must not panic
}
