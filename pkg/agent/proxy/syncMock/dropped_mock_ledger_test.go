package manager

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestTCHasDroppedMock_Ownership covers the window-ownership semantics of the
// dropped-mock ledger: a recorded drop is owned only by windows whose
// [start, end] contains its request timestamp (inclusive), matching
// ResolveRange's own attribution test.
func TestTCHasDroppedMock_Ownership(t *testing.T) {
	m := New(zap.NewNop())

	// Lock-free fast path: no drop recorded yet.
	if m.TCHasDroppedMock(time.Now().Add(-time.Hour), time.Now().Add(time.Hour)) {
		t.Fatalf("expected no ownership before any drop is recorded")
	}

	drop := time.Now()
	m.RecordDroppedMock(drop)

	cases := []struct {
		name       string
		start, end time.Time
		want       bool
	}{
		{"contains", drop.Add(-time.Second), drop.Add(time.Second), true},
		{"left-boundary", drop, drop.Add(time.Second), true},
		{"right-boundary", drop.Add(-time.Second), drop, true},
		{"before", drop.Add(-2 * time.Second), drop.Add(-time.Second), false},
		{"after", drop.Add(time.Second), drop.Add(2 * time.Second), false},
		{"zero-start", time.Time{}, drop.Add(time.Second), false},
		{"zero-end", drop.Add(-time.Second), time.Time{}, false},
	}
	for _, tc := range cases {
		if got := m.TCHasDroppedMock(tc.start, tc.end); got != tc.want {
			t.Errorf("%s: TCHasDroppedMock=%v want %v", tc.name, got, tc.want)
		}
	}
}

// TestRecordDroppedMock_ZeroIgnored proves a zero request timestamp (a mock that
// never got one) is not recorded — it cannot be attributed to any window and
// would otherwise be a spurious suppression source.
func TestRecordDroppedMock_ZeroIgnored(t *testing.T) {
	m := New(zap.NewNop())
	m.RecordDroppedMock(time.Time{})
	if c := m.DroppedMockCount(); c != 0 {
		t.Fatalf("expected zero-timestamp drop to be ignored, ledger has %d entries", c)
	}
	if m.droppedSeen.Load() {
		t.Fatalf("expected droppedSeen to stay false when only a zero timestamp was offered")
	}
}

// TestAddMock_PressureDropRecordsLedger proves the syncMock-internal pressure
// drop (a mock whose request fell inside an active pressure interval) is
// recorded in the ledger, so its owning TC is later suppressed.
func TestAddMock_PressureDropRecordsLedger(t *testing.T) {
	m := New(zap.NewNop())
	out := make(chan *models.Mock, 4)
	m.SetOutputChannel(out)

	m.SetMemoryPressure(true)
	reqTs := time.Now()
	m.AddMock(&models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{ReqTimestampMock: reqTs, ResTimestampMock: reqTs},
	})

	if _, dropped, _, _ := m.GetDropStats(); dropped != 1 {
		t.Fatalf("expected the mock to be pressure-dropped, pressureDropped=%d", dropped)
	}
	if c := m.DroppedMockCount(); c != 1 {
		t.Fatalf("expected the pressure drop to be recorded in the ledger, count=%d", c)
	}
	if !m.TCHasDroppedMock(reqTs.Add(-time.Second), reqTs.Add(time.Second)) {
		t.Fatalf("expected the owning window to see the dropped mock")
	}
	select {
	case <-out:
		t.Fatalf("dropped mock must not be forwarded downstream")
	default:
	}
}

// TestAddMock_ReusableLifetimeDropNotRecorded is the precision-refinement
// regression test (#4336): a dropped SESSION- or CONNECTION-lifetime mock (mongo
// handshake/heartbeat, SCRAM, prepared-statement setup) must NOT enter the
// dropped-mock ledger, even when its request timestamp falls inside a real test
// case's window. Those mocks are reusable across every test and are not owned by
// any single TC window, so recording a dropped one would spuriously suppress a
// test case that never depended on it. Only per-test drops belong in the ledger.
func TestAddMock_ReusableLifetimeDropNotRecorded(t *testing.T) {
	m := New(zap.NewNop())
	out := make(chan *models.Mock, 8)
	m.SetOutputChannel(out)

	m.SetMemoryPressure(true)
	reqTs := time.Now() // inside the open pressure interval

	// Session-lifetime (tag "config") dropped under pressure -> excluded.
	m.AddMock(&models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{
			ReqTimestampMock: reqTs, ResTimestampMock: reqTs,
			Metadata: map[string]string{"type": "config"},
		},
	})
	// Connection-lifetime (tag "connection" + connID) dropped under pressure -> excluded.
	m.AddMock(&models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{
			ReqTimestampMock: reqTs, ResTimestampMock: reqTs,
			Metadata: map[string]string{"type": "connection", "connID": "c1"},
		},
	})

	if _, dropped, _, _ := m.GetDropStats(); dropped != 2 {
		t.Fatalf("expected both reusable mocks to be pressure-dropped, got %d", dropped)
	}
	if c := m.DroppedMockCount(); c != 0 {
		t.Fatalf("reusable session/connection drops must NOT enter the ledger, count=%d", c)
	}
	// A TC whose window overlaps the dropped reusable mock's reqTs must NOT be
	// suppressed — it never depended on that mock.
	if m.TCHasDroppedMock(reqTs.Add(-time.Second), reqTs.Add(time.Second)) {
		t.Fatalf("a TC window overlapping a dropped reusable mock must NOT be suppressed")
	}

	// Contrast: a per-test (default/untagged) drop under pressure IS recorded.
	perTestTs := time.Now()
	m.AddMock(&models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{ReqTimestampMock: perTestTs, ResTimestampMock: perTestTs},
	})
	if c := m.DroppedMockCount(); c != 1 {
		t.Fatalf("a dropped per-test mock MUST enter the ledger, count=%d", c)
	}
	if !m.TCHasDroppedMock(perTestTs.Add(-time.Second), perTestTs.Add(time.Second)) {
		t.Fatalf("the per-test drop's owning window must be suppressible")
	}
}

// TestBufferWipe_ReusableLifetimeDropNotRecorded proves the same exclusion on
// the buffer-wipe path: a wiped SESSION-lifetime mock is not recorded.
func TestBufferWipe_ReusableLifetimeDropNotRecorded(t *testing.T) {
	m := New(zap.NewNop())
	out := make(chan *models.Mock, 8)
	m.SetOutputChannel(out)
	m.SetFirstRequestSignaled()
	for i := 0; i < models.StartupMockTestCaseWindow+1; i++ {
		m.ResolveRange(time.Now(), time.Now(), "warm", true, false)
	}

	// Closed pressure interval 1.
	m.SetMemoryPressure(true)
	inInterval1 := time.Now()
	time.Sleep(2 * time.Millisecond)
	m.SetMemoryPressure(false)

	// A late-decoded SESSION mock (tag "config") whose request was during
	// interval 1, added during calm -> buffered (not forwarded, since the
	// lifetime carve-out only forwards when !firstReqSeen).
	m.AddMock(&models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{
			ReqTimestampMock: inInterval1, ResTimestampMock: inInterval1,
			Metadata: map[string]string{"type": "config"},
		},
	})

	// Interval 2 opens -> buffer wipe drops mocks whose reqTs is in interval 1.
	m.SetMemoryPressure(true)

	if c := m.DroppedMockCount(); c != 0 {
		t.Fatalf("a wiped session-lifetime mock must NOT enter the ledger, count=%d", c)
	}
	if m.TCHasDroppedMock(inInterval1.Add(-time.Second), inInterval1.Add(time.Second)) {
		t.Fatalf("a TC window overlapping a wiped reusable mock must NOT be suppressed")
	}
}

// TestBufferWipeDropRecordsLedger proves the THIRD pressure-drop path — the
// SetMemoryPressure buffer wipe — is recorded in the ledger (#4336). A mock can
// sit in the buffer with a request timestamp inside a CLOSED pressure interval
// when it was decoded late (during calm) for a request that happened during an
// earlier pressure spike. When the next pressure interval opens, the wipe drops
// it; without recording that drop its owning test case would be orphaned and not
// suppressed.
func TestBufferWipeDropRecordsLedger(t *testing.T) {
	m := New(zap.NewNop())
	out := make(chan *models.Mock, 8)
	m.SetOutputChannel(out)
	// Force the buffered path: past firstReqSeen so AddMock buffers instead of
	// forwarding, and past the startup window so the mock isn't IsStartup-rescued
	// from the wipe.
	m.SetFirstRequestSignaled()
	for i := 0; i < models.StartupMockTestCaseWindow+1; i++ {
		m.ResolveRange(time.Now(), time.Now(), "warm", true, false)
	}

	// Pressure interval 1: open then close, leaving a CLOSED interval.
	m.SetMemoryPressure(true)
	inInterval1 := time.Now() // this instant is inside interval 1
	time.Sleep(2 * time.Millisecond)
	m.SetMemoryPressure(false)

	// During calm, a late-decoded mock arrives whose REQUEST happened during
	// interval 1 (reqTs = inInterval1). AddMock (calm) buffers it rather than
	// dropping it.
	m.AddMock(&models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{ReqTimestampMock: inInterval1, ResTimestampMock: inInterval1},
	})
	if got := m.DroppedMockCount(); got != 0 {
		t.Fatalf("precondition: no drop should be recorded yet, got %d", got)
	}

	// Pressure interval 2 opens -> the buffer wipe drops the buffered mock
	// (its reqTs is inside the still-retained closed interval 1) and must record
	// it in the ledger.
	m.SetMemoryPressure(true)

	if got := m.DroppedMockCount(); got != 1 {
		t.Fatalf("expected the buffer-wiped mock to be recorded in the ledger, got %d", got)
	}
	if !m.TCHasDroppedMock(inInterval1.Add(-time.Second), inInterval1.Add(time.Second)) {
		t.Fatalf("expected the owning window to see the buffer-wiped drop")
	}
}

// TestRecordDroppedMock_CapEviction proves the ledger stays bounded under a
// pathological volume of drops, evicting oldest-first and retaining the newest.
func TestRecordDroppedMock_CapEviction(t *testing.T) {
	m := New(zap.NewNop())
	base := time.Now()
	// Record cap+extra drops with strictly increasing timestamps.
	total := droppedMockReqCap + 100
	for i := 0; i < total; i++ {
		m.RecordDroppedMock(base.Add(time.Duration(i) * time.Millisecond))
	}
	if c := m.DroppedMockCount(); c != droppedMockReqCap {
		t.Fatalf("expected ledger bounded at %d, got %d", droppedMockReqCap, c)
	}
	// The newest timestamp must still be owned; the oldest (evicted) must not.
	newest := base.Add(time.Duration(total-1) * time.Millisecond)
	if !m.TCHasDroppedMock(newest.Add(-time.Millisecond), newest.Add(time.Millisecond)) {
		t.Fatalf("newest drop should survive eviction")
	}
	oldest := base
	if m.TCHasDroppedMock(oldest.Add(-time.Millisecond), oldest.Add(time.Millisecond)) {
		t.Fatalf("oldest drop should have been evicted")
	}
}

// TestDroppedMockLedger_SurvivesPruneWherePressureRangeDoesNot is the core
// differential proof that the atomic-drop ledger closes the exact race the
// pressure-range overlap heuristic (#4338) cannot.
//
// It reproduces the real go-memory-load-mongo prune race:
//
//  1. Memory pressure opens, a mock owned by an exchange window is dropped, and
//     pressure closes — leaving a CLOSED pressure range plus a ledger entry.
//  2. Time advances past pressureRangeStaleness (7s) and the real reaper (a
//     SetMemoryPressure call) prunes the closed range — exactly what happens
//     while the TC-persist path lags behind a 100-deep tcChan under load.
//
// After the prune:
//
//   - WasPressureActiveInWindow returns FALSE — the pressure-range signal is
//     gone, so a gate relying on it lets the orphaned TC through. THIS is the
//     residual flake.
//   - TCHasDroppedMock returns TRUE — the never-pruned ledger still records the
//     drop, so the atomic-drop gate still suppresses the TC.
//
// Skipped under -short because it must cross the real 7s staleness horizon.
func TestDroppedMockLedger_SurvivesPruneWherePressureRangeDoesNot(t *testing.T) {
	if testing.Short() {
		t.Skip("crosses the real 7s pressure-range staleness horizon; skipped in -short mode")
	}
	m := New(zap.NewNop())
	out := make(chan *models.Mock, 4)
	m.SetOutputChannel(out)

	m.SetMemoryPressure(true)
	reqTs := time.Now()
	resTs := reqTs
	m.AddMock(&models.Mock{
		Kind: models.Mongo,
		Spec: models.MockSpec{ReqTimestampMock: reqTs, ResTimestampMock: resTs},
	})
	m.SetMemoryPressure(false) // close the range; still fresh (not yet pruned)

	// While fresh, BOTH signals see the overlap.
	if overlap, _ := m.WasPressureActiveInWindow(reqTs.Add(-time.Second), resTs.Add(time.Second)); !overlap {
		t.Fatalf("precondition: expected pressure overlap visible before the prune")
	}
	if !m.TCHasDroppedMock(reqTs.Add(-time.Second), resTs.Add(time.Second)) {
		t.Fatalf("precondition: expected the drop recorded in the ledger")
	}

	// Cross the staleness horizon and fire the real reaper (a false->false call
	// is a pure prune trigger — no buffer wipe, which only runs on ->true).
	time.Sleep(pressureRangeStaleness + 500*time.Millisecond)
	m.SetMemoryPressure(false)

	// The pressure-range signal is now GONE — this is the #4338 false negative.
	if overlap, _ := m.WasPressureActiveInWindow(reqTs.Add(-time.Second), resTs.Add(time.Second)); overlap {
		t.Fatalf("expected the closed pressure range to be pruned after %s", pressureRangeStaleness)
	}
	// The ledger survives — the atomic-drop gate still catches the orphan.
	if !m.TCHasDroppedMock(reqTs.Add(-time.Second), resTs.Add(time.Second)) {
		t.Fatalf("REGRESSION: dropped-mock ledger was pruned; the orphan TC would slip through exactly as it did with the pressure-range-only check")
	}
}
