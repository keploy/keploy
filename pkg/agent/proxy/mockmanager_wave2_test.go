// Package proxy — Wave 2 MockManager tier-partition tests.
//
// Verifies the strict three-way split introduced by Wave 2:
//
//   - GetStartupMocks()        → mocks with req < firstWindowStart
//   - GetSessionScopedMocks()  → session + connection-tagged mocks
//   - GetPerTestMocksInWindow()→ per-test mocks inside [start, end]
//
// Plus the legacy GetSessionMocks() union shim (startup + session) and
// the HasFirstTestFired() signal, which is sticky WITHIN a test-set and
// clears at each set boundary (ResetForReplaySession).
package proxy

import (
	"sync/atomic"
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
// Wave-3 C1/C3/H3 composite behaviour during Runner/Replayer's initial
// BaseTime staging call. Pre-first-test fire, tier-aware parsers (the
// v3 dispatcher) route every live query to the startup engine with no
// fallback — so EVERY mock visible to the app during bootstrap must be
// reachable via GetStartupMocks. Both per-test and session-tagged
// inputs are copied into the startup pool here; the session tree is
// still populated normally so matchers that read GetSessionScopedMocks
// directly see session-only (kept for post-first-test operation when
// the dispatcher routes live queries to the session tier).
//
// After the first real test fires, SetMocksWithWindow re-partitions —
// session mocks fall out of startup (rebuilt from firstStart cutoff)
// and revert to session-only routing. That re-partitioning is covered
// by TestSetMocksWithWindow_FirstRealWindow_RepartitionsStartup below.
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

	// GetStartupMocks returns ALL bootstrap-reachable mocks — pt1, pt2,
	// and sess — because during pre-first-test the dispatcher's
	// StartupTransactional engine is the only engine it will route to.
	// Routing sess through the session tier alone would make it
	// unreachable until after the first test fires, silently dropping
	// legitimate bootstrap-phase DDL / config queries.
	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if len(startup) != 3 {
		t.Fatalf("GetStartupMocks: want 3 (pt1+pt2+sess), got %d: %v", len(startup), mockNames(startup))
	}
	for _, want := range []string{"pt1", "pt2", "sess"} {
		if !containsMockNamed(startup, want) {
			t.Fatalf("GetStartupMocks: missing %q, got %v", want, mockNames(startup))
		}
	}

	// GetPerTestMocksInWindow returns nothing — no real window active.
	pt, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow: %v", err)
	}
	if len(pt) != 0 {
		t.Fatalf("GetPerTestMocksInWindow: want empty, got %v", mockNames(pt))
	}

	// GetSessionScopedMocks still returns only sess — the session tree
	// is populated from the unfiltered input unchanged, so post-first-
	// test session routing keeps working. Per-test mocks are never
	// promoted to the strict session tier under any staging path.
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

	// GetSessionMocks (legacy union shim: startup + session, deduped
	// by pointer identity) returns every routable mock exactly once.
	// sess lives in BOTH startup and session trees during initial
	// staging — pre-N-R1-fix the concat returned it twice, skewing
	// HitCount / consumedIndex accounting on the initial-staging
	// path. Post-fix the union returns 3 entries (pt1, pt2, sess),
	// with sess deduped by *Mock pointer identity. Pre-wave-2
	// parsers see every bootstrap mock via this shim exactly once.
	union, err := mm.GetSessionMocks()
	if err != nil {
		t.Fatalf("GetSessionMocks: %v", err)
	}
	if len(union) != 3 {
		t.Fatalf("GetSessionMocks: want 3 (pt1+pt2+sess, deduped), "+
			"got %d: %v", len(union), mockNames(union))
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

// TestIsTestWindowActive_BaseTimeStagingIsInactive is a regression pin
// for the Wave-3 H3 fix. During Runner/Replayer's initial staging call
// (start=models.BaseTime), SetMocksWithWindow must NOT publish BaseTime
// into m.windowStart — otherwise IsTestWindowActive (which only rejects
// zero-time) would flip true while no real test has fired, mis-routing
// tier-aware parsers (Postgres v3 dispatcher) to the per-test engine.
// The per-test tree is empty during initial staging (all filtered input
// goes to startup), so PerTest routing yields `candidates=0` KP001
// misses for legitimate startup-tier mocks (e.g. listmonk install
// `select count(*) from settings`).
//
// Invariants asserted:
//  1. IsTestWindowActive stays false after a BaseTime staging call.
//  2. HasFirstTestFired stays false after a BaseTime staging call.
//  3. The first real-window call flips IsTestWindowActive to true, and
//     a second BaseTime call (exotic but defensible) does NOT regress
//     the window back to an inactive state.
func TestIsTestWindowActive_BaseTimeStagingIsInactive(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	// Before any SetMocksWithWindow call, the window is inactive.
	if mm.IsTestWindowActive() {
		t.Fatalf("IsTestWindowActive: want false on a fresh manager, got true")
	}

	// Initial staging with BaseTime must NOT activate the window. This
	// is the exact case the Runner/Replayer fires before any test runs.
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if mm.IsTestWindowActive() {
		t.Fatalf("IsTestWindowActive: want false after BaseTime staging, got true " +
			"(dispatcher would mis-route startup traffic to PerTest engine)")
	}
	if mm.HasFirstTestFired() {
		t.Fatalf("HasFirstTestFired: want false after BaseTime staging, got true")
	}

	// First real-window call flips the active flag.
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Second)
	mm.SetMocksWithWindow(nil, nil, start, end)
	if !mm.IsTestWindowActive() {
		t.Fatalf("IsTestWindowActive: want true after first real-window call, got false")
	}
	if !mm.HasFirstTestFired() {
		t.Fatalf("HasFirstTestFired: want true after first real-window call, got false")
	}

	// A subsequent BaseTime call must NOT regress the window back to
	// inactive. Staging re-fires have no legitimate reason to undo the
	// real window, and the fix must be targeted at "don't set it in
	// the first place" rather than "unset it to BaseTime".
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if !mm.IsTestWindowActive() {
		t.Fatalf("IsTestWindowActive: want true (unchanged) after a re-staging BaseTime call, got false")
	}
}

// TestGetSessionMocks_DedupsStartupSessionOverlap_DuringInitialStaging
// pins the N-R1 (round 2) fix: during Runner/Replayer's BaseTime
// staging call, SetMocksWithWindow deliberately over-populates the
// startup tree with both filtered AND unfiltered input (so the v3
// dispatcher's StartupTransactional engine can serve session-tagged
// bootstrap DDL — listmonk's `DROP TYPE IF EXISTS` is the canonical
// case). That means the SAME *Mock pointer lives in both the startup
// tree AND the session tree during the pre-first-test window. The
// legacy GetSessionMocks union shim used to concat both lists and
// return each overlapping pointer TWICE, double-counting it against
// any HitCount / consumedIndex accounting that walks the union. The
// fix: pointer-identity dedup at the union point so every mock
// surfaces exactly once regardless of how many tiers hold the same
// pointer.
//
// Invariants pinned:
//  1. During initial staging (BaseTime), a session mock and a perTest
//     mock each appear in the startup tree AND the session tree (the
//     session one via the real session tree, the perTest one via
//     initial-staging's copy into startup).
//  2. GetSessionMocks returns exactly 2 (startup+session deduped),
//     NOT 3 (overlap double-count) and NOT 4 (both overlap variants).
//  3. After the first real window fires and the trees re-partition,
//     GetSessionMocks still returns exactly the same deduped count —
//     the dedup path is free at steady state (no pointer-overlap
//     between startup and session once re-partitioning completes).
func TestGetSessionMocks_DedupsStartupSessionOverlap_DuringInitialStaging(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	// Stage a LifetimeSession mock and a LifetimePerTest mock at
	// BaseTime. Both fire during Runner/Replayer's pre-first-test
	// sweep; the session mock is the listmonk-style DDL, the perTest
	// mock is any filtered-tier bootstrap query.
	bootTs := time.Date(2023, 12, 31, 23, 59, 0, 0, time.UTC)
	mSession := newMockForTest("boot-session", bootTs, models.LifetimeSession)
	mPerTest := newMockForTest("boot-pertest", bootTs.Add(time.Millisecond), models.LifetimePerTest)

	mm.SetMocksWithWindow(
		[]*models.Mock{mPerTest},
		[]*models.Mock{mSession},
		models.BaseTime, time.Now(),
	)

	// GetStartupMocks returns both — the initial-staging branch copies
	// filtered AND unfiltered into startup so the dispatcher's
	// StartupTransactional engine can serve every bootstrap query.
	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if len(startup) != 2 {
		t.Fatalf("GetStartupMocks: want 2 (session+perTest via initial-staging copy), got %d: %v",
			len(startup), mockNames(startup))
	}
	if !containsMockNamed(startup, "boot-session") || !containsMockNamed(startup, "boot-pertest") {
		t.Fatalf("GetStartupMocks missing entries: got %v", mockNames(startup))
	}

	// GetSessionScopedMocks returns JUST the session mock — the
	// session tree is populated from the unfiltered input only; the
	// perTest input never bleeds into the strict session tier.
	session, err := mm.GetSessionScopedMocks()
	if err != nil {
		t.Fatalf("GetSessionScopedMocks: %v", err)
	}
	if len(session) != 1 || !containsMockNamed(session, "boot-session") {
		t.Fatalf("GetSessionScopedMocks: want [boot-session], got %v", mockNames(session))
	}

	// GetSessionMocks union MUST dedup: the session-tagged mock lives
	// in both trees (startup via initial-staging, session via the
	// unfiltered tree) with IDENTICAL pointers. A naive concat would
	// return 3 entries; the dedup path returns 2.
	union, err := mm.GetSessionMocks()
	if err != nil {
		t.Fatalf("GetSessionMocks: %v", err)
	}
	if len(union) != 2 {
		t.Fatalf("GetSessionMocks: want 2 (deduped startup+session), got %d: %v",
			len(union), mockNames(union))
	}
	// Count occurrences by pointer to prove the dedup is pointer-
	// identity (not name-based, not structural): a later refactor
	// that swaps the dedup key from pointer to Name would still
	// satisfy the len==2 assertion but regress the guarantee
	// HitCount / consumedIndex accounting depends on.
	occurrencesOf := func(list []*models.Mock, target *models.Mock) int {
		n := 0
		for _, m := range list {
			if m == target {
				n++
			}
		}
		return n
	}
	if got := occurrencesOf(union, mSession); got != 1 {
		t.Fatalf("session pointer appears %d times in union (want 1)", got)
	}
	if got := occurrencesOf(union, mPerTest); got != 1 {
		t.Fatalf("perTest pointer appears %d times in union (want 1)", got)
	}

	// Fire the first real test window. SetMocksWithWindow re-partitions
	// into startup (req < firstStart), perTest window, session. Our
	// perTest mock has req == bootTs (well before firstStart), so it
	// moves back into startup; our session mock stays in the session
	// tree AND is NOT re-copied into startup (that copy is unique to
	// the BaseTime staging branch). The union should now be cleanly
	// disjoint.
	firstStart := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	firstEnd := firstStart.Add(10 * time.Second)
	mm.SetMocksWithWindow(
		[]*models.Mock{mPerTest},
		[]*models.Mock{mSession},
		firstStart, firstEnd,
	)

	unionAfter, err := mm.GetSessionMocks()
	if err != nil {
		t.Fatalf("GetSessionMocks (post-first-test): %v", err)
	}
	if len(unionAfter) != 2 {
		t.Fatalf("GetSessionMocks (post-first-test): want 2, got %d: %v",
			len(unionAfter), mockNames(unionAfter))
	}
	if occurrencesOf(unionAfter, mSession) != 1 {
		t.Fatalf("session pointer double-counted post-first-test")
	}
	if occurrencesOf(unionAfter, mPerTest) != 1 {
		t.Fatalf("perTest pointer double-counted post-first-test")
	}
}

// TestWindowSnapshot_CoherentPair pins the C2 (round 2) fix: the
// (IsTestWindowActive, HasFirstTestFired) pair must NEVER be observable
// in the forbidden Active=true && FirstTestFired=false state under
// concurrent SetMocksWithWindow transitions. The individual bool
// accessors read under different locks (windowMu vs swapMu) so a
// sequential observer can catch the intermediate. WindowSnapshot takes
// both locks under swapMu and returns a tear-free snapshot.
//
// Strategy: cycle a fresh MockManager through BaseTime staging ->
// real-window transitions in a writer goroutine while N reader
// goroutines call WindowSnapshot and assert the forbidden pair is
// never observed. Between iterations we reset to a fresh manager so
// the "first real window fires for the first time" transition can
// repeat — that transition is the one a torn read can misobserve.
func TestWindowSnapshot_CoherentPair(t *testing.T) {
	const outerRounds = 40
	const readers = 8
	const iterationsPerReader = 500

	realStart := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	realEnd := realStart.Add(10 * time.Second)

	for round := 0; round < outerRounds; round++ {
		mm := NewMockManager(nil, nil, zap.NewNop())

		stop := make(chan struct{})
		done := make(chan struct{}, readers)
		fail := make(chan string, readers)

		// Writer: alternate BaseTime staging with real-window calls.
		// Each BaseTime -> real-window edge is the one under race-
		// risk: windowMu updates first, firstWindowStart is written
		// first but both release windowMu's critical section at
		// different times. WindowSnapshot must sample both bits
		// under one outer lock to stay coherent.
		go func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if i%2 == 0 {
					mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
				} else {
					mm.SetMocksWithWindow(nil, nil, realStart, realEnd)
				}
			}
		}()

		for r := 0; r < readers; r++ {
			go func() {
				defer func() { done <- struct{}{} }()
				for j := 0; j < iterationsPerReader; j++ {
					snap := mm.WindowSnapshot()
					// The forbidden state. If the manager ever
					// publishes an active window without also marking
					// firstWindowStart non-zero, tier-aware parsers
					// would route the same statement's Parse/Describe
					// and Execute to different tiers.
					if snap.Active && !snap.FirstTestFired {
						select {
						case fail <- "observed Active=true && FirstTestFired=false":
						default:
						}
						return
					}
				}
			}()
		}

		for i := 0; i < readers; i++ {
			<-done
		}
		close(stop)
		select {
		case msg := <-fail:
			mm.Close()
			t.Fatalf("WindowSnapshot coherency violation (round %d): %s", round, msg)
		default:
		}
		mm.Close()
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

// TestMockManager_FirstTestWindowStart_AccessorBehavior locks in the
// FirstTestWindowStart accessor contract that the agent's tier-aware
// strictMockWindow filter depends on:
//
//   - Zero on a fresh manager (no SetMocksWithWindow yet).
//   - Zero after a BaseTime staging call (BaseTime doesn't count).
//   - Equal to the first real-window start after the first non-BaseTime
//     call.
//   - Does NOT advance when a later-starting test fires (only moves
//     earlier to protect genuine startup-mock classification).
func TestMockManager_FirstTestWindowStart_AccessorBehavior(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	// Fresh: zero.
	if got := mm.FirstTestWindowStart(); !got.IsZero() {
		t.Fatalf("FirstTestWindowStart: fresh manager want zero, got %v", got)
	}

	// BaseTime staging: still zero.
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := mm.FirstTestWindowStart(); !got.IsZero() {
		t.Fatalf("FirstTestWindowStart: after BaseTime staging want zero, got %v", got)
	}

	// First real window sets the cutoff.
	first := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	mm.SetMocksWithWindow(nil, nil, first, first.Add(10*time.Second))
	if got := mm.FirstTestWindowStart(); !got.Equal(first) {
		t.Fatalf("FirstTestWindowStart: after first real window want %v, got %v", first, got)
	}

	// A later test does NOT push firstWindowStart forward (that would
	// re-classify genuine startup mocks as stale).
	later := first.Add(1 * time.Hour)
	mm.SetMocksWithWindow(nil, nil, later, later.Add(10*time.Second))
	if got := mm.FirstTestWindowStart(); !got.Equal(first) {
		t.Fatalf("FirstTestWindowStart: later window must not advance cutoff; want %v, got %v", first, got)
	}
}

// TestMockManager_TierAwareStrictGate_StartupSurvivesPartition is the
// integration test for the tier-aware strictMockWindow fix. It stages
// startup + per-test-1 + per-test-2 mocks, fires two test windows in
// sequence, and asserts that after the second test:
//
//   - The startup-tier mock is reachable via GetStartupMocks (routed
//     into the startup tree by SetMocksWithWindow's req < firstStart
//     partition).
//   - The test-1 mock is NOT in per-test (it was stale cross-test bleed
//     for test 2 — dropped by SetMocksWithWindow's per-test partition).
//   - The test-2 mock IS in per-test.
//
// Before the tier-aware fix, the agent's strict gate would drop the
// startup mock BEFORE SetMocksWithWindow saw it, so
// GetStartupMocks returned empty and the v3 dispatcher routed startup-
// tier traffic with candidates=0.
func TestMockManager_TierAwareStrictGate_StartupSurvivesPartition(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	// Window layout:
	//   startup_req < test1_start < test1_end < test2_start < test2_end
	test1Start := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	test1End := test1Start.Add(10 * time.Second)
	test2Start := test1End.Add(1 * time.Minute)
	test2End := test2Start.Add(10 * time.Second)

	startupReq := test1Start.Add(-5 * time.Minute)
	test1Req := test1Start.Add(1 * time.Second)
	test2Req := test2Start.Add(1 * time.Second)

	startupMock := newMockForTest("startup", startupReq, models.LifetimePerTest)
	test1Mock := newMockForTest("test1", test1Req, models.LifetimePerTest)
	test2Mock := newMockForTest("test2", test2Req, models.LifetimePerTest)

	// Test 1 fires with ALL THREE mocks in the filtered slice — this is
	// the input shape the tier-aware agent filter produces (startup +
	// in-window per-test mocks kept in filtered; only the stale past-
	// window mocks are dropped at the agent). The manager partitions
	// them:
	//   startupMock: req < firstStart(=test1Start) → startup tree
	//   test1Mock:   in-window → per-test tree
	//   test2Mock:   req > test1End → stale future; dropped out-of-window
	mm.SetMocksWithWindow([]*models.Mock{startupMock, test1Mock, test2Mock}, nil, test1Start, test1End)

	if got := mm.FirstTestWindowStart(); !got.Equal(test1Start) {
		t.Fatalf("FirstTestWindowStart: after test 1, want %v, got %v", test1Start, got)
	}

	// After test 1, startup tree has the startupMock.
	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if !containsMockNamed(startup, "startup") {
		t.Fatalf("GetStartupMocks after test 1 missing 'startup' mock; got %v", mockNames(startup))
	}

	// Now test 2 fires. Input shape from the tier-aware agent filter:
	//   startupMock: req < firstWindowStart (=test1Start) → preserved
	//                in filtered as startup-tier (not dropped as bleed).
	//   test2Mock:   in-window for test 2 → per-test.
	//   test1Mock:   firstWindowStart <= req < test2Start → STALE
	//                cross-test bleed; the agent's tier-aware gate
	//                DROPS this before it reaches SetMocksWithWindow.
	//
	// So test 2's input to SetMocksWithWindow is {startup, test2}.
	mm.SetMocksWithWindow([]*models.Mock{startupMock, test2Mock}, nil, test2Start, test2End)

	// firstWindowStart must still be test1Start (doesn't advance).
	if got := mm.FirstTestWindowStart(); !got.Equal(test1Start) {
		t.Fatalf("FirstTestWindowStart: after test 2, want %v (stuck at test 1), got %v", test1Start, got)
	}

	// Startup tree still has the startup mock.
	startup, err = mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if !containsMockNamed(startup, "startup") {
		t.Fatalf("GetStartupMocks after test 2 missing 'startup' mock; got %v", mockNames(startup))
	}

	// Per-test tree contains only test2, not test1.
	perTest, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow: %v", err)
	}
	if !containsMockNamed(perTest, "test2") {
		t.Fatalf("GetPerTestMocksInWindow after test 2 missing 'test2'; got %v", mockNames(perTest))
	}
	if containsMockNamed(perTest, "test1") {
		t.Fatalf("GetPerTestMocksInWindow after test 2 must NOT contain stale 'test1' (cross-test bleed); got %v", mockNames(perTest))
	}
}

// TestGetStartupMocksByKind pins the N1 API-symmetry addition: the
// startup tier exposes a by-kind filter that mirrors
// GetUnFilteredMocksByKind / GetFilteredMocksByKind on the session /
// per-test tiers. Three mocks of differing kinds are seeded during
// initial staging; the by-kind accessor returns only matching mocks,
// the unfiltered snapshot still surfaces all three, and an empty-kind
// query returns an empty slice (not an error).
func TestGetStartupMocksByKind(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	req := time.Date(2024, 1, 1, 11, 59, 0, 0, time.UTC)
	httpMock := newMockForTest("http-boot", req, models.LifetimeSession)
	httpMock.Kind = models.HTTP
	pgMock := newMockForTest("pg-boot", req.Add(time.Second), models.LifetimeSession)
	pgMock.Kind = models.Kind("POSTGRES_V3")
	dnsMock := newMockForTest("dns-boot", req.Add(2*time.Second), models.LifetimeSession)
	dnsMock.Kind = models.DNS

	mm.SetMocksWithWindow(nil, []*models.Mock{httpMock, pgMock, dnsMock}, models.BaseTime, time.Now())

	all, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("GetStartupMocks: want 3 mocks, got %d: %v", len(all), mockNames(all))
	}

	pgOnly, err := mm.GetStartupMocksByKind(models.Kind("POSTGRES_V3"))
	if err != nil {
		t.Fatalf("GetStartupMocksByKind(POSTGRES_V3): %v", err)
	}
	if len(pgOnly) != 1 || pgOnly[0].Name != "pg-boot" {
		t.Fatalf("GetStartupMocksByKind(POSTGRES_V3): want [pg-boot], got %v", mockNames(pgOnly))
	}

	httpOnly, err := mm.GetStartupMocksByKind(models.HTTP)
	if err != nil {
		t.Fatalf("GetStartupMocksByKind(HTTP): %v", err)
	}
	if len(httpOnly) != 1 || httpOnly[0].Name != "http-boot" {
		t.Fatalf("GetStartupMocksByKind(HTTP): want [http-boot], got %v", mockNames(httpOnly))
	}

	// Unseeded kind: empty slice, not an error.
	kafkaOnly, err := mm.GetStartupMocksByKind(models.KAFKA)
	if err != nil {
		t.Fatalf("GetStartupMocksByKind(KAFKA): %v", err)
	}
	if len(kafkaOnly) != 0 {
		t.Fatalf("GetStartupMocksByKind(KAFKA): want empty, got %v", mockNames(kafkaOnly))
	}

	// Fresh manager with no SetMocksWithWindow ever called: the startup
	// tree is non-nil (allocated by NewMockManager) but empty, so we
	// get an empty (possibly nil-cap) slice back without error.
	fresh := NewMockManager(nil, nil, zap.NewNop())
	defer fresh.Close()
	got, err := fresh.GetStartupMocksByKind(models.HTTP)
	if err != nil {
		t.Fatalf("GetStartupMocksByKind on fresh manager: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("GetStartupMocksByKind on fresh manager: want empty, got %v", mockNames(got))
	}
}

// TestSetMocksWithWindow_StartupRebuild_DoesNotClobberUnfilteredID pins
// the H4 round-2 fix: during Runner/Replayer's initial BaseTime staging,
// a session mock appears in BOTH the startup tree AND the unfiltered
// tree. Pre-fix, the startup-tree loop stamped
// `mk.TestModeInfo.ID = idx` on the shared *models.Mock pointer, then
// SetUnFilteredMocks re-stamped the same pointer with its own idx. The
// shared mock's in-memory .ID thereafter reflected only the unfiltered
// stamping, desynchronising the startup tree's idIndex from the mock's
// live state. Post-fix, the startup tree uses a TIER-LOCAL copy of
// TestModeInfo as its key — mk.TestModeInfo.ID belongs to whichever
// tier stamped it last (unfiltered), and the startup tree stays
// internally consistent on its own copy.
//
// Assertion shape:
//  1. A session mock seeded during initial staging appears in BOTH
//     startup and session pools.
//  2. Its mock.TestModeInfo.ID reflects SetUnFilteredMocks' stamping
//     (index 0 in a one-element unfiltered slice), NOT the startup
//     tree's stamping.
//  3. The mock is still reachable via GetStartupMocks — i.e. the
//     startup tree's internal indexing survived the unfiltered
//     re-stamp.
func TestSetMocksWithWindow_StartupRebuild_DoesNotClobberUnfilteredID(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	req := time.Date(2024, 1, 1, 11, 59, 0, 0, time.UTC)
	sess := newMockForTest("sess", req, models.LifetimeSession)

	// Pre-set a visible SortOrder so we don't rely on the auto-stamp for
	// identifying the mock across tiers.
	sess.TestModeInfo.SortOrder = 42

	// Initial staging seeds BOTH the startup tree (via startupInit ∪
	// unfiltered copy) AND the unfiltered tree (via SetUnFilteredMocks).
	mm.SetMocksWithWindow(nil, []*models.Mock{sess}, models.BaseTime, time.Now())

	// The shared mock pointer's .ID now reflects SetUnFilteredMocks'
	// stamping (index 0 in the 1-element unfiltered slice). The startup
	// tree's internal idIndex is keyed off a separate copy — a stamp
	// collision here would be invisible at this layer but would
	// manifest on any startup-tier lookup that hit the idIndex
	// fallback.
	if got := sess.TestModeInfo.ID; got != 0 {
		t.Fatalf("sess.TestModeInfo.ID: want 0 (unfiltered stamp), got %d", got)
	}
	if got := sess.TestModeInfo.SortOrder; got != 42 {
		t.Fatalf("sess.TestModeInfo.SortOrder: pre-existing value must be preserved, want 42 got %d", got)
	}

	// The startup tree must still surface the mock. Pre-fix this could
	// still pass because the tree walk doesn't use idIndex — but the
	// intent of this assertion is to lock in the invariant that the
	// startup tree is usable after the subsequent unfiltered re-stamp.
	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if !containsMockNamed(startup, "sess") {
		t.Fatalf("GetStartupMocks: want 'sess' after initial staging, got %v", mockNames(startup))
	}

	// And the unfiltered / session-scoped tree as well.
	session, err := mm.GetSessionScopedMocks()
	if err != nil {
		t.Fatalf("GetSessionScopedMocks: %v", err)
	}
	if !containsMockNamed(session, "sess") {
		t.Fatalf("GetSessionScopedMocks: want 'sess' in session tier, got %v", mockNames(session))
	}
}

// proxy.go reuses ONE MockManager for the whole replay — deliberately, to keep
// revision tracking continuous for parsers that captured it at MockOutgoing
// time — and marks each test-set boundary with ResetForReplaySession.
//
// Tier routing is a per-test-set question, so it has to be back at "nothing
// has fired" when the next set is staged. It was not: the routing bit was read
// off the process-global firstWindowStart, so from the first test of the FIRST
// set onward it stayed true forever, and SetMocksWithWindow's isInitialStaging
// branch kept the previous set's window besides. A parser staging set 2 saw
// Active=true, routed that set's bootstrap traffic to the per-test engine, and
// missed all of it — staging had just copied those mocks into the startup tree
// precisely because it expected routing to land on the startup engine.
func TestResetForReplaySession_PutsTierRoutingBackToPreTestState(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()

	// Test set 1: one real test fires.
	start := time.Now().Add(-time.Minute)
	mm.SetMocksWithWindow(nil, nil, start, start.Add(time.Second))
	if snap := mm.WindowSnapshot(); !snap.Active || !snap.FirstTestFired {
		t.Fatalf("precondition: a real test must publish an active window, got %+v", snap)
	}

	// Set 1 ends, set 2 begins and is staged with the "no test yet" sentinel.
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

	snap := mm.WindowSnapshot()
	if snap.Active {
		t.Errorf("window still Active while staging test set 2 — bootstrap traffic routes to the per-test engine, whose tree staging just left empty")
	}
	if snap.FirstTestFired {
		t.Errorf("FirstTestFired still set while staging test set 2 — routing skips the startup engine that holds the staged mocks")
	}

	// And it must come back for set 2's own first test, or every query in
	// that set would route to startup.
	s2 := time.Now()
	mm.SetMocksWithWindow(nil, nil, s2, s2.Add(time.Second))
	if snap := mm.WindowSnapshot(); !snap.Active || !snap.FirstTestFired {
		t.Errorf("test set 2's first real test did not re-arm routing: %+v", snap)
	}
}

// The other half of the same boundary, and the reason routing alone is not
// enough: the startup-init vs. stale-bleed cutoff is per-set too.
//
// It is a running MINIMUM over every real window the manager has seen, so
// preserving it across sets pinned it to whichever set recorded earliest.
// Every other set's own bootstrap mocks fall AFTER that cutoff, so they are
// classified as stale previous-test bleed and dropped from every tier the
// instant that set's first test fires — the startup tier is empty for the
// rest of the run even though staging had just populated it.
//
// Real mocks, not nil slices: the flags being right is worth nothing if the
// mock is not actually in the tier the flags route to.
func TestResetForReplaySession_SecondTestSetKeepsItsOwnBootstrapMocks(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()

	// Test set 1, recorded earlier than set 2 (the normal ordering).
	s1 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	mm.SetMocksWithWindow(
		[]*models.Mock{newMockForTest("s1test", s1.Add(time.Second), models.LifetimePerTest)},
		nil, s1, s1.Add(10*time.Second))

	// Boundary, then set 2 stages with the "no test yet" sentinel.
	mm.ResetForReplaySession()

	s2 := s1.Add(time.Hour)
	boot := newMockForTest("s2boot", s2.Add(-5*time.Second), models.LifetimePerTest)
	test := newMockForTest("s2test", s2.Add(time.Second), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{boot, test}, nil, models.BaseTime, time.Now())

	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks during staging: %v", err)
	}
	if !containsMockNamed(startup, "s2boot") {
		t.Fatalf("staging did not put set 2's bootstrap mock in the startup tier: %v", startup)
	}

	// Set 2's first real test fires. Its bootstrap mock must STAY in the
	// startup tier — it was recorded before this set's first test, which is
	// the definition of startup-init.
	mm.SetMocksWithWindow([]*models.Mock{boot, test}, nil, s2, s2.Add(10*time.Second))

	startup, err = mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks after set 2's first test: %v", err)
	}
	if !containsMockNamed(startup, "s2boot") {
		perTest, _ := mm.GetPerTestMocksInWindow()
		session, _ := mm.GetSessionScopedMocks()
		t.Fatalf("set 2's own bootstrap mock was dropped from every tier once its first test fired "+
			"(cutoff stuck at set 1's start): startup=%v perTest=%v session=%v",
			startup, perTest, session)
	}

	// And the mock that IS in this set's window still routes per-test, so the
	// cutoff reset did not simply reclassify everything as startup.
	perTest, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow: %v", err)
	}
	if !containsMockNamed(perTest, "s2test") {
		t.Fatalf("set 2's in-window test mock is missing from the per-test tier: %v", perTest)
	}
}

// The startup-init cutoff must follow the set's earliest RECORDED test, not
// whichever test happens to fire first.
//
// firstWindowStart is a running minimum over the windows that actually fire, so
// when the first fired test is not the earliest recorded — a --test-sets
// selection, an ignored test, the streaming deferral — the cutoff lands late.
// Mocks belonging to a test that is NOT being run then fall before it, are
// classified as startup-init instead of dropped as previous-test bleed, and are
// served as bootstrap traffic.
//
// SeedStartupCutoff closes that: the replayer already loads test cases sorted by
// request timestamp, so it supplies testCases[0]'s time before any test runs.
func TestSeedStartupCutoff_SkippedFirstTestDoesNotLeakAsBootstrap(t *testing.T) {
	t1 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	build := func() (*MockManager, []*models.Mock) {
		mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
		t.Cleanup(func() { mm.Close() })
		return mm, []*models.Mock{
			newMockForTest("boot", t1.Add(-time.Minute), models.LifetimePerTest),
			newMockForTest("t1mock", t1.Add(time.Second), models.LifetimePerTest),
			newMockForTest("t2mock", t2.Add(time.Second), models.LifetimePerTest),
		}
	}

	// Without the seed the cutoff follows the fired window (t2), so t1's mock —
	// belonging to a test that never runs — reads as bootstrap.
	unseeded, all := build()
	unseeded.SetMocksWithWindow(all, nil, models.BaseTime, time.Now())
	unseeded.SetMocksWithWindow(all, nil, t2, t2.Add(10*time.Second))
	before, err := unseeded.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if !containsMockNamed(before, "t1mock") {
		t.Skip("baseline no longer reproduces; the cutoff is derived differently now")
	}

	// With the seed it is classified against t1, so only genuine bootstrap
	// traffic survives in the startup tier.
	seeded, all := build()
	seeded.SetMocksWithWindow(all, nil, models.BaseTime, time.Now())
	seeded.SeedStartupCutoff(t1) // what the replayer supplies: testCases[0]
	seeded.SetMocksWithWindow(all, nil, t2, t2.Add(10*time.Second))

	after, err := seeded.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if !containsMockNamed(after, "boot") {
		t.Fatalf("the genuine bootstrap mock must stay in the startup tier, got %v", after)
	}
	if containsMockNamed(after, "t1mock") {
		t.Fatal("a mock from a test that never ran is still classified as startup-init and " +
			"will be served as bootstrap traffic")
	}
}

// Seeding the cutoff must NOT make routing think a test has fired. If it did,
// staging would look like a live test and the set's bootstrap traffic would be
// routed to the per-test engine over a tree staging had just emptied — the
// exact outage the per-test-set reset exists to prevent.
func TestSeedStartupCutoff_DoesNotMarkATestAsFired(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()

	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.SeedStartupCutoff(start)

	if snap := mm.WindowSnapshot(); snap.Active || snap.FirstTestFired {
		t.Fatalf("seeding the cutoff must leave routing at 'nothing has fired', got %+v", snap)
	}
	if mm.FirstTestWindowStart().IsZero() {
		t.Fatal("the cutoff was not seeded")
	}

	// A real window still flips routing.
	mm.SetMocksWithWindow(nil, nil, start, start.Add(10*time.Second))
	if snap := mm.WindowSnapshot(); !snap.Active || !snap.FirstTestFired {
		t.Fatalf("a real window must flip routing, got %+v", snap)
	}
}

// A consume must always terminate. Every parser except mongo v2 treats a false
// from DeleteFilteredMock as "another goroutine won the race, retry against the
// shrunk pool" and loops — http/match.go's `for {}` re-fetches the pool and
// continues, and generic, grpcV2, http2, mongo v1, sqs and kafka share the
// shape. But those parsers match against GetSessionMocks, which is startup UNION
// session, so they can pick a STARTUP-tier mock — and DeleteStartupMock has no
// caller in OSS at all. If the per-test door simply refuses such a mock, the
// pool never shrinks, the retry re-picks it, and the loop spins until the
// request context dies.
//
// Before the tier keys were disambiguated this never surfaced, because the
// blind delete always "succeeded" by evicting whatever shared the key. Fixing
// that eviction without giving the mock a door would have converted a
// wrong-mock bug into a livelock.
func TestDeleteFilteredMock_ConsumesAStartupTierMockSoRetryLoopsTerminate(t *testing.T) {
	mm, bootCopy := tierFixture(t)

	if !mm.DeleteFilteredMock(bootCopy) {
		t.Fatal("a startup-tier mock matched out of the startup-union pool was refused by every " +
			"door; the caller's retry loop re-picks it forever and spins until its context dies")
	}

	// And it must be genuinely consumed, not merely reported as such.
	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if containsMockNamed(startup, "boot") {
		t.Fatal("reported consumed but still present in the startup tier; the next match re-picks it")
	}

	// The per-test mock sharing its key is still untouched — the original defect.
	perTest, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow: %v", err)
	}
	if !containsMockNamed(perTest, "live") {
		t.Fatal("consuming the startup mock evicted the running test's own per-test mock")
	}
}

// tierFixture stages a set the way the agent does — fresh copies with
// SortOrder unset on every call, so each tier restamps its keys from 1 and the
// first entry of every tree lands on (SortOrder:1, ID:0). Reusing pointers
// across calls carries the first stamping forward and hides the collision
// entirely, so a test that does that proves nothing.
func tierFixture(t *testing.T) (*MockManager, models.Mock) {
	t.Helper()
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(func() { mm.Close() })

	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	boot := newMockForTest("boot", start.Add(-time.Minute), models.LifetimePerTest)
	live := newMockForTest("live", start.Add(time.Second), models.LifetimePerTest)
	sess := newMockForTest("sess", start.Add(2*time.Second), models.LifetimeSession)

	fresh := func() ([]*models.Mock, []*models.Mock) {
		a, b, c := *boot, *live, *sess
		a.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimePerTest}
		b.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimePerTest}
		c.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimeSession}
		return []*models.Mock{&a, &b}, []*models.Mock{&c}
	}
	f, u := fresh()
	mm.SetMocksWithWindow(f, u, models.BaseTime, time.Now())
	f, u = fresh()
	mm.SetMocksWithWindow(f, u, start, start.Add(10*time.Second))

	startup, err := mm.GetStartupMocks()
	if err != nil || !containsMockNamed(startup, "boot") {
		t.Fatalf("precondition: boot must be in the startup tier, got %v (err %v)", startup, err)
	}
	var bootCopy models.Mock
	for _, m := range startup {
		if m != nil && m.Name == "boot" {
			bootCopy = *m
		}
	}
	return mm, bootCopy
}

// Consuming a startup-tier mock through the FILTERED door must not evict the
// per-test entry that shares its key. mongo v2 reaches this: it tries
// DeleteFilteredMock before falling back to DeleteStartupMock.
func TestDeleteFilteredMock_LeavesAnotherTiersMockAtTheSameKeyAlone(t *testing.T) {
	mm, bootCopy := tierFixture(t)

	// It reports success — it consumes the mock from the tier it actually lives
	// in, which is what keeps the caller's retry loop terminating (see
	// TestDeleteFilteredMock_ConsumesAStartupTierMockSoRetryLoopsTerminate).
	// What must NOT happen is the per-test entry sharing its key being evicted.
	mm.DeleteFilteredMock(bootCopy)
	perTest, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow: %v", err)
	}
	if !containsMockNamed(perTest, "live") {
		t.Fatal("deleting the startup mock evicted the running test's own per-test mock")
	}
}

// The same for the UNFILTERED door, and this is the one that matters most:
// HTTP and MySQL match against the startup-union pool and, when
// DeleteFilteredMock declines, fall through to UpdateUnFilteredMock. Its ID
// index is keyed on ID alone — and ID is stamped from zero per tier — so an
// unguarded fallback rewrites whatever sits at that ID in the session tree.
// The victim there is reused by every test in the set, which is strictly worse
// than the per-test eviction it replaces.
func TestUpdateUnFilteredMock_DoesNotRewriteAnotherTiersMockViaTheIDIndex(t *testing.T) {
	mm, bootCopy := tierFixture(t)

	before, err := mm.GetSessionScopedMocks()
	if err != nil || !containsMockNamed(before, "sess") {
		t.Fatalf("precondition: the session mock must be present, got %v (err %v)", before, err)
	}

	updated := bootCopy
	updated.TestModeInfo.SortOrder = 9999
	mm.UpdateUnFilteredMock(&bootCopy, &updated)

	after, err := mm.GetSessionScopedMocks()
	if err != nil {
		t.Fatalf("GetSessionScopedMocks: %v", err)
	}
	if !containsMockNamed(after, "sess") {
		t.Fatal("updating with a startup-tier mock destroyed the session mock at the same ID; " +
			"it is reused by every test in the set")
	}
	if containsMockNamed(after, "boot") {
		t.Fatal("a startup-tier mock was inserted into the session tier, where it will be " +
			"served for the rest of the run")
	}
}

// The guard must not cost a legitimate consume. There was no positive-case test
// for DeleteFilteredMock anywhere in this package before.
func TestDeleteFilteredMock_StillConsumesItsOwnTiersMock(t *testing.T) {
	mm, _ := tierFixture(t)

	perTest, err := mm.GetPerTestMocksInWindow()
	if err != nil || !containsMockNamed(perTest, "live") {
		t.Fatalf("precondition: live must be in the per-test tier, got %v (err %v)", perTest, err)
	}
	var liveCopy models.Mock
	for _, m := range perTest {
		if m != nil && m.Name == "live" {
			liveCopy = *m
		}
	}

	if !mm.DeleteFilteredMock(liveCopy) {
		t.Fatal("a per-test mock consumed through its own door was refused; the identity guard " +
			"must not turn a legitimate consume into a no-op, or the mock is served twice")
	}
	after, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow: %v", err)
	}
	if containsMockNamed(after, "live") {
		t.Fatal("DeleteFilteredMock reported success but the mock is still in the tier")
	}
}

// The window bits and the mock trees they describe must change TOGETHER.
//
// ResetForReplaySession runs at the test-set boundary, but the next set's mocks
// do not arrive until its staging call. If the reset cleared the bits, then for
// the whole gap between the two the manager said "nothing has fired" while the
// trees still held the PREVIOUS set's mocks — so a query arriving in that gap
// was routed to the startup engine and answered out of the previous set's
// startup tier. Reachable whenever the app survives the boundary
// (--keep-app-alive, compose reuse).
//
// Clearing the trees at the reset instead would serve an empty pool to a parser
// racing it, and a hard miss against a live app is what crash-loops it. So the
// bits are deferred to staging, which is where the trees are replaced anyway.
func TestResetForReplaySession_WindowBitsAndTreesChangeTogether(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()

	s1 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	boot := newMockForTest("s1boot", s1.Add(-time.Minute), models.LifetimePerTest)
	test := newMockForTest("s1test", s1.Add(time.Second), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{boot, test}, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow([]*models.Mock{boot, test}, nil, s1, s1.Add(10*time.Second))

	mm.ResetForReplaySession()

	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if containsMockNamed(startup, "s1boot") && !mm.WindowSnapshot().FirstTestFired {
		t.Fatal("half torn down: the startup tier still holds the previous set's mocks while " +
			"the window bits already say nothing has fired, so a query in the gap is routed to " +
			"the startup engine and answered from the previous set")
	}

	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if snap := mm.WindowSnapshot(); snap.Active || snap.FirstTestFired {
		t.Fatalf("staging must put routing back to 'nothing has fired', got %+v", snap)
	}
}

// A startup consume must be recorded, and must NOT be recorded as Deleted.
//
// Unrecorded, it is invisible to the run's consumed set, so the resend guard
// re-sends a request that already burned a mock — and because startupInit is
// derived from the same filtered slice, the burned mock is re-staged by the
// next SetMocksWithWindow and served again.
//
// Recorded as Deleted it is worse: filterOutDeleted prunes on Deleted and is
// applied to the slice the startup tier is rebuilt from, so the boot mock would
// disappear permanently and a driver reconnect that legitimately replays the
// bootstrap chain would miss.
func TestDeleteStartupMock_RecordsTheConsumeWithoutPruningTheBootMock(t *testing.T) {
	mm, bootCopy := tierFixture(t)

	if !mm.DeleteStartupMock(bootCopy) {
		t.Fatal("precondition: the startup mock must be consumable")
	}

	consumed := mm.GetConsumedMocks()
	var got *models.MockState
	for i := range consumed {
		if consumed[i].Name == "boot" {
			got = &consumed[i]
		}
	}
	if got == nil {
		t.Fatal("the startup consume was not recorded; the resend guard cannot see it and the " +
			"mock is re-staged by the next SetMocksWithWindow")
	}
	if got.Usage == models.Deleted {
		t.Fatal("recorded as Deleted: filterOutDeleted prunes on that, and it is applied to the " +
			"slice the startup tier is rebuilt from, so the boot mock is lost for the whole run")
	}
}

// A revision must never be observable for a torn state.
//
// SetMocksWithWindow swaps startup, filtered and unfiltered in three
// independent treesMu sections, and the by-kind readers each take treesMu on
// their own. When each swap bumped the revision, the value published midway was
// legitimately "current" for a half-applied state — so a parser following the
// usual sample-revision / read-tiers / cache-under-that-revision idiom could
// capture test N+1's per-test mocks beside test N's session mocks and never
// invalidate, because the revision it recorded really was the newest.
func TestSetMocksWithWindow_PublishesOneRevisionAfterAllTiersLand(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()

	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	perTest := newMockForTest("pt", start.Add(time.Second), models.LifetimePerTest)
	sess := newMockForTest("ss", start.Add(2*time.Second), models.LifetimeSession)

	before := mm.Revision()
	mm.SetMocksWithWindow([]*models.Mock{perTest}, []*models.Mock{sess}, start, start.Add(10*time.Second))
	after := mm.Revision()

	if after == before {
		t.Fatal("the swap published no revision at all; cached indexes never invalidate")
	}
	// Exactly one publication for the whole swap. More than one means there is
	// an intermediate value a consumer can sample and cache a torn read under.
	if got := after - before; got != 1 {
		t.Fatalf("the swap published %d revisions; a consumer can sample an intermediate one "+
			"and cache an index built from a half-applied state", got)
	}
}

// A kind that LEAVES the pool has to bump too. `touched` is built from the
// incoming slice, so without also walking the map being replaced, a kind that
// was present and is now absent keeps its old revision — and a consumer caching
// by that revision never learns its mocks are gone and keeps serving them.
func TestSetMocks_BumpsAKindThatLeavesThePool(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()

	now := time.Now()
	http := newMockForTest("h1", now, models.LifetimePerTest)
	http.Kind = models.HTTP
	mm.SetFilteredMocks([]*models.Mock{http})
	revWithHTTP := mm.RevisionByKind(models.HTTP)

	// Replace the pool with a different kind entirely: HTTP has left.
	pg := newMockForTest("p1", now, models.LifetimePerTest)
	pg.Kind = models.PostgresV3
	mm.SetFilteredMocks([]*models.Mock{pg})

	if mm.RevisionByKind(models.HTTP) == revWithHTTP {
		t.Fatal("HTTP left the pool but its revision is frozen; a consumer caching under it " +
			"keeps serving mocks that are no longer there")
	}
}

// The startup fallback in DeleteFilteredMock must not destroy a REUSABLE mock.
//
// During BaseTime staging the startup tree also holds the session mocks, so the
// fallback can reach one. Consuming its startup-tier view is fine — that view
// exists to make bootstrap traffic matchable — but the session tier is the
// durable home and must still serve it, or a handshake/auth recording every
// later test depends on disappears on its first use.
func TestDeleteFilteredMock_StartupFallbackLeavesTheSessionTierIntact(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()

	sess := newMockForTest("sess", time.Now(), models.LifetimeSession)
	mm.SetMocksWithWindow(nil, []*models.Mock{sess}, models.BaseTime, time.Now())

	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	var copyOf models.Mock
	for _, m := range startup {
		if m != nil && m.Name == "sess" {
			copyOf = *m
		}
	}
	if copyOf.Name == "" {
		t.Skip("session mocks are no longer staged into the startup tree; the fallback " +
			"cannot reach one and this test has nothing to protect")
	}

	mm.DeleteFilteredMock(copyOf)

	session, err := mm.GetSessionScopedMocks()
	if err != nil {
		t.Fatalf("GetSessionScopedMocks: %v", err)
	}
	if !containsMockNamed(session, "sess") {
		t.Fatal("the startup fallback consumed a session mock out of its durable tier; every " +
			"later test that needs that handshake now misses")
	}
}

// The replayer seeds the set's startup-init cutoff and then stages its mocks in
// ONE UpdateMockParams call — SeedStartupCutoff (agent.go) followed by
// SetMocksWithWindow. Staging is also where the set boundary clears the window
// bits, so a seed written straight into firstWindowStart was wiped microseconds
// after it was written, for EVERY set: the feature was inert in production
// while its unit tests passed, because they used a fresh manager where the
// clear had nothing to remove.
func TestSeedStartupCutoff_SurvivesTheStagingThatFollowsIt(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(func() { mm.Close() })

	first := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	// Exactly the production order for one test set.
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(first)
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

	if got := mm.FirstTestWindowStart(); !got.Equal(first) {
		t.Fatalf("cutoff after staging = %v, want %v: the boundary clear wiped the "+
			"seed the replayer had just supplied, so every mock recorded before the "+
			"set's first test is dropped instead of routed to the startup tier", got, first)
	}

	// Installing a cutoff must not look like a fired test. If it did, tier-aware
	// parsers would route the set's bootstrap traffic to the per-test engine
	// over a tree that staging just emptied.
	if mm.HasFirstTestFired() {
		t.Fatal("the installed cutoff marked a test as fired")
	}
	if snap := mm.WindowSnapshot(); snap.Active || snap.FirstTestFired {
		t.Fatalf("window snapshot after staging = %+v, want inactive with no test fired", snap)
	}
}

// Test sets are recorded in chronological order, so set N+1's cutoff is always
// LATER than set N's. A running-minimum guard against the live value therefore
// REFUSES it, and set N+1 runs the whole set on set N's cutoff — every mock
// recorded between the two sets reads as previous-test bleed and is dropped
// rather than served as that set's bootstrap traffic.
func TestSeedStartupCutoff_SecondSetDoesNotInheritTheFirstsCutoff(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(func() { mm.Close() })

	setA := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	setB := setA.Add(time.Hour)

	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(setA)
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow(nil, nil, setA, setA.Add(time.Second)) // a test fires in set A

	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(setB)
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

	if got := mm.FirstTestWindowStart(); !got.Equal(setB) {
		t.Fatalf("set B cutoff = %v, want %v: set B is running on set A's cutoff", got, setB)
	}

	// The user-visible consequence: set B's own bootstrap mock, recorded after
	// set A ended, must reach the startup tier.
	boot := newMockForTest("setB-bootstrap", setB.Add(-5*time.Minute), models.LifetimePerTest)
	mm.SetMocksWithWindow([]*models.Mock{boot}, nil, setB, setB.Add(time.Second))

	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if !containsMockNamed(startup, "setB-bootstrap") {
		t.Fatalf("set B's bootstrap mock was dropped instead of routed to startup "+
			"(startup=%d dropped=%d cutoff=%v)", len(startup), mm.DroppedOutOfWindow(),
			mm.FirstTestWindowStart())
	}
}

// A kind that LEAVES the pool must bump its own per-kind revision, or a
// consumer caching an index under RevisionByKind never learns its mocks are
// gone and keeps serving the previous set's index.
//
// The walk used to read filteredByKind/unfilteredByKind AFTER the tier swap, so
// it read the NEW maps — exactly redundant with walking the new input slices,
// and departing kinds were published nowhere. A kind living only in the startup
// tier was missed in both directions, because during BaseTime staging
// filteredForTree is nil and such a kind never enters filteredByKind at all.
func TestSetMocksWithWindow_BumpsRevisionForDepartingKinds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		startup bool // seed via the startup tier (BaseTime staging) instead of a live window
	}{
		{name: "per-test tier"},
		{name: "startup tier", startup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
			t.Cleanup(func() { mm.Close() })

			at := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
			redis := newMockForTest("redis-1", at, models.LifetimePerTest)
			redis.Kind = models.REDIS

			if tc.startup {
				mm.SetMocksWithWindow([]*models.Mock{redis}, nil, models.BaseTime, time.Now())
			} else {
				mm.SetMocksWithWindow([]*models.Mock{redis}, nil, at, at.Add(time.Second))
			}
			before := mm.RevisionByKind(models.REDIS)
			if before == 0 {
				t.Fatal("precondition: staging redis mocks must move the redis revision")
			}

			// Next set holds no redis mocks at all.
			mm.SetMocksWithWindow(nil, nil, at.Add(time.Hour), at.Add(time.Hour+time.Second))

			if got := mm.RevisionByKind(models.REDIS); got == before {
				t.Fatalf("redis per-kind revision stayed %d after its mocks left the pool; "+
					"a revision-gated consumer keeps serving the previous set's redis index", got)
			}
		})
	}
}

// A parked cutoff must not outlive the set that seeded it.
//
// UpdateMockParams can abort between SeedStartupCutoff and SetMocksWithWindow
// — the "no mocks stored for client ID" bail and the loadPerTestMocks error
// path both return in that gap — leaving a park with no staging call to consume
// it. Sets are recorded chronologically, so the orphan is always the EARLIER
// value: it would win any running-minimum guard and the next set would replay
// on the aborted set's cutoff, dropping its bootstrap traffic as bleed.
func TestSeedStartupCutoff_AbortedSetDoesNotLeakItsCutoffToTheNext(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(func() { mm.Close() })

	setA := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	setB := setA.Add(time.Hour)

	// Set A seeds and then aborts before staging.
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(setA)

	// Set B runs normally.
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(setB)
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

	if got := mm.FirstTestWindowStart(); !got.Equal(setB) {
		t.Fatalf("set B cutoff = %v, want %v: the aborted set's parked cutoff survived "+
			"its own set and was installed here", got, setB)
	}

	// The set after an abort need not seed at all — the replayer supplies a
	// cutoff only when it knows the set's first recorded test. Then nothing
	// overwrites the orphan, so the reset is the only thing that can clear it.
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(setB.Add(time.Hour))
	mm.ResetForReplaySession() // that set aborts before staging

	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now()) // no seed for this one

	if got := mm.FirstTestWindowStart(); !got.IsZero() {
		t.Fatalf("cutoff = %v, want zero: an unseeded set inherited a parked cutoff "+
			"from an aborted earlier set", got)
	}
}

// MarkMockAsUsed must move a startup-tier mock's HitCount.
//
// rebuildHitIndex was handed only the per-test and session slices, and
// bumpHitCount's slow path walked filteredByKind, unfilteredByKind and the
// connection trees — never the startup tree. A startup-only mock therefore
// missed the index forever: every call took the process-wide exclusive hitMu,
// walked all of those trees, found nothing, and seeded nothing, so the next
// call repeated the whole walk — and the mock's HitCount never moved.
func TestMarkMockAsUsed_CountsStartupTierMocks(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(func() { mm.Close() })

	first := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	boot := newMockForTest("boot-mock", first.Add(-time.Minute), models.LifetimePerTest)

	// Stage, then open a window so the boot mock is classified startup-init.
	mm.SetMocksWithWindow([]*models.Mock{boot}, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow([]*models.Mock{boot}, nil, first, first.Add(time.Second))

	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if !containsMockNamed(startup, "boot-mock") {
		t.Fatal("precondition: the mock must be in the startup tier")
	}

	if !mm.MarkMockAsUsed(*boot) {
		t.Fatal("MarkMockAsUsed reported failure")
	}

	live, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	for _, mk := range live {
		if mk != nil && mk.Name == "boot-mock" {
			if got := atomic.LoadUint64(&mk.TestModeInfo.HitCount); got == 0 {
				t.Fatal("the startup-tier mock's HitCount is still 0: it is absent from " +
					"hitIdx and from the slow path's search, so every MarkMockAsUsed " +
					"takes the exclusive lock, walks every other tree and seeds nothing")
			}
			return
		}
	}
	t.Fatal("the startup mock disappeared from the tier")
}

// SetMocksWithWindowThreeTier inserts its explicit startup slice AFTER the
// SetMocksWithWindow it delegates to has already rebuilt hitIdx, so those mocks
// could never reach the index — and the slow path does not search the startup
// tree. Their HitCount could never move at all.
func TestSetMocksWithWindowThreeTier_IndexesItsExplicitStartupSlice(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(func() { mm.Close() })

	at := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	boot := newMockForTest("threetier-boot", at.Add(-time.Minute), models.LifetimePerTest)

	mm.SetMocksWithWindowThreeTier(nil, nil, []*models.Mock{boot}, at, at.Add(time.Second))

	mm.hitMu.RLock()
	_, indexed := mm.hitIdx["threetier-boot"]
	mm.hitMu.RUnlock()
	if !indexed {
		t.Fatal("the explicit startup mock never reached hitIdx: it is inserted after " +
			"SetMocksWithWindow's rebuild, and the slow path does not search the startup tree")
	}

	if !mm.MarkMockAsUsed(*boot) {
		t.Fatal("MarkMockAsUsed reported failure")
	}
	if got := atomic.LoadUint64(&boot.TestModeInfo.HitCount); got == 0 {
		t.Fatal("HitCount did not move for a mock in the explicit startup slice")
	}

	// The seeding is additive, never displacing: an explicit startup mock must
	// not steal the index entry of a session mock with the same name, or the
	// rebuild's precedence is inverted by the back door.
	mm2 := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(func() { mm2.Close() })
	sessionCopy := newMockForTest("dup-name", at.Add(time.Second), models.LifetimeSession)
	startupCopy := newMockForTest("dup-name", at.Add(-time.Minute), models.LifetimePerTest)

	mm2.SetMocksWithWindowThreeTier(nil, []*models.Mock{sessionCopy},
		[]*models.Mock{startupCopy}, at, at.Add(time.Minute))

	mm2.hitMu.RLock()
	claimed := mm2.hitIdx["dup-name"]
	mm2.hitMu.RUnlock()
	if claimed != sessionCopy {
		t.Fatal("the explicit startup mock displaced the session mock's index entry; " +
			"addToHitIndexIfAbsent must not overwrite what the rebuild established")
	}
}

// A duplicate name must not let a startup mock outrank a session or per-test
// one. rebuildHitIndex resolves duplicates to the LAST slice that carries the
// name, so the tier order it is called with is load-bearing: passing startup
// last would silently invert the precedence this call has always had.
func TestRebuildHitIndex_StartupDoesNotOutrankTheOtherTiers(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(func() { mm.Close() })

	at := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// Same name, distinct pointers: one lands in the startup tier (recorded
	// before the window), one in the session tier.
	bootCopy := newMockForTest("shared-name", at.Add(-time.Minute), models.LifetimePerTest)
	sessionCopy := newMockForTest("shared-name", at.Add(time.Second), models.LifetimeSession)

	mm.SetMocksWithWindow([]*models.Mock{bootCopy}, []*models.Mock{sessionCopy}, at, at.Add(time.Minute))

	mm.hitMu.RLock()
	got := mm.hitIdx["shared-name"]
	mm.hitMu.RUnlock()
	if got != sessionCopy {
		t.Fatal("a startup-tier mock claimed the index entry over the session-tier mock " +
			"of the same name; rebuildHitIndex must be called with startup FIRST so it " +
			"loses the duplicate-name tie")
	}
}
