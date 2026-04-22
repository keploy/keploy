// Package proxy — Wave 2 MockManager tier-partition tests.
//
// Verifies the strict three-way split introduced by Wave 2:
//
//   - GetStartupMocks()        → mocks with req < firstWindowStart
//   - GetSessionScopedMocks()  → session + connection-tagged mocks
//   - GetPerTestMocksInWindow()→ per-test mocks inside [start, end]
//
// Plus the legacy GetSessionMocks() union shim (startup + session) and
// the HasFirstTestFired() sticky signal.
package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// newMockForTest assembles a minimal *models.Mock with a distinct Name
// and the given ReqTimestampMock + Lifetime. ResTimestampMock is set to
// req+1ms so the manager's invalid-order sanity check passes.
func newMockForTest(name string, req time.Time, lifetime models.Lifetime) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			ReqTimestampMock: req,
			ResTimestampMock: req.Add(time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime: lifetime,
		},
	}
}

// containsMockNamed reports whether any mock in list has Name == name.
func containsMockNamed(list []*models.Mock, name string) bool {
	for _, m := range list {
		if m != nil && m.Name == name {
			return true
		}
	}
	return false
}

// TestMockManager_Wave2_StrictTierPartition verifies the startup /
// session / per-test three-way split is strictly disjoint, that the
// legacy GetSessionMocks union returns startup+session (no per-test),
// HasFirstTestFired flips true after the first non-BaseTime call, and
// the recorder-emitted Lifetime tag is NOT mutated by the manager.
func TestMockManager_Wave2_StrictTierPartition(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	firstStart := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	firstEnd := firstStart.Add(10 * time.Second)

	// Build three mocks spanning the three tiers:
	//   m_startup: req = firstStart - 1s, LifetimeSession (recorder's
	//              tag preserved; request-timestamp is what routes it
	//              to the startup tier, not the Lifetime field).
	//   m_session: req = firstStart + 1s, LifetimeSession.
	//   m_perTest: req = firstStart + 5s, LifetimePerTest.
	startupReq := firstStart.Add(-1 * time.Second)
	sessionReq := firstStart.Add(1 * time.Second)
	perTestReq := firstStart.Add(5 * time.Second)

	mStartup := newMockForTest("startup", startupReq, models.LifetimeSession)
	mSession := newMockForTest("session", sessionReq, models.LifetimeSession)
	mPerTest := newMockForTest("perTest", perTestReq, models.LifetimePerTest)

	// Sanity: before any SetMocksWithWindow call, HasFirstTestFired is
	// false — startup tier is conceptually valid but empty.
	if mm.HasFirstTestFired() {
		t.Fatalf("HasFirstTestFired: want false before first SetMocksWithWindow, got true")
	}

	// Fire the first real test window. `filtered` carries m_startup +
	// m_perTest (the runner emits every request-time-indexed mock there
	// during pre-filter; SetMocksWithWindow re-partitions below).
	// `unfiltered` carries the session pool (m_session).
	mm.SetMocksWithWindow(
		[]*models.Mock{mStartup, mPerTest},
		[]*models.Mock{mSession},
		firstStart, firstEnd,
	)

	// HasFirstTestFired must now be true, sticky for the rest of the
	// manager's lifetime.
	if !mm.HasFirstTestFired() {
		t.Fatalf("HasFirstTestFired: want true after first non-BaseTime SetMocksWithWindow, got false")
	}

	// Startup tier: exactly m_startup.
	startupList, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: unexpected err: %v", err)
	}
	if len(startupList) != 1 || !containsMockNamed(startupList, "startup") {
		t.Fatalf("GetStartupMocks: want [startup], got %v",
			mockNames(startupList))
	}

	// Session tier: exactly m_session (strict — no startup entries).
	sessionList, err := mm.GetSessionScopedMocks()
	if err != nil {
		t.Fatalf("GetSessionScopedMocks: unexpected err: %v", err)
	}
	if len(sessionList) != 1 || !containsMockNamed(sessionList, "session") {
		t.Fatalf("GetSessionScopedMocks: want [session], got %v",
			mockNames(sessionList))
	}
	if containsMockNamed(sessionList, "startup") {
		t.Fatalf("GetSessionScopedMocks: startup entry leaked into strict session tier: %v",
			mockNames(sessionList))
	}

	// Per-test tier inside the current window: exactly m_perTest.
	perTestList, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow: unexpected err: %v", err)
	}
	if len(perTestList) != 1 || !containsMockNamed(perTestList, "perTest") {
		t.Fatalf("GetPerTestMocksInWindow: want [perTest], got %v",
			mockNames(perTestList))
	}

	// Legacy union shim: startup + session, NO per-test.
	union, err := mm.GetSessionMocks()
	if err != nil {
		t.Fatalf("GetSessionMocks (legacy): unexpected err: %v", err)
	}
	if len(union) != 2 {
		t.Fatalf("GetSessionMocks (legacy): want 2 (startup+session), got %d: %v",
			len(union), mockNames(union))
	}
	if !containsMockNamed(union, "startup") || !containsMockNamed(union, "session") {
		t.Fatalf("GetSessionMocks (legacy): missing expected entries, got %v",
			mockNames(union))
	}
	if containsMockNamed(union, "perTest") {
		t.Fatalf("GetSessionMocks (legacy): per-test mock leaked into union, got %v",
			mockNames(union))
	}

	// Recorder-emitted Lifetime must NOT be mutated by the manager.
	// m_startup went in tagged LifetimeSession — it should come out the
	// same way. Any silent mutation here re-introduces exactly the
	// behaviour Wave 2 was built to eliminate.
	if mStartup.TestModeInfo.Lifetime != models.LifetimeSession {
		t.Fatalf("startup mock Lifetime mutated by manager: got %v want %v",
			mStartup.TestModeInfo.Lifetime, models.LifetimeSession)
	}
	if mPerTest.TestModeInfo.Lifetime != models.LifetimePerTest {
		t.Fatalf("per-test mock Lifetime mutated by manager: got %v want %v",
			mPerTest.TestModeInfo.Lifetime, models.LifetimePerTest)
	}
}

// TestMockManager_Wave2_BaseTimeStagingDoesNotFireFirstTest ensures the
// Runner/Replayer initial staging call (SetMocksWithWindow with
// start=models.BaseTime) does NOT flip HasFirstTestFired to true.
// Parsers rely on this to distinguish bootstrap (before any test) from
// between-tests gaps.
func TestMockManager_Wave2_BaseTimeStagingDoesNotFireFirstTest(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	// Initial staging fire from the Runner/Replayer: start == BaseTime.
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

	if mm.HasFirstTestFired() {
		t.Fatalf("HasFirstTestFired: BaseTime staging must not register as first test, got true")
	}
}

// TestSetMocksWithWindow_InitialStaging_SeedsStartupTree covers the
// Wave-3 C1/C3 fix: during Runner/Replayer's initial BaseTime staging
// call, every per-test input mock is bootstrap traffic (no real test
// has fired yet) and must be routed into the startup tree — not into
// the session / unfiltered pool. The strict session tier must remain
// unpolluted; the legacy GetSessionMocks union shim keeps returning
// startup+session so pre-wave-2 parsers observe bootstrap mocks via
// that path.
func TestSetMocksWithWindow_InitialStaging_SeedsStartupTree(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	// Two per-test mocks seen during the BaseTime staging sweep, plus
	// one session mock in the unfiltered slice.
	ptReq := time.Date(2024, 1, 1, 11, 59, 0, 0, time.UTC)
	pt1 := newMockForTest("pt1", ptReq, models.LifetimePerTest)
	pt2 := newMockForTest("pt2", ptReq.Add(time.Second), models.LifetimePerTest)
	sess := newMockForTest("sess", ptReq.Add(2*time.Second), models.LifetimeSession)

	mm.SetMocksWithWindow(
		[]*models.Mock{pt1, pt2},
		[]*models.Mock{sess},
		models.BaseTime, time.Now(),
	)

	// GetStartupMocks returns all per-test mocks (pt1, pt2).
	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if len(startup) != 2 || !containsMockNamed(startup, "pt1") || !containsMockNamed(startup, "pt2") {
		t.Fatalf("GetStartupMocks: want [pt1 pt2], got %v", mockNames(startup))
	}

	// GetPerTestMocksInWindow returns nothing — no real window active.
	pt, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow: %v", err)
	}
	if len(pt) != 0 {
		t.Fatalf("GetPerTestMocksInWindow: want empty, got %v", mockNames(pt))
	}

	// GetSessionScopedMocks returns only sess — no pollution from pt1/pt2.
	session, err := mm.GetSessionScopedMocks()
	if err != nil {
		t.Fatalf("GetSessionScopedMocks: %v", err)
	}
	if len(session) != 1 || !containsMockNamed(session, "sess") {
		t.Fatalf("GetSessionScopedMocks: want [sess] only, got %v", mockNames(session))
	}
	if containsMockNamed(session, "pt1") || containsMockNamed(session, "pt2") {
		t.Fatalf("session tier polluted with per-test mocks: %v", mockNames(session))
	}

	// GetSessionMocks (legacy union shim) returns sess + pt1 + pt2
	// because startup ∪ session is what pre-wave-2 parsers see.
	union, err := mm.GetSessionMocks()
	if err != nil {
		t.Fatalf("GetSessionMocks: %v", err)
	}
	if len(union) != 3 {
		t.Fatalf("GetSessionMocks: want 3 (sess+pt1+pt2), got %d: %v", len(union), mockNames(union))
	}
	for _, want := range []string{"sess", "pt1", "pt2"} {
		if !containsMockNamed(union, want) {
			t.Fatalf("GetSessionMocks: missing %q, got %v", want, mockNames(union))
		}
	}

	// HasFirstTestFired must still be false — BaseTime doesn't count.
	if mm.HasFirstTestFired() {
		t.Fatalf("HasFirstTestFired: BaseTime staging must not register as first test")
	}
}

// TestSetMocksWithWindow_FirstRealWindow_RepartitionsStartup verifies
// that after initial staging, the first real test window re-partitions
// the per-test input: req < start stays in startup; req ∈ [start, end]
// moves to filtered; stale previous-test bleed drops.
func TestSetMocksWithWindow_FirstRealWindow_RepartitionsStartup(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Second)

	// Initial staging to seed firstWindowStart later. Leave the startup
	// tree empty so we can see it populate from the real-window call.
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

	// Build three per-test mocks spanning the three partitions:
	//   bootstrap: req = start - 1s    → routed to startup
	//   inWindow:  req = start + 2s    → kept in filtered
	//   strayFuture: req = end + 5s    → dropped out-of-window
	bootstrap := newMockForTest("bootstrap", start.Add(-1*time.Second), models.LifetimePerTest)
	inWindow := newMockForTest("inWindow", start.Add(2*time.Second), models.LifetimePerTest)
	strayFuture := newMockForTest("strayFuture", end.Add(5*time.Second), models.LifetimePerTest)

	mm.SetMocksWithWindow(
		[]*models.Mock{bootstrap, inWindow, strayFuture},
		nil,
		start, end,
	)

	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if len(startup) != 1 || !containsMockNamed(startup, "bootstrap") {
		t.Fatalf("GetStartupMocks: want [bootstrap], got %v", mockNames(startup))
	}

	pt, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow: %v", err)
	}
	if len(pt) != 1 || !containsMockNamed(pt, "inWindow") {
		t.Fatalf("GetPerTestMocksInWindow: want [inWindow], got %v", mockNames(pt))
	}

	// strayFuture is neither in startup nor in per-test → dropped.
	if containsMockNamed(startup, "strayFuture") || containsMockNamed(pt, "strayFuture") {
		t.Fatalf("strayFuture leaked into a pool: startup=%v pt=%v",
			mockNames(startup), mockNames(pt))
	}
}

// TestSetMocksWithWindow_StartupTreeRebuildsOnNewTestSet verifies the
// Wave-3 C3 fix: the startup tree rebuilds unconditionally on every
// SetMocksWithWindow call, so a subsequent test-set run with an empty
// per-test slice cannot leak the previous run's startup entries.
func TestSetMocksWithWindow_StartupTreeRebuildsOnNewTestSet(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	start1 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	end1 := start1.Add(10 * time.Second)
	bootstrap := newMockForTest("bootstrap_run1", start1.Add(-1*time.Second), models.LifetimePerTest)

	// First test-set: seed startup with a bootstrap mock.
	mm.SetMocksWithWindow(
		[]*models.Mock{bootstrap},
		nil,
		start1, end1,
	)
	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks (run1): %v", err)
	}
	if len(startup) != 1 || !containsMockNamed(startup, "bootstrap_run1") {
		t.Fatalf("GetStartupMocks (run1): want [bootstrap_run1], got %v", mockNames(startup))
	}

	// Second test-set: fresh window, empty per-test slice. The previous
	// bootstrap entry must NOT leak — rebuild on an empty input clears it.
	start2 := start1.Add(1 * time.Hour)
	end2 := start2.Add(10 * time.Second)
	mm.SetMocksWithWindow(nil, nil, start2, end2)

	startup, err = mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks (run2): %v", err)
	}
	if len(startup) != 0 {
		t.Fatalf("GetStartupMocks (run2): stale startup tree bled across test-sets, got %v",
			mockNames(startup))
	}
}

// mockNames returns a slice of mock Names, for readable test failure
// diagnostics.
func mockNames(list []*models.Mock) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		if m == nil {
			out = append(out, "<nil>")
			continue
		}
		out = append(out, m.Name)
	}
	return out
}
