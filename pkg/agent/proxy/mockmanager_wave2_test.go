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
