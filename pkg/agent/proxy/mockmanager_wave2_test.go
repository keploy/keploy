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
