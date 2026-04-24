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
