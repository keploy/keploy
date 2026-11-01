package proxy

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// ---------------- MockManager (kind-aware) ----------------
//
// Lock-ordering invariant (MUST be preserved to avoid deadlock):
//
//	swapMu  →  treesMu  →  windowMu
//	swapMu  →  revMu         (staging publishes the tiers, publishFiltered /
//	                          publishUnfiltered, which bump revisions; revMu
//	                          is a leaf lock — never re-acquire
//	                          swapMu/treesMu from under it)
//	swapMu  →  consumedMu    (independent; never hold consumedMu then swapMu)
//	hitMu   →  treesMu  →  connMu  →  TreeDb.mu
//	                         (bumpHitCount's slow path; rebuildHitIndex takes
//	                          hitMu as a LEAF — never acquire treesMu from
//	                          under it, and never take hitMu while holding
//	                          treesMu)
//	swapMu  →  hitMu         (the staging calls index the staged names
//	                          with hitMu as that leaf)
//	sigMu                    (window-change signal; a LEAF — nothing is
//	                          taken under it, and it may be taken under any)
//	swapMu  →  exact.outside.mu  →  treesMu  →  TreeDb.mu
//	                         (prepareOutside reads the staged trees under the
//	                          staging lock; nothing takes swapMu under it)
//	swapMu  →  treesMu, carry.mu
//	                         (fileCarryOver, under SetMocksWithWindow;
//	                          carry.mu is a LEAF, also taken bare by
//	                          DeleteFilteredMock and the carry-over readers)
//
// Any new code path that acquires more than one of these MUST take them
// in the declared order. Readers of {mock pool, test window} as an
// atomic pair MUST go through GetFilteredMocksInWindow (which RLocks
// swapMu). Writers that swap the pool AND publish a new window MUST go
// through SetMocksWithWindow (which Locks swapMu end-to-end).
//
// SetFilteredMocks and SetUnFilteredMocks take swapMu too: they stage, and
// staging settles which mocks it may stamp under it (see mockOwner).
// SetCurrentTestWindow does NOT take swapMu — it only serializes on its
// fine-grained mutex. Callers who need whole-snapshot consistency must use
// the atomic variants above.

type MockManager struct {
	// statements is the stored set's statement catalog, by kind
	// (SeedStatementCatalog): describe-only, in no tree, never served or
	// consumed. Replaced whole when a set's mocks are stored; read under
	// statementsMu.
	statementsMu sync.RWMutex
	statements   map[models.Kind][]*models.Mock

	// servedObserver, when set, is told of every mock a parser is served
	// (consumed or reused), before the claim takes it out of the pool and
	// outside the manager's own locks: under the proven switch, the learn
	// step's earlier places of the test's recording (learn.Tracker.ServedMock).
	servedObserver atomic.Pointer[func(*models.Mock)]
	// servedInObserver, when set, is told instead of a mock a consume door
	// serves in a request its parser said (integrations.ServingIn,
	// learn.Tracker.ServedMockIn).
	servedInObserver atomic.Pointer[func(*models.Mock, *learn.Flight)]
	// openTestEpoch, when set, says the learn tracker's open test
	// (learn.Tracker.OpenEpoch) as a test's view is installed: the view's
	// stamp (exactView.stamp, ServedInOpenTest).
	openTestEpoch atomic.Pointer[func() uint64]
	// trackerSetAt, when set, says the learn tracker's running set
	// (learn.Tracker.SetAt) as a staging is made: the set it stages
	// (roundState.stagedSet).
	trackerSetAt atomic.Pointer[func() uint64]

	// legacy "all" trees (kept for compatibility with existing callers)
	filtered   *TreeDb
	unfiltered *TreeDb

	// startup tier (Wave 2) holds app-bootstrap traffic recorded BEFORE
	// the first test window fires — Flyway migrations, Hibernate
	// metadata-boot queries, HikariCP pool validation, driver-handshake
	// SQL. It is strictly disjoint from the session tier (`unfiltered`)
	// and from the per-test tier (`filtered`): every mock lives in
	// exactly one of the three trees.
	//
	// Populated by SetMocksWithWindow when a mock's ReqTimestampMock
	// predates firstWindowStart; queried via GetStartupMocks. The
	// deprecated GetSessionMocks union shim concatenates the startup
	// walk + the session walk so legacy parsers keep seeing startup
	// mocks in their "session" snapshot until they migrate.
	startup *TreeDb
	// startupKeys maps each mock the startup tree holds to its key there, so
	// a change made to the same mock through the session tier can follow it
	// into the startup tree (see mirrorStartup). Rebuilt with the tree; guarded
	// by treesMu. An entry whose mock DeleteStartupMock removed stays until the
	// next staging; every swap and remove is pointer-checked, so it is inert.
	startupKeys map[*models.Mock]models.TestModeInfo
	// startupShared is set while the startup tier may hold the session tier's
	// own mocks — after a staging that filed them in both (see mirrorStartup)
	// — and clear otherwise, so session updates pay for mirroring only then.
	startupShared atomic.Bool
	// mirrorMu makes a session-tier update or delete and its mirror into the
	// startup tier one step, while startupShared. Without it two matchers
	// updating the same recording at once could each mirror out of order and
	// leave the tiers holding different copies. Taken before treesMu.
	mirrorMu sync.Mutex

	// global revision (legacy)
	rev uint64
	// staged is rev less the moves a consume under exact serving makes
	// (StagingGeneration).
	staged uint64
	// stored is staged less the moves an exact view's install makes: it
	// moves on a staging, a store into a tree or a delete (SharedGeneration).
	stored uint64

	// NEW: per-kind revisions; stagedByKind is revByKind less the moves a
	// consume under exact serving makes (StagingGenerationByKind), and
	// storedByKind stagedByKind less the moves an exact view's install makes.
	revMu        sync.RWMutex
	revByKind    map[models.Kind]*uint64
	stagedByKind map[models.Kind]*uint64
	storedByKind map[models.Kind]*uint64

	// NEW: per-kind trees (guarded by treesMu)
	treesMu          sync.RWMutex
	filteredByKind   map[models.Kind]*TreeDb
	unfilteredByKind map[models.Kind]*TreeDb

	logger *zap.Logger

	// consumedMu guards consumedList and consumedIndex.
	// consumedList records MockState entries in the order they were first
	// intercepted from the network (first call to flagMockAsUsed wins the
	// position; subsequent calls for the same name update the state in-place
	// without changing order).
	consumedMu    sync.Mutex
	consumedList  []models.MockState
	consumedIndex map[string]int
	// consumedPersistent accumulates the LATEST MockState per mock name across
	// the whole session and is NEVER drained (unlike consumedList, which
	// GetConsumedMocks empties). It lets the agent apply filterOutDeleted from
	// its OWN consumption history instead of a copy the client re-sends every
	// testcase. Read only when MockFilterParams.AgentOwnsConsumed is set.
	consumedPersistent map[string]models.MockState
	// servedCount counts, per mock name, every time the mock answered a
	// request this session, reset with consumedPersistent. A mock that may
	// answer more than once keeps its first-use timestamp and its usage in
	// consumedPersistent, so only a count tells that it answered again.
	servedCount map[string]uint64
	// scopeConsumed names the per-test mocks served while a test of keploy
	// mock replay begun without a worker pid was open (SetScopeMocks), in any
	// way: consumed out of its per-test tier, or used where it stays. The
	// staging that ends such a test leaves them out, so a test's recording
	// answers no call after its test ended. Reset with consumedPersistent.
	scopeConsumed map[string]struct{}
	// answered keeps, of the per-test recordings that answered a call in this
	// test set, the request side, by request key and bounded by bytes, for a
	// miss's diagnosis (FindAnsweredEarlier).
	// Reset with consumedPersistent.
	answered answeredLog

	// cursorMu guards cursors: the per-request-key cursor used to serve
	// successive recorded responses for a stateful data-plane dependency
	// (ConsumeCursorSaturate). Session-scoped — reset whenever the mock set is
	// replaced (per test-set) so cursors don't bleed across test-sets.
	cursorMu sync.Mutex
	cursors  map[string]int
	// testCursors are, under exact serving, each test view's own cursors by
	// its number (cursorSlotIn): one is made as its view is installed, so a
	// test's sequences start empty. Once another view replaced it, it is
	// closed (closedTests) and kept: only a call read in that test reaches
	// it, and such a call walked after the test ended reads and moves the
	// test's sequence where the test left it. Closed ones are dropped at the
	// next staging (ResetStatefulCursors). Guarded by cursorMu.
	testCursors map[uint64]map[string]int
	// closedTests are the numbers of the test views another view replaced
	// (closeTestCursors), whose cursors the next staging drops. Guarded by
	// cursorMu.
	closedTests map[uint64]struct{}
	// kept are the per-test recordings an exact match kept for its test's
	// byte-identical repeats (integrations.RepeatKeeper), by key (a worker's
	// view prefixes its scope). Guarded by cursorMu, and dropped with the
	// per-test cursors at every staging.
	kept map[string]struct{}
	// usedUp are the recordings a worker's test consumed, held for the test
	// instead of deleted, so a run of the same test again is served them
	// (scopedMockDb.DeleteFilteredMock); by its scope and the name. Guarded by
	// cursorMu, and dropped with the per-test cursors at every staging.
	usedUp map[string]struct{}

	// spent are the per-test recordings of stateful dependencies a worker's
	// test kept for its repeats, by name, once that test is over (Spend):
	// Spent hides them from every reader but a later scope of the same test
	// (scopedMockDb.Spent). Reset with the replay session.
	spentMu sync.RWMutex
	spent   map[string]struct{}

	// vars holds the test variables this replay has bound (package
	// variables): reset as each test case of keploy test and each test of
	// keploy mock replay begins. Under the proven switch each test set
	// begins it anew (NewVariableSet), and what the set's start-up binds
	// before its first test is the run's, which each of its tests reads.
	// Read only under the proven switch.
	vars variables.Table
	// aheadVars holds what the next test's calls in flight bind, answered in
	// the gap before it (ServeExactPool's ahead, exact serving): the next
	// test's scope, begun early, apart from the ended test's (vars), which
	// every other call of the gap reads and binds in. It is carried into the
	// next test's scope as it begins (StartVariables).
	aheadVars variables.Table

	// exact is exact serving, under the proven switch (exact.go): when on,
	// it decides what every read shows and which consume succeeds.
	exact exactState

	// Optimized lookup maps
	statelessFiltered   map[models.Kind]map[string][]*models.Mock
	statelessUnfiltered map[models.Kind]map[string][]*models.Mock

	// windowMu guards windowStart/windowEnd. They bound the REQUEST-time
	// window for the outer HTTP/gRPC test currently being replayed; parsers
	// use them via GetFilteredMocksInWindow to reject non-config mocks whose
	// recorded REQUEST timestamp falls outside the window. Recorded
	// responses may legitimately extend past windowEnd (downstream async
	// completion is normal), so response containment is NOT enforced — only
	// mocks with response timestamps earlier than their request timestamps
	// (inconsistent recordings) are dropped as a sanity check.
	// Zero values mean "no window set", in which case GetFilteredMocksInWindow
	// falls back to GetFilteredMocks behaviour.
	windowMu    sync.RWMutex
	windowStart time.Time
	windowEnd   time.Time
	// scopeOpen is a test of keploy mock replay that its harness began
	// without a worker pid, while it runs (SetScopeMocks): a test window with
	// no recorded-time bounds, since such a test is served the mocks its
	// mapping names rather than the ones recorded in a span of time. It makes
	// the window active and the set's first test fired, and leaves
	// windowStart/windowEnd zero. Every window staging (SetMocksWithWindow,
	// SetMocksWithWindowThreeTier, SetScopeMocks) sets it, with the window,
	// after the trees: true only for SetScopeMocks opening a test;
	// SetCurrentTestWindow clears it with the window it sets; SetFilteredMocks
	// and SetUnFilteredMocks leave it. Guarded by windowMu; the stagings write
	// it under swapMu, so WindowSnapshot reads it with testFiredThisSet
	// tear-free.
	scopeOpen bool

	// firstWindowStart caches the earliest windowStart observed across
	// all SetMocksWithWindow calls for this manager's lifetime. It's
	// used by the strict pre-filter to distinguish STARTUP-INIT mocks
	// (req < firstWindowStart — app bootstrapped before the first test
	// fired, e.g. Hibernate/HikariCP pool init on Spring apps) from
	// STALE PREVIOUS-TEST mocks (firstWindowStart <= req < currentStart).
	// Wave 2: startup-init mocks are routed to the dedicated `startup`
	// tree (disjoint from session / perTest); legacy GetSessionMocks
	// surfaces them via a union shim so pre-wave-2 parsers stay working.
	// Previous-test mocks continue to be dropped to prevent cross-test
	// bleed.
	//
	// Zero until either the replayer seeds this set's cutoff (see
	// cutoffSeeded, installed by the boundary staging call) or the first
	// SetMocksWithWindow call with a non-zero start arrives — whichever comes
	// first, which in production is the seed. After that it is sticky within
	// the set (only goes earlier, never later), and the set boundary REPLACES
	// it with the next set's seeded value rather than clearing it: the clear
	// used to happen in ResetForReplaySession, then moved to staging, and
	// staging is also where the seed lands. Read under swapMu.RLock.
	firstWindowStart time.Time

	// testFiredThisSet is the TIER-ROUTING half of what firstWindowStart used
	// to answer alone. They are separate now because they are seeded at
	// different moments: SeedStartupCutoff sets the cutoff from the set's
	// earliest RECORDED test before any test has run, while routing must keep
	// reading "nothing has fired" until a real window actually arrives.
	// Deriving routing from a non-zero cutoff would make the seed itself look
	// like a fired test and send the set's bootstrap traffic to the per-test
	// engine over a tree staging just emptied. Written under swapMu.
	testFiredThisSet bool

	// boundaryPending marks that ResetForReplaySession has run and the next
	// set's mocks have not arrived yet. It is what lets the window bits be
	// cleared by staging instead of by the reset, without a stray BaseTime
	// call mid-set being able to deactivate a live test's window. Written
	// under swapMu.
	boundaryPending bool

	// setFence marks that the reset before the next set boundary started a
	// test set (ArmSetFence), not a re-registration of the current one: the
	// boundary staging starts a new set generation (setGen) and drops what the
	// previous set's pool was consumed of and nobody drained. Written under
	// swapMu.
	setFence bool

	// setGen is the test-set generation: every staging stamps it on the mocks
	// it pools (TestModeInfo.StagedSet), a fenced set boundary moves it, and
	// a consume of a mock pooled under an earlier one is refused
	// (flagMockAsUsed), so no consume of the previous set's pool, however it
	// races the boundary, reaches this set's drain. Starts at 1 (0 stamps
	// nothing). Moved under consumedMu.
	setGen atomic.Uint64

	// bootTaken holds the carry-over mocks (models.IsCarryOver) the app
	// consumed as it started: from a startup tier built before any test or
	// scope of the set fired (TreeDb.bootBuilt). Each is consumed once, and the
	// replay never re-arms one (its baseline keeps them, a retry cycle
	// included), but it drains such a consume only after the next staging,
	// which reloads the startup band and the carry range and is handed it
	// again: a staging leaves them out (withoutBootCarry), and every consume
	// door refuses another consume of one (takeBootOnce, bootConsumed), a
	// copy a staging filed before the consume landed included. Emptied at a
	// fenced set boundary; RestartExactKey forgets its names. Guarded by
	// bootTakenMu, a leaf.
	bootTakenMu sync.Mutex
	bootTaken   map[carryKey]struct{}

	// scopeFiredThisSet is testFiredThisSet for keploy mock replay's scopes:
	// set by a scope that opens a test, cleared at the set boundary. It
	// stays set, unlike scopeOpen, so a startup tier a staging builds between
	// scope tests is not the app's startup (TreeDb.bootBuilt). Written under
	// swapMu.
	scopeFiredThisSet bool

	// cutoffSeeded parks the startup-init cutoff the replayer supplies for the
	// NEXT set, until that set's staging call installs it.
	//
	// The seed cannot be written straight into firstWindowStart: staging clears
	// firstWindowStart at the boundary, and the replayer seeds BEFORE staging in
	// the same UpdateMockParams call (SeedStartupCutoff then
	// SetMocksWithWindow), so a directly-written seed was wiped microseconds
	// later — for every set. Parking it separately lets staging INSTALL the
	// value instead of clearing to zero, which is also what stops set N+1 from
	// inheriting set N's cutoff: sets are recorded in chronological order, so
	// the next set's cutoff is always LATER and a running-minimum guard against
	// the live value would refuse it. Written under swapMu.
	cutoffSeeded time.Time

	// recordedWindows is the current set's recorded test windows — every test
	// of the set, selected or not — as seeded by SeedRecordedWindows. nil when
	// the replayer sent none or nothing registered carry-over
	// (see SeedRecordedWindows). It changes at the same moments as the startup
	// cutoff: parked in windowsSeeded until the set's staging call installs it.
	// Stored under swapMu, read lock-free (RecordedWindows).
	recordedWindows atomic.Pointer[models.WindowSchedule]
	// testWindows is the current set's recorded test windows as seeded, kept
	// whether or not the release rule uses them: where a recorded mock falls
	// among them (InRecordedTestWindow). Like recordedWindows, a seed at a
	// set boundary is parked (testWindowsSeeded) until that set's staging
	// call installs it with the trees it describes, and a set (or a replay
	// session) that seeds none has none there, not the previous one's: a
	// keploy test session's windows would otherwise decide whose a later
	// keploy mock replay's recordings are, over its scope table.
	testWindows       atomic.Pointer[models.WindowSchedule]
	testWindowsSeeded *models.WindowSchedule
	windowsSeeded     *models.WindowSchedule

	// statedKinds are the kinds of which a mock staged since the current
	// set's boundary states its lifetime (models.Mock.StatesLifetime): the
	// set's recording serves each of their per-test mocks to its own test
	// only (StatesLifetimes). A set's first staging hands over its whole
	// recording; a later one only adds. Replaced, never mutated, under
	// swapMu.
	statedKinds atomic.Pointer[map[models.Kind]bool]

	// windowSig is closed, and replaced by a fresh channel, every time the
	// published test window changes (SetMocksWithWindow, SetCurrentTestWindow),
	// after the new state is visible. WindowChanged hands out the current one.
	// sigMu guards the swap and is a LEAF lock: nothing is acquired under it.
	sigMu     sync.Mutex
	windowSig chan struct{}

	// stagingEpoch changes at every staging call (a SetMocksWithWindow with the
	// models.BaseTime sentinel): a new set, or the same set staged again after
	// an agent replacement. State a consumer builds from one staging snapshot
	// is valid for one epoch. Written under swapMu, read lock-free.
	stagingEpoch atomic.Uint64

	// carry is the carry-over tier: carry-over mocks (models.IsCarryOver: a
	// RegisterCarryOver kind's, a StagingRelease need's) that stay reachable
	// outside their own window (carryover.go). Empty, and never touched, while
	// nothing is registered. Reset at every staging call
	// and when a pass goes back to an earlier window.
	carry *carryOverPool
	// carryLastStart is the window start of the last SetMocksWithWindow that
	// filed the carry-over tier, and carryLoadedTo the recorded time up to
	// which the agent has loaded registered mocks ahead of their window in this
	// pass (CarryOverLoadRange). Under swapMu.
	carryLastStart time.Time
	carryLoadedTo  time.Time

	// swapMu guards the {filtered, unfiltered, window} swap performed by
	// SetMocksWithWindow. Writers Lock(); readers via GetFilteredMocksInWindow
	// RLock() the snapshot read so they cannot observe a torn (newMocks,
	// oldWindow) view. Legacy GetFilteredMocks/GetUnFilteredMocks do NOT
	// take this lock — they trade snapshot atomicity for back-compat with
	// callers that don't care about the window. New windowed callers should
	// use GetFilteredMocksInWindow exclusively.
	swapMu sync.RWMutex

	// droppedOutOfWindow counts mocks that GetFilteredMocksInWindow filtered
	// out because their timestamps fell outside the current test window.
	// Reset to 0 by SetMocksWithWindow / SetCurrentTestWindow so each test
	// gets a fresh count usable for "did this test drop anything?" debugging.
	droppedOutOfWindow uint64

	// connMu guards the connection-scoped pool map. Phase 2.5 of the
	// mock-lifetime unification plan: prepared-statement setup mocks
	// (Postgres Parse, MySQL COM_STMT_PREPARE) live here, keyed by the
	// connID that owns them. Visibility is bounded by the connection's
	// lifecycle rather than the outer test window — executes on the
	// same connID can reference a setup across test boundaries without
	// leaking between unrelated connections.
	//
	// Lifecycle:
	//   - Entries are lazily materialised on AddConnectionMock.
	//   - Entries are torn down by DeleteConnection(connID) on EOF/Close
	//     from the parser, OR by the idle-retention sweeper when no
	//     activity has been observed on that connID for
	//     connectionIdleRetention.
	//
	// Lock ordering: swapMu → treesMu → windowMu → connMu. connMu is a
	// leaf in the existing invariant — never re-acquire any of the
	// outer locks from under it.
	connMu           sync.RWMutex
	connectionTrees  map[string]*TreeDb
	connectionLastTs map[string]time.Time

	// sweeperStop terminates the per-MockManager idle-sweeper
	// goroutine. Closed by Close() so long-running agents that spin
	// up a new MockManager per proxy session (pkg/agent/proxy/proxy.go
	// Record / Mock paths) don't leak a ticker goroutine every time.
	// closeOnce guards the close so concurrent Close() callers don't
	// double-close the channel (which would panic). `closed` is an
	// atomic flag read by connection-pool / hit-index mutation paths
	// so a matcher racing with Close() sees a no-op rather than a
	// spurious write to a torn-down tree. Reads are stale-tolerant
	// (a matcher that reads just before Close lands will still see
	// a valid snapshot); writes that arrive after Close are silently
	// dropped.
	sweeperStop chan struct{}
	closeOnce   sync.Once
	closed      atomic.Bool

	// restaged holds, by slot, what the parsers asked to run once the
	// manager lets go of the recordings its reads handed out
	// (integrations.Restaged): run and emptied at each move of every kind's
	// stored generation (bumpRevisionAll) and at Close.
	restagedMu sync.Mutex
	restaged   map[any]func()

	// noConnMocks is a negative cache for GetConnectionMocks's
	// fallback scan. A connID that returned zero connection-scoped
	// mocks on a full scan gets memoised here so subsequent matches
	// from the same connection don't pay the O(N) walk again.
	// Entries live until Close / explicit DeleteConnection (we also
	// drop them on AddConnectionMock so a later arrival invalidates).
	noConnMocks sync.Map // map[connID]struct{}

	// hitIdx counts matches by mock name for SessionMockHitCounts, so the
	// hot MarkMockAsUsed path bumps a counter in O(1). Each staging
	// re-indexes the names it stages (rebuildHitIndex), and a name keeps its
	// counter until a set boundary (the set's initial staging after
	// ResetForReplaySession; see boundaryPending) or a staging that leaves
	// it out. A name staged outside a staging call is seeded by
	// bumpHitCount's slow path. hitSeeds counts those seeds, so a rebuild can
	// tell whether one landed while it read the index.
	//
	// The counts live here, not on the mocks: matchers copy pooled mocks
	// whole while other connections count matches against them, so a count
	// kept on the mock raced every such copy.
	hitMu    sync.RWMutex
	hitIdx   map[string]*atomic.Uint64
	hitSeeds uint64 // written under hitMu.Lock
	// hitIndexBuilt, when set, runs between building a new hit index and
	// swapping it in. Tests only: it lets one land a seed in that gap.
	hitIndexBuilt func()
}

// defaultConnectionIdleRetention is the baseline for how long a
// connection-scoped pool survives without activity before the idle-
// sweeper reclaims it. Generous enough to tolerate HikariCP-style
// pooled connections that bridge test boundaries without activity;
// tight enough not to hold leaked recordings between test runs.
//
// Overridable via SetConnectionIdleRetention for long-running
// integration tests that sleep between requests.
const defaultConnectionIdleRetention = 5 * time.Minute

// connectionIdleRetentionNanos is the current per-process retention
// stored as nanoseconds in an atomic so the setter and the sweeper
// goroutine can race-freely read/write it. Initialised at package-
// load to the default. Callers adjust via SetConnectionIdleRetention;
// readers go through connectionIdleRetention().
var connectionIdleRetentionNanos atomic.Int64

func init() {
	connectionIdleRetentionNanos.Store(int64(defaultConnectionIdleRetention))
}

// connectionIdleRetention reads the current retention atomically.
// Used by the idle sweeper; hot path is a single atomic load.
func connectionIdleRetention() time.Duration {
	return time.Duration(connectionIdleRetentionNanos.Load())
}

// SetConnectionIdleRetention overrides the default 5-minute
// connection-pool retention. Intended for long-running integration
// tests where a connection may sit idle between requests for more
// than five minutes.
//
// Process-wide. Safe to call concurrently with any running sweeper —
// the store is atomic, and a sweep already in flight uses the value
// loaded at its start. Zero or negative reverts to the default.
func SetConnectionIdleRetention(d time.Duration) {
	if d <= 0 {
		d = defaultConnectionIdleRetention
	}
	connectionIdleRetentionNanos.Store(int64(d))
}

func NewMockManager(filtered, unfiltered *TreeDb, logger *zap.Logger) *MockManager {
	if filtered == nil {
		filtered = NewTreeDb(customComparator)
	}
	if unfiltered == nil {
		unfiltered = NewTreeDb(customComparator)
	}
	mm := &MockManager{
		filtered:            filtered,
		unfiltered:          unfiltered,
		startup:             NewTreeDb(customComparator),
		filteredByKind:      make(map[models.Kind]*TreeDb),
		unfilteredByKind:    make(map[models.Kind]*TreeDb),
		statelessFiltered:   make(map[models.Kind]map[string][]*models.Mock),
		statelessUnfiltered: make(map[models.Kind]map[string][]*models.Mock),
		revByKind:           make(map[models.Kind]*uint64),
		stagedByKind:        make(map[models.Kind]*uint64),
		consumedIndex:       make(map[string]int),
		consumedPersistent:  make(map[string]models.MockState),
		servedCount:         make(map[string]uint64),
		scopeConsumed:       make(map[string]struct{}),
		connectionTrees:     make(map[string]*TreeDb),
		connectionLastTs:    make(map[string]time.Time),
		sweeperStop:         make(chan struct{}),
		hitIdx:              make(map[string]*atomic.Uint64),
		windowSig:           make(chan struct{}),
		carry:               newCarryOverPool(),
		logger:              logger,
	}
	mm.setGen.Store(1)
	mm.answered.fence(1)
	// Start the per-connection idle sweeper. Reclaims
	// connection-scoped pools whose last activity exceeded
	// connectionIdleRetention so long-running agents don't accumulate
	// pools for closed connections the parser forgot to call
	// DeleteConnection on (net.Pipe-based tests, upstream EOF from a
	// Docker container restart, etc.). Fires on a slow ticker — the
	// sweep itself is a cheap map scan under connMu. Terminates when
	// Close() is invoked on the manager (closes sweeperStop).
	go mm.runIdleSweeper()
	return mm
}

// Close terminates background goroutines owned by this MockManager and
// marks it so connection-pool mutations become no-ops. Idempotent and
// safe under concurrent calls (sync.Once guards the channel close;
// atomic bool guards the mutation paths).
//
// Callers that rotate MockManager per session (pkg/agent/proxy/proxy.go
// Record / Mock paths) MUST call this on the outgoing manager before
// letting it go out of scope, or the idle-sweeper goroutine will leak
// for the process lifetime.
//
// Reads (GetSessionMocks / GetPerTestMocksInWindow / GetConnectionMocks)
// remain legal after Close — they just return whatever snapshot they
// can; concurrent access is memory-safe because the underlying trees
// are still live, they just don't accept new mutations.
func (m *MockManager) Close() {
	m.closeOnce.Do(func() {
		m.closed.Store(true)
		close(m.sweeperStop)
		// A learn step waiting for this manager's round to move
		// (AwaitRoundMove) is told it never will.
		m.closeRound()
		// Wake anything waiting on a window change so it can see IsClosed and
		// stop. The replacement channel stays open, so a waiter that does not
		// check IsClosed blocks again instead of spinning.
		m.signalWindowChange()
		// A parser's data built from this manager's reads goes with it.
		m.letDerivedGo()
	})
}

// IsClosed returns true once Close has been invoked. Exposed so
// higher-level callers (e.g. the agent lifecycle) can decide whether
// to invoke methods that would mutate the manager's state.
func (m *MockManager) IsClosed() bool {
	return m.closed.Load()
}

// ResetForReplaySession clears connection-scoped state and per-test
// counters so a MockManager that survives across test-set boundaries
// (Proxy.Mock reuse, see pkg/agent/proxy/proxy.go) doesn't leak the
// previous test-set's connection pools into the next one.
//
// What it clears:
//   - connectionTrees / connectionLastTs: per-connID dedicated trees
//     populated by AddConnectionMock from the previous test-set's
//     LifetimeConnection mocks. SetMocksWithWindow only seeds new
//     entries via AddConnectionMock — it never evicts prior ones, so
//     without an explicit reset a long-lived connection's per-connID
//     tree would accumulate test-set-N's mocks alongside test-set-
//     N+1's, and GetConnectionMocks (which returns the cached tree
//     without consulting the freshly swapped unfiltered tree) would
//     keep matching stale entries.
//   - noConnMocks: the negative cache paired with connectionTrees;
//     stale entries from the previous test-set would suppress fallback
//     scans for connIDs that now legitimately have connection-scoped
//     mocks.
//   - droppedOutOfWindow: per-test diagnostic counter that should
//     reset at the test-set boundary so "dropped for this test-set"
//     stays meaningful in metrics.
//     (The tier-routing window and the startup-init cutoff used to be cleared
//     here too. They are now cleared by the next set's initial staging call, so
//     that the window bits and the mock trees they describe change together
//     rather than leaving a gap where the bits describe a set whose mocks have
//     not been swapped in yet. See the note in the body.)
//
// What it preserves:
//   - filtered / unfiltered / startup trees: SetMocksWithWindow swaps
//     these atomically on the next call from the orchestrator, so
//     clearing them here would just briefly serve an empty pool to
//     any parser racing the reset.
//   - hitIdx: the match counts are not cleared here; like the window
//     bits, they start afresh at the next set's initial staging.
//   - sweeperStop / closed: the idle sweeper goroutine keeps running
//     across the reset; closing it would stop the per-connection
//     idle reaping for the rest of the replay.
//
// Safe to call concurrently with active matchers — connMu guards
// the maps and noConnMocks is a sync.Map. Matchers holding stale
// references to the cleared trees still observe valid (empty) trees
// rather than panicking.
func (m *MockManager) ResetForReplaySession() {
	m.spentMu.Lock()
	m.spent = nil
	m.spentMu.Unlock()
	m.connMu.Lock()
	m.connectionTrees = make(map[string]*TreeDb)
	m.connectionLastTs = make(map[string]time.Time)
	m.connMu.Unlock()
	// sync.Map has no Clear()/Reset() — Range+Delete is the idiomatic
	// reset. Concurrent readers see the same final state as if they
	// raced a fresh-Map swap; the negative-cache contract is
	// "absence means re-probe", which is exactly what an emptied map
	// produces.
	m.noConnMocks.Range(func(k, _ interface{}) bool {
		m.noConnMocks.Delete(k)
		return true
	})
	atomic.StoreUint64(&m.droppedOutOfWindow, 0)
	// Exact serving belongs to one replay session. One that served exactly
	// goes on serving, until the agent serves the next session, only the
	// protocol-phase mocks of what is staged: no data-plane mock of the
	// previous session answers an application kept alive across them.
	m.lockRound()
	m.exact.fired.Store(false)
	m.exact.last.Store(nil)
	m.exact.ended.Store(nil)
	m.exact.round.bound = nil
	m.exact.round.mu.Unlock()
	// Nor any of its worker scope (models.Mapping.Worker), or its recording
	// source: names are unique only within a set, and the next set names its
	// own.
	m.exact.worker.Store(nil)
	// And so do the rules its set's background is served by: the next
	// session's staging names its own (ServeExactBackground), and until it
	// does, today's serve.
	m.exact.bg.Store(nil)
	m.exact.poolCursor.Store(nil)
	m.SetRecordingSource(nil)
	m.SetRecordedBefore(nil)
	m.SetRecordedOfKind(nil)
	// Nor its scope table's pool names (SetPoolNames) and owners
	// (SetPoolOwners): until the next session sends its own, whose mock is
	// the pool's, and whose tests list each, is not known.
	m.exact.poolNames.Store(nil)
	m.exact.poolOwners.Store(nil)
	if m.exact.view.Load() != nil {
		m.installExact(&exactView{poolKey: models.PoolKeyNone, pool: map[string]struct{}{}})
	}

	// The never-drained per-name consumption history belongs to ONE test set.
	//
	// Mock names are only unique within a set: mockdb numbers them from a
	// per-store counter (`mock.Name = fmt.Sprint("mock-", ys.getNextID())`), and
	// each set is recorded by its own run, so every set has a mock-1, a mock-2,
	// and so on. filterOutDeleted keys purely on that name.
	//
	// Carried across a boundary, this map therefore drops the NEXT set's mock-1
	// because the PREVIOUS set's unrelated mock-1 was consumed — silently, as an
	// ordinary mock miss. It also STAMPS state: filterOutDeleted copies a
	// surviving entry's IsFiltered/SortOrder onto the mock it matched by name,
	// so a stale non-Deleted entry (MarkMockAsUsed writes Usage: Updated) would
	// put the previous set's ordering on this set's same-named mock. Clearing
	// has to be wholesale for that reason, not a sweep of Deleted entries.
	//
	// The client's map is re-allocated per test set (RunTestSet), which is why
	// the client-sent path does not degrade the same way. Not a perfect
	// contrast: that fresh map is immediately re-seeded from a GetConsumedMocks
	// drain, and consumedList is deliberately NOT cleared here, so when the app
	// survives a boundary the client could inherit a trailing few names too,
	// until the boundary was fenced (ArmSetFence). The difference was
	// magnitude, not kind — and this clear closes the agent-owned
	// path's set-boundary divergence specifically. It does not claim blanket
	// equivalence: --retry-passing-test rewinds the CLIENT's map to a baseline
	// per retry cycle and nothing rewinds this one, which is a separate defect
	// that must be closed before AgentOwnsConsumed is ever defaulted on.
	//
	// Unlike the window bits below, this genuinely belongs here rather than at
	// the next set's staging call: it is read by UpdateMockParams, which runs
	// BEFORE staging, so a late clear would still serve the previous set's
	// history to the first filter of the new one.
	//
	// This also runs MID-SET on the agent-replacement repair path, which calls
	// MockOutgoing again (pkg/service/replay). That is safe, but only because
	// the repair latches agentHistoryIncompleteForRun BEFORE that call, so every
	// later send carries the CLI's map and this (cleared) history is not
	// consulted — including on a false-positive repair, where the agent is
	// alive and its history was real. Move that latch after the MockOutgoing
	// and this clear becomes a live-history wipe; replay's own test pins the
	// ordering.
	//
	// A second, order-independent guarantee backs that up: consumedList and
	// consumedIndex are deliberately NOT cleared here, so the client's
	// post-bring-up GetConsumedMocks drain still recovers everything the app
	// consumed during bootstrap, and that is what the latched sends carry.
	// A reset that starts another test set says so (ArmSetFence), and its
	// boundary staging drops them instead: they were consumed of the previous
	// set's pool.
	m.answered.reset()
	m.consumedMu.Lock()
	if m.consumedPersistent != nil {
		m.consumedPersistent = make(map[string]models.MockState)
	}
	if m.servedCount != nil {
		m.servedCount = make(map[string]uint64)
	}
	if m.scopeConsumed != nil {
		m.scopeConsumed = make(map[string]struct{})
	}
	m.consumedMu.Unlock()

	m.swapMu.Lock()
	m.boundaryPending = true
	// A parked cutoff belongs to the set that seeded it and must never outlive
	// it. UpdateMockParams can abort between SeedStartupCutoff and
	// SetMocksWithWindow (the "no mocks stored for client ID" bail and the
	// loadPerTestMocks error path), leaving a park with no staging call to
	// consume it — and since sets are recorded chronologically the stale value
	// is always the EARLIER one, so it would win any running-minimum guard and
	// the next set would run on the aborted set's cutoff.
	m.cutoffSeeded = time.Time{}
	// Same for a parked window seed: it belongs to the set that sent it.
	m.windowsSeeded = nil
	m.testWindowsSeeded = nil
	m.swapMu.Unlock()

	// NOTE: the window bits and the startup-init cutoff are deliberately NOT
	// cleared here. They are cleared by the next set's initial staging call
	// instead — see SetMocksWithWindow's isInitialStaging branch.
	//
	// Clearing them here left the manager half torn down for the whole gap
	// between this reset and that staging call: the bits already said "nothing
	// has fired" while the trees still held the PREVIOUS set's mocks, so a
	// query arriving in the gap was routed to the startup engine and answered
	// out of the previous set's startup tier. That gap is real whenever the
	// application survives the test-set boundary (--keep-app-alive, compose
	// reuse). Clearing the trees here instead is not the answer either: that
	// serves an empty pool to a parser racing the reset, and a hard miss
	// against a live app is what crash-loops it.
	//
	// Deferring makes the two flip TOGETHER. Through the gap the manager is
	// consistently the previous set — previous bits, previous trees — and
	// staging then replaces both. The single caller (Proxy.Mock) always
	// follows this reset with a BaseTime staging call, so the deferral cannot
	// strand the bits.
}

// runIdleSweeper is the background loop that calls SweepIdleConnections
// every connectionSweepInterval. Terminates when sweeperStop is
// closed (by Close()).
func (m *MockManager) runIdleSweeper() {
	ticker := time.NewTicker(connectionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.sweeperStop:
			return
		case <-ticker.C:
			n := m.SweepIdleConnections()
			if n > 0 && m.logger != nil {
				m.logger.Debug("reclaimed idle connection-scoped mock pools",
					zap.Int("count", n),
					zap.Duration("idle_retention", connectionIdleRetention()))
			}
		}
	}
}

// connectionSweepInterval is how often runIdleSweeper walks the
// connection map. Cheap (map scan under connMu) so a minute is fine;
// longer intervals mean dead pools sit slightly longer before
// reclamation.
const connectionSweepInterval = 1 * time.Minute

func (m *MockManager) GetStatelessMocks(kind models.Kind, key string) (filtered, unfiltered []*models.Mock) {
	filtered, unfiltered = m.statelessMocks(kind, key)
	// Under exact serving a mock a parser evicted is gone, as from every
	// other read: until the next begin or end, or for a data-plane recording
	// consumed outside every test, until the next staging.
	if v := m.exactView(); v != nil {
		filtered, unfiltered = v.withLearned(v.unspent(filtered)), v.withLearned(v.unspent(unfiltered))
	}
	return filtered, unfiltered
}

func (m *MockManager) statelessMocks(kind models.Kind, key string) (filtered, unfiltered []*models.Mock) {
	if kind == models.DNS {
		key = strings.ToLower(dns.Fqdn(key))
	}
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()
	if km, ok := m.statelessFiltered[kind]; ok {
		if list := km[key]; len(list) > 0 {
			filtered = append([]*models.Mock(nil), list...)
		}
	}
	if km, ok := m.statelessUnfiltered[kind]; ok {
		if list := km[key]; len(list) > 0 {
			unfiltered = append([]*models.Mock(nil), list...)
		}
	}
	return
}

// ---------- revision helpers ----------

func (m *MockManager) Revision() uint64 {
	return atomic.LoadUint64(&m.rev)
}

func (m *MockManager) bumpRevisionAll() {
	atomic.AddUint64(&m.stored, 1)
	m.bumpViewAll()
}

// bumpViewAll is bumpRevisionAll for an exact view's install: the stored
// generation does not move, since what the trees hold did not change.
func (m *MockManager) bumpViewAll() {
	atomic.AddUint64(&m.staged, 1)
	atomic.AddUint64(&m.rev, 1)
}

func (m *MockManager) RevisionByKind(kind models.Kind) uint64 {
	m.revMu.RLock()
	ptr := m.revByKind[kind]
	m.revMu.RUnlock()
	if ptr == nil {
		return 0
	}
	return atomic.LoadUint64(ptr)
}

func (m *MockManager) bumpRevisionKind(kind models.Kind) {
	_, _, stored := m.kindRevisions(kind)
	atomic.AddUint64(stored, 1)
	m.bumpViewKind(kind)
}

// bumpViewKind is bumpRevisionKind for an exact view's install (bumpViewAll).
func (m *MockManager) bumpViewKind(kind models.Kind) {
	rev, staged, _ := m.kindRevisions(kind)
	atomic.AddUint64(staged, 1)
	atomic.AddUint64(rev, 1)
}

// kindRevisions returns kind's revision, staging generation and stored
// generation counters, made on first use. The pointers stay valid after the
// lock is let go: the counters move by atomics.
func (m *MockManager) kindRevisions(kind models.Kind) (rev, staged, stored *uint64) {
	m.revMu.Lock()
	defer m.revMu.Unlock()
	if m.revByKind == nil {
		m.revByKind = make(map[models.Kind]*uint64)
	}
	if m.stagedByKind == nil {
		m.stagedByKind = make(map[models.Kind]*uint64)
	}
	if m.storedByKind == nil {
		m.storedByKind = make(map[models.Kind]*uint64)
	}
	rev, staged, stored = m.revByKind[kind], m.stagedByKind[kind], m.storedByKind[kind]
	if rev == nil {
		rev = new(uint64)
		m.revByKind[kind] = rev
	}
	if staged == nil {
		staged = new(uint64)
		m.stagedByKind[kind] = staged
	}
	if stored == nil {
		stored = new(uint64)
		m.storedByKind[kind] = stored
	}
	return rev, staged, stored
}

// bumpConsumed moves the revisions for a consume or a spend under exact
// serving of a mock of kind: Revision and RevisionByKind move, so a parser
// that keys what it read on them reads again, as before; the staging
// generations do not, since what every read gives changes then only by the
// mock MockSpent now reports.
func (m *MockManager) bumpConsumed(kind models.Kind) {
	rev, _, _ := m.kindRevisions(kind)
	atomic.AddUint64(rev, 1)
	atomic.AddUint64(&m.rev, 1)
}

// StagingGeneration is Revision less the moves a consume under exact serving
// makes. It moves when the staged set changes (a staging, a store into a
// tree, a delete; outside exact serving a consume deletes the mock from its
// tree, so it moves then too) and when exact serving installs a view (a test
// opens or ends, a key restarts), never when a parser consumes or spends a
// mock the view in force shows. So what a read gives (GetPerTestMocksInWindow,
// GetSessionMocks, GetUnFilteredMocks, GetStartupMocks, ...) while one
// generation holds is what it gave when the generation began, less the mocks
// MockSpent reports: a parser can build its index once per generation and
// skip at lookup what MockSpent reports, rather than build it again on each
// consume as Revision has it do (integrations.StagingGenerations).
func (m *MockManager) StagingGeneration() uint64 {
	return atomic.LoadUint64(&m.staged)
}

// StagingGenerationByKind is StagingGeneration for the reads of kind's mocks:
// RevisionByKind less the moves a consume under exact serving makes.
func (m *MockManager) StagingGenerationByKind(kind models.Kind) uint64 {
	m.revMu.RLock()
	ptr := m.stagedByKind[kind]
	m.revMu.RUnlock()
	if ptr == nil {
		return 0
	}
	return atomic.LoadUint64(ptr)
}

// MockSpent reports whether the staged mock named name has answered in a way
// that drops it from the reads while the view in force holds: under exact
// serving, an open test's own recording (or a pool view's ahead) consumed, or
// another test took (a shared recording), a staged mock spent through a
// delete door in the view, a data-plane recording consumed outside every test
// in the staging. Those are the only changes a consume or a spend makes to
// what the reads give, and none of them moves StagingGeneration. False with
// no view in force: outside exact serving a consume deletes the mock from its
// tree, which moves the staging generations.
func (m *MockManager) MockSpent(name string) bool {
	v := m.exactView()
	if v == nil {
		return false
	}
	return v.spentNow(name)
}

// ensureKindTrees returns per-kind trees, creating them if missing.
// It is safe for concurrent use.
func (m *MockManager) ensureKindTrees(kind models.Kind) (f *TreeDb, u *TreeDb) {
	// Fast path: read lock
	m.treesMu.RLock()
	f = m.filteredByKind[kind]
	u = m.unfilteredByKind[kind]
	m.treesMu.RUnlock()
	if f != nil && u != nil {
		return f, u
	}

	// Slow path: upgrade to write lock and double-check
	m.treesMu.Lock()
	if f = m.filteredByKind[kind]; f == nil {
		f = NewTreeDb(customComparator)
		m.filteredByKind[kind] = f
	}
	if u = m.unfilteredByKind[kind]; u == nil {
		u = NewTreeDb(customComparator)
		m.unfilteredByKind[kind] = u
	}
	m.treesMu.Unlock()
	return f, u
}

// ---------- getters ----------

func (m *MockManager) GetFilteredMocks() ([]*models.Mock, error) {
	m.treesMu.RLock()
	tree := m.filtered
	m.treesMu.RUnlock()
	if v := m.exactView(); v != nil {
		return m.exactPerTest(v, "", v.shownOf(tree, "")), nil
	}
	results := make([]*models.Mock, 0, 64)
	tree.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

// SetCurrentTestWindow records the outer test's req/res timestamps so that
// GetFilteredMocksInWindow can enforce Option-1 strict containment. Pass
// zero values to clear the window (e.g., between test sets).
//
// Also resets the per-test droppedOutOfWindow counter so a new test starts
// with a fresh "this test dropped N" tally.
//
// Locking: windowMu serializes the window write, atomic.StoreUint64 is
// lock-free for the counter reset. No swapMu is needed here — window
// consistency with the mock pool is only promised by SetMocksWithWindow.
// A reader using GetFilteredMocksInWindow around a concurrent
// SetCurrentTestWindow may see the new window against the old pool, but
// that is a per-field update, not a pool replacement.
func (m *MockManager) SetCurrentTestWindow(start, end time.Time) {
	m.windowMu.Lock()
	m.windowStart = start
	m.windowEnd = end
	m.scopeOpen = false
	m.windowMu.Unlock()
	atomic.StoreUint64(&m.droppedOutOfWindow, 0)
	m.signalWindowChange()
}

// InTestWindow reports whether a mock recorded at t was recorded inside the
// window of the test the replay runs now (integrations.TestWindows).
func (m *MockManager) InTestWindow(t time.Time) bool {
	m.windowMu.RLock()
	defer m.windowMu.RUnlock()
	if m.windowStart.IsZero() || m.windowEnd.IsZero() || t.IsZero() {
		return false
	}
	return !t.Before(m.windowStart) && !t.After(m.windowEnd)
}

// InFlightFrom is, for the test window that starts at start, the exclusive
// start of the gap before it (models.WindowSchedule.GapBefore): the agent
// loads, beside the window's own mocks, those sent in that gap and answered at
// or after start, which were in flight when the test began. ok is false when
// the replay seeded no windows, for the first test, or with no gap.
func (m *MockManager) InFlightFrom(start time.Time) (from time.Time, ok bool) {
	return m.testWindows.Load().GapBefore(start)
}

// InRecordedTestWindow reports whether a mock recorded at t was recorded
// inside the window of some recorded test of the set (SeedRecordedWindows);
// known is false when the replay seeded none (integrations.TestWindows).
func (m *MockManager) InRecordedTestWindow(t time.Time) (inside, known bool) {
	s := m.testWindows.Load()
	if s.Len() == 0 || t.IsZero() {
		return false, false
	}
	return s.Contains(t), true
}

// RecordedWindowIndex is the index of the last recorded test window
// (SeedRecordedWindows) that starts at or before t, -1 before the first or
// for a zero t; known is false when the replay seeded none
// (integrations.RecordedWindowIndex).
func (m *MockManager) RecordedWindowIndex(t time.Time) (index int, known bool) {
	s := m.testWindows.Load()
	if s.Len() == 0 {
		return -1, false
	}
	return s.CurrentIndex(t), true
}

// SameRecordedTestWindow reports whether a and b were recorded inside one
// recorded test's window; known is false when the replay seeded none.
func (m *MockManager) SameRecordedTestWindow(a, b time.Time) (same, known bool) {
	s := m.testWindows.Load()
	if s.Len() == 0 || a.IsZero() || b.IsZero() {
		return false, false
	}
	i := s.IndexOf(a)
	return i >= 0 && i == s.IndexOf(b), true
}

// IsTestWindowActive reports whether a non-zero test window is currently
// set on this MockManager (via SetCurrentTestWindow or SetMocksWithWindow).
// Parsers that split mocks into per-test and session tiers use this as
// the authoritative signal for which tier a live query should be routed
// to: a true result means the runner is inside a test-body window, a
// false result means session/connection-scoped traffic (startup, idle
// gap between tests, post-last-test teardown).
//
// Concurrency: acquires windowMu for read only. A racy observation is
// tolerable — callers that need strict window/pool atomicity go
// through GetPerTestMocksInWindow (which snapshots both under swapMu).
//
// A test of keploy mock replay begun without a worker pid (SetScopeMocks) is
// an active window too, one with no recorded-time bounds.
func (m *MockManager) IsTestWindowActive() bool {
	if v := m.exactView(); v != nil {
		return v.test != ""
	}
	m.windowMu.RLock()
	defer m.windowMu.RUnlock()
	return m.windowActiveLocked()
}

// windowActiveLocked is IsTestWindowActive with windowMu held.
func (m *MockManager) windowActiveLocked() bool {
	return m.scopeOpen || (!m.windowStart.IsZero() && !m.windowEnd.IsZero())
}

// GetFilteredMocksInWindow returns non-config filtered mocks placed inside the
// current test window (models.PlaceOf): their recorded REQUEST timestamp, or
// their answer's for a mock placed at its answer (a consumer's long poll).
// Other response timestamps may legitimately straggle outside the window
// (downstream async completion) and are not used for window containment —
// only mocks whose response timestamp is EARLIER than their request
// timestamp are dropped as a sanity check on inconsistent recordings.
//
// Also kept: a per-test mock in flight when the window's test began
// (models.InFlightEligible), sent in the gap before the window
// (models.WindowSchedule.GapBefore) and answered at or after its start. The
// staging filed it in this window's tier (setMocksWithWindowLocked), and it is
// this test's.
//
// Consumers: this method is a PUBLIC extension point on the MockMemDb
// interface consumed by OUT-OF-REPO protocol parsers (keploy/integrations,
// keploy/enterprise). Those parsers call it when strict-window matching is
// enabled to reject cross-test mock bleed at read time. There are no
// in-repo callers by design: the keploy-core agent only plumbs the window
// via SetMocksWithWindow/SetCurrentTestWindow; the parser that owns the
// replayed connection does the read-time filtering. Grepping only this
// repo will show it unused; that does not mean it is dead code.
//
// If no window is set (zero values), behaves exactly like GetFilteredMocks.
//
// Mocks lacking either timestamp are kept (defensive: legacy/incomplete
// recordings should not silently disappear; the agent-level pre-filter
// already routes them to the filtered tree).
//
// Atomicity: takes swapMu.RLock() while sampling both the mock pool and
// the window so a concurrent SetMocksWithWindow cannot expose a torn
// (newMocks, oldWindow) view. The mock-list iteration runs OUTSIDE the
// lock — we own a slice snapshot at that point.
//
// Logging: emits ONE summary debug log per call (count of dropped + count
// of timestamp-missing mocks) rather than one log per mock, to avoid
// flooding the hot path. Set zap level to Debug to see the summary.
//
// Drop count is exposed via DroppedOutOfWindow() for observability.
func (m *MockManager) GetFilteredMocksInWindow() ([]*models.Mock, error) {
	// Atomicity contract: snapshot the per-test tree POINTER and the
	// window together under swapMu.RLock so SetMocksWithWindow can't
	// interleave a (tree, window) swap between our two reads. Once
	// both snapshots are in hand, release swapMu and let tree
	// iteration run unlocked — otherwise a long walk over a large
	// pool would block SetMocksWithWindow writers for its duration.
	m.swapMu.RLock()
	m.treesMu.RLock()
	tree := m.filtered
	m.treesMu.RUnlock()
	m.windowMu.RLock()
	start, end := m.windowStart, m.windowEnd
	m.windowMu.RUnlock()
	m.swapMu.RUnlock()

	// Under exact serving the test's mapping decides, not a recorded time,
	// over what the view shows of the tier (shownOf).
	if v := m.exactView(); v != nil {
		return m.exactPerTest(v, "", v.shownOf(tree, "")), nil
	}
	all := make([]*models.Mock, 0, 64)
	tree.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			all = append(all, mock)
		}
		return true
	})
	if start.IsZero() || end.IsZero() {
		return all, nil
	}
	out := make([]*models.Mock, 0, len(all))
	var droppedOutOfWindow, droppedInvalidOrder, missingTs uint64
	gapFrom, hasGap := m.testWindows.Load().GapBefore(start)
	for _, mock := range all {
		if mock == nil {
			continue
		}
		req := mock.Spec.ReqTimestampMock
		res := mock.Spec.ResTimestampMock
		if req.IsZero() || res.IsZero() {
			missingTs++
			out = append(out, mock)
			continue
		}
		// Defensive sanity check: a mock whose response-timestamp is
		// BEFORE its request-timestamp is inconsistent (clock skew,
		// serialisation bug, or corrupted recording). Drop it — keeping
		// it would just confuse downstream scoring. Counted separately
		// from the window-containment drops so the per-test counter
		// and debug log can distinguish the two root causes, and said of
		// once for each such mock; never under the proven switch
		// (pkg.LeaveOutAnsweredBeforeAsked).
		if pkg.LeaveOutAnsweredBeforeAsked(m.logger, mock) {
			droppedInvalidOrder++
			continue
		}
		// Containment by place (models.PlaceOf): the request timestamp,
		// or the answer's for a mock placed at its answer. Responses may
		// straggle past the outer test's response timestamp (async
		// completion on downstream calls); that's not a bleed signal. The
		// bleed bug this window protects against manifests as REQUESTS
		// landing outside the window — those are always from a different
		// test.
		switch place := models.PlaceOf(mock); {
		case !place.Before(start) && !place.After(end):
			out = append(out, mock)
		case inFlightAtStart(mock, req, res, gapFrom, start, hasGap):
			// In flight when the window's test began: its own.
			out = append(out, mock)
		default:
			droppedOutOfWindow++
		}
	}
	if droppedOutOfWindow > 0 {
		atomic.AddUint64(&m.droppedOutOfWindow, droppedOutOfWindow)
	}
	if (droppedOutOfWindow > 0 || droppedInvalidOrder > 0 || missingTs > 0) && m.logger != nil {
		m.logger.Debug("window filter summary",
			zap.Uint64("dropped_out_of_window", droppedOutOfWindow),
			zap.Uint64("dropped_invalid_order", droppedInvalidOrder),
			zap.Uint64("kept_missing_timestamps", missingTs),
			zap.Int("kept_total", len(out)),
			zap.Time("windowStart", start),
			zap.Time("windowEnd", end))
	}
	return out, nil
}

// DroppedOutOfWindow returns the number of mocks that
// GetFilteredMocksInWindow has filtered out since the most recent
// SetMocksWithWindow / SetCurrentTestWindow call — i.e., for the
// CURRENT test. The counter resets at every test boundary.
//
// Useful for surfacing as a metric or in debug dumps when users ask
// "where did my mock go?".
func (m *MockManager) DroppedOutOfWindow() uint64 {
	return atomic.LoadUint64(&m.droppedOutOfWindow)
}

// SetMocksWithWindow atomically replaces the filtered/unfiltered mock
// trees AND updates the active test window. Readers using
// GetFilteredMocksInWindow (which RLocks swapMu) cannot observe a torn
// (newMocks, oldWindow) view; legacy GetFilteredMocks/GetUnFilteredMocks
// callers that don't take swapMu can still see partial swaps but they
// don't depend on window consistency anyway.
//
// Phase 3 specialization: when both start and end are non-zero, the
// per-test (filtered) slice is pre-filtered by request-timestamp
// containment BEFORE the tree is built. The net effect is that the
// tree stored in m.filtered only holds mocks the matcher could
// actually match for THIS test — match time drops from O(log N)
// across the whole session to O(log M) over the per-test slice, and
// the old match-loop filter-and-skip dance inside
// GetFilteredMocksInWindow becomes a no-op fast path. Mocks with a
// missing timestamp are kept defensively (legacy recordings); mocks
// whose response is earlier than request are dropped as a sanity
// check and counted into the per-test drop counter.
//
// Connection-scoped mocks landing in `unfiltered` are additionally
// seeded into their per-connID dedicated trees via AddConnectionMock
// so subsequent GetConnectionMocks lookups take the O(1) path.
//
// Also resets the per-test droppedOutOfWindow counter, and closes the channel
// WindowChanged handed out once the new trees and window are visible.
func (m *MockManager) SetMocksWithWindow(filtered, unfiltered []*models.Mock, start, end time.Time) {
	m.keepRunOnlyForAnExactStartUp(start)
	m.setMocksWithWindow(filtered, unfiltered, start, end)
}

// keepRunOnlyForAnExactStartUp, under the proven switch, decides at a
// staging from models.BaseTime whether the set's start-up binds the run's.
// Without exact serving such a staging arms every test's recordings for the
// start-up (setMocksWithWindowLocked): a start-up call one of them answers
// binds a value of that test's, which cannot be told from the run's, and
// kept as the run's, every later test of the set would read it. The set
// keeps no run's then (variables.Table.KeepNoRun): what its start-up binds
// is the first test's to forget, as before the run's were kept, and the set's
// Mock coming again (NewVariableSet, a re-registration) keeps none either,
// while those trees are staged. Exact serving serves the start-up the set's
// pool alone, what no test owns (ServeExactPool): its staging lets the next
// set keep the run's again (AllowRun). It never turns them on itself: only a
// new set does, whose gen makes every call checked before it stale. Called
// outside the variables' lock (OpenTestWithWindow), which a staging takes
// after it.
func (m *MockManager) keepRunOnlyForAnExactStartUp(start time.Time) {
	if !provenswitch.On() || !start.Equal(models.BaseTime) {
		return
	}
	if m.exactView() == nil {
		m.vars.KeepNoRun()
		return
	}
	m.vars.AllowRun()
}

// setMocksWithWindow is SetMocksWithWindow but for the run's (KeepNoRun).
func (m *MockManager) setMocksWithWindow(filtered, unfiltered []*models.Mock, start, end time.Time) {
	// Signal once the new trees and window are visible: the deferred calls
	// run last in, first out, so swapMu is released first.
	defer m.signalWindowChange()
	m.swapMu.Lock()
	defer m.swapMu.Unlock()
	ownForStaging(&filtered, &unfiltered)
	m.setMocksWithWindowLocked(filtered, unfiltered, nil, start, end, false)
}

// SetScopeMocks stages the pool of keploy mock replay, with the proven switch
// off, for a test its harness began (open) or ended (!open) without a worker
// pid. Such a test is served the mocks its mapping names, not the ones
// recorded in a span of time, so it has no recorded-time window to stage.
//
// open: filtered is the test's whole per-test pool and goes into the per-test
// tier as it is, with no window containment and no startup-init routing;
// unfiltered is the session tier; the startup tier is emptied. The test window
// is open (IsTestWindowActive, and HasFirstTestFired with it) until the next
// staging, with no time bounds: CurrentTestWindow stays zero, so nothing that
// reads time bounds (GetFilteredMocksInWindow, carry-over, matchers comparing
// recorded times) changes. Tier-aware parsers route the test's calls to the
// per-test tier and report a miss as one inside a test.
//
// !open: the pool between tests, staged exactly as a models.BaseTime staging
// stages it (everything in the startup tier), which closes the window, less
// every per-test mock served while a test was open (scopeConsumed): a test's
// recording answers no call after its test ended. Between tests is routed as
// before any test, as it was before tests opened windows, and what is served
// there is served from the startup tier, re-armed by each staging as always.
// A begin stages a test's own mocks afresh, so a test run again is served its
// whole recording.
//
// Like a models.BaseTime staging, each call starts a new staging epoch. A
// reader that takes swapMu (WindowSnapshot, GetPerTestMocksInWindow) sees the
// trees and the window of one staging; the others see each change as it
// lands, the trees before the window, as with any staging.
func (m *MockManager) SetScopeMocks(filtered, unfiltered []*models.Mock, open bool) {
	defer m.signalWindowChange()
	m.swapMu.Lock()
	defer m.swapMu.Unlock()
	if !open {
		served := m.scopeServed()
		filtered, unfiltered = withoutScopeServed(filtered, served), withoutScopeServed(unfiltered, served)
		ownForStaging(&filtered, &unfiltered)
		m.setMocksWithWindowLocked(filtered, unfiltered, nil, models.BaseTime, time.Now(), false)
		return
	}
	ownForStaging(&filtered, &unfiltered)
	// The epoch and carry-over bookkeeping a BaseTime staging resets, which
	// the zero-window staging below leaves alone.
	m.bumpStagingEpoch()
	m.carry.reset()
	m.carryLastStart, m.carryLoadedTo = time.Time{}, time.Time{}
	// A zero window is no time window: the per-test tier takes filtered as
	// it is, the startup tier is rebuilt empty, and the published window
	// stays zero, published with the open scope.
	m.setMocksWithWindowLocked(filtered, unfiltered, nil, time.Time{}, time.Time{}, true)
}

// scopeIsOpen reports whether a test begun without a worker pid is open
// (SetScopeMocks).
func (m *MockManager) scopeIsOpen() bool {
	m.windowMu.RLock()
	defer m.windowMu.RUnlock()
	return m.scopeOpen
}

// scopeServed is a copy of scopeConsumed: the per-test mocks served while a
// test begun without a worker pid was open.
func (m *MockManager) scopeServed() map[string]struct{} {
	m.consumedMu.Lock()
	defer m.consumedMu.Unlock()
	out := make(map[string]struct{}, len(m.scopeConsumed))
	for name := range m.scopeConsumed {
		out[name] = struct{}{}
	}
	return out
}

// withoutScopeServed is mocks less the per-test mocks in served.
func withoutScopeServed(mocks []*models.Mock, served map[string]struct{}) []*models.Mock {
	if len(served) == 0 {
		return mocks
	}
	out := make([]*models.Mock, 0, len(mocks))
	for _, mk := range mocks {
		if mk != nil && mk.TestModeInfo.Lifetime == models.LifetimePerTest {
			if _, ok := served[mk.Name]; ok {
				continue
			}
		}
		out = append(out, mk)
	}
	return out
}

// mockOwner settles which objects a staging call may stamp.
//
// Staging stamps each mock's tree ID and, when it has none, its sort order,
// into the object it puts in a pool. A mock that is already pooled may be in a
// matcher's hands (updateMock copies every pooled mock it serves), so it is
// never written again: one a caller hands back (a mock it got from the pools)
// is replaced by a copy (Mock.ShallowCopy), one copy per object for the whole
// call, so a mock listed in two tiers of one staging call stays one object in
// the pools, which GetSessionMocks dedups by pointer. (Proxy.SetMocks stages
// its two tiers in two calls, so a mock it lists in both is copied by the
// second.) A mock no pool has held yet (the agent stages fresh copies every
// time) is taken over as it is.
//
// Mock.Pooled tells the two apart. ownForStaging marks every mock a staging
// takes, once ownership of all its slices is settled, and staging stamps every
// mock of a tier before that tier is published. Ownership is settled under
// swapMu, which every staging holds while it stamps, so two stagings handed
// the same fresh mock cannot both take it over.
type mockOwner map[*models.Mock]*models.Mock

// ownForStaging settles ownership of a staging's input slices, in place, then
// marks each mock the staging takes pooled. Marking comes after every slice is
// owned, so a fresh mock listed in two slices is taken over once rather than
// copied by the second. It is a pass of its own, rather than marking in
// buildTier, in both startup-tier loops and in the carry-over filing, so that
// one place covers every mock the staging takes.
func ownForStaging(slices ...*[]*models.Mock) {
	o := mockOwner{}
	for _, s := range slices {
		*s = o.own(*s)
	}
	for _, s := range slices {
		for _, mk := range *s {
			if mk != nil {
				mk.MarkPooled()
			}
		}
	}
}

// own returns in with every pooled mock replaced by its copy. It returns in
// itself when no mock is pooled, as when the agent stages fresh copies.
func (o mockOwner) own(in []*models.Mock) []*models.Mock {
	var out []*models.Mock
	for i, mk := range in {
		if mk == nil || !mk.Pooled() {
			continue
		}
		if out == nil {
			out = append([]*models.Mock(nil), in...)
		}
		c, ok := o[mk]
		if !ok {
			c = mk.ShallowCopy()
			o[mk] = c
		}
		out[i] = c
	}
	if out == nil {
		return in
	}
	return out
}

// setMocksWithWindowLocked is SetMocksWithWindow without the window-change
// signal. The caller holds swapMu and owns the mocks (mockOwner).
//
// explicitStartup is SetMocksWithWindowThreeTier's startup slice: it joins
// the startup tier this call builds, and is stamped with the rest of the
// staging, before any tier is published.
//
// scope opens a test with no time bounds (SetScopeMocks); it is published
// with the window, and every other staging closes one.
func (m *MockManager) setMocksWithWindowLocked(filtered, unfiltered, explicitStartup []*models.Mock, start, end time.Time, scope bool) {
	// Every staging starts the per-test stateful cursors over: a test's own
	// recordings of a request advance their cursor within that test alone.
	m.resetPerTestCursors()
	// Track the earliest window-start we've ever seen on this manager.
	// Used below to distinguish startup-init mocks (recorded before
	// any test fired) from stale previous-test mocks. Once set, only
	// moves earlier — a later test's start can never push it forward
	// because that would re-categorise genuine startup mocks as stale.
	//
	// Skip models.BaseTime (2000-01-01) — Runner/Replayer fire one
	// initial SetMocksWithWindow with [BaseTime, Now] to stage mocks
	// before any test runs; that sentinel isn't a real test start
	// and caching it would defeat the "recorded before test 1"
	// comparison below (everything is trivially after 2000).
	if !start.IsZero() && !start.Equal(models.BaseTime) {
		if m.firstWindowStart.IsZero() || start.Before(m.firstWindowStart) {
			m.firstWindowStart = start
		}
		m.testFiredThisSet = true
	}
	firstStart := m.firstWindowStart
	// Wave 2: startup-init mocks (req < firstStart) are routed into a
	// dedicated `startup` tree instead of being promoted into the
	// session pool with a Lifetime mutation. The recorder's tag is
	// preserved intact — strict-accessor callers (v3 parser) see
	// startup/session/perTest as disjoint tiers. The legacy union shim
	// GetSessionMocks returns startup+session so pre-wave-2 parsers
	// don't notice the split.
	var startupInit []*models.Mock

	// Runner/Replayer's initial SetMocksWithWindow call fires with
	// start=models.BaseTime before any test runs — to stage mocks for
	// the app-startup window (Hibernate init, HikariCP pool warm-up,
	// JDBC driver handshake) that fires AFTER the app launches but
	// BEFORE the first test request. Wave-3 C1/C3 fix: every per-test
	// mock seen DURING this staging window is, by definition, bootstrap
	// traffic — no real test has fired yet, so there is no "per-test"
	// tier to populate. Route the whole `filtered` slice straight into
	// the startup tree. The next SetMocksWithWindow call — the first
	// real test — re-partitions: mocks with req ∈ [start, end] move
	// to filtered; mocks with req < firstWindowStart stay in startup;
	// mocks with firstWindowStart <= req < start are stale previous-
	// test bleed and get dropped.
	isInitialStaging := start.Equal(models.BaseTime)
	// A set boundary: the first staging after ResetForReplaySession. A
	// BaseTime staging without one is a mid-set stage (see boundaryPending).
	setBoundary := isInitialStaging && m.boundaryPending
	// A boundary staging that starts another test set (ArmSetFence): it
	// starts a new set generation as it publishes the new trees, and drops
	// what the previous set's pool was consumed of (dropPreviousSetConsumes).
	fenced := setBoundary && m.setFence
	if fenced {
		m.setFence = false
	}
	if scope {
		m.scopeFiredThisSet = true
	}
	if !fenced {
		// A carry-over mock the app consumed as it started is not staged
		// again, whatever this staging was handed (bootTaken). The previous
		// set's are not this one's.
		filtered = m.withoutBootCarry(filtered)
	}
	// Every mock this staging pools is of the set generation current once
	// it is published.
	gen := m.setGen.Load()
	if fenced {
		gen++
	}
	for _, list := range [][]*models.Mock{filtered, unfiltered, explicitStartup} {
		for _, mk := range list {
			if mk != nil {
				mk.TestModeInfo.StagedSet = gen
			}
		}
	}
	if isInitialStaging {
		// Every staging call starts a new epoch, boundary or not: whatever a
		// consumer built from the previous staging snapshot is stale now.
		m.bumpStagingEpoch()
		// The carry-over tier and its lookahead bookkeeping belong to that
		// snapshot too.
		m.carry.reset()
		m.carryLastStart, m.carryLoadedTo = time.Time{}, time.Time{}
		// Staging is the set boundary as far as routing is concerned: this is
		// where the previous set's window bits and startup-init cutoff go, so
		// they are replaced in the same call that replaces the trees rather
		// than in ResetForReplaySession (see the note there). Until this
		// point the manager still describes the previous set, consistently.
		// Only when a set boundary is actually pending. A BaseTime call that
		// is NOT preceded by a reset is a mid-set stage, and deactivating a
		// live test's window there would mis-route it to the startup engine.
		if m.boundaryPending {
			m.boundaryPending = false
			m.testFiredThisSet = false
			m.scopeFiredThisSet = false
			// Install this set's seeded cutoff. Zero when nothing was seeded,
			// which is exactly the historical clear.
			m.firstWindowStart = m.cutoffSeeded
			m.cutoffSeeded = time.Time{}
			// Install this set's recorded windows, or none: a set the replayer
			// did not seed must not inherit the previous set's.
			m.recordedWindows.Store(m.windowsSeeded)
			m.windowsSeeded = nil
			// The learn step's too (InRecordedTestWindow): a set, or a replay
			// session, that seeded none places no recording by the last one's.
			m.testWindows.Store(m.testWindowsSeeded)
			m.testWindowsSeeded = nil
			m.windowMu.Lock()
			m.windowStart = time.Time{}
			m.windowEnd = time.Time{}
			m.windowMu.Unlock()
			// The previous set's recording is not this one's.
			m.statedKinds.Store(nil)
		}
		// Wave-3 H3 fix: include BOTH filtered (per-test) and unfiltered
		// (session + connection) mocks in the startup pool during initial
		// staging. Rationale: during Runner/Replayer's pre-test staging
		// no test has fired, so tier-aware parsers (Postgres v3
		// dispatcher) route ALL live queries to the startup engine —
		// `!IsTestWindowActive && !HasFirstTestFired` falls through to
		// StartupTransactional with no fallback. If a session-tagged
		// mock (e.g. listmonk's `drop type if exists list_type cascade`
		// DDL, recorder-tagged `type: config` → LifetimeSession) fires
		// during the app's install/bootstrap phase, routing it through
		// the session tier would make it unreachable until after the
		// first test fires. Copy it into the startup pool so the
		// startup engine's index covers every bootstrap-phase query.
		//
		// The session tree is still populated below (buildTier), so once
		// the first real test fires and SetMocksWithWindow
		// re-partitions, session mocks revert to being session-tier-only
		// in the startup tree (which rebuilds from the real-window
		// firstStart cutoff, not from the unfiltered input).
		for _, mk := range filtered {
			if mk == nil {
				continue
			}
			startupInit = append(startupInit, mk)
		}
		for _, mk := range unfiltered {
			if mk == nil {
				continue
			}
			startupInit = append(startupInit, mk)
		}
	}
	m.noteStatedLifetimes(filtered, unfiltered, explicitStartup)

	// Pre-filter per-test mocks by the outer test window. Zero start or
	// zero end means "no window" and we preserve the legacy behaviour
	// (no pre-filtering). Readers of the per-test tree can then skip
	// any window check at match time because the tree already contains
	// only in-window mocks.
	filteredForTree := filtered
	var droppedInvalid, droppedOutOfWindow uint64
	// carryIn collects the carry-over mocks (models.IsCarryOver) that this
	// window does not hold (loaded ahead of their window, or mapped to this
	// test from a gap); they are offered to the carry-over tier below instead
	// of being dropped. Nil while nothing is registered.
	// Carry-over also needs the recorded windows the replayer seeds at staging
	// (SeedRecordedWindows): without them there is no release rule to hold a
	// mock by, and a closed window's mocks are dropped as they always were.
	carryOn := models.CarryOverRegistered() && !isInitialStaging && !start.IsZero() && !end.IsZero() &&
		m.recordedWindows.Load().Len() > 0
	var carryIn []*models.Mock
	// Initial-staging skips the per-test partition entirely — everything
	// was already routed into startupInit above.
	if isInitialStaging {
		filteredForTree = nil
	}
	// sessionIn is what the per-test slice holds that lax mode promotes to
	// the session pool (KEPLOY_STRICT_MOCK_WINDOW=0: an unstated per-test mock
	// out of the window), as the agent's window filter promotes it.
	var sessionIn []*models.Mock
	if !isInitialStaging && !start.IsZero() && !end.IsZero() {
		out := make([]*models.Mock, 0, len(filtered))
		// Every mock is staged by the agent's one serve decision
		// (pkg.StageInWindow, the window filter's own rule, which the
		// replayer's mock prune asks too), strict as this manager filters,
		// and by where it is placed whatever its lifetime, which the filter
		// has split the slices by:
		//
		//   - no recorded time: kept, so legacy or incomplete recordings
		//     still match;
		//   - response stamped before its request: left out, and said of once
		//     for each such mock (pkg.LeaveOutAnsweredBeforeAsked);
		//   - placed in the window (models.PlaceOf, ends included): its own.
		//     Responses may straggle past end, async downstream completion
		//     being normal;
		//   - placed before the earliest window this manager has seen: app
		//     bootstrap traffic (Flyway migrations, Hibernate init, HikariCP
		//     validation, driver handshakes), routed to the startup tree, not
		//     the session tree and with no Lifetime mutation, so tier-aware
		//     parsers see startup, session and per-test as a strict three-way
		//     partition (legacy GetSessionMocks callers see startup through
		//     the union shim). Stale previous-test mocks (firstStart <= place
		//     < start) are dropped: the bleed this machinery exists to stop;
		//   - in flight when the window's test began (sent in the gap before
		//     it, models.WindowSchedule.GapBefore, and answered at or after its
		//     start): a consumer's long poll (a Kafka fetch, an SQS
		//     ReceiveMessage, a BLPOP) the app keeps outstanding between
		//     tests, whose answer the test waits for (models.InFlightEligible);
		//   - a window-bound mock (a connection failure): its own window or
		//     the startup band, nowhere else.
		//
		// A carry-over mock no rule serves here (loaded ahead of its window,
		// or mapped to this test from a gap) is offered to the carry-over
		// tier below, which serves it from its release window on
		// (models.WindowSchedule.CarryOverReachable).
		sw := pkg.ServeWindow{Start: start, End: end, FirstStart: firstStart, StrictPerCall: true, StrictWindow: true}
		if gapFrom, hasGap := m.testWindows.Load().GapBefore(start); hasGap {
			sw.InFlightFrom = gapFrom
		}
		for _, mock := range filtered {
			if mock == nil {
				continue
			}
			serve, answeredBeforeAsked := pkg.StageInWindow(m.logger, mock, sw)
			switch {
			case serve == pkg.ServedPerTest:
				out = append(out, mock)
			case serve == pkg.ServedStartup:
				startupInit = append(startupInit, mock)
			case serve == pkg.ServedSession:
				sessionIn = append(sessionIn, mock)
			case answeredBeforeAsked:
				droppedInvalid++
			case carryOn && models.IsCarryOver(mock):
				carryIn = append(carryIn, mock)
			default:
				droppedOutOfWindow++
			}
		}
		filteredForTree = out
	}

	// Wave 2/3: rebuild the dedicated startup tree from the startupInit
	// slice extracted above. The legacy GetSessionMocks shim will
	// concatenate a walk of this tree with the session walk so
	// pre-wave-2 parsers keep observing startup entries in their
	// session snapshot. Strict-accessor callers (v3 parser) go through
	// GetStartupMocks / GetSessionScopedMocks and see the partition.
	//
	// Wave-3 C3 fix: rebuild UNCONDITIONALLY with a fresh empty tree,
	// every SetMocksWithWindow call. Previously the rebuild was gated
	// on `len(startupInit) > 0 || m.startup != nil`, which meant a
	// subsequent test-set whose startupInit was empty and whose prior
	// run had left m.startup == nil would silently leak a stale pool.
	// A fresh tree here means cross-test-set bleed is structurally
	// impossible. Lock-wise we're already inside swapMu.Lock(), so the
	// pointer swap is safe; readers of startup go through a treesMu
	// RLock (see GetStartupMocks below).
	unfilteredForTree := unfiltered
	if len(sessionIn) > 0 {
		unfilteredForTree = append(append(make([]*models.Mock, 0, len(unfiltered)+len(sessionIn)), unfiltered...), sessionIn...)
	}
	newStartup := NewTreeDb(customComparator)
	newStartup.bootBuilt = !m.testFiredThisSet && !m.scopeFiredThisSet
	newStartupKeys := make(map[*models.Mock]models.TestModeInfo, len(startupInit)+len(explicitStartup))
	// H4 round-2: use a TIER-LOCAL TestModeInfo copy as the startup tree
	// key rather than mutating mk.TestModeInfo.ID in place. The same
	// *models.Mock pointer can appear in BOTH startupInit and
	// unfilteredForTree during initial staging (see the BaseTime branch
	// above that copies unfiltered→startupInit); the session tier's build
	// below stamps mk.TestModeInfo.ID with its index in the unfiltered
	// slice, which would invalidate any startup-tier
	// reference that relied on the original stamp. A mock pointer's
	// .ID then no longer matches the startup tree's idIndex, so a later
	// (currently hypothetical but structurally reachable — e.g. future
	// UpdateStartupMock / DeleteStartupMock paths, or any parser that
	// indexes startup mocks by mock.TestModeInfo.ID for its own cache)
	// lookup that derives the key from the live mock's state would
	// miss. By keying the startup tree off a local copy of
	// TestModeInfo, we keep the startup tree internally consistent
	// (idIndex matches the inserted key) while leaving the shared
	// mock's .ID free to be stamped by the session tier's build without
	// corrupting the startup index.
	for idx, mk := range startupInit {
		if mk == nil {
			continue
		}
		// Stamp a SortOrder on the shared mock if missing so the mock
		// has a deterministic comparator field in every tier it lives
		// in. SortOrder is derived from the recording and doesn't
		// collide across re-stampings the way ID does.
		if mk.TestModeInfo.SortOrder == 0 {
			mk.TestModeInfo.SortOrder = int64(idx) + 1
		}
		// Tier-local key: copy TestModeInfo and stamp the startup-tree
		// ID on the copy. mk.TestModeInfo.ID is left untouched so the
		// session tier's build, which stamps the same mock during
		// BaseTime staging, does not corrupt the startup tree's idIndex.
		key := tierKey(mk, idx)
		newStartup.insert(key, mk)
		newStartupKeys[mk] = key
	}
	// Harvest the kinds present BEFORE any tier is swapped.
	//
	// A kind that LEAVES the pool has to bump its own per-kind revision, or a
	// consumer caching an index under RevisionByKind never learns its mocks are
	// gone and keeps serving the previous set's index. Reading the by-kind maps
	// after the swaps — as this used to — reads the NEW maps, so it is exactly
	// redundant with walking the new input slices and departing kinds are
	// published nowhere.
	//
	// The old startup tree is walked too: during BaseTime staging
	// filteredForTree is nil, so a kind whose mocks all live in the startup tier
	// never appears in filteredByKind either, and reading only the two by-kind
	// maps misses it in both directions.
	outgoingKinds := map[models.Kind]struct{}{}
	m.treesMu.RLock()
	for k := range m.filteredByKind {
		outgoingKinds[k] = struct{}{}
	}
	for k := range m.unfilteredByKind {
		outgoingKinds[k] = struct{}{}
	}
	oldStartup := m.startup
	oldPerTest := m.filtered
	m.treesMu.RUnlock()
	if oldStartup != nil {
		oldStartup.rangeValues(func(v interface{}) bool {
			if mk, ok := v.(*models.Mock); ok && mk != nil {
				outgoingKinds[mk.Kind] = struct{}{}
			}
			return true
		})
	}

	// Build both remaining tiers before publishing anything: building stamps
	// tree IDs and sort orders, and one staged mock can sit in more than one
	// tier (startup and session during BaseTime staging), so stamping after
	// a tier went live would write into a mock its readers may be copying.
	filteredBuild := m.buildTier(filteredForTree, true)
	unfilteredBuild := m.buildTier(unfilteredForTree, false)
	// The explicit startup slice of a three-tier staging joins the startup
	// tier, stamped after every other tier, as the three-tier call has always
	// stamped it.
	for idx, mk := range explicitStartup {
		if mk == nil {
			continue
		}
		if mk.TestModeInfo.SortOrder == 0 {
			mk.TestModeInfo.SortOrder = int64(idx) + 1
		}
		// Tier-local key with an ID offset by a large constant so it cannot
		// collide with the startup IDs above (0..N).
		key := tierKey(mk, 1_000_000+idx)
		newStartup.insert(key, mk)
		newStartupKeys[mk] = key
	}

	if fenced {
		// Right before the first tree is published, so a consume of the
		// previous pool flags before the drop or is refused after it, and a
		// parser that is refused reads the new pool on its next try.
		m.dropPreviousSetConsumes()
	}
	m.treesMu.Lock()
	m.startup = newStartup
	m.startupKeys = newStartupKeys
	// Initial staging files the session pool in the startup tier too; an
	// explicit startup slice may share mocks with it as well.
	m.startupShared.Store(isInitialStaging || len(explicitStartup) > 0)
	m.treesMu.Unlock()
	if m.logger != nil && len(startupInit) > 0 {
		m.logger.Debug("routed startup-init mocks into startup tree",
			zap.Int("count", len(startupInit)),
			zap.Time("firstTestWindow", firstStart),
			zap.Bool("initialStaging", isInitialStaging))
	}

	// Wave-3 C1 fix: per-test input slice NEVER leaks into unfiltered.
	// The previous "initial staging" branch used to append filteredForTree
	// into unfilteredForTree so legacy matchers reading only
	// GetSessionMocks / GetUnFilteredMocks could serve bootstrap DB
	// traffic; with the startup tree now rebuilt unconditionally and the
	// GetSessionMocks union shim walking (startup ∪ session), legacy
	// matchers already see the bootstrap mocks via the startup path.
	// Routing per-test into unfiltered on top of that double-counted
	// bootstrap traffic and polluted the strict session tier seen by
	// tier-aware parsers.

	// Publish ONE revision after all three tiers have landed.
	//
	// The three swaps happen in independent treesMu sections — startup above,
	// then filtered, then unfiltered — while the by-kind readers each take
	// treesMu on their own. Bumping after each swap meant the revision
	// published midway was legitimately "current" for a torn state: a consumer
	// that samples the revision, reads the three tiers, and caches its index
	// under that revision could capture test N+1's per-test mocks alongside
	// test N's session mocks and never invalidate it, because the revision it
	// recorded really was the newest at the moment it looked. Every kind-aware
	// parser follows that sample-read-install idiom.
	//
	// Suppressing the inner bumps and publishing once at the end closes the
	// window: any revision a consumer can observe now corresponds to a state
	// where all three tiers agree. Suppression is per-call, not a flag on the
	// manager, so it cannot swallow a concurrent consumer's own bump.
	m.publishFiltered(filteredBuild, false)
	m.publishUnfiltered(unfilteredBuild, false)

	touchedAll := map[models.Kind]struct{}{}
	if carryOn {
		for k := range m.fileCarryOver(oldPerTest, carryIn, filteredForTree, start) {
			touchedAll[k] = struct{}{}
		}
	}
	for _, mk := range filteredForTree {
		if mk != nil {
			touchedAll[mk.Kind] = struct{}{}
		}
	}
	for _, mk := range unfilteredForTree {
		if mk != nil {
			touchedAll[mk.Kind] = struct{}{}
		}
	}
	// The startup tree is swapped in this function without any bump of its own,
	// so its kinds have to be published here or a startup-only kind never
	// invalidates a cached index.
	for _, list := range [][]*models.Mock{startupInit, explicitStartup} {
		for _, mk := range list {
			if mk != nil {
				touchedAll[mk.Kind] = struct{}{}
			}
		}
	}
	// Kinds that were present before this call but are absent from every input
	// slice above. Collected pre-swap; see outgoingKinds.
	for k := range outgoingKinds {
		touchedAll[k] = struct{}{}
	}
	for k := range touchedAll {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
	// What the parsers built of the trees this staging replaced goes with
	// them (WhenRestaged), once every generation moved. A store into the
	// trees or a delete from them lets nothing go: it moves every kind's
	// stored generation, but the parsers' indexes of a kind it did not touch
	// still hold, and what an index of a touched kind keeps is of this set.
	m.letDerivedGo()
	// Index the staged names for counting so MarkMockAsUsed takes the
	// O(1) fast path; a set boundary starts the counts afresh.
	// The startup tier is included: without it a startup-only mock never enters
	// hitIdx, so MarkMockAsUsed on one takes the exclusive-lock slow path,
	// walks the filtered tree, the unfiltered tree and every connection tree,
	// finds nothing and seeds nothing — repeating that walk on every call for a
	// mock whose count never moves. Session and connection mocks copied into
	// startup during BaseTime staging are already covered via unfilteredForTree;
	// the ones that miss are those the startup tier alone holds.
	m.rebuildHitIndex(setBoundary, startupInit, filteredForTree, unfilteredForTree, explicitStartup)

	// Seed per-connID trees for connection-scoped mocks so
	// GetConnectionMocks takes the O(1) path from the first lookup.
	for _, mk := range unfilteredForTree {
		if mk != nil && mk.TestModeInfo.Lifetime == models.LifetimeConnection {
			m.AddConnectionMock(mk)
		}
	}

	// Wave-3 H3 fix: do NOT publish the BaseTime sentinel as an active
	// window. Runner/Replayer's initial SetMocksWithWindow fires with
	// start=models.BaseTime BEFORE any test runs, solely to stage
	// bootstrap mocks into the startup tree. If we wrote BaseTime into
	// m.windowStart, IsTestWindowActive (which only rejects zero-time)
	// would flip true and tier-routing parsers (Postgres v3 dispatcher)
	// would mis-route bootstrap traffic to the per-test engine — whose
	// per-test tree is empty during initial staging because all filtered
	// input was routed to the startup tree above. Symptom: startup-tier
	// queries (listmonk's first `select count(*) from settings`) get
	// `candidates=0` / KP001 misses even though the mock is present in
	// the startup index.
	//
	// Keep windowStart/windowEnd at their current (zero for a fresh
	// manager) values during initial staging. The next
	// SetMocksWithWindow call — the first real test — overwrites them
	// with real timestamps, at which point IsTestWindowActive becomes
	// true and the dispatcher routes to PerTest as intended.
	//
	// A scope is closed by every staging but the one that opens it, BaseTime
	// staging included: that staging moves the pool to the startup tier, so a
	// test window left open over it would route the test's calls to an empty
	// per-test tier.
	m.windowMu.Lock()
	if !isInitialStaging {
		m.windowStart = start
		m.windowEnd = end
	}
	m.scopeOpen = scope
	m.windowMu.Unlock()
	// Counter reflects pre-filtering drops for THIS test; legacy
	// match-time filtering in GetFilteredMocksInWindow will add zero
	// now that the tree is pre-filtered, which keeps the semantic
	// ("how many mocks did we drop for this test?") intact across the
	// Phase 3 pre-filter migration.
	atomic.StoreUint64(&m.droppedOutOfWindow, droppedOutOfWindow)
	if (droppedOutOfWindow > 0 || droppedInvalid > 0) && m.logger != nil {
		m.logger.Debug("per-test tree pre-filter summary",
			zap.Uint64("dropped_out_of_window", droppedOutOfWindow),
			zap.Uint64("dropped_invalid_order", droppedInvalid),
			zap.Int("kept_total", len(filteredForTree)),
			zap.Time("windowStart", start),
			zap.Time("windowEnd", end))
	}
}

func (m *MockManager) GetUnFilteredMocks() ([]*models.Mock, error) {
	m.treesMu.RLock()
	tree := m.unfiltered
	m.treesMu.RUnlock()
	// Under exact serving only what the view shows of each tier is read
	// (shownOf): reusable narrows it as it narrowed the tier's values.
	if v := m.exactView(); v != nil {
		return v.reusable("", v.shownOf(tree, ""), v.shownOf(m.startupTree(), "")), nil
	}
	results := make([]*models.Mock, 0, 128)
	tree.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

// SetMocksWithWindowThreeTier is the three-pool variant of
// SetMocksWithWindow. It accepts an EXPLICIT startup pool (third
// argument) pre-partitioned by the core filter layer (see
// FilterByTimeStampThreeTier), in addition to the familiar
// filtered+unfiltered pair. This lets callers that have already done
// the Lifetime-aware partition avoid relying on SetMocksWithWindow's
// internal firstWindowStart/BaseTime heuristic to re-derive the
// startup tier from the union of filtered+unfiltered.
//
// Semantic equivalence to SetMocksWithWindow:
//   - `filtered` lands in the per-test tree after window containment
//     (same pre-filter the legacy entry does).
//   - `unfiltered` lands in the session (unfiltered) tree.
//   - `startup` joins the dedicated startup tree, AFTER the internal
//     BaseTime/firstWindowStart heuristic has routed `filtered` and
//     `unfiltered` into it, under keys offset so the two cannot collide.
//     The heuristic still fires so legacy callers that pass a nil
//     startup slice still get the automatic startup-preservation
//     behaviour for mocks inside `filtered` whose req < firstStart.
//
// Integrations that don't emit LifetimeConnection mocks (HTTP, generic,
// gRPC, Kafka, etc.) can pass nil for `startup` and get behaviour
// identical to SetMocksWithWindow.
func (m *MockManager) SetMocksWithWindowThreeTier(filtered, unfiltered, startup []*models.Mock, start, end time.Time) {
	m.keepRunOnlyForAnExactStartUp(start)
	// Signal once everything below has landed, startup additions included.
	defer m.signalWindowChange()
	m.swapMu.Lock()
	defer m.swapMu.Unlock()
	ownForStaging(&filtered, &unfiltered, &startup)
	// The legacy staging rebuilds the startup tree with the
	// BaseTime/firstWindowStart heuristic applied to the filtered/unfiltered
	// inputs, and adds the explicit `startup` slice to it: connection-scoped
	// mocks the caller partitioned that the heuristic can't derive on its
	// own. The slice is stamped, filed and indexed for counting with the
	// rest of the staging, before any tier is published.
	m.setMocksWithWindowLocked(filtered, unfiltered, startup, start, end, false)
}

// GetSessionMocks returns the UNION of startup-tier and session-tier
// mocks. Wave 2 split the old "session pool" into two disjoint tiers
// (startup: recorded before the first test window fired; session:
// reusable across tests) but most parsers still call this accessor, so
// the method stays as a backward-compat shim.
//
// Deprecated: v3 and any new tier-aware parser should call
// GetStartupMocks + GetSessionScopedMocks directly. This method returns
// the union (startup + session) exclusively for parsers that have not
// yet migrated. The order is startup-first so matchers that iterate in
// order still see bootstrap traffic before regular session mocks, which
// matches pre-wave-2 behaviour.
//
// N-R1 fix: during Runner/Replayer's initial BaseTime staging call the
// startup tree is deliberately over-populated with the full unfiltered
// slice (to keep session-tagged bootstrap DDL like listmonk's
// `DROP TYPE IF EXISTS` reachable by the dispatcher's StartupTransactional
// engine before the first test fires — see the isInitialStaging branch
// in SetMocksWithWindow). That means the SAME *Mock pointer lives in
// both m.startup and m.unfiltered during the pre-first-test window, and
// a naive concat here would double-count it in the union — skewing
// consumedIndex accounting on the one replay path that
// matters most (Flyway + HikariCP + ORM boot). Dedup by pointer
// identity before returning. At steady state (post-first-test) the
// trees are disjoint and the dedup is a free no-op (zero duplicate
// insertions into the seen-set).
func (m *MockManager) GetSessionMocks() ([]*models.Mock, error) {
	startup, err := m.GetStartupMocks()
	if err != nil {
		return nil, err
	}
	session, err := m.GetSessionScopedMocks()
	if err != nil {
		return nil, err
	}
	// Under exact serving the open test's unanswered mocks lead the union:
	// it is every mock a parser that reads nothing else may serve.
	if v := m.exactView(); v != nil {
		if owned := v.unanswered(""); len(owned) > 0 {
			startup = append(owned, startup...)
		}
	}
	if len(startup) == 0 {
		return session, nil
	}
	// Pointer-identity dedup. Map-with-struct{}-value keyed on *Mock is
	// cheaper than a slice-scan per candidate because the worst case
	// (initial staging with N ~ 10^2 startup mocks, M ~ 10^2 session
	// mocks, each pointer shared) would be O(N*M) under the naive
	// form. The map keeps it O(N+M) with one allocation.
	out := make([]*models.Mock, 0, len(startup)+len(session))
	seen := make(map[*models.Mock]struct{}, len(startup)+len(session))
	for _, mk := range startup {
		if mk == nil {
			continue
		}
		if _, dup := seen[mk]; dup {
			continue
		}
		seen[mk] = struct{}{}
		out = append(out, mk)
	}
	for _, mk := range session {
		if mk == nil {
			continue
		}
		if _, dup := seen[mk]; dup {
			continue
		}
		seen[mk] = struct{}{}
		out = append(out, mk)
	}
	return out, nil
}

// MockCursorIndex returns the record-ordered position to serve for the request
// identified by key, given n recorded responses for it, WITHOUT advancing. It
// returns min(cursor, n-1), so once a request has been replayed more times than
// it was recorded it saturates on the LAST recorded response instead of missing
// — which keeps fixture re-reads (the same row read many times, recorded once)
// working while a genuine stateful sequence (1,2,3) is served in order. The
// caller advances via AdvanceMockCursor only once the response is actually
// served (a successful claim), so a failed/retried match does not skip a
// recorded response. Session-scoped: the cursor map is cleared per test-set
// (ResetStatefulCursors). Under exact serving a key's cursor is the pool
// slice's between tests and the open test's own in a test (cursorSlotIn). The
// map also keeps the HTTP walks' served marks, one per reusable recording
// under a key of its own (n = 2: served or not), which reset with the
// cursors. Satisfies the matcher's optional cursor
// interface; parsers that do not type-assert it are unaffected.
func (m *MockManager) MockCursorIndex(key string, n int) int {
	return m.cursorIndexAt(cursorSlotIn(m.exactView(), key), n)
}

// cursorIndexAt is MockCursorIndex at a cursor slot already resolved
// (cursorSlotIn, backgroundCursorKey). A slot of a test view no longer in
// force reads where its test's calls left it (closeTestCursors), so a call
// read in the test and walked after it ended answers what it would have
// answered before; once the next set is staged, 0.
func (m *MockManager) cursorIndexAt(slot cursorSlot, n int) int {
	if n <= 1 {
		// Zero or one recorded response: always the first (only) one — identical
		// to the legacy reuse behaviour, no cursor state needed.
		return 0
	}
	m.cursorMu.Lock()
	defer m.cursorMu.Unlock()
	if slot.gen != 0 {
		return clampCursor(m.testCursors[slot.gen][slot.key], n)
	}
	return clampCursor(m.cursors[slot.key], n)
}

// AdvanceMockCursor moves the cursor for key one past servedIdx — the index the
// caller actually served — taking the MAX with the current cursor so it is
// monotonic and idempotent. Advancing from the served index (not current+1)
// means two concurrent identical requests that both peeked the same index and
// both served it settle the cursor to servedIdx+1 rather than jumping two,
// which would skip a recorded response. Stored value tops out at n (reads clamp
// to n-1), so a hot repeated request never grows it unbounded. Call it only
// after the chosen response was actually served. Under exact serving it
// resolves the view again: a caller that peeks and commits across a view
// install reads one view for both (SequenceView).
func (m *MockManager) AdvanceMockCursor(key string, servedIdx, n int) {
	m.advanceCursorAt(cursorSlotIn(m.exactView(), key), servedIdx, n)
}

// advanceCursorAt is AdvanceMockCursor at a cursor slot already resolved
// (cursorSlotIn, backgroundCursorKey). A commit to a test view no longer in
// force moves that test's own sequence, which only calls read in the test
// walk (closeTestCursors): two calls read in the test and walked after its
// end answer in record order, wherever the end lands between them. Once the
// next set is staged it moves nothing.
func (m *MockManager) advanceCursorAt(slot cursorSlot, servedIdx, n int) {
	if n <= 1 {
		return
	}
	m.cursorMu.Lock()
	defer m.cursorMu.Unlock()
	cursors := m.cursors
	if slot.gen != 0 {
		var open bool
		if cursors, open = m.testCursors[slot.gen]; !open {
			return
		}
	} else if cursors == nil {
		m.cursors = make(map[string]int)
		cursors = m.cursors
	}
	next := servedIdx + 1
	if cur := cursors[slot.key]; cur > next {
		next = cur
	}
	if next > n {
		next = n
	}
	cursors[slot.key] = next
}

// Spend records that the test which kept the named per-test recordings of
// stateful dependencies for its repeats is over (integrations.SpentReader).
func (m *MockManager) Spend(names ...string) {
	if len(names) == 0 {
		return
	}
	m.spentMu.Lock()
	defer m.spentMu.Unlock()
	if m.spent == nil {
		m.spent = make(map[string]struct{}, len(names))
	}
	for _, n := range names {
		m.spent[n] = struct{}{}
	}
}

// Spent reports whether mk is a per-test recording a test kept for its
// repeats and is over with (Spend).
func (m *MockManager) Spent(mk *models.Mock) bool {
	if mk == nil {
		return false
	}
	m.spentMu.RLock()
	defer m.spentMu.RUnlock()
	_, spent := m.spent[mk.Name]
	return spent
}

// resetPerTestCursors drops every per-test stateful cursor
// (models.IsPerTestSequenceKey), and keeps the reusable ones, whose
// sequences run across tests.
func (m *MockManager) resetPerTestCursors() {
	m.cursorMu.Lock()
	defer m.cursorMu.Unlock()
	for k := range m.cursors {
		if models.IsPerTestSequenceKey(k) {
			delete(m.cursors, k)
		}
	}
	m.kept = nil
	m.usedUp = nil
}

// holdUsedUp records that the recording key names (a scope and a name) was
// consumed in that scope, as DeleteFilteredMock records a delete: false when
// it already was. The revisions move as a delete moves them, so a parser that
// caches what it read reads again.
func (m *MockManager) holdUsedUp(key string, kind models.Kind) bool {
	m.cursorMu.Lock()
	if _, held := m.usedUp[key]; held {
		m.cursorMu.Unlock()
		return false
	}
	if m.usedUp == nil {
		m.usedUp = make(map[string]struct{})
	}
	m.usedUp[key] = struct{}{}
	m.cursorMu.Unlock()
	m.bumpRevisionKind(kind)
	m.bumpRevisionAll()
	return true
}

// usedUpKeys returns whether a recording, by key, is held used up
// (holdUsedUp): nil when none is. It reads a copy, taken under one lock.
func (m *MockManager) usedUpKeys() func(key string) bool {
	m.cursorMu.Lock()
	defer m.cursorMu.Unlock()
	return snapshotKeys(m.usedUp)
}

// spentNames returns whether a recording, by name, is spent (Spend): nil
// when none is. It reads a copy, taken under one lock.
func (m *MockManager) spentNames() func(name string) bool {
	m.spentMu.RLock()
	defer m.spentMu.RUnlock()
	return snapshotKeys(m.spent)
}

// snapshotKeys returns a lookup in a copy of set, or nil when set is empty.
func snapshotKeys(set map[string]struct{}) func(string) bool {
	if len(set) == 0 {
		return nil
	}
	cp := make(map[string]struct{}, len(set))
	for k := range set {
		cp[k] = struct{}{}
	}
	return func(k string) bool {
		_, ok := cp[k]
		return ok
	}
}

// KeepForRepeats records that an exact match kept the recording key names for
// its test's byte-identical repeats (integrations.RepeatKeeper).
func (m *MockManager) KeepForRepeats(key string) {
	m.cursorMu.Lock()
	defer m.cursorMu.Unlock()
	if m.kept == nil {
		m.kept = make(map[string]struct{})
	}
	m.kept[key] = struct{}{}
}

// KeptForRepeats returns whether an exact match kept a recording, by key, for
// its test's repeats since the last staging: nil when none is. It reads a
// copy, taken under one lock.
func (m *MockManager) KeptForRepeats() func(key string) bool {
	m.cursorMu.Lock()
	defer m.cursorMu.Unlock()
	return snapshotKeys(m.kept)
}

// OwnedByOpenTest reports whether mk is the open test's own recording
// (integrations.TestOwner): a test is open (IsTestWindowActive) and mk is in
// the per-test pool served for it: under exact serving, the test's mapping
// (ServeExactTest); else the per-test tier, by the test's recorded window when
// it has one, as GetPerTestMocksInWindow serves it: a call of the window, or
// one in flight when the test began (inFlightAtStart). With no test open (the
// staging before the first test, between tests, or a replay that opens none)
// no mock is.
func (m *MockManager) OwnedByOpenTest(mk *models.Mock) bool {
	if mk == nil {
		return false
	}
	if v := m.exactView(); v != nil {
		// The open test's test mocks (ServeExactTest), by name: its index
		// is fixed when the view is made.
		_, owned := v.index[mk.Name]
		return v.test != "" && owned
	}
	m.windowMu.RLock()
	active := m.windowActiveLocked()
	start, end := m.windowStart, m.windowEnd
	m.windowMu.RUnlock()
	if !active {
		return false
	}
	m.treesMu.RLock()
	tree := m.filtered
	m.treesMu.RUnlock()
	if !tree.holds(mk.TestModeInfo, mk) {
		return false
	}
	if start.IsZero() || end.IsZero() {
		return true
	}
	gapFrom, hasGap := m.testWindows.Load().GapBefore(start)
	return inRecordedWindow(mk, start, end, gapFrom, hasGap)
}

// InFlightIntoOpenTest reports whether mk was in flight when the open test
// began: its request recorded outside the test's window, in the gap before it,
// and answered inside it (inFlightAtStart), which the windowed read serves as
// the test's own (integrations.InFlightOwner). Under exact serving, where the
// manager holds no window, it is one of the open test's mocks the pool view
// before it served as in flight (ServeExactPool's ahead, exactView.inFlight):
// its mapping gives it the test, and the replay named it so. No mock is with
// no test open, in the gap itself.
func (m *MockManager) InFlightIntoOpenTest(mk *models.Mock) bool {
	if mk == nil {
		return false
	}
	if v := m.exactView(); v != nil {
		return v.test != "" && v.inFlight[mk.Name]
	}
	m.windowMu.RLock()
	active := m.windowActiveLocked()
	start, end := m.windowStart, m.windowEnd
	m.windowMu.RUnlock()
	req, res := mk.Spec.ReqTimestampMock, mk.Spec.ResTimestampMock
	if !active || start.IsZero() || end.IsZero() || req.IsZero() || res.IsZero() {
		return false
	}
	if place := models.PlaceOf(mk); !place.Before(start) && !place.After(end) {
		return false
	}
	gapFrom, hasGap := m.testWindows.Load().GapBefore(start)
	return inFlightAtStart(mk, req, res, gapFrom, start, hasGap)
}

// inRecordedWindow is GetFilteredMocksInWindow's test of one mock against a
// test's recorded window [start, end]: placed inside it (models.PlaceOf: its
// request, or its answer for a mock placed at its answer), in flight when
// the test began (inFlightAtStart, with gapFrom and hasGap the window's
// models.WindowSchedule.GapBefore), or no recorded times. A mock whose
// response is stamped before its request is in no window, as that read leaves
// it out, but under the proven switch, where the read keeps it
// (pkg.LeaveOutAnsweredBeforeAsked).
func inRecordedWindow(mk *models.Mock, start, end, gapFrom time.Time, hasGap bool) bool {
	req, res := mk.Spec.ReqTimestampMock, mk.Spec.ResTimestampMock
	if req.IsZero() || res.IsZero() {
		return true
	}
	if res.Before(req) && !provenswitch.On() {
		return false
	}
	if place := models.PlaceOf(mk); !place.Before(start) && !place.After(end) {
		return true
	}
	return inFlightAtStart(mk, req, res, gapFrom, start, hasGap)
}

// clampCursor bounds a stored cursor to the last valid index [0, n-1].
func clampCursor(cur, n int) int {
	if cur > n-1 {
		return n - 1
	}
	if cur < 0 {
		return 0
	}
	return cur
}

// ResetStatefulCursors drops every per-request stateful cursor so the next
// stateful sequence starts from its first recorded response. The agent calls it
// once per test-set (on the staging call) via the StatefulCursorResetter
// capability, so a sequence spans a whole test-set but does not carry across
// sets. An ended test's frozen cursors (closeTestCursors) are dropped. Safe
// to call when no cursors exist.
func (m *MockManager) ResetStatefulCursors() {
	m.cursorMu.Lock()
	m.cursors = nil
	for gen := range m.closedTests {
		delete(m.testCursors, gen)
	}
	m.closedTests = nil
	for gen := range m.testCursors {
		m.testCursors[gen] = make(map[string]int)
	}
	m.cursorMu.Unlock()
}

// SeedStatementCatalog replaces the stored set's statement catalog: the first
// recorded mock of each statement its on-disk store holds
// (DiskMocks.LoadStatements), none for a set with no on-disk store. They go in
// no tree: a parser reads them only to
// answer a prepare (StatementCatalog), and no match or consume ever reaches
// them, so a statement recorded inside a test is described at app start
// without its answer being served there.
func (m *MockManager) SeedStatementCatalog(mocks []*models.Mock) {
	byKind := make(map[models.Kind][]*models.Mock)
	for _, mk := range mocks {
		if mk != nil {
			byKind[mk.Kind] = append(byKind[mk.Kind], mk)
		}
	}
	m.statementsMu.Lock()
	m.statements = byKind
	m.statementsMu.Unlock()
}

// StatementCatalog returns the stored set's statement catalog's mocks of kind, in
// recorded order. Satisfies the parsers' optional
// integrations.StatementCatalog capability. The mocks are shared: a caller
// reads them and never changes them.
func (m *MockManager) StatementCatalog(kind models.Kind) []*models.Mock {
	m.statementsMu.RLock()
	defer m.statementsMu.RUnlock()
	return m.statements[kind]
}

// Variables returns the table of the test variables this replay has bound.
// Satisfies the matchers' optional integrations.VariableStore capability.
// Between tests it is the ended test's: a call of the gap reads and binds in
// it, but for the next test's calls in flight (InAheadScope).
func (m *MockManager) Variables() *variables.Table {
	return &m.vars
}

// AheadVariables is the table the next test's calls in flight bind in, when
// the gap before it answers them under exact serving (ServeExactPool's ahead):
// the next test's scope, begun early, carried into it as it begins
// (StartVariables). Satisfies integrations.VariableScopes.
func (m *MockManager) AheadVariables() *variables.Table {
	return &m.aheadVars
}

// InAheadScope reports whether a call mk answers binds in AheadVariables: mk
// is one of the next test's calls in flight the pool view answers, between
// tests under exact serving. Satisfies integrations.VariableScopes.
func (m *MockManager) InAheadScope(mk *models.Mock) bool {
	if mk == nil {
		return false
	}
	v := m.exactView()
	if v == nil || v.test != "" || len(v.owned) == 0 {
		return false
	}
	_, ahead := v.index[mk.Name]
	return ahead
}

// StartVariables begins a test's variables (variables.Table.Start) with the
// values its test case's request gave bound, and, under exact serving, the
// bindings the answers of its calls in flight made in the gap before it
// (exactView.carried, aheadVars), after the given ones: a given value wins.
//
// Begun after the test's mocks went live, a call one of them answers between
// the two binds in no scope of the test's: a replay opens a test with its
// variables in one step (OpenExactTest, OpenTestWithWindow).
func (m *MockManager) StartVariables(given []models.VariableBinding) {
	m.vars.StartCarrying(given, m.carriedInto(m.exactView()))
	m.aheadVars.Reset()
}

// carriedInto is what the answers of test view v's calls in flight bound in
// the gap before it (exactView.carried, aheadVars): the bindings its
// variables begin carrying. None for a view that is not a test's.
func (m *MockManager) carriedInto(v *exactView) []models.VariableBinding {
	if v == nil || v.test == "" || len(v.carried) == 0 {
		return nil
	}
	var carry []models.VariableBinding
	for _, b := range m.aheadVars.Snapshot() {
		if v.carried[b.Mock] {
			carry = append(carry, b)
		}
	}
	return carry
}

// OpenTestWithWindow stages a test's window as SetMocksWithWindow does, and
// begins the test's variables (StartVariables) with given bound, in the same
// step (variables.Table.StartOpening): no call the window's mocks answer
// binds before them. Opened in two steps, a call of a set's first test
// answered between them bound in the run's, which every later test of the
// set reads (NewVariableSet).
func (m *MockManager) OpenTestWithWindow(filtered, unfiltered []*models.Mock, start, end time.Time, given []models.VariableBinding) {
	m.keepRunOnlyForAnExactStartUp(start)
	carry := m.carriedInto(m.exactView())
	m.vars.StartOpening(func() { m.setMocksWithWindow(filtered, unfiltered, start, end) }, given, carry)
	m.aheadVars.Reset()
}

// BoundVariables is the test variables the current test (or the one that just
// ended, between tests) has bound: never what the next test's calls in flight
// bound in the gap.
func (m *MockManager) BoundVariables() []models.VariableBinding {
	return m.vars.Snapshot()
}

// ResetVariables forgets every binding of the test's scope, and the gap's.
// The run's stay (NewVariableSet).
func (m *MockManager) ResetVariables() {
	m.vars.Reset()
	m.aheadVars.Reset()
}

// NewVariableSet begins a test set's variables, as the set's mocks are set
// up (Proxy.Mock, under the proven switch, beside the learn step's
// Tracker.NewSet): every binding of the previous set is forgotten, its run's
// too, and what this set's start-up binds before its first test begins is
// the run's (variables.Table.KeepRun) when the start-up is served exactly
// (keepRunOnlyForAnExactStartUp), which every test of the set reads,
// as does the scope the next test's calls in flight bind in before it
// begins (AheadVariables). A test's own bindings stay its own: each test
// begins with none of the test before it (StartVariables, ResetVariables).
func (m *MockManager) NewVariableSet() {
	m.vars.NewSet()
	m.vars.KeepRun()
	m.aheadVars.NewSet()
	m.aheadVars.ReadRunOf(&m.vars)
}

// GetStartupMocks returns the startup-tier mocks — exactly the set
// whose ReqTimestampMock predates the earliest test window this manager
// has seen (firstWindowStart). Strictly disjoint from
// GetSessionScopedMocks, GetConnectionMocks, and
// GetPerTestMocksInWindow: every mock lives in exactly one tree.
//
// Tier-aware parsers (the Wave 2 Postgres v3 replayer is the first and
// currently only in-tree caller) consult this pool to serve
// app-bootstrap traffic — Flyway migrations, ORM metadata boot,
// HikariCP/JDBC pool warm-up queries. Pool is rebuilt by every
// SetMocksWithWindow call using the latest firstWindowStart.
func (m *MockManager) GetStartupMocks() ([]*models.Mock, error) {
	m.treesMu.RLock()
	tree := m.startup
	m.treesMu.RUnlock()
	if tree == nil {
		return nil, nil
	}
	if v := m.exactView(); v != nil {
		return v.reusable("", v.shownOf(tree, ""), v.shownOf(m.unfilteredTree(), "")), nil
	}
	results := make([]*models.Mock, 0, 32)
	tree.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

// GetStartupMocksByKind returns startup-tier mocks whose Kind matches the
// given kind. Mirror of GetUnFilteredMocksByKind / GetFilteredMocksByKind
// for the startup tier.
//
// The startup tree is not partitioned by kind internally — typical startup
// pool size is ~10^2 mocks (Flyway migrations + ORM metadata boot +
// driver-handshake queries), so an in-memory filter walk is O(N) but with
// a small N and no lock-surface expansion. If the startup pool ever grows
// to a size where this is measurable, the remediation is to mirror the
// per-kind trees the session / per-test tiers maintain.
//
// Returns nil, nil when the startup tree has never been populated (fresh
// manager pre-first SetMocksWithWindow).
func (m *MockManager) GetStartupMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	m.treesMu.RLock()
	tree := m.startup
	m.treesMu.RUnlock()
	if tree == nil {
		return nil, nil
	}
	if v := m.exactView(); v != nil {
		return v.reusable(kind, v.shownOf(tree, kind), v.shownOf(m.unfilteredTree(), kind)), nil
	}
	results := make([]*models.Mock, 0, 16)
	tree.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil && mock.Kind == kind {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

// GetSessionScopedMocks returns session-tier + connection-tagged mocks
// strictly — startup-tier mocks are NOT included. Parsers opting into
// tier-aware routing (Wave 2 Postgres v3) call this in preference to
// the legacy GetSessionMocks union shim.
//
// Backed by the existing `unfiltered` tree (session + connection live
// there together; connection-tagged mocks are additionally indexed in
// per-connID trees for O(1) lookup). Startup entries are in `m.startup`
// and deliberately excluded here.
func (m *MockManager) GetSessionScopedMocks() ([]*models.Mock, error) {
	return m.GetUnFilteredMocks()
}

// HasFirstTestFired reports whether at least one real test window has
// been set on this manager — i.e. some SetMocksWithWindow call arrived
// with a non-zero start that was NOT the models.BaseTime sentinel used
// for pre-test staging. Sticky within a test set; ResetForReplaySession
// clears it at each set boundary, so the next set reads false until its
// own first test fires.
//
// Parsers use this to distinguish "app bootstrap" from "between tests"
// when IsTestWindowActive() returns false. Before the first test fires
// all non-test traffic is bootstrap; after it, the same signal means
// the runner is in the idle gap between tests (or post-last-test
// teardown).
//
// Concurrency: firstWindowStart is written only under swapMu.Lock() — in
// SetMocksWithWindow when a real window arrives, and in
// ResetForReplaySession at the set boundary. A reader here takes
// swapMu.RLock so it cannot observe a torn time.Time or a half-applied
// reset. An inherently-racy pre-check is tolerable — a caller that needs strict window/pool atomicity
// consults GetPerTestMocksInWindow, which snapshots both under swapMu.
//
// Non-atomic-pair warning: a caller that reads IsTestWindowActive and
// HasFirstTestFired sequentially can observe an inconsistent pair
// during a SetCurrentTestWindow / SetMocksWithWindow transition
// because the two bits live under different locks (windowMu vs
// swapMu). For pair-coherent reads use WindowSnapshot, which
// takes both locks in declared order and returns a tear-free snapshot.
//
// A test of keploy mock replay begun without a worker pid has fired while it
// runs (SetScopeMocks), so WindowSnapshot never reads Active without
// FirstTestFired for it. Its end goes back to the routing before it: unlike a
// test window, a scope does not stay fired.
func (m *MockManager) HasFirstTestFired() bool {
	if m.exactView() != nil {
		return m.exact.fired.Load()
	}
	m.swapMu.RLock()
	defer m.swapMu.RUnlock()
	m.windowMu.RLock()
	defer m.windowMu.RUnlock()
	return m.testFiredThisSet || m.scopeOpen
}

// FirstTestWindowStart returns this set's startup-init cutoff: the earliest
// test window start observed, or — from the boundary staging call onward — the
// value the replayer seeded via SeedStartupCutoff.
//
// It is NOT "zero until a non-BaseTime SetMocksWithWindow lands". In production
// the seed arrives first, so this returns a non-zero cutoff throughout the
// staging window. A non-zero value here does not mean a test has fired; use
// HasFirstTestFired for that.
//
// Exposed so the agent's tier-aware strictMockWindow filter can
// distinguish:
//
//   - startup-init mocks (req < firstWindowStart) — legitimate
//     app-bootstrap traffic that predates the first test window
//     (Flyway migrations, Hibernate init, HikariCP pool warm-up,
//     listmonk's cacheUsers SELECT) and MUST be preserved so
//     MockManager.SetMocksWithWindow can route them into the
//     startup tree.
//
//   - stale previous-test mocks (firstWindowStart <= req < currentStart) —
//     cross-test bleed the strict gate exists to protect against.
//     These are DROPPED.
//
// Concurrency: read under swapMu.RLock to avoid a torn time.Time read
// against the writer inside SetMocksWithWindow. Inherently racy as a
// point-in-time sample but that's tolerable — the filter only needs
// a stable enough value to tell "before any test fired" (zero) from
// "startup cutoff is at T".
func (m *MockManager) FirstTestWindowStart() time.Time {
	m.swapMu.RLock()
	defer m.swapMu.RUnlock()
	return m.firstWindowStart
}

// WindowSnapshot returns the (IsTestWindowActive, HasFirstTestFired)
// pair under a single outer lock so callers that need BOTH bits as a
// coherent point-in-time read cannot observe the torn intermediate
// state the individual accessors admit. Takes swapMu.RLock first
// (matches the writer-side SetMocksWithWindow lock order: swapMu →
// windowMu; see mockmanager.go's lock-ordering invariant at the top)
// then windowMu.RLock, preserving the declared acquisition order.
//
// This is the accessor the v3 Postgres dispatcher's routeTransactional
// and types.TierIndex.orderForCurrentState consume — both read the
// pair on the hot path and a torn read causes a cross-tier misroute
// (a statement's Parse/Describe lands on one tier and its Execute on
// another). Legacy callers that only read one bit at a time keep
// using IsTestWindowActive / HasFirstTestFired unchanged.
//
// The individual field accessors are preserved for back-compat with
// legacy parsers and out-of-repo consumers; those callers don't need
// the pair and a single-lock read is materially cheaper per call.
func (m *MockManager) WindowSnapshot() models.WindowSnapshot {
	// Under exact serving a test is open between its begin and its end,
	// whatever the recorded times say.
	if v := m.exactView(); v != nil {
		return models.WindowSnapshot{Active: v.test != "", FirstTestFired: m.exact.fired.Load()}
	}
	m.swapMu.RLock()
	defer m.swapMu.RUnlock()
	m.windowMu.RLock()
	defer m.windowMu.RUnlock()
	return models.WindowSnapshot{
		Active:         m.windowActiveLocked(),
		FirstTestFired: m.testFiredThisSet || m.scopeOpen,
	}
}

// CurrentTestWindow returns the [start, end] request-timestamp bounds of the
// outer test currently being replayed (as set by SetCurrentTestWindow /
// SetMocksWithWindow). Both zero means no time window: none is active
// (initial staging or between tests), or the active one has no recorded-time
// bounds (SetScopeMocks), which IsTestWindowActive tells apart. Read under
// windowMu so callers see a tear-free pair.
//
// Exposed for matchers that receive data mocks in the SESSION pool (because
// the enterprise agent lax-promotes per-test data mocks to session and lets
// MockManager.SetMocksWithWindow enforce strict windowing) but still need to
// distinguish a mock recorded INSIDE the current test window from one
// recorded for an earlier test. The MySQL replayer uses this to disambiguate
// repeated identical prepared-statement reads (e.g. login's username lookup,
// register's read-back of a freshly-generated id) so each consumes the row
// recorded at that position rather than a stale earlier-test row.
func (m *MockManager) CurrentTestWindow() (time.Time, time.Time) {
	// Under exact serving no recorded time narrows what a test is served:
	// its mapping does.
	if m.exactView() != nil {
		return time.Time{}, time.Time{}
	}
	m.windowMu.RLock()
	defer m.windowMu.RUnlock()
	return m.windowStart, m.windowEnd
}

// GetPerTestMocksInWindow is the unification-plan canonical name for
// the time-windowed per-test pool. Aliases GetFilteredMocksInWindow
// during Phase 2 migration. SetMocksWithWindow already pre-filters
// the per-test slice by request-timestamp containment (and keeps the
// calls in flight when the window's test began) BEFORE building the
// tree, so GetFilteredMocksInWindow's "iterate and window-check"
// is effectively a no-op fast path; the remaining Phase-3 work is to
// collapse this accessor into a plain tree Snapshot with no iteration
// overhead at all.
func (m *MockManager) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	return m.GetFilteredMocksInWindow()
}

// RangePerTestMocksInWindow is GetPerTestMocksInWindow handed to fn one mock
// at a time (integrations.PerTestRanger). Under exact serving the open test's
// unanswered mocks are read a few at a time and the tier only once fn has
// not stopped at them (rangeExactPerTest); otherwise the read is walked.
func (m *MockManager) RangePerTestMocksInWindow(fn func(*models.Mock) bool) error {
	m.swapMu.RLock()
	m.treesMu.RLock()
	tree := m.filtered
	m.treesMu.RUnlock()
	m.swapMu.RUnlock()
	if v := m.exactView(); v != nil {
		m.rangeExactPerTest(v, "", tree, fn)
		return nil
	}
	mocks, err := m.GetPerTestMocksInWindow()
	if err != nil {
		return err
	}
	for _, mk := range mocks {
		if !fn(mk) {
			break
		}
	}
	return nil
}

// StatesLifetimes reports whether the recording of the set being replayed
// states its kind mocks' lifetimes (models.Mock.StatesLifetime): a mock of the
// kind staged since the set's boundary does. Its per-test mocks are then
// served to their own test only, and a test that misses a call another test
// recorded is told how to declare it shared (keploy.yaml sessionCalls). It
// holds after the test's own mocks are consumed, which a scan of the pools
// does not. See integrations.StatesLifetimes.
func (m *MockManager) StatesLifetimes(kind models.Kind) bool {
	if kinds := m.statedKinds.Load(); kinds != nil {
		return (*kinds)[kind]
	}
	return false
}

// noteStatedLifetimes adds the kinds of which a mock of the slices states its
// lifetime to statedKinds. Under swapMu.
func (m *MockManager) noteStatedLifetimes(slices ...[]*models.Mock) {
	cur := m.statedKinds.Load()
	var next map[models.Kind]bool
	for _, ms := range slices {
		for _, mk := range ms {
			if mk == nil || !mk.StatesLifetime() {
				continue
			}
			if cur != nil && (*cur)[mk.Kind] {
				continue
			}
			if next == nil {
				next = map[models.Kind]bool{}
				if cur != nil {
					for k := range *cur {
						next[k] = true
					}
				}
			}
			next[mk.Kind] = true
		}
	}
	if next != nil {
		m.statedKinds.Store(&next)
	}
}

// GetConnectionMocks returns connection-scoped mocks (Lifetime ==
// LifetimeConnection) whose Spec.Metadata["connID"] matches the
// caller-supplied connID. Phase 2.5 plumbing: each connID owns its own
// TreeDb in m.connectionTrees for O(1) lookup; if the connection has
// no dedicated tree yet (legacy recordings pre-dating the tag
// convention, or mocks loaded before AddConnectionMock was wired in)
// we fall back to a filtered scan of the unfiltered tree so the result
// stays correct even on not-yet-migrated data.
//
// Semantics:
//   - Empty connID returns nil (callers that lack a connID should go
//     straight to session / per-test).
//   - An empty per-connID tree returns nil (not an error); callers
//     fall through to the session / per-test pools.
//   - Every successful read touches connectionLastTs so the
//     idle-sweeper doesn't reclaim an active connection.
func (m *MockManager) GetConnectionMocks(connID string) ([]*models.Mock, error) {
	mocks, err := m.connectionMocks(connID)
	// Under exact serving a connection mock a parser evicted is gone, as from
	// every other read (unspent).
	if v := m.exactView(); v != nil && err == nil {
		mocks = v.withLearned(v.unspent(mocks))
	}
	return mocks, err
}

func (m *MockManager) connectionMocks(connID string) ([]*models.Mock, error) {
	if connID == "" {
		return nil, nil
	}
	// Post-Close no-op: honour the Close() contract so a late reader
	// (matcher still draining, delayed goroutine) doesn't mutate
	// connectionTrees / connectionLastTs / noConnMocks after the
	// manager has been logically released. Returning nil is safe:
	// matchers interpret an empty slice as "no connection-scoped
	// mocks for this connID" and fall through to the session/per-test
	// pools, which is exactly what a shut-down manager should expose.
	if m.closed.Load() {
		return nil, nil
	}
	// Negative cache: if a prior fallback scan confirmed this connID
	// has no connection-scoped mocks, skip the O(N) walk. Entries get
	// cleared by AddConnectionMock so a late arrival still seeds the
	// tree properly, and by SweepIdleConnections via the shared
	// connectionLastTs timestamp (touched here so a long-lived but
	// always-empty connID is still eligible for idle reaping rather
	// than living forever in the sync.Map).
	if _, none := m.noConnMocks.Load(connID); none {
		m.connMu.Lock()
		m.connectionLastTs[connID] = time.Now()
		m.connMu.Unlock()
		return nil, nil
	}
	m.connMu.RLock()
	tree := m.connectionTrees[connID]
	m.connMu.RUnlock()

	if tree != nil {
		results := make([]*models.Mock, 0, 8)
		tree.rangeValues(func(v interface{}) bool {
			if mock, ok := v.(*models.Mock); ok && mock != nil {
				results = append(results, mock)
			}
			return true
		})
		// Touch last-seen so the idle-sweeper leaves this connID alone.
		m.connMu.Lock()
		m.connectionLastTs[connID] = time.Now()
		m.connMu.Unlock()
		return results, nil
	}

	// Fallback path for unmigrated recordings — scan the unfiltered
	// tree and materialise matches on demand. First-time discovery
	// also seeds the dedicated tree so subsequent lookups for this
	// connID take the O(1) path.
	//
	// Checks the connID tag BEFORE the typed Lifetime so mocks that
	// somehow reach the pool without DeriveLifetime having set their
	// Lifetime (inline test constructions, legacy recordings, any
	// bypass of the standard ingest flow) still surface here as long
	// as they carry the connID tag. We additionally accept either the
	// typed Lifetime or the raw "connection" metadata tag as the
	// connection-scope signal — either alone is sufficient.
	m.treesMu.RLock()
	unf := m.unfiltered
	m.treesMu.RUnlock()
	results := make([]*models.Mock, 0, 8)
	unf.rangeValues(func(v interface{}) bool {
		mock, ok := v.(*models.Mock)
		if !ok || mock == nil {
			return true
		}
		if mock.Spec.Metadata == nil || mock.Spec.Metadata["connID"] != connID {
			return true
		}
		if mock.TestModeInfo.Lifetime != models.LifetimeConnection &&
			mock.Spec.Metadata["type"] != "connection" {
			return true
		}
		results = append(results, mock)
		return true
	})
	if len(results) > 0 {
		m.connMu.Lock()
		if m.connectionTrees[connID] == nil {
			seeded := NewTreeDb(customComparator)
			for _, mk := range results {
				seeded.insert(mk.TestModeInfo, mk)
			}
			m.connectionTrees[connID] = seeded
			m.connectionLastTs[connID] = time.Now()
		}
		m.connMu.Unlock()
	} else {
		// Seed the negative cache so a later lookup for the same
		// connID takes the fast-no-op path instead of walking the
		// unfiltered tree again. Cleared by AddConnectionMock below,
		// or reaped by SweepIdleConnections via connectionLastTs age.
		m.noConnMocks.Store(connID, struct{}{})
		m.connMu.Lock()
		m.connectionLastTs[connID] = time.Now()
		m.connMu.Unlock()
	}
	return results, nil
}

// AddConnectionMock inserts a connection-scoped mock into the dedicated
// per-connID tree. Parsers that record prepared-statement setup
// (Postgres Parse, MySQL COM_STMT_PREPARE) call this in addition to
// whatever normal recording path emits the mock, so replay has an
// O(1) lookup by connID.
//
// connID is derived from mock.Spec.Metadata["connID"]. A mock without
// a connID or without LifetimeConnection is silently dropped —
// callers are expected to check before calling.
func (m *MockManager) AddConnectionMock(mock *models.Mock) {
	if mock == nil || mock.TestModeInfo.Lifetime != models.LifetimeConnection {
		return
	}
	if m.closed.Load() {
		// Ignore mutations after Close so a matcher racing with
		// Proxy session rotation can't silently scribble into a
		// manager the agent has already retired.
		return
	}
	var connID string
	if mock.Spec.Metadata != nil {
		connID = mock.Spec.Metadata["connID"]
	}
	if connID == "" {
		return
	}
	m.connMu.Lock()
	tree := m.connectionTrees[connID]
	if tree == nil {
		tree = NewTreeDb(customComparator)
		m.connectionTrees[connID] = tree
	}
	mock.MarkPooled() // matchers can reach it from here on; see models.Mock.pooled
	tree.insert(mock.TestModeInfo, mock)
	m.connectionLastTs[connID] = time.Now()
	m.connMu.Unlock()
	// Invalidate the negative cache so a reader that previously
	// memoised "no connection mocks for this connID" revisits the
	// dedicated tree on the next GetConnectionMocks call.
	m.noConnMocks.Delete(connID)
}

// DeleteConnection tears down the per-connID pool on EOF/Close from
// the parser. Safe to call with an unknown connID (no-op). Recording
// anywhere inside the agent is expected to call this when a client
// closes its connection so the pool doesn't hold mocks for dead
// connections indefinitely.
func (m *MockManager) DeleteConnection(connID string) {
	if connID == "" {
		return
	}
	m.connMu.Lock()
	delete(m.connectionTrees, connID)
	delete(m.connectionLastTs, connID)
	m.connMu.Unlock()
	// Drop the negative-cache entry too; an identical connID
	// reassigned later should re-scan rather than stay memoised to
	// the dead connection's state.
	m.noConnMocks.Delete(connID)
}

// SweepIdleConnections reclaims connection-scoped pools whose last
// activity timestamp exceeded connectionIdleRetention. Intended to be
// called periodically from the agent main loop (cheap — one map scan
// under connMu). Returns the number of reclaimed connIDs for logging.
func (m *MockManager) SweepIdleConnections() int {
	if m.closed.Load() {
		// Post-Close the goroutine should already have exited; this
		// guard covers a paranoid caller that invokes the method
		// directly while a concurrent Close is in flight.
		return 0
	}
	cutoff := time.Now().Add(-connectionIdleRetention())
	reclaimed := 0
	var staleConnIDs []string
	m.connMu.Lock()
	for connID, ts := range m.connectionLastTs {
		if ts.Before(cutoff) {
			delete(m.connectionTrees, connID)
			delete(m.connectionLastTs, connID)
			staleConnIDs = append(staleConnIDs, connID)
			reclaimed++
		}
	}
	m.connMu.Unlock()
	// Drop negative-cache entries for reaped connIDs so the sync.Map
	// does not grow unbounded on long-running agents that see many
	// short-lived connections without any connection-scoped mocks.
	// Negative-cache hits touch connectionLastTs above, so this walk
	// finds every stale entry without requiring a second timestamp
	// keyed by noConnMocks itself.
	for _, connID := range staleConnIDs {
		m.noConnMocks.Delete(connID)
	}
	return reclaimed
}

// SessionMockHitCounts returns a snapshot of the match counts of every
// session- or connection-scoped mock currently in memory, keyed by
// mock.Name: the matches MarkMockAsUsed has counted for that name in this
// test set, summed over the mocks that share it (see hitIdx). Per-test mocks are
// excluded — they are consumed on match, so their count is 0 or 1 and adds
// no observability value.
//
// Intended for replay-report output and "which reusable mocks
// actually got reused?" debugging — non-zero values confirm tagging
// is working; zero values on long-lived session mocks hint at dead
// recordings worth re-capturing.
func (m *MockManager) SessionMockHitCounts() map[string]uint64 {
	out := make(map[string]uint64, 32)
	m.treesMu.RLock()
	tree := m.unfiltered
	m.treesMu.RUnlock()
	tree.rangeValues(func(v interface{}) bool {
		mock, ok := v.(*models.Mock)
		if !ok || mock == nil {
			return true
		}
		if mock.TestModeInfo.Lifetime == models.LifetimePerTest {
			return true
		}
		out[mock.Name] = 0
		return true
	})
	if len(out) == 0 {
		return out
	}
	m.hitMu.RLock()
	for name := range out {
		if n := m.hitIdx[name]; n != nil {
			out[name] = n.Load()
		}
	}
	m.hitMu.RUnlock()
	return out
}

// NEW: kind-scoped getters used by Redis matcher
// HasMocksByKind reports whether any loaded mock of the given kind satisfies
// match, across all tiers (unfiltered, filtered, startup, carry-over). It
// reads the live trees on every call — no caching — so it stays correct as
// the mock set is replaced between test-sets. Without exact serving it
// returns on the first match and allocates nothing. A nil per-kind tree means
// no such mocks were set.
//
// Under exact serving (KEPLOY_PROVEN) it answers from what the open view
// shows, as every read does: the open test's own mocks that have not
// answered, which the view alone serves, then the staged and carry-over
// mocks the view shows; a staged mock the view hides (another test's) does
// not count. It then collects a tree's matches and narrows them once the
// tree's lock is released.
func (m *MockManager) HasMocksByKind(kind models.Kind, match func(*models.Mock) bool) bool {
	m.treesMu.RLock()
	flt := m.filteredByKind[kind]
	unf := m.unfilteredByKind[kind]
	startup := m.startup
	m.treesMu.RUnlock()
	v := m.exactView()
	if v != nil {
		for _, mk := range v.unanswered(kind) {
			if match(mk) {
				return true
			}
		}
	}

	anyMatch := func(tree *TreeDb, requireKind bool) bool {
		if tree == nil {
			return false
		}
		found := false
		// Under exact serving the matches are collected and narrowed to what
		// the view shows once the tree's lock is released, as every exact
		// read narrows.
		var matched []*models.Mock
		tree.rangeValues(func(val interface{}) bool {
			mock, ok := val.(*models.Mock)
			if !ok || mock == nil {
				return true
			}
			if requireKind && mock.Kind != kind {
				return true
			}
			if match(mock) {
				if v != nil {
					matched = append(matched, mock)
					return true
				}
				found = true
				return false // stop ranging
			}
			return true
		})
		return found || (v != nil && len(v.keep(matched)) > 0)
	}

	// Per-kind trees already hold only this kind; the startup tree is mixed.
	if anyMatch(unf, false) || anyMatch(flt, false) || anyMatch(startup, true) {
		return true
	}
	if models.CarryOverKind(kind) {
		carried := m.carry.snapshot(kind)
		if v != nil {
			carried = v.keep(carried)
		}
		for _, mk := range carried {
			if match(mk) {
				return true
			}
		}
	}
	return false
}

func (m *MockManager) GetFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	// Fetch pointer safely; the tree itself is responsible for its own safety.
	m.treesMu.RLock()
	flt := m.filteredByKind[kind]
	m.treesMu.RUnlock()
	if flt == nil {
		flt, _ = m.ensureKindTrees(kind)
	}

	if v := m.exactView(); v != nil {
		return m.exactPerTest(v, kind, v.shownOf(flt, "")), nil
	}
	results := make([]*models.Mock, 0, 64)
	flt.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

func (m *MockManager) GetUnFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	m.treesMu.RLock()
	unf := m.unfilteredByKind[kind]
	m.treesMu.RUnlock()
	if unf == nil {
		_, unf = m.ensureKindTrees(kind)
	}

	if v := m.exactView(); v != nil {
		return v.reusable(kind, v.shownOf(unf, ""), v.shownOf(m.startupTree(), kind)), nil
	}
	results := make([]*models.Mock, 0, 128)
	unf.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

// ---------- setters (populate both legacy + per-kind) ----------

func (m *MockManager) SetFilteredMocks(mocks []*models.Mock) {
	m.swapMu.Lock()
	defer m.swapMu.Unlock()
	ownForStaging(&mocks)
	m.stampSet(mocks)
	m.publishFiltered(m.buildTier(mocks, true), true)
}

// stampSet stamps the current set generation on the mocks a bare tier setter
// pools, as a staging stamps what it pools (TestModeInfo.StagedSet), so a
// mock an earlier set's staging stamped and handed in again is this set's.
func (m *MockManager) stampSet(mocks []*models.Mock) {
	gen := m.setGen.Load()
	for _, mk := range mocks {
		if mk != nil {
			mk.TestModeInfo.StagedSet = gen
		}
	}
}

// tierBuild is a filtered or unfiltered tier built from a staged slice and
// not yet published. Building stamps the staged mocks' tree IDs and sort
// orders; publishing only swaps pointers. setMocksWithWindowLocked builds
// every tier before it publishes any, so no reader of a published tier can
// see a mock being stamped. Publishing with bump=false lets it publish one
// revision for all three tiers (see the note there).
type tierBuild struct {
	tree         *TreeDb
	byKind       map[models.Kind]*TreeDb
	stateless    map[models.Kind]map[string][]*models.Mock
	touched      map[models.Kind]struct{}
	maxSortOrder int64
	inputs       int
	v3Names      []string // filtered tier, for the DEBUG_TRACE line
	v3Trace      bool
}

func (m *MockManager) buildTier(mocks []*models.Mock, filtered bool) tierBuild {
	b := tierBuild{
		tree:      NewTreeDb(customComparator),
		byKind:    make(map[models.Kind]*TreeDb),
		stateless: make(map[models.Kind]map[string][]*models.Mock),
		touched:   map[models.Kind]struct{}{},
		inputs:    len(mocks),
	}
	// DEBUG_TRACE: collect PostgresV3 mock names in the main loop only
	// when Debug logging is enabled, so non-Debug runs don't allocate
	// the slice or do the per-mock kind check beyond what the swap
	// already does. The Check() call short-circuits below if the level
	// is filtered out.
	b.v3Trace = filtered && m.logger != nil && m.logger.Core().Enabled(zap.DebugLevel)
	if b.v3Trace {
		b.v3Names = make([]string, 0, 8)
	}
	for index, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		if mock.TestModeInfo.SortOrder > b.maxSortOrder {
			b.maxSortOrder = mock.TestModeInfo.SortOrder
		}
		mock.TestModeInfo.ID = index
		b.tree.insert(mock.TestModeInfo, mock)
		k := mock.Kind
		td := b.byKind[k]
		if td == nil {
			td = NewTreeDb(customComparator)
			b.byKind[k] = td
		}
		td.insert(mock.TestModeInfo, mock)
		b.touched[k] = struct{}{}
		if b.stateless[k] == nil {
			b.stateless[k] = make(map[string][]*models.Mock)
		}
		key := mock.Name
		if mock.Kind == models.DNS && mock.Spec.DNSReq != nil {
			key = strings.ToLower(dns.Fqdn(mock.Spec.DNSReq.Name))
		}
		b.stateless[k][key] = append(b.stateless[k][key], mock)
		if b.v3Trace && mock.Kind == models.PostgresV3 {
			b.v3Names = append(b.v3Names, mock.Name)
		}
	}
	return b
}

func (m *MockManager) publishFiltered(b tierBuild, bump bool) {
	if b.maxSortOrder > 0 {
		pkg.UpdateSortCounterIfHigher(b.maxSortOrder)
	}
	m.treesMu.Lock()
	// A kind that LEAVES the pool must be bumped too: `touched` is built from
	// the incoming slice only, so a kind present before and absent now keeps
	// its old revision — and a consumer caching by that revision never learns
	// its mocks are gone and keeps serving them.
	for k := range m.filteredByKind {
		b.touched[k] = struct{}{}
	}
	m.filtered, m.filteredByKind, m.statelessFiltered = b.tree, b.byKind, b.stateless
	m.treesMu.Unlock()
	if bump {
		for k := range b.touched {
			m.bumpRevisionKind(k)
		}
		m.bumpRevisionAll()
	}
	// DEBUG_TRACE: prove what landed in m.filtered after the swap +
	// bump. Lists every PostgresV3 mock so we can correlate by mock name
	// against the failing hashes the parser later misses. Gated behind
	// logger.Check so non-Debug runs pay nothing for the fmt.Sprintf
	// + Revision() read.
	if b.v3Trace {
		if ce := m.logger.Check(zap.DebugLevel, "DEBUG_TRACE SetFilteredMocks: tree swapped + revision bumped"); ce != nil {
			ce.Write(
				zap.String("mockManagerPtr", fmt.Sprintf("%p", m)),
				zap.Int("inputMocks", b.inputs),
				zap.Int("postgresV3Mocks", len(b.v3Names)),
				zap.Strings("postgresV3MockNames", b.v3Names),
				zap.Uint64("newRevision", m.Revision()),
			)
		}
	}
}

func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	m.swapMu.Lock()
	defer m.swapMu.Unlock()
	ownForStaging(&mocks)
	m.stampSet(mocks)
	m.publishUnfiltered(m.buildTier(mocks, false), true)
}

func (m *MockManager) publishUnfiltered(b tierBuild, bump bool) {
	if b.maxSortOrder > 0 {
		pkg.UpdateSortCounterIfHigher(b.maxSortOrder)
	}
	m.treesMu.Lock()
	// Same as the filtered path: bump kinds leaving the pool, not just those
	// arriving, or a consumer caches a stale index for a kind that emptied.
	for k := range m.unfilteredByKind {
		b.touched[k] = struct{}{}
	}
	m.unfiltered, m.unfilteredByKind, m.statelessUnfiltered = b.tree, b.byKind, b.stateless
	m.treesMu.Unlock()
	if bump {
		for k := range b.touched {
			m.bumpRevisionKind(k)
		}
		m.bumpRevisionAll()
	}
}

// ---------- point updates / deletes (keep per-kind in sync) ----------

// mirrorStartup keeps the startup tier's copy of a recording in step with the
// session tier's. Staging at BaseTime files the session pool in BOTH tiers as
// one shared pointer (see SetMocksWithWindow's isInitialStaging branch), and
// GetSessionMocks — every parser's view of the reusable pool — lists the two
// tiers as one, telling them apart by pointer. When a match replaced the
// session tier's entry with an updated copy (a fresh sort order, learned
// noise) and left the startup tier holding the old pointer, every matched
// recording was listed twice from then on: a stateful dependency's cursor
// counted each duplicate as another recording and replayed a 1,2,3 sequence as
// 1,1,2,2,3, and a reader of the startup tier never saw what the match learned.
// So the startup tier now holds the same copy as the session tier: replaced is
// the copy the session tier held until this update. Called under mirrorMu.
func (m *MockManager) mirrorStartup(replaced, new *models.Mock) {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()
	key, ok := m.startupKeys[replaced]
	if !ok || m.startup == nil {
		return
	}
	if m.startup.swapValue(key, replaced, new) {
		delete(m.startupKeys, replaced)
		m.startupKeys[new] = key
	}
}

// mirrorStartupDelete removes from the startup tier the mock a delete just
// took out of the session tier, when the startup tier holds it too (see
// mirrorStartup); otherwise the deleted recording stayed in the pool every
// parser reads through GetSessionMocks. Called under mirrorMu.
func (m *MockManager) mirrorStartupDelete(removed *models.Mock) {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()
	key, ok := m.startupKeys[removed]
	if !ok || m.startup == nil {
		return
	}
	if m.startup.removeValue(key, removed) {
		delete(m.startupKeys, removed)
	}
}

func (m *MockManager) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	return m.updateUnFilteredMock(old, new, nil)
}

// updateUnFilteredMock is UpdateUnFilteredMock, settled under exact serving
// only while the store is at g's round when g is set (WithinRound).
func (m *MockManager) updateUnFilteredMock(old *models.Mock, new *models.Mock, g *roundGuard) bool {
	// The copy the parser reports carries what it detected and what the
	// learn step learned onto the recording; the exact view knows it by
	// name, as it knows old.
	served := new
	if served == nil {
		served = old
	}
	return m.consumeAt(served, false, g,
		func() bool { return m.updateUnFilteredTier(old, new) },
		func() { m.observeServed(new, g.flight()) })
}

// updateUnFilteredTier files new over old in the session tier, and reports
// whether it was consumed. It may run holding the round lock (consumeAt).
func (m *MockManager) updateUnFilteredTier(old *models.Mock, new *models.Mock) bool {
	if old != nil && m.stalePooled(old) {
		return false
	}
	// Snapshot the legacy tree pointer safely
	m.treesMu.RLock()
	globalTree := m.unfiltered
	m.treesMu.RUnlock()
	new.MarkPooled() // matchers can reach it from here on; see models.Mock.pooled
	shared := m.startupShared.Load()
	if shared {
		m.mirrorMu.Lock()
	}
	// Update legacy/global tree first (it keeps its window index right; see
	// TreeDb.update)
	updatedGlobal, replaced := globalTree.updateReplacing(old.TestModeInfo, new.TestModeInfo, new, *old)
	if updatedGlobal && replaced != nil && provenswitch.On() && m.exactView() != nil {
		// What the staging put in the trees, before this staging's first
		// filing of a copy of it: the base its retry cycles are served
		// (keptOverlay.onto).
		m.exact.kept.filed.note(m.stagingEpoch.Load(), replaced)
	}
	if shared {
		if updatedGlobal && replaced != nil {
			// The copy the tree held, which is not always the caller's old: a
			// concurrent match of the same recording may have replaced it
			// first. Mirrored under the same lock as the update, so two
			// matches cannot mirror out of order.
			m.mirrorStartup(replaced, new)
		}
		m.mirrorMu.Unlock()
	}

	oldK, newK := old.Kind, new.Kind
	var updatedOldKind, updatedNewKind bool

	if oldK == newK {
		// Same kind: update the per-kind tree under lock
		_, unf := m.ensureKindTrees(newK)
		m.treesMu.Lock()
		updatedNewKind = unf.update(old.TestModeInfo, new.TestModeInfo, new, *old)

		// Self-heal if global updated but per-kind missed.
		//
		// This is the legitimate filtered→unfiltered promotion case: a
		// parser (Postgres v2 matcher is the load-bearing caller) calls
		// UpdateUnFilteredMock on a mock that lived only in the filtered
		// pool, so the unfiltered per-kind tree was never seeded with
		// it. The insert below brings per-kind into sync with global so
		// future per-kind fast-path reads (GetMySQLCounts,
		// GetPostgresSessionMocks, etc.) see the consumed mock. Kept at
		// Debug because it fires dozens of times per normal replay and
		// is not user-actionable.
		if updatedGlobal && !updatedNewKind {
			if m.logger != nil && m.logger.Core().Enabled(zap.DebugLevel) {
				m.logger.Debug("seeding per-kind unfiltered tree from global on first update",
					zap.String("kind", string(newK)),
					zap.String("mockName", new.Name),
					zap.Any("testModeInfo", new.TestModeInfo),
				)
			}
			unf.insert(new.TestModeInfo, new)
			updatedNewKind = true
		}
		m.treesMu.Unlock()
	} else {
		// Kind changed: remove from old kind tree, insert/update in new kind tree under one lock
		_, oldUnf := m.ensureKindTrees(oldK)
		_, newUnf := m.ensureKindTrees(newK)
		m.treesMu.Lock()
		updatedOldKind = oldUnf.delete(old.TestModeInfo)
		updatedNewKind = newUnf.update(old.TestModeInfo, new.TestModeInfo, new, *old)
		if !updatedNewKind {
			newUnf.insert(new.TestModeInfo, new)
			updatedNewKind = true
		}
		m.treesMu.Unlock()
		if m.logger != nil {
			m.logger.Info("moved mock across kinds",
				zap.String("mockName", new.Name),
				zap.String("fromKind", string(oldK)),
				zap.String("toKind", string(newK)),
			)
		}
	}

	// Mark usage if global changed (legacy behavior)
	if updatedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:             new.Name,
			Kind:             new.Kind,
			Usage:            models.Updated,
			IsFiltered:       new.TestModeInfo.IsFiltered,
			SortOrder:        new.TestModeInfo.SortOrder,
			Type:             new.Spec.Metadata["type"],
			DataPlane:        dataPlaneTyped(new),
			Lifetime:         new.TestModeInfo.Lifetime,
			ReqTimestampMock: models.FormatMockTimestamp(new.Spec.ReqTimestampMock),
			ResTimestampMock: models.FormatMockTimestamp(new.Spec.ResTimestampMock),
			AnswerPlaced:     models.IsAnswerPlaced(new),
			ReqBodyNoise:     reqBodyNoiseOf(new.Spec),
			Variables:        variablesOf(new.Spec),
			Clocks:           clocksOf(new.Spec),
			Binds:            bindsOf(new.Spec),
			LearnedAfterEnd:  m.learnedAfterEnd(old, new),
		}, old.TestModeInfo.StagedSet); err != nil {
			if errors.Is(err, errPreviousSet) {
				// The previous set's pool: not this set's consume.
				updatedGlobal = false
			} else {
				m.logger.Error("failed to flag mock as used", zap.Error(err))
			}
		}
	}

	// Bump revisions accurately:
	// - global only if the global tree changed
	// - per-kind only for kinds whose per-kind tree changed
	if oldK != newK {
		if updatedOldKind {
			m.bumpRevisionKind(oldK)
		}
		if updatedNewKind {
			m.bumpRevisionKind(newK)
		}
	} else if updatedNewKind {
		m.bumpRevisionKind(newK)
	}
	if updatedGlobal {
		m.bumpRevisionAll()
	}
	return updatedGlobal
}

func (m *MockManager) DeleteFilteredMock(mock models.Mock) bool {
	return m.deleteFilteredMock(mock, nil)
}

// deleteFilteredMock is DeleteFilteredMock, settled under exact serving only
// while the store is at g's round when g is set (WithinRound).
func (m *MockManager) deleteFilteredMock(mock models.Mock, g *roundGuard) bool {
	return m.consumeValueAt(mock, true, g, true, m.deleteFilteredTier)
}

// deleteFilteredTier takes mock out of its tier, the per-test one first, and
// reports whether it was consumed.
func (m *MockManager) deleteFilteredTier(mock models.Mock) bool {
	if m.stalePooled(&mock) {
		return false
	}
	m.treesMu.RLock()
	globalTree := m.filtered
	m.treesMu.RUnlock()
	// Identity-checked: the key is tier-local, so a mock taken from another
	// tier addresses a DIFFERENT mock here. See TreeDb.sameMock.
	deletedGlobal, removed := globalTree.deleteMockReturning(mock.TestModeInfo, mock)

	// per-kind
	k := mock.Kind
	flt, _ := m.ensureKindTrees(k)
	m.treesMu.Lock()
	deletedKind := flt.deleteMock(mock.TestModeInfo, mock)
	m.treesMu.Unlock()

	// consumed: the per-test tree held it and the consume counts. A
	// carry-over mock the app consumed as it started is not consumed again
	// (bootConsumed: a copy test-1's staging filed before that consume
	// landed), and a mock the previous set pooled is not this set's
	// (errPreviousSet): either leaves the tree without it and reports it not
	// consumed, so the caller matches again against the pool.
	consumed := deletedGlobal
	if deletedGlobal && models.IsCarryOver(&mock) && m.bootConsumed(&mock) {
		consumed = false
	}
	if consumed {
		if err := m.flagMockAsUsed(models.MockState{
			Name:             mock.Name,
			Kind:             mock.Kind,
			Usage:            models.Deleted,
			IsFiltered:       mock.TestModeInfo.IsFiltered,
			SortOrder:        mock.TestModeInfo.SortOrder,
			Type:             mock.Spec.Metadata["type"],
			DataPlane:        dataPlaneTyped(&mock),
			Lifetime:         mock.TestModeInfo.Lifetime,
			ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
			ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
			AnswerPlaced:     models.IsAnswerPlaced(&mock),
			ReqBodyNoise:     reqBodyNoiseOf(mock.Spec),
			Variables:        variablesOf(mock.Spec),
			Clocks:           clocksOf(mock.Spec),
			Binds:            bindsOf(mock.Spec),
			LearnedAfterEnd:  m.learnedAfterEndAt(removed, &mock),
		}, mock.TestModeInfo.StagedSet); err != nil {
			if errors.Is(err, errPreviousSet) {
				consumed = false
			} else {
				m.logger.Error("failed to flag mock as used", zap.Error(err))
			}
		}
		// The log refuses a mock an earlier set pooled itself, as the flag
		// does (answeredLog.note).
		m.noteAnswered(mock)
	}

	// Bump per-kind only if that tree changed; global only if global changed
	if deletedKind {
		m.bumpRevisionKind(k)
	}
	if deletedGlobal {
		m.bumpRevisionAll()
	}
	if deletedGlobal {
		return consumed
	}

	// Not in the per-test tier. Fall back to the startup tier before reporting
	// failure, because the caller almost certainly matched this mock out of
	// GetSessionMocks — which is startup UNION session — and a startup-tier
	// mock has no other consume door: mongo v2 is the ONLY parser in the tree
	// that calls DeleteStartupMock itself.
	//
	// This matters far more than it looks. Every other consume site treats a
	// false here as "another goroutine won the race, retry against the shrunk
	// pool" and loops: http/match.go's `for {}` re-fetches the pool and
	// continues, and generic, grpcV2, http2, mongo v1, sqs and kafka all have
	// the same shape. The pool does not shrink, so the retry re-picks the same
	// mock and spins until the request context dies. Before the tier keys were
	// disambiguated the blind delete always "succeeded" — evicting the WRONG
	// mock — which is what accidentally kept those loops terminating.
	//
	// Returning true here keeps the caller contract ("consumed, stop looping")
	// exactly as it was, while the mock that actually gets consumed is now the
	// one the caller matched instead of whatever shared its key.
	if m.deleteStartupMock(mock, nil, false) {
		return true
	}
	// Last, the carry-over tier (models.CarryOverKind kinds only): a mock
	// reachable outside its own window, consumed by whichever test the
	// application happened to publish it in. Deleted, like any consumed
	// per-test mock, so the agent's next load filters it out; CarryOver, so
	// the replay attributes it to its own window.
	if models.CarryOverKind(mock.Kind) {
		if mk, ok := m.carry.take(mock); ok {
			// Not one the app consumed as it started, and only of this set's
			// pool, as at the per-test door above.
			if m.bootConsumed(mk) {
				m.bumpRevisionKind(mk.Kind)
				m.bumpRevisionAll()
				return false
			}
			if err := m.flagMockAsUsed(models.MockState{
				Name:             mk.Name,
				Kind:             mk.Kind,
				Usage:            models.Deleted,
				IsFiltered:       mk.TestModeInfo.IsFiltered,
				SortOrder:        mk.TestModeInfo.SortOrder,
				Type:             mk.Spec.Metadata["type"],
				DataPlane:        dataPlaneTyped(mk),
				Lifetime:         mk.TestModeInfo.Lifetime,
				ReqTimestampMock: models.FormatMockTimestamp(mk.Spec.ReqTimestampMock),
				ResTimestampMock: models.FormatMockTimestamp(mk.Spec.ResTimestampMock),
				AnswerPlaced:     models.IsAnswerPlaced(mk),
				ReqBodyNoise:     reqBodyNoiseOf(mock.Spec),
				Variables:        variablesOf(mock.Spec),
				Clocks:           clocksOf(mock.Spec),
				Binds:            bindsOf(mock.Spec),
				LearnedAfterEnd:  m.learnedAfterEnd(m.servedAs(mk), &mock),
				CarryOver:        true,
			}, mk.TestModeInfo.StagedSet); err != nil {
				if errors.Is(err, errPreviousSet) {
					m.bumpRevisionKind(mk.Kind)
					m.bumpRevisionAll()
					return false
				}
				m.logger.Error("failed to flag carry-over mock as used", zap.Error(err))
			}
			m.bumpRevisionKind(mk.Kind)
			m.bumpRevisionAll()
			return true
		}
	}
	return false
}

func (m *MockManager) DeleteUnFilteredMock(mock models.Mock) bool {
	return m.deleteUnFilteredMock(mock, nil)
}

// deleteUnFilteredMock is DeleteUnFilteredMock, settled under exact serving
// only while the store is at g's round when g is set (WithinRound).
func (m *MockManager) deleteUnFilteredMock(mock models.Mock, g *roundGuard) bool {
	return m.consumeValueAt(mock, true, g, true, m.deleteUnFilteredTier)
}

// deleteUnFilteredTier takes mock out of the session tier, and reports
// whether it was consumed.
func (m *MockManager) deleteUnFilteredTier(mock models.Mock) bool {
	if m.stalePooled(&mock) {
		return false
	}
	m.treesMu.RLock()
	globalTree := m.unfiltered
	m.treesMu.RUnlock()
	shared := m.startupShared.Load()
	if shared {
		m.mirrorMu.Lock()
	}
	// Identity-checked for the same reason as DeleteFilteredMock: the key is
	// tier-local, so a mock from another tier addresses a different entry here.
	deletedGlobal, removed := globalTree.deleteMockReturning(mock.TestModeInfo, mock)
	if shared {
		if deletedGlobal && removed != nil {
			m.mirrorStartupDelete(removed)
		}
		m.mirrorMu.Unlock()
	}

	// per-kind
	k := mock.Kind
	_, unf := m.ensureKindTrees(k)
	m.treesMu.Lock()
	deletedKind := unf.deleteMock(mock.TestModeInfo, mock)
	m.treesMu.Unlock()

	if deletedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:             mock.Name,
			Kind:             mock.Kind,
			Usage:            models.Deleted,
			IsFiltered:       mock.TestModeInfo.IsFiltered,
			SortOrder:        mock.TestModeInfo.SortOrder,
			Type:             mock.Spec.Metadata["type"],
			DataPlane:        dataPlaneTyped(&mock),
			Lifetime:         mock.TestModeInfo.Lifetime,
			ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
			ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
			AnswerPlaced:     models.IsAnswerPlaced(&mock),
			ReqBodyNoise:     reqBodyNoiseOf(mock.Spec),
			Variables:        variablesOf(mock.Spec),
			Clocks:           clocksOf(mock.Spec),
			Binds:            bindsOf(mock.Spec),
			LearnedAfterEnd:  m.learnedAfterEndAt(removed, &mock),
		}, mock.TestModeInfo.StagedSet); err != nil {
			if errors.Is(err, errPreviousSet) {
				// The previous set's pool: not this set's consume. The
				// tree changed all the same.
				m.bumpRevisionAll()
				deletedGlobal = false
			} else {
				m.logger.Error("failed to flag mock as used", zap.Error(err))
			}
		}
	}

	// Bump per-kind only if that tree changed; global only if global changed
	if deletedKind {
		m.bumpRevisionKind(k)
	}
	if deletedGlobal {
		m.bumpRevisionAll()
	}
	return deletedGlobal
}

// DeleteStartupMock removes a matched startup-tier mock from the
// startup tree so the next identical query served from that tier (at
// boot phase, or by the startup-tier rescue after a test's per-test pool
// misses) picks the next-recorded same-shape mock in chronological
// order. This is the boot-path analogue of DeleteFilteredMock for the
// startup tier.
//
// Required for boot-phase replay correctness: when an application
// issues the same query repeatedly during recording while DB state
// mutates, the recorder captures multiple same-shape mocks with
// diverging responses. The mongo/v2 matcher (keploy/integrations,
// pkg/mongo/v2/match.go) serves startup-tier candidates that score the
// same earliest-recorded first, at boot phase and in the startup-tier
// rescue; this consumer advances through that order so the booting app
// sees the same response chain it saw at record time. Without
// consumption, the matcher repeatedly picks the same earliest mock and
// follow-on revalidation queries fail.
//
// Implementation note: the startup tree is keyed by a tier-local
// TestModeInfo copy (see the comment on the SetMocksWithWindow branch
// that builds m.startup), so a direct key-based delete using
// mock.TestModeInfo from the caller would miss. Instead, scan by name
// to find the live tier-local key, then delete via that key.
//
// Returns true if the mock was found and removed; false if the mock
// was never in the startup tree (e.g. it was a filtered/session/
// connection-tier mock, or a prior DeleteStartupMock already consumed
// it).
func (m *MockManager) DeleteStartupMock(mock models.Mock) bool {
	return m.deleteStartupMock(mock, nil, true)
}

// deleteStartupMock is DeleteStartupMock, telling the served observer only
// with observe (a caller that told it already does not), and settled under
// exact serving only while the store is at g's round when g is set
// (WithinRound).
func (m *MockManager) deleteStartupMock(mock models.Mock, g *roundGuard, observe bool) bool {
	if mock.Name == "" {
		// The observer is told of no unnamed mock.
		return false
	}
	return m.consumeValueAt(mock, true, g, observe, m.deleteStartupTier)
}

// deleteStartupTier takes mock out of the startup tier, and reports whether
// it was consumed.
func (m *MockManager) deleteStartupTier(mock models.Mock) bool {
	if m.stalePooled(&mock) {
		return false
	}
	m.treesMu.RLock()
	tree := m.startup
	m.treesMu.RUnlock()
	return m.deleteStartupMockFrom(tree, mock)
}

// stalePooled reports whether mk was pooled for an earlier test set
// (TestModeInfo.StagedSet): a consume door refuses it before it touches a
// tree, since a door that matches by name would take this set's same-named
// mock for it.
func (m *MockManager) stalePooled(mk *models.Mock) bool {
	g := mk.TestModeInfo.StagedSet
	return g != 0 && g != m.setGen.Load()
}

// deleteStartupMockFrom is deleteStartupMock on tree, the startup tier the
// caller read: a consume that read it before a staging replaced it consumes
// from it, and is judged by it (TreeDb.bootBuilt, the generation of the mock
// it holds).
func (m *MockManager) deleteStartupMockFrom(tree *TreeDb, mock models.Mock) bool {
	if tree == nil {
		return false
	}

	// The startup tree is keyed off a tier-local TestModeInfo copy whose
	// ID was stamped at insert time and may not match the live mock's
	// .ID (which the session tier's build stamps too). We can't
	// look up by mock.TestModeInfo directly. Iterate the tree with key
	// access to find the entry whose value matches by name, then delete
	// via the actual stored key.
	tree.mu.Lock()
	defer tree.mu.Unlock()
	it := tree.rbt.Iterator()
	for it.Next() {
		mk, ok := it.Value().(*models.Mock)
		if !ok || mk == nil || mk.Name != mock.Name {
			continue
		}
		key := it.Key()
		tree.rbt.Remove(key)
		if info, ok := key.(models.TestModeInfo); ok {
			delete(tree.idIndex, info.ID)
		}
		// Record the consume. Every sibling door flags, and without it a
		// startup consume is invisible to the run's consumed set — so
		// upsertActualTestMockMapping, FailureInfo.MatchedCalls and the
		// resend guard all miss it, and resetResendUnsafe will re-send a
		// request that already burned a mock.
		//
		// Usage is Updated, NOT Deleted, and that distinction is load-bearing:
		// filterOutDeleted prunes only on Deleted, and it is applied to the
		// filtered slice the startup tier is REBUILT from. Flagging Deleted
		// here would drop the boot mock permanently and break a driver
		// reconnect that legitimately replays the bootstrap chain.
		//
		// A carry-over mock (models.IsCarryOver: a registered kind's, or a
		// StagingRelease need's) is consumed once, wherever it is served
		// from: a publish, or a message the app received, happens once, and
		// the startup tier holds one only because it was recorded before the
		// first test or loaded for the app's startup ahead of its window. It
		// is Deleted, so the next load filters it out once the replay drained
		// it, and CarryOver, so the replay does not re-arm it: consumed before
		// the first test it is mapped to the startup section, later by its
		// own window. One the app consumed as it started is consumed once:
		// another consume of it (a copy a staging filed before the replay
		// drained the first) is refused, and every later staging leaves it
		// out (bootTaken).
		usage, carried := models.Updated, m.consumedOutOfWindow(*mk)
		if models.IsCarryOver(mk) {
			usage, carried = models.Deleted, true
			if tree.bootBuilt && !m.exact.fired.Load() {
				if !m.takeBootOnce(mk) {
					return false
				}
			} else if m.bootConsumed(mk) {
				return false
			}
		}
		if err := m.flagMockAsUsed(models.MockState{
			Name:             mk.Name,
			Kind:             mk.Kind,
			Usage:            usage,
			IsFiltered:       mk.TestModeInfo.IsFiltered,
			SortOrder:        mk.TestModeInfo.SortOrder,
			Type:             mk.Spec.Metadata["type"],
			DataPlane:        dataPlaneTyped(mk),
			Lifetime:         mk.TestModeInfo.Lifetime,
			ReqTimestampMock: models.FormatMockTimestamp(mk.Spec.ReqTimestampMock),
			ResTimestampMock: models.FormatMockTimestamp(mk.Spec.ResTimestampMock),
			AnswerPlaced:     models.IsAnswerPlaced(mk),
			ReqBodyNoise:     learnedNoiseOf(mk.Spec, mock.Spec),
			Variables:        variablesOf(mock.Spec),
			Clocks:           clocksOf(mock.Spec),
			Binds:            bindsOf(mock.Spec),
			LearnedAfterEnd:  m.learnedAfterEnd(m.servedAs(mk), &mock),
			// A registered kind's startup-band mock first consumed after a
			// test fired (a startup push to a consumer created lazily, or
			// held back by its permits) is consumed outside its own window,
			// as a late push through MarkMockAsUsed is: the replay keeps it
			// whatever the running test's verdict and maps it to the
			// startup section.
			CarryOver: carried,
		}, mk.TestModeInfo.StagedSet); err != nil {
			if errors.Is(err, errPreviousSet) {
				return false
			}
			m.logger.Error("failed to flag startup mock as used", zap.Error(err))
		}
		// Bump the global revision so any cached snapshot held by a
		// matcher consumer rebuilds on next access.
		m.bumpRevisionAll()
		return true
	}
	return false
}

// SeedStartupCutoff sets the startup-init cutoff from the set's earliest
// RECORDED test, before any test of that set has run.
//
// The cutoff decides whether a mock was recorded before this set's tests began.
// Derived from the first EXECUTED window it gets that wrong whenever the first
// executed test is not the earliest recorded one — a --test-sets selection, an
// ignored test, or the streaming deferral all do that. The cutoff then lands
// late, and mocks belonging to a test that is NOT being run fall before it, get
// classified as startup-init, and are served as bootstrap traffic.
//
// The manager cannot derive this itself: the mocks it is staged with include
// the bootstrap ones, so their minimum timestamp cannot separate "recorded
// before the tests began" from "recorded during the earliest test". The caller
// supplies it — the replayer already loads test cases sorted by request
// timestamp, so it is testCases[0]'s request time.
//
// Seeds only when nothing has been recorded for this set yet, and never marks a
// test as fired: routing must still read "nothing has fired" until a real
// window arrives. A later real window that turns out to be earlier still lowers
// the cutoff through the running minimum in SetMocksWithWindow.
func (m *MockManager) SeedStartupCutoff(start time.Time) {
	if start.IsZero() || start.Equal(models.BaseTime) {
		return
	}
	m.swapMu.Lock()
	defer m.swapMu.Unlock()
	if m.boundaryPending {
		// This set has not been staged yet, so firstWindowStart still describes
		// the PREVIOUS set and comparing against it is meaningless. Park the
		// value; staging installs it.
		//
		// Unconditionally, NOT as a running minimum: the reset clears the park,
		// so anything already here belongs to this same set, and a minimum
		// would be backwards anyway — later sets always seed later, so an
		// earlier value would win every comparison.
		m.cutoffSeeded = start
		return
	}
	// Already staged: apply directly, keeping the running-minimum semantics.
	// Parking it here instead would strand the value until the NEXT boundary,
	// where it would install a cutoff belonging to the wrong set.
	if m.firstWindowStart.IsZero() || start.Before(m.firstWindowStart) {
		m.firstWindowStart = start
	}
}

// SeedRecordedWindows installs the recorded [request, response] window of every
// test of the set being staged (MockFilterParams.RecordedWindows), the
// unselected and ignored ones included.
//
// It follows SeedStartupCutoff's rules, for the same reason: the replayer sends
// it in the staging call, before SetMocksWithWindow, so at a set boundary the
// value is parked and the staging call installs it together with the new trees.
// Seeded mid-set (no boundary pending) it applies at once. An empty ws is
// ignored.
//
// Consumers read it through RecordedWindows, to release traffic that the test
// windows do not pace by themselves at the recorded window it belongs to.
//
// It is kept only while a kind has registered a carry-over predicate or a
// StagingRelease need (models.CarryOverRegistered): every reader is the
// carry-over tier or the parser of a kind that paces by the windows, and such
// a kind registers one.
// With none registered the schedule (about 92 bytes a test, for the whole set)
// would be read by nobody, so it is not built, the staging call's windows are
// garbage once the call returns, and no schedule is left installed or parked.
func (m *MockManager) SeedRecordedWindows(ws []models.TestWindow) {
	// The learn step tells the pool's mocks from a test's by them, whatever
	// the release rule does with them (InRecordedTestWindow): parked at a
	// set boundary until the set's staging call, applied at once mid-set. A
	// set with none: the learn step knows none, not the previous set's.
	var tw *models.WindowSchedule
	if len(ws) > 0 {
		tw = models.NewWindowSchedule(ws)
	}
	m.swapMu.Lock()
	if m.boundaryPending {
		m.testWindowsSeeded = tw
	} else {
		m.testWindows.Store(tw)
	}
	m.swapMu.Unlock()
	if len(ws) == 0 {
		return
	}
	if !models.CarryOverRegistered() {
		m.swapMu.Lock()
		m.windowsSeeded = nil
		if !m.boundaryPending {
			m.recordedWindows.Store(nil)
		}
		m.swapMu.Unlock()
		if m.logger != nil {
			m.logger.Debug("recorded test windows not kept: no kind registered a carry-over predicate or a StagingRelease need",
				zap.Int("windows", len(ws)))
		}
		return
	}
	s := models.NewWindowSchedule(ws)
	m.swapMu.Lock()
	defer m.swapMu.Unlock()
	if m.boundaryPending {
		m.windowsSeeded = s
		return
	}
	m.recordedWindows.Store(s)
}

// RecordedWindows returns the current set's recorded test windows, or nil when
// the replayer sent none (mock replay, an older CLI, SetFilteredMocks-only
// callers) or no kind registered a carry-over predicate: consumers then treat
// everything as released, as before. The returned schedule is immutable.
func (m *MockManager) RecordedWindows() *models.WindowSchedule {
	// Under exact serving what the view shows is due: a test's push is its
	// own mocks, served while it is open, and the pool's is served between
	// tests. No recorded window paces it (CurrentTestWindow is zero).
	if m.exactView() != nil {
		return nil
	}
	return m.recordedWindows.Load()
}

// WindowChanged returns a channel that is closed at the next change of the
// published test window — every SetMocksWithWindow (per-test and staging) and
// SetCurrentTestWindow — once the new trees and window are visible, and at
// Close. Each change hands out a fresh channel.
//
// Take the channel BEFORE reading the state it guards, or a change that lands
// between the read and the wait is missed:
//
//	for {
//		ch := db.WindowChanged()
//		start, _ := db.CurrentTestWindow()
//		serveWhatIsDue(start, db.StagingEpoch())
//		select {
//		case <-ch:
//		case <-connDone:
//			return
//		}
//	}
func (m *MockManager) WindowChanged() <-chan struct{} {
	m.sigMu.Lock()
	defer m.sigMu.Unlock()
	return m.windowSig
}

// StagingEpoch identifies the current staging snapshot. It changes at every
// staging call (SetMocksWithWindow with models.BaseTime): a new test set, or the
// same set staged again for a replacement agent; and at every SetScopeMocks,
// which re-stages the pool as a test of keploy mock replay begins or ends. Anything a consumer derived
// from a staging snapshot — a per-set delivery queue — must be rebuilt when it
// changes. Zero until the first staging call.
func (m *MockManager) StagingEpoch() uint64 {
	return m.stagingEpoch.Load()
}

// signalWindowChange wakes every WindowChanged waiter and arms a fresh channel.
func (m *MockManager) signalWindowChange() {
	m.sigMu.Lock()
	old := m.windowSig
	m.windowSig = make(chan struct{})
	m.sigMu.Unlock()
	close(old)
}

// MarkMockAsUsed marks the given mock as used (consumed) without modifying
// its sort order or removing it from any tree. This is intended for parsers
// (e.g. mongo v2) that need to record mock usage without changing mock ordering.
//
// Unification observability: also counts the match against the mock's name
// (see bumpHitCount). For session- and connection-scoped mocks that's the
// reuse telemetry surfaced by SessionMockHitCounts; for per-test mocks it
// stays at 0 or 1 since they're consumed on match so the bump has no
// semantic meaning but is harmless.
func (m *MockManager) MarkMockAsUsed(mock models.Mock) bool {
	return m.markMockAsUsed(mock, nil)
}

// markMockAsUsed is MarkMockAsUsed, settled under exact serving only while
// the store is at g's round when g is set (WithinRound).
func (m *MockManager) markMockAsUsed(mock models.Mock, g *roundGuard) bool {
	if mock.Name == "" {
		// The observer is told of no unnamed mock.
		return false
	}
	// What the tier reads of the staging (learnedAfterEndPrepared), prepared
	// before the door: that takes swapMu, which nothing holding the round
	// lock takes, and the tier may run holding it.
	if provenswitch.On() && m.exact.learnEnded.Load() {
		m.prepareOutside()
	}
	return m.consumeValueAt(mock, false, g, true, m.markUsedTier)
}

// markUsedTier records mock's use, and reports whether it was consumed. It
// may run holding the round lock (consumeAt). A consume a view settles itself
// never gets here, so a test's own mock, which the staging does not hold, is
// never read against the staging.
func (m *MockManager) markUsedTier(mock models.Mock) bool {
	if m.stalePooled(&mock) {
		return false
	}
	learned, ok := m.learnedAfterEndPrepared(&mock)
	if !ok {
		return false
	}
	m.bumpHitCount(mock.Name, mock.Kind)
	if err := m.flagMockAsUsed(models.MockState{
		Name:             mock.Name,
		Kind:             mock.Kind,
		Usage:            models.Updated,
		IsFiltered:       mock.TestModeInfo.IsFiltered,
		SortOrder:        mock.TestModeInfo.SortOrder,
		Type:             mock.Spec.Metadata["type"],
		DataPlane:        dataPlaneTyped(&mock),
		Lifetime:         mock.TestModeInfo.Lifetime,
		ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
		AnswerPlaced:     models.IsAnswerPlaced(&mock),
		// Carry schema-detected request noise for non-HTTP integrations
		// (e.g. Pulsar) that consume via MarkMockAsUsed rather than the
		// Delete*/Update* paths, so UpdateMocks can persist it back to disk.
		ReqBodyNoise: reqBodyNoiseOf(mock.Spec),
		// What record's full round learned onto a mock this door keeps (a
		// reusable stateful recording, a scoped worker's held own mock), as
		// the other doors carry it (nil with the switch off).
		Variables: variablesOf(mock.Spec),
		Clocks:    clocksOf(mock.Spec),
		Binds:     bindsOf(mock.Spec),
		// Learning the consume carries beyond what exact serving handed
		// the parser out, once the set's learn step ended.
		LearnedAfterEnd: learned,
		// A registered kind's per-test mock consumed outside the current
		// window (a server push delivered late) belongs to its own window.
		CarryOver: m.consumedOutOfWindow(mock),
	}, mock.TestModeInfo.StagedSet); err != nil {
		if m.logger != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
		return false
	}
	// A per-test recording this door keeps (HTTP's stateful one, kept for
	// its test's repeats) answered a call too (FindAnsweredEarlier).
	if mock.TestModeInfo.Lifetime == models.LifetimePerTest {
		m.noteAnswered(mock)
	}
	return true
}

// bumpHitCount counts a match against name in the hit index. O(1) when the
// last staging indexed the name — MarkMockAsUsed sits on the match hot path,
// so a tree scan per call would be a measurable CPU cost on large pools. A
// name it did not index is seeded on its first match, if the per-test,
// session or connection pools hold a mock of that name (not the startup
// tier, which every staging indexes).
//
// Race handling: the slow path (hitIdx miss) takes the index write
// lock for the WHOLE discover-and-seed sequence. A concurrent fast-
// path bump during the slow-path tree walk can't double-increment
// because the fast path holds a read lock while the slow path is
// trying to upgrade — one of them wins the mutex-acquisition order
// and the loser sees the already-seeded entry on re-check.
//
// No error on miss — telemetry is best-effort; the caller's primary
// action (match / consume) is orthogonal.
func (m *MockManager) bumpHitCount(name string, kind models.Kind) {
	if name == "" {
		return
	}
	// Honour the Close contract: post-Close mutations are no-ops so a
	// late caller (matcher still draining on the way down, delayed
	// goroutine completion, etc.) doesn't race with teardown by
	// touching hitIdx for a pool the manager has logically released.
	if m.closed.Load() {
		return
	}
	// Fast path: O(1) index lookup under RLock.
	m.hitMu.RLock()
	n, fastHit := m.hitIdx[name]
	m.hitMu.RUnlock()
	if fastHit {
		n.Add(1)
		return
	}

	// Slow path: hold the write lock across the discover-and-seed
	// sequence so a concurrent slow-path caller for the same name
	// re-checks and sees the seeded entry instead of walking the
	// tree again (and double-incrementing).
	m.hitMu.Lock()
	defer m.hitMu.Unlock()
	if n2, ok := m.hitIdx[name]; ok {
		n2.Add(1)
		return
	}

	m.treesMu.RLock()
	flt := m.filteredByKind[kind]
	unf := m.unfilteredByKind[kind]
	m.treesMu.RUnlock()
	findFirstByName := func(tree *TreeDb) *models.Mock {
		if tree == nil {
			return nil
		}
		var found *models.Mock
		tree.rangeValues(func(v interface{}) bool {
			mk, ok := v.(*models.Mock)
			if !ok || mk == nil || mk.Name != name {
				return true
			}
			found = mk
			return false
		})
		return found
	}
	var target *models.Mock
	if target = findFirstByName(unf); target == nil {
		if target = findFirstByName(flt); target == nil {
			m.connMu.RLock()
			for _, tree := range m.connectionTrees {
				if target = findFirstByName(tree); target != nil {
					break
				}
			}
			m.connMu.RUnlock()
		}
	}
	if target != nil {
		n := new(atomic.Uint64)
		n.Store(1)
		m.hitIdx[name] = n
		m.hitSeeds++
	}
}

// tierKey builds a mock's tier-local tree key: its TestModeInfo with an
// explicit ID.
func tierKey(mk *models.Mock, id int) models.TestModeInfo {
	k := mk.TestModeInfo
	k.ID = id
	return k
}

// rebuildHitIndex replaces hitIdx with a counter for every name in the given
// slices; mocks that share a name share its counter. A name the previous index
// held keeps its counter, so a session mock's count spans the set although the
// agent stages fresh copies of the mocks for every test; a name this staging
// leaves out (a consumed per-test mock's) is dropped, so the index holds the
// staged pool and no more. At a set boundary every count starts afresh: names
// are only unique within a set (mockdb numbers them per set), so one set's
// mock-1 is not the next's.
//
// Its sole caller is the staging call, after all three tier swaps, so the
// fast-path bump sees the new pool. The explicit startup slice of
// SetMocksWithWindowThreeTier is passed in too: its mocks need not be in the
// other slices, and those that are not would never reach the index otherwise.
//
// The new index is built under the read lock and swapped in under the write
// lock, so matches counted meanwhile are not held up: they land on the
// counters the rebuild keeps. Only a slow-path seed in between (hitSeeds)
// sends it back over the names under the write lock, to keep that counter.
//
// At a set boundary nothing is kept: the trees are swapped before this runs,
// so a match against the new set's mocks counted in between lands on the
// previous set's counters and is dropped with them.
//
// NOTE: a bare SetFilteredMocks / SetUnFilteredMocks swaps its tree WITHOUT
// indexing here, leaving its new names to bumpHitCount's slow path.
func (m *MockManager) rebuildHitIndex(setBoundary bool, slices ...[]*models.Mock) {
	total := 0
	for _, s := range slices {
		total += len(s)
	}
	idx := make(map[string]*atomic.Uint64, total)
	keep := func() {
		for _, s := range slices {
			for _, mk := range s {
				if mk == nil || mk.Name == "" {
					continue
				}
				if n := m.hitIdx[mk.Name]; n != nil && !setBoundary {
					idx[mk.Name] = n
				} else if idx[mk.Name] == nil {
					idx[mk.Name] = new(atomic.Uint64)
				}
			}
		}
	}
	m.hitMu.RLock()
	seeds := m.hitSeeds
	keep()
	m.hitMu.RUnlock()
	if m.hitIndexBuilt != nil {
		m.hitIndexBuilt()
	}
	m.hitMu.Lock()
	defer m.hitMu.Unlock()
	if !setBoundary && m.hitSeeds != seeds {
		keep()
	}
	m.hitIdx = idx
}

// ---------- bookkeeping ----------

// SetServedObserver makes f told of every mock a parser is served, from now
// on (nil: none). Under exact serving f runs in the step that settles the
// consume, holding the round lock (exact_round.go), so a snapshot of the
// round reads the consume and f's account of it together: f must not call
// back into the manager (a consume door, a snapshot, a claim or a spend,
// LearnBoundary), or it waits on the lock it runs under. The learn
// tracker's ServedMock takes only its own lock.
func (m *MockManager) SetServedObserver(f func(*models.Mock)) {
	if f == nil {
		m.servedObserver.Store(nil)
		return
	}
	m.servedObserver.Store(&f)
}

// SetServedInObserver sets the observer told, instead of the served
// observer, of a mock served in a request its parser said (ServingFor);
// nil clears it. The same rules as SetServedObserver's hold.
func (m *MockManager) SetServedInObserver(f func(*models.Mock, *learn.Flight)) {
	if f == nil {
		m.servedInObserver.Store(nil)
		return
	}
	m.servedInObserver.Store(&f)
}

// SetOpenTestEpoch makes f say the learn tracker's open test
// (learn.Tracker.OpenEpoch) as each test's view is installed, from now on
// (nil: none). The view carries what it said (exactView.stamp), and
// ServedInOpenTest returns it with the view's account, so a reader compares
// test numbers, not what the two accounts hold. Without it every view
// carries 0. f runs as the view is installed, holding the install lock and
// the round lock (exact_round.go), so no snapshot reads the tracker between
// the stamp and the view's start: f must not call back into the manager (an
// install, a consume door, a snapshot, a claim or a spend, LearnBoundary), or
// it waits on a lock it runs under. learn.Tracker.OpenEpoch takes only the
// tracker's own lock.
func (m *MockManager) SetOpenTestEpoch(f func() uint64) {
	if f == nil {
		m.openTestEpoch.Store(nil)
		return
	}
	m.openTestEpoch.Store(&f)
}

// SetTrackerSetAt makes f say the learn tracker's running set
// (learn.Tracker.SetAt) as each staging is made, from now on (nil: none):
// the set the staging stages is the tracker's own, whether or not the
// boundary that began it reached this manager. keploy test's Mock begins
// a fresh agent's first set on the tracker before the proxy holds a
// manager (Proxy.learnBoundary then runs on the tracker alone), so this
// manager's account of the tracker's boundaries (LearnBoundary) does not
// hold it. Without f a staging stages the set that account last held. f
// runs holding swapMu (written) and the round lock (bumpStagingEpoch):
// it must not call back into the manager (a staging, an install, a consume
// door, a snapshot, a claim or a spend, LearnBoundary), or it waits on a
// lock it runs under. learn.Tracker.SetAt takes only the tracker's own lock.
func (m *MockManager) SetTrackerSetAt(f func() uint64) {
	if f == nil {
		m.trackerSetAt.Store(nil)
		return
	}
	m.trackerSetAt.Store(&f)
}

// observeServed tells the served observer of mock, a consume that went
// through: its consume door calls it only once the consume succeeded
// (consumeAt), never for one the view or the tier refused, since nothing
// served that one. A parser asks again after a refusal, and the learn tracker
// numbers every serve it hears in F's order of the set, so one told early
// reads as a serve the application never got. Under exact serving it is told
// in the step that settles the consume, holding the round lock, with the take
// before it, so only a reader that reads both under that lock (RoundSnapshot)
// never finds the mock in neither; no reader reads them without it, and none
// may. in is the request it is served in, as its parser said (nil: not said).
//
// With an observer (the served-in observer for a said request, else the
// served observer), the serve is then filed in the open test's view
// (exactView.hear, ServedInOpenTest), the view read before the observer is
// told: the replay (keploy test's loops, the agent's scope begin) begins the
// learn tracker's test before it installs the test's view, and serves the
// pool before the next test begins, so a mock a test view holds is one the
// tracker heard of in that test, and a reader that reads the view's account
// before the tracker's never finds there a serve the tracker has not heard
// of. The view's stamp (SetOpenTestEpoch) says which tracker test that is.
// Without one (the switch off) nothing is filed.
func (m *MockManager) observeServed(mock *models.Mock, in *learn.Flight) {
	if mock == nil || mock.Name == "" {
		return
	}
	if in != nil {
		if f := m.servedInObserver.Load(); f != nil {
			v := m.exactView()
			(*f)(mock, in)
			v.hear(mock)
			return
		}
	}
	if f := m.servedObserver.Load(); f != nil {
		v := m.exactView()
		(*f)(mock)
		v.hear(mock)
	}
}

// observeServedCopy is observeServed of a mock a door took by value: the
// observer keeps a copy of its own, made only when there is an observer (the
// proven switch), so the door's mock does not move to the heap without one.
func (m *MockManager) observeServedCopy(mock models.Mock, in *learn.Flight) {
	if mock.Name == "" {
		return
	}
	if in != nil {
		if f := m.servedInObserver.Load(); f != nil {
			c := mock
			v := m.exactView()
			(*f)(&c, in)
			v.hear(&c)
			return
		}
	}
	if f := m.servedObserver.Load(); f != nil {
		c := mock
		v := m.exactView()
		(*f)(&c)
		v.hear(&c)
	}
}

// reqBodyNoiseOf returns the field-path request-body noise detected on a mock's
// request (nil when none was detected). Used to carry schema-detected noise out
// on MockState so UpdateMocks can persist it. Every parser — HTTP and non-HTTP
// alike — stores it on the kind-agnostic MockSpec.ReqBodyNoise field.
func reqBodyNoiseOf(spec models.MockSpec) map[string][]string {
	return spec.ReqBodyNoise
}

// learnedNoiseOf is the request noise a consumed start-up mock's state
// carries: the tree's copy's, as before the proven switch, and under it the
// caller's (the copy the learn step attached what it learned to).
func learnedNoiseOf(tree, caller models.MockSpec) map[string][]string {
	if provenswitch.On() {
		return caller.ReqBodyNoise
	}
	return tree.ReqBodyNoise
}

// variablesOf is what a consumed mock's state carries of its variables:
// under the proven switch, its own (MockSpec.Variables) and the ones the
// learn step learned on it (MockSpec.Learned), which record's full round
// writes onto the recording. Without the switch nothing, as before it.
func variablesOf(spec models.MockSpec) models.MockVariables {
	if !provenswitch.On() {
		return nil
	}
	out := spec.Variables.Clone()
	for name, v := range spec.Learned().Clone() {
		if out == nil {
			out = models.MockVariables{}
		}
		out[name] = v
	}
	return out
}

// clocksOf is what a consumed mock's state carries of the places its call's
// learn step learned as reads of the application's clock (MockSpec.Clocks):
// under the proven switch its own, which record's full round writes onto the
// recording. Without the switch nothing, as before it.
func clocksOf(spec models.MockSpec) map[string]models.ClockOffset {
	if !provenswitch.On() {
		return nil
	}
	return maps.Clone(spec.Clocks)
}

// bindsOf is what a consumed mock's state carries of the variables the learn
// step learned while answering it that are bound at an earlier mock
// (MockSpec.LearnedBinds): under the proven switch only.
func bindsOf(spec models.MockSpec) map[string]models.MockVariables {
	binds := spec.LearnedBinds()
	if !provenswitch.On() || len(binds) == 0 {
		return nil
	}
	out := make(map[string]models.MockVariables, len(binds))
	for m, v := range binds {
		out[m] = v.Clone()
	}
	return out
}

// flagMockAsUsed records that a mock was consumed from the network.
// The first call for a given name establishes its position in the consumption
// order; subsequent calls for the same name update the stored state in-place
// without changing its position. This preserves true network call order in
// GetConsumedMocks.
// errPreviousSet is flagMockAsUsed refusing a consume of a mock an earlier
// set's staging pooled (TestModeInfo.StagedSet): the consume door that asked
// reports it not consumed.
var errPreviousSet = errors.New("the mock was pooled for an earlier test set")

// flagMockAsUsed records a consume. staged is the consumed mock's
// TestModeInfo.StagedSet: a consume of a mock pooled under an earlier set
// generation is refused with errPreviousSet, checked under consumedMu with
// the record, as the fence moves the generation with its drop (zero: not
// checked).
func (m *MockManager) flagMockAsUsed(mock models.MockState, staged uint64) error {
	if mock.Name == "" {
		return fmt.Errorf("mock is empty")
	}
	if mock.Timestamp == 0 {
		mock.Timestamp = time.Now().UnixNano() // when it was first served, so a client can tell which test used it
	}
	inScope := mock.Lifetime == models.LifetimePerTest && m.scopeIsOpen()
	m.consumedMu.Lock()
	if staged != 0 && staged != m.setGen.Load() {
		m.consumedMu.Unlock()
		return errPreviousSet
	}
	// Under the proven switch what record's full round learned onto a mock
	// is cumulative across its states: a reusable recording the learn step
	// pairs twice keeps both pairings' variables, binds and noise, whichever
	// door each left through (a door that keeps the pool's copy, as
	// MarkMockAsUsed does, hands the second pairing a copy without the
	// first's).
	if provenswitch.On() {
		var prev models.MockState
		var ok bool
		if m.consumedPersistent != nil {
			prev, ok = m.consumedPersistent[mock.Name]
		} else if idx, exists := m.consumedIndex[mock.Name]; exists {
			prev, ok = m.consumedList[idx], true
		}
		if ok {
			mock.Variables = models.MergeVariables(prev.Variables, mock.Variables)
			mock.Clocks = models.MergeClocks(prev.Clocks, mock.Clocks)
			mock.Binds = models.MergeBinds(prev.Binds, mock.Binds)
			if len(prev.ReqBodyNoise) > 0 {
				mock.ReqBodyNoise = mocknoise.MergeLearned(mock.ReqBodyNoise, prev.ReqBodyNoise)
			}
		}
	}
	if inScope && m.scopeConsumed != nil {
		m.scopeConsumed[mock.Name] = struct{}{}
	}
	if idx, exists := m.consumedIndex[mock.Name]; exists {
		mock.Timestamp = m.consumedList[idx].Timestamp
		// A consume in this drain that learned after the learn step ended
		// is not hidden by a later one that did not: the drain's consumers
		// were served through it. Only the drain's: another drain's
		// consumers were not.
		mock.LearnedAfterEnd = mock.LearnedAfterEnd || m.consumedList[idx].LearnedAfterEnd
		m.consumedList[idx] = mock // update state, preserve position and first use
	} else {
		m.consumedIndex[mock.Name] = len(m.consumedList)
		m.consumedList = append(m.consumedList, mock)
	}
	// Never-drained accumulation (latest state per name) mirroring exactly what
	// the client builds from successive GetConsumedMocks drains — so the agent
	// can self-apply filterOutDeleted with no client round-trip.
	if m.consumedPersistent != nil {
		m.consumedPersistent[mock.Name] = mock
	}
	if m.servedCount != nil {
		m.servedCount[mock.Name]++
	}
	m.consumedMu.Unlock()
	return nil
}

// ServedCounts returns a copy of how many times each mock answered a request
// this session. Never drained.
func (m *MockManager) ServedCounts() map[string]uint64 {
	m.consumedMu.Lock()
	defer m.consumedMu.Unlock()
	out := make(map[string]uint64, len(m.servedCount))
	for k, v := range m.servedCount {
		out[k] = v
	}
	return out
}

// GetConsumedMocks returns and drains the list of mocks that were consumed
// since the last call, in the order they were first intercepted from the
// network.
func (m *MockManager) GetConsumedMocks() []models.MockState {
	m.consumedMu.Lock()
	out := append([]models.MockState(nil), m.consumedList...)
	m.consumedList = m.consumedList[:0]
	m.consumedIndex = make(map[string]int)
	m.consumedMu.Unlock()
	return out
}

// GetPersistentConsumed returns a COPY of the never-drained per-name
// consumption map (latest MockState per mock name for the whole session).
// Unlike GetConsumedMocks it does NOT drain. It is the agent-side equivalent of
// the client's accumulated totalConsumedMocks, used to apply filterOutDeleted
// from the agent's own history when MockFilterParams.AgentOwnsConsumed is set.
func (m *MockManager) GetPersistentConsumed() map[string]models.MockState {
	m.consumedMu.Lock()
	defer m.consumedMu.Unlock()
	out := make(map[string]models.MockState, len(m.consumedPersistent))
	for k, v := range m.consumedPersistent {
		out[k] = v
	}
	return out
}

// GetMySQLCounts computes counts of MySQL mocks.
// Uses the per-kind unfiltered tree if available, otherwise falls back
// to scanning the legacy unfiltered tree.
func (m *MockManager) GetMySQLCounts() (total, config, data int) {
	// Fast path: snapshot the per-kind tree pointer under lock
	m.treesMu.RLock()
	tree := m.unfilteredByKind[models.MySQL]
	m.treesMu.RUnlock()

	if tree != nil {
		tree.rangeValues(func(v interface{}) bool {
			mock, ok := v.(*models.Mock)
			if !ok || mock == nil {
				return true
			}
			total++
			// Read the typed Lifetime with a raw-tag fallback so
			// mocks that reach the pool without DeriveLifetime
			// populating Lifetime still classify correctly. Same
			// semantic as the pre-unification "type == config" read.
			isSession := mock.TestModeInfo.Lifetime == models.LifetimeSession ||
				(mock.TestModeInfo.Lifetime == models.LifetimePerTest &&
					mock.Spec.Metadata != nil &&
					mock.Spec.Metadata["type"] == "config")
			if isSession {
				config++
			} else {
				data++
			}
			return true
		})
		return
	}

	// Fallback: legacy scan of the combined tree
	m.treesMu.RLock()
	legacyTree := m.unfiltered
	m.treesMu.RUnlock()
	legacyTree.rangeValues(func(v interface{}) bool {
		mock, ok := v.(*models.Mock)
		if !ok || mock == nil || mock.Kind != models.MySQL {
			return true
		}
		total++
		if mock.Spec.Metadata != nil && mock.Spec.Metadata["type"] == "config" {
			config++
		} else {
			data++
		}
		return true
	})
	return
}

// fileCarryOver updates the carry-over tier for the window that starts at
// start, right after the per-test and session trees were swapped. Caller holds
// swapMu. Returns the kinds whose carry-over contents changed.
//
//   - A pass that goes back to an earlier window (a retry cycle, the deferred
//     streaming tests) starts the tier over: CarryOverLoadRange has told the
//     agent to reload from the set's start, and the reload is filtered by what
//     was consumed.
//   - The registered mocks the closing window's per-test tree still holds
//     (unconsumed) and those the agent passed outside this window (carryIn)
//     are offered, if reachable from this window: the release window of their
//     recorded time minus models.CarryOverLookahead has started.
//   - A mock the new per-test tree holds is served from there, so it leaves
//     (or never enters) the tier.
//   - A mock the session tree also holds (lax mode promotes out-of-window
//     per-test mocks there) is reachable already and is not duplicated.
func (m *MockManager) fileCarryOver(oldPerTest *TreeDb, carryIn, newPerTest []*models.Mock, start time.Time) map[models.Kind]struct{} {
	changed := map[models.Kind]struct{}{}
	if !m.carryLastStart.IsZero() && start.Before(m.carryLastStart) {
		for _, mk := range m.carry.snapshot("") {
			changed[mk.Kind] = struct{}{}
		}
		m.carry.reset()
		m.carryLoadedTo = time.Time{}
	}
	sched := m.recordedWindows.Load()
	defer func() {
		m.carryLastStart = start
		if sched.Len() > 0 {
			m.carryLoadedTo = carryLoadHorizon(sched, start)
		}
	}()

	var candidates []*models.Mock
	offer := func(mk *models.Mock) {
		if mk == nil || !models.IsCarryOver(mk) {
			return
		}
		if carryReachable(sched, mk.Spec.ReqTimestampMock, start) {
			candidates = append(candidates, mk)
		}
	}
	if oldPerTest != nil {
		oldPerTest.rangeValues(func(v interface{}) bool {
			if mk, ok := v.(*models.Mock); ok {
				offer(mk)
			}
			return true
		})
	}
	for _, mk := range carryIn {
		offer(mk)
	}

	inTree := map[carryKey]struct{}{}
	for _, mk := range newPerTest {
		if mk != nil && models.IsCarryOver(mk) {
			inTree[carryKeyOf(mk)] = struct{}{}
		}
	}
	if len(candidates) > 0 {
		// Lax mode: skip what the session tree already serves.
		kinds := map[models.Kind]struct{}{}
		for _, mk := range candidates {
			kinds[mk.Kind] = struct{}{}
		}
		m.treesMu.RLock()
		for k := range kinds {
			if tree := m.unfilteredByKind[k]; tree != nil {
				tree.rangeValues(func(v interface{}) bool {
					if mk, ok := v.(*models.Mock); ok && mk != nil && mk.TestModeInfo.Lifetime == models.LifetimePerTest {
						inTree[carryKeyOf(mk)] = struct{}{}
					}
					return true
				})
			}
		}
		m.treesMu.RUnlock()
	}
	if len(candidates) == 0 && len(inTree) == 0 {
		return changed
	}

	res := m.carry.file(candidates, inTree)
	for k := range res.kinds {
		changed[k] = struct{}{}
	}
	if res.leftOut > 0 && m.logger != nil {
		fields := []zap.Field{
			zap.Int("leftOut", res.leftOut),
			zap.Int("leftOutThisSet", res.leftOutTotal),
			zap.Int("held", res.heldCount),
			zap.Int("heldBytes", res.heldBytes),
			zap.Int("capBytes", carryOverCapBytes),
			zap.Time("windowStart", start),
		}
		if res.firstCapHit {
			m.logger.Warn("carry-over tier is full: mocks the application may publish outside their own test window "+
				"are no longer kept past that window, in recorded order from here on; a test that consumes one of "+
				"them late will miss it. The held mocks are ones earlier tests left unconsumed", fields...)
		} else {
			m.logger.Debug("carry-over tier still full; more mocks left out", fields...)
		}
	}
	return changed
}

// ArmSetFence tells the manager that the session ResetForReplaySession just
// reset starts a test set of the replay (models.OutgoingOptions.StartsTestSet),
// not a re-registration of the current set with an agent the replay took for
// replaced. The set's boundary staging then drops what was consumed before it
// and not drained: the previous set's pool's consumes, under that set's mock
// names, which every set numbers afresh, so a drain must never hand them to
// this one (a Deleted mock-7 there drops this set's unrelated mock-7 from
// every load). Until that staging installs the set's pool, whatever the app
// consumes is still of the previous one. A re-registration arms nothing: it
// comes before any of the set's tests, and what the app consumed as it
// started is the set's own.
func (m *MockManager) ArmSetFence() {
	m.swapMu.Lock()
	m.setFence = true
	m.swapMu.Unlock()
}

// dropPreviousSetConsumes starts a new set generation and empties what the
// previous set's pool was consumed of and nobody drained, at a fenced set
// boundary (ArmSetFence), right before the staging publishes the new trees:
// the consumes not drained yet, the agent-owned history a session that
// outlived the reset kept adding to, and bootTaken. The generation moves
// under consumedMu, with the drop, so a consume of the previous set's pool
// flags before both or is refused after (flagMockAsUsed). Caller holds
// swapMu; consumedMu and bootTakenMu are leaves.
func (m *MockManager) dropPreviousSetConsumes() {
	m.consumedMu.Lock()
	// The answered log follows the generation (answeredLog.fence); its
	// lock is a leaf.
	m.answered.fence(m.setGen.Add(1))
	// What was kept of the previous set's learning is not this set's.
	m.exact.kept.set(nil, false)
	dropped := len(m.consumedList)
	m.consumedList = m.consumedList[:0]
	m.consumedIndex = make(map[string]int)
	if m.consumedPersistent != nil {
		m.consumedPersistent = make(map[string]models.MockState)
	}
	m.consumedMu.Unlock()
	m.bootTakenMu.Lock()
	m.bootTaken = nil
	m.bootTakenMu.Unlock()
	if dropped > 0 && m.logger != nil {
		m.logger.Debug("a new test set's staging dropped the consumes the previous set's pool was not drained of; they were the previous set's",
			zap.Int("dropped", dropped))
	}
}

// takeBootOnce records the consume of a carry-over mock the app made as it
// started (bootTaken), and reports whether it is the first: another is
// refused, and so is one of a mock pooled for an earlier set.
func (m *MockManager) takeBootOnce(mk *models.Mock) bool {
	k := carryKeyOf(mk)
	m.bootTakenMu.Lock()
	defer m.bootTakenMu.Unlock()
	// One pooled for an earlier set records nothing: the fence moves the
	// generation before it empties bootTaken.
	if g := mk.TestModeInfo.StagedSet; g != 0 && g != m.setGen.Load() {
		return false
	}
	if _, taken := m.bootTaken[k]; taken {
		return false
	}
	if m.bootTaken == nil {
		m.bootTaken = make(map[carryKey]struct{})
	}
	m.bootTaken[k] = struct{}{}
	return true
}

// bootConsumed reports whether the app consumed mk, a carry-over mock, as it
// started (bootTaken).
func (m *MockManager) bootConsumed(mk *models.Mock) bool {
	m.bootTakenMu.Lock()
	defer m.bootTakenMu.Unlock()
	_, taken := m.bootTaken[carryKeyOf(mk)]
	return taken
}

// forgetBootTaken forgets the startup consumes of the named mocks
// (RestartExactKey: a retried package's pool is served afresh).
func (m *MockManager) forgetBootTaken(names []string) {
	if len(names) == 0 {
		return
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	m.bootTakenMu.Lock()
	for k := range m.bootTaken {
		if _, ok := set[k.name]; ok {
			delete(m.bootTaken, k)
		}
	}
	m.bootTakenMu.Unlock()
}

// withoutBootCarry is mocks less the carry-over mocks the app consumed as it
// started (bootTaken); mocks itself when there are none.
func (m *MockManager) withoutBootCarry(mocks []*models.Mock) []*models.Mock {
	m.bootTakenMu.Lock()
	defer m.bootTakenMu.Unlock()
	if len(m.bootTaken) == 0 {
		return mocks
	}
	out := make([]*models.Mock, 0, len(mocks))
	for _, mk := range mocks {
		if mk != nil && models.IsCarryOver(mk) {
			if _, taken := m.bootTaken[carryKeyOf(mk)]; taken {
				continue
			}
		}
		out = append(out, mk)
	}
	return out
}

// stagingCarryOverRange is CarryOverLoadRange for a set's staging call, before
// its app starts: the app's startup is the window then. A carry-over mock
// recorded at t is reachable from it while the release window of
// t - models.CarryOverLookahead is the startup band, before the set's first
// recorded window: up to carryLoadHorizon of a start before every window,
// the first window's start plus the lookahead, that end excluded. It is
// loaded from the startup cutoff on: those recorded before the cutoff are in
// the startup band the staging reads anyway. The cutoff and windows are the
// set's as seeded for it (cutoffSeeded, windowsSeeded) while its staging has
// not installed them, else the installed ones (a set staged again). ok is
// false with no recorded windows, no cutoff, or an empty range.
func (m *MockManager) stagingCarryOverRange() (from, to time.Time, ok bool) {
	m.swapMu.RLock()
	cutoff, sched := m.firstWindowStart, m.recordedWindows.Load()
	if m.boundaryPending {
		cutoff, sched = m.cutoffSeeded, m.windowsSeeded
	}
	m.swapMu.RUnlock()
	if sched.Len() == 0 || cutoff.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	to = carryLoadHorizon(sched, models.BaseTime)
	if to.Before(cutoff) {
		return time.Time{}, time.Time{}, false
	}
	return cutoff, to, true
}

// carryLoadHorizon is the latest recorded time a registered mock can have and
// still be reachable while the window that starts at start is current.
func carryLoadHorizon(sched *models.WindowSchedule, start time.Time) time.Time {
	h, bounded := sched.ReleaseHorizon(start)
	if !bounded {
		return maxRecordedTime
	}
	return h.Add(models.CarryOverLookahead)
}

// maxRecordedTime stands for "no upper bound" in a load range; its UnixNano is
// representable.
var maxRecordedTime = time.Unix(0, math.MaxInt64)

// CarryOverLoadRange tells the agent which recorded-time range of carry-over
// mocks (models.IsCarryOver: RegisterCarryOver, a StagingRelease need) to load, beside the window's own mocks,
// for the window that starts at start: from where the previous window's load
// ended (or from the set's start, on the first window of a pass or when a
// pass goes back to an earlier window) up to the latest recorded time this
// window makes reachable. The loaded mocks are passed to SetMocksWithWindow
// in the filtered slice like any other; this manager files the out-of-window
// ones into the carry-over tier and advances the range.
//
// ok is false when there is nothing to load: no registered kind, no recorded
// windows (the replayer did not send them), or an empty range. A staging call
// (start at models.BaseTime) loads what the app's startup reaches
// (stagingCarryOverRange).
func (m *MockManager) CarryOverLoadRange(start time.Time) (from, to time.Time, ok bool) {
	if !models.CarryOverRegistered() || start.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	if start.Equal(models.BaseTime) {
		return m.stagingCarryOverRange()
	}
	sched := m.recordedWindows.Load()
	if sched.Len() == 0 {
		return time.Time{}, time.Time{}, false
	}
	m.swapMu.RLock()
	cutoff, last, loaded := m.firstWindowStart, m.carryLastStart, m.carryLoadedTo
	m.swapMu.RUnlock()
	switch {
	case !loaded.IsZero() && !start.Before(last):
		from = loaded
	case !cutoff.IsZero():
		from = cutoff
	default:
		from = sched.Window(0).Start
	}
	to = carryLoadHorizon(sched, start)
	if to.Before(from) {
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// GetCarryOverMocks returns the carry-over tier (see RegisterCarryOver and
// models.StagingRelease) in recorded order: carry-over mocks reachable outside
// their own window. A parser serves them after the running test's own mocks
// and consumes them through DeleteFilteredMock. Empty while nothing is
// registered.
func (m *MockManager) GetCarryOverMocks() ([]*models.Mock, error) {
	if v := m.exactView(); v != nil {
		return v.keep(m.carry.snapshot("")), nil
	}
	return m.carry.snapshot(""), nil
}

// GetCarryOverMocksByKind is GetCarryOverMocks for one kind.
func (m *MockManager) GetCarryOverMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	if v := m.exactView(); v != nil {
		return v.keep(m.carry.snapshot(kind)), nil
	}
	return m.carry.snapshot(kind), nil
}

// consumedOutOfWindow reports whether a per-test mock that marks so
// (models.MarksOutOfWindow: any of a RegisterCarryOver kind, those a
// StagingRelease need declares) is being consumed while a window that does not
// contain it is current — a server push delivered after its window, consumed
// through MarkMockAsUsed. A call in flight when the current window's test
// began (models.InFlightEligible, sent in the gap before it and answered at
// or after its start) is that window's, as its tier holds it: whichever door
// consumes it, it is mapped to that test.
func (m *MockManager) consumedOutOfWindow(mock models.Mock) bool {
	if !models.MarksOutOfWindow(&mock) {
		return false
	}
	req := mock.Spec.ReqTimestampMock
	if req.IsZero() {
		return false
	}
	start, end := m.CurrentTestWindow()
	if start.IsZero() || end.IsZero() {
		return false
	}
	if place := models.PlaceOf(&mock); !place.Before(start) && !place.After(end) {
		return false
	}
	from, ok := m.testWindows.Load().GapBefore(start)
	return !inFlightAtStart(&mock, req, mock.Spec.ResTimestampMock, from, start, ok)
}

// inFlightAtStart reports whether mk, its request recorded at req and its
// response at res, was in flight when the test whose window starts at start
// began: a per-test call the in-flight rule is for (models.InFlightEligible),
// sent in the gap before the window (gapFrom, hasGap: the recorded windows'
// models.WindowSchedule.GapBefore(start)) and answered at or after its start
// (models.SentInGapAnsweredBy). It is that test's own. One rule for what the
// windowed read serves (GetFilteredMocksInWindow), what the open test owns
// (OwnedByOpenTest) and what consumes out of no window (consumedOutOfWindow);
// the staging files the window's tier by the agent's serve decision
// (pkg.StageInWindow), whose in-flight rule is this one.
//
// The times are looked at first: a reader asks of every mock it holds, and
// InFlightEligible may derive a copy's lifetime.
func inFlightAtStart(mk *models.Mock, req, res, gapFrom, start time.Time, hasGap bool) bool {
	return hasGap && models.SentInGapAnsweredBy(req, res, gapFrom, start) && models.InFlightEligible(mk)
}
