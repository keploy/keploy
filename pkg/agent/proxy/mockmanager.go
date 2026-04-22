package proxy

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// ---------------- MockManager (kind-aware) ----------------
//
// Lock-ordering invariant (MUST be preserved to avoid deadlock):
//
//	swapMu  →  treesMu  →  windowMu
//	swapMu  →  revMu         (SetMocksWithWindow calls Set*Mocks which
//	                          bump revisions; revMu is a leaf lock —
//	                          never re-acquire swapMu/treesMu from under it)
//	swapMu  →  consumedMu    (independent; never hold consumedMu then swapMu)
//
// Any new code path that acquires more than one of these MUST take them
// in the declared order. Readers of {mock pool, test window} as an
// atomic pair MUST go through GetFilteredMocksInWindow (which RLocks
// swapMu). Writers that swap the pool AND publish a new window MUST go
// through SetMocksWithWindow (which Locks swapMu end-to-end).
//
// Fine-grained single-field updates (SetFilteredMocks, SetUnFilteredMocks,
// SetCurrentTestWindow) do NOT take swapMu — they only serialize on their
// respective fine-grained mutex. Callers who need whole-snapshot
// consistency must use the atomic variants above.

type MockManager struct {
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

	// global revision (legacy)
	rev uint64

	// NEW: per-kind revisions
	revMu     sync.RWMutex
	revByKind map[models.Kind]*uint64

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
	// Zero until the first SetMocksWithWindow call with a non-zero
	// start arrives; after that it's sticky (only goes earlier, never
	// later — see updateFirstWindowStart). HasFirstTestFired reads
	// this field under swapMu.RLock.
	firstWindowStart time.Time

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

	// invalidOrderWarnOnce fires the first-time Info log when
	// SetMocksWithWindow drops a mock because its response timestamp
	// precedes its request timestamp. Logging policy disallows Warn
	// for this; Info is prominent enough that operators see the first
	// hit. Subsequent drops stay at Debug level (via the per-call
	// counter) so a pathological recording doesn't flood logs.
	invalidOrderWarnOnce sync.Once

	// noConnMocks is a negative cache for GetConnectionMocks's
	// fallback scan. A connID that returned zero connection-scoped
	// mocks on a full scan gets memoised here so subsequent matches
	// from the same connection don't pay the O(N) walk again.
	// Entries live until Close / explicit DeleteConnection (we also
	// drop them on AddConnectionMock so a later arrival invalidates).
	noConnMocks sync.Map // map[connID]struct{}

	// hitIdx is a name → *Mock index for O(1) HitCount bumps on the
	// hot MarkMockAsUsed path. Populated lazily by the indexed tree
	// walkers used for Set{Filtered,UnFiltered}Mocks; cleared on pool
	// swaps.
	hitMu  sync.RWMutex
	hitIdx map[string]*models.Mock
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
		consumedIndex:       make(map[string]int),
		connectionTrees:     make(map[string]*TreeDb),
		connectionLastTs:    make(map[string]time.Time),
		sweeperStop:         make(chan struct{}),
		hitIdx:              make(map[string]*models.Mock),
		logger:              logger,
	}
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
	})
}

// IsClosed returns true once Close has been invoked. Exposed so
// higher-level callers (e.g. the agent lifecycle) can decide whether
// to invoke methods that would mutate the manager's state.
func (m *MockManager) IsClosed() bool {
	return m.closed.Load()
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
	m.revMu.Lock()
	ptr := m.revByKind[kind]
	if ptr == nil {
		var v uint64
		// Store pointer in map; safe to use after unlocking as we mutate via atomics.
		ptr = &v
		m.revByKind[kind] = ptr
	}
	m.revMu.Unlock()
	atomic.AddUint64(ptr, 1)
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
	m.windowMu.Unlock()
	atomic.StoreUint64(&m.droppedOutOfWindow, 0)
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
func (m *MockManager) IsTestWindowActive() bool {
	m.windowMu.RLock()
	defer m.windowMu.RUnlock()
	return !m.windowStart.IsZero() && !m.windowEnd.IsZero()
}

// GetFilteredMocksInWindow returns non-config filtered mocks whose recorded
// REQUEST timestamp lies inside the current test window. Response timestamps
// may legitimately straggle outside the window (downstream async completion)
// and are not used for window containment — only mocks whose response
// timestamp is EARLIER than their request timestamp are dropped as a sanity
// check on inconsistent recordings.
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
		// and debug log can distinguish the two root causes.
		if res.Before(req) {
			droppedInvalidOrder++
			continue
		}
		// Request-timestamp containment. Responses may straggle past
		// the outer test's response timestamp (async completion on
		// downstream calls); that's not a bleed signal. The bleed bug
		// this window protects against manifests as REQUESTS landing
		// outside the window — those are always from a different test.
		if !req.Before(start) && !req.After(end) {
			out = append(out, mock)
		} else {
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
// Also resets the per-test droppedOutOfWindow counter.
func (m *MockManager) SetMocksWithWindow(filtered, unfiltered []*models.Mock, start, end time.Time) {
	m.swapMu.Lock()
	defer m.swapMu.Unlock()

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
	if isInitialStaging {
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
		// The session tree is still populated below via SetUnFilteredMocks,
		// so once the first real test fires and SetMocksWithWindow
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

	// Pre-filter per-test mocks by the outer test window. Zero start or
	// zero end means "no window" and we preserve the legacy behaviour
	// (no pre-filtering). Readers of the per-test tree can then skip
	// any window check at match time because the tree already contains
	// only in-window mocks.
	filteredForTree := filtered
	var droppedInvalid, droppedOutOfWindow uint64
	// Initial-staging skips the per-test partition entirely — everything
	// was already routed into startupInit above.
	if isInitialStaging {
		filteredForTree = nil
	}
	if !isInitialStaging && !start.IsZero() && !end.IsZero() {
		out := make([]*models.Mock, 0, len(filtered))
		for _, mock := range filtered {
			if mock == nil {
				continue
			}
			req := mock.Spec.ReqTimestampMock
			res := mock.Spec.ResTimestampMock
			// Missing timestamps: keep defensively so legacy/incomplete
			// recordings still match.
			if req.IsZero() || res.IsZero() {
				out = append(out, mock)
				continue
			}
			// Inconsistent recording — response before request.
			// Info-level one-shot per manager (logging policy
			// disallows Warn here) so operators with a clock-skewed
			// recording see a prominent one-line message pointing at
			// the fix; subsequent drops stay on the per-call counter
			// + Debug summary below so a pathological recording
			// doesn't spam logs.
			if res.Before(req) {
				droppedInvalid++
				if m.logger != nil {
					mockName := mock.Name
					req := req
					res := res
					m.invalidOrderWarnOnce.Do(func() {
						m.logger.Info(
							"mock dropped: response timestamp precedes request timestamp (inconsistent recording). "+
								"This usually indicates clock skew at record time. Mocks with this pattern are "+
								"dropped to avoid confusing the matcher; re-record with a synchronised clock if "+
								"these mocks are needed. Further instances will only be counted (see Debug summary).",
							zap.String("mock", mockName),
							zap.Time("req", req),
							zap.Time("res", res))
					})
				}
				continue
			}
			// Window containment on request timestamp only. Responses
			// may legitimately straggle past `end` (async downstream
			// completion is normal).
			if !req.Before(start) && !req.After(end) {
				out = append(out, mock)
				continue
			}
			// Wave 2: startup-init preservation. A mock whose request
			// timestamp is strictly BEFORE the earliest test window
			// this manager has ever seen is app-bootstrap traffic
			// (Flyway migrations, Hibernate init, HikariCP connection
			// validation, driver handshake queries) that fired before
			// any test started. It's routed to the dedicated startup
			// tree (NOT the session tree, NOT with a Lifetime mutation)
			// so tier-aware parsers can see startup/session/perTest as
			// a strict three-way partition. Legacy parsers that call
			// GetSessionMocks still observe startup entries via the
			// union shim below.
			//
			// Stale previous-test mocks (firstStart <= req < start)
			// continue to be dropped — that's the bleed case this
			// whole machinery exists to protect against.
			if !firstStart.IsZero() && req.Before(firstStart) {
				startupInit = append(startupInit, mock)
				continue
			}
			droppedOutOfWindow++
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
	newStartup := NewTreeDb(customComparator)
	// H4 round-2: use a TIER-LOCAL TestModeInfo copy as the startup tree
	// key rather than mutating mk.TestModeInfo.ID in place. The same
	// *models.Mock pointer can appear in BOTH startupInit and
	// unfilteredForTree during initial staging (see the BaseTime branch
	// above that copies unfiltered→startupInit); a subsequent
	// SetUnFilteredMocks call at L~812 re-stamps mk.TestModeInfo.ID to
	// its index in the unfiltered slice, invalidating any startup-tier
	// reference that relied on the original stamp. A mock pointer's
	// .ID then no longer matches the startup tree's idIndex, so a later
	// (currently hypothetical but structurally reachable — e.g. future
	// UpdateStartupMock / DeleteStartupMock paths, or any parser that
	// indexes startup mocks by mock.TestModeInfo.ID for its own cache)
	// lookup that derives the key from the live mock's state would
	// miss. By keying the startup tree off a local copy of
	// TestModeInfo, we keep the startup tree internally consistent
	// (idIndex matches the inserted key) while leaving the shared
	// mock's .ID free to be re-stamped by SetUnFilteredMocks without
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
		// ID on the copy. mk.TestModeInfo.ID is left untouched so a
		// later SetUnFilteredMocks re-stamp does not corrupt the
		// startup tree's idIndex.
		startupKey := mk.TestModeInfo
		startupKey.ID = idx
		newStartup.insert(startupKey, mk)
	}
	m.treesMu.Lock()
	m.startup = newStartup
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

	m.SetFilteredMocks(filteredForTree)
	m.SetUnFilteredMocks(unfilteredForTree)
	// Rebuild the HitCount name→*Mock index so MarkMockAsUsed takes
	// the O(1) fast path. Called after the tree swap so the index
	// always points at the freshest mock pointers.
	m.rebuildHitIndex(filteredForTree, unfilteredForTree)

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
	if !isInitialStaging {
		m.windowMu.Lock()
		m.windowStart = start
		m.windowEnd = end
		m.windowMu.Unlock()
	}
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
	results := make([]*models.Mock, 0, 128)
	tree.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
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
// HitCount / consumedIndex accounting on the one replay path that
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
	results := make([]*models.Mock, 0, 32)
	tree.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
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
// for pre-test staging. Sticky: once true, stays true for the manager's
// lifetime.
//
// Parsers use this to distinguish "app bootstrap" from "between tests"
// when IsTestWindowActive() returns false. Before the first test fires
// all non-test traffic is bootstrap; after it, the same signal means
// the runner is in the idle gap between tests (or post-last-test
// teardown).
//
// Concurrency: firstWindowStart is updated only under swapMu.Lock()
// inside SetMocksWithWindow. A reader here uses swapMu.RLock to avoid
// a torn read on the time.Time. An inherently-racy pre-check is
// tolerable — a caller that needs strict window/pool atomicity
// consults GetPerTestMocksInWindow, which snapshots both under swapMu.
//
// Non-atomic-pair warning: a caller that reads IsTestWindowActive and
// HasFirstTestFired sequentially can observe an inconsistent pair
// during a SetCurrentTestWindow / SetMocksWithWindow transition
// because the two bits live under different locks (windowMu vs
// swapMu). For pair-coherent reads use WindowSnapshot, which
// takes both locks in declared order and returns a tear-free snapshot.
func (m *MockManager) HasFirstTestFired() bool {
	m.swapMu.RLock()
	defer m.swapMu.RUnlock()
	return !m.firstWindowStart.IsZero()
}

// FirstTestWindowStart returns the earliest test window start this
// MockManager has observed, or the zero time if no non-BaseTime
// SetMocksWithWindow call has landed yet.
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
	m.swapMu.RLock()
	defer m.swapMu.RUnlock()
	m.windowMu.RLock()
	defer m.windowMu.RUnlock()
	return models.WindowSnapshot{
		Active:         !m.windowStart.IsZero() && !m.windowEnd.IsZero(),
		FirstTestFired: !m.firstWindowStart.IsZero(),
	}
}

// GetPerTestMocksInWindow is the unification-plan canonical name for
// the time-windowed per-test pool. Aliases GetFilteredMocksInWindow
// during Phase 2 migration. SetMocksWithWindow already pre-filters
// the per-test slice by request-timestamp containment BEFORE building
// the tree, so GetFilteredMocksInWindow's "iterate and window-check"
// is effectively a no-op fast path; the remaining Phase-3 work is to
// collapse this accessor into a plain tree Snapshot with no iteration
// overhead at all.
func (m *MockManager) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	return m.GetFilteredMocksInWindow()
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

// SessionMockHitCounts returns a snapshot of HitCount for every
// session- or connection-scoped mock currently in memory. Keyed by
// mock.Name; value is the current atomic counter read. Per-test
// mocks are excluded — their counter is always 0 or 1 (consumed on
// match) so they add no observability value.
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
		out[mock.Name] = atomic.LoadUint64(&mock.TestModeInfo.HitCount)
		return true
	})
	return out
}

// NEW: kind-scoped getters used by Redis matcher
func (m *MockManager) GetFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	// Fetch pointer safely; the tree itself is responsible for its own safety.
	m.treesMu.RLock()
	flt := m.filteredByKind[kind]
	m.treesMu.RUnlock()
	if flt == nil {
		flt, _ = m.ensureKindTrees(kind)
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
	newFiltered := NewTreeDb(customComparator)
	newFilteredByKind := make(map[models.Kind]*TreeDb)
	newStateless := make(map[models.Kind]map[string][]*models.Mock)
	touched := map[models.Kind]struct{}{}
	var maxSortOrder int64
	for index, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		if mock.TestModeInfo.SortOrder > maxSortOrder {
			maxSortOrder = mock.TestModeInfo.SortOrder
		}
		mock.TestModeInfo.ID = index
		newFiltered.insert(mock.TestModeInfo, mock)
		k := mock.Kind
		td := newFilteredByKind[k]
		if td == nil {
			td = NewTreeDb(customComparator)
			newFilteredByKind[k] = td
		}
		td.insert(mock.TestModeInfo, mock)
		touched[k] = struct{}{}
		if newStateless[k] == nil {
			newStateless[k] = make(map[string][]*models.Mock)
		}
		key := mock.Name
		if mock.Kind == models.DNS && mock.Spec.DNSReq != nil {
			key = strings.ToLower(dns.Fqdn(mock.Spec.DNSReq.Name))
		}
		newStateless[k][key] = append(newStateless[k][key], mock)
	}
	if maxSortOrder > 0 {
		pkg.UpdateSortCounterIfHigher(maxSortOrder)
	}
	m.treesMu.Lock()
	m.filtered, m.filteredByKind, m.statelessFiltered = newFiltered, newFilteredByKind, newStateless
	m.treesMu.Unlock()
	for k := range touched {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
}

func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	newUnFiltered := NewTreeDb(customComparator)
	newUnFilteredByKind := make(map[models.Kind]*TreeDb)
	newStateless := make(map[models.Kind]map[string][]*models.Mock)
	touched := map[models.Kind]struct{}{}
	var maxSortOrder int64
	for index, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		if mock.TestModeInfo.SortOrder > maxSortOrder {
			maxSortOrder = mock.TestModeInfo.SortOrder
		}
		mock.TestModeInfo.ID = index
		newUnFiltered.insert(mock.TestModeInfo, mock)
		k := mock.Kind
		td := newUnFilteredByKind[k]
		if td == nil {
			td = NewTreeDb(customComparator)
			newUnFilteredByKind[k] = td
		}
		td.insert(mock.TestModeInfo, mock)
		touched[k] = struct{}{}
		if newStateless[k] == nil {
			newStateless[k] = make(map[string][]*models.Mock)
		}
		key := mock.Name
		if mock.Kind == models.DNS && mock.Spec.DNSReq != nil {
			key = strings.ToLower(dns.Fqdn(mock.Spec.DNSReq.Name))
		}
		newStateless[k][key] = append(newStateless[k][key], mock)
	}
	if maxSortOrder > 0 {
		pkg.UpdateSortCounterIfHigher(maxSortOrder)
	}
	m.treesMu.Lock()
	m.unfiltered, m.unfilteredByKind, m.statelessUnfiltered = newUnFiltered, newUnFilteredByKind, newStateless
	m.treesMu.Unlock()
	for k := range touched {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
}

// ---------- point updates / deletes (keep per-kind in sync) ----------

func (m *MockManager) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	// Snapshot the legacy tree pointer safely
	m.treesMu.RLock()
	globalTree := m.unfiltered
	m.treesMu.RUnlock()
	// Update legacy/global tree first
	updatedGlobal := globalTree.update(old.TestModeInfo, new.TestModeInfo, new)

	oldK, newK := old.Kind, new.Kind
	var updatedOldKind, updatedNewKind bool

	if oldK == newK {
		// Same kind: update the per-kind tree under lock
		_, unf := m.ensureKindTrees(newK)
		m.treesMu.Lock()
		updatedNewKind = unf.update(old.TestModeInfo, new.TestModeInfo, new)

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
		updatedNewKind = newUnf.update(old.TestModeInfo, new.TestModeInfo, new)
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
			ReqTimestampMock: models.FormatMockTimestamp(new.Spec.ReqTimestampMock),
			ResTimestampMock: models.FormatMockTimestamp(new.Spec.ResTimestampMock),
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
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
	m.treesMu.RLock()
	globalTree := m.filtered
	m.treesMu.RUnlock()
	deletedGlobal := globalTree.delete(mock.TestModeInfo)

	// per-kind
	k := mock.Kind
	flt, _ := m.ensureKindTrees(k)
	m.treesMu.Lock()
	deletedKind := flt.delete(mock.TestModeInfo)
	m.treesMu.Unlock()

	if deletedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:             mock.Name,
			Kind:             mock.Kind,
			Usage:            models.Deleted,
			IsFiltered:       mock.TestModeInfo.IsFiltered,
			SortOrder:        mock.TestModeInfo.SortOrder,
			Type:             mock.Spec.Metadata["type"],
			ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
			ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
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

func (m *MockManager) DeleteUnFilteredMock(mock models.Mock) bool {
	m.treesMu.RLock()
	globalTree := m.unfiltered
	m.treesMu.RUnlock()
	deletedGlobal := globalTree.delete(mock.TestModeInfo)

	// per-kind
	k := mock.Kind
	_, unf := m.ensureKindTrees(k)
	m.treesMu.Lock()
	deletedKind := unf.delete(mock.TestModeInfo)
	m.treesMu.Unlock()

	if deletedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:             mock.Name,
			Kind:             mock.Kind,
			Usage:            models.Deleted,
			IsFiltered:       mock.TestModeInfo.IsFiltered,
			SortOrder:        mock.TestModeInfo.SortOrder,
			Type:             mock.Spec.Metadata["type"],
			ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
			ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
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

// MarkMockAsUsed marks the given mock as used (consumed) without modifying
// its sort order or removing it from any tree. This is intended for parsers
// (e.g. mongo v2) that need to record mock usage without changing mock ordering.
//
// Unification observability: also bumps the HitCount on the LIVE mock
// in the tree (not on the snapshot value passed by the caller, which
// is a struct copy). For session- and connection-scoped mocks that's
// the reuse telemetry surfaced by SessionMockHitCounts; for per-test
// mocks it stays at 0 or 1 since they're consumed on match so the
// bump has no semantic meaning but is harmless.
func (m *MockManager) MarkMockAsUsed(mock models.Mock) bool {
	if mock.Name == "" {
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
		ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
	}); err != nil {
		if m.logger != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
		return false
	}
	return true
}

// bumpHitCount locates the live *models.Mock by name via the hit
// index and atomically increments its TestModeInfo.HitCount. O(1) —
// MarkMockAsUsed sits on the match hot path so a tree scan per call
// would be a measurable CPU cost on large pools.
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
	// touching hitIdx / mutating TestModeInfo counters on mocks the
	// manager has logically released.
	if m.closed.Load() {
		return
	}
	// Fast path: O(1) index lookup under RLock.
	m.hitMu.RLock()
	mk, fastHit := m.hitIdx[name]
	m.hitMu.RUnlock()
	if fastHit && mk != nil {
		atomic.AddUint64(&mk.TestModeInfo.HitCount, 1)
		return
	}

	// Slow path: hold the write lock across the discover-and-seed
	// sequence so a concurrent slow-path caller for the same name
	// re-checks and sees the seeded entry instead of walking the
	// tree again (and double-incrementing).
	m.hitMu.Lock()
	defer m.hitMu.Unlock()
	if mk2, ok := m.hitIdx[name]; ok && mk2 != nil {
		atomic.AddUint64(&mk2.TestModeInfo.HitCount, 1)
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
		atomic.AddUint64(&target.TestModeInfo.HitCount, 1)
		m.hitIdx[name] = target
	}
}

// rebuildHitIndex walks the given mock slices and replaces m.hitIdx
// with a fresh name → *Mock map. Called from SetFilteredMocks +
// SetUnFilteredMocks after their tree swap so the fast-path bump
// sees the new pool. Collisions (duplicate names across pools)
// resolve to the last inserted pointer — acceptable for a telemetry
// counter; the reuse metric aggregates by name anyway.
func (m *MockManager) rebuildHitIndex(slices ...[]*models.Mock) {
	total := 0
	for _, s := range slices {
		total += len(s)
	}
	idx := make(map[string]*models.Mock, total)
	for _, s := range slices {
		for _, mk := range s {
			if mk != nil && mk.Name != "" {
				idx[mk.Name] = mk
			}
		}
	}
	m.hitMu.Lock()
	m.hitIdx = idx
	m.hitMu.Unlock()
}

// ---------- bookkeeping ----------

// flagMockAsUsed records that a mock was consumed from the network.
// The first call for a given name establishes its position in the consumption
// order; subsequent calls for the same name update the stored state in-place
// without changing its position. This preserves true network call order in
// GetConsumedMocks.
func (m *MockManager) flagMockAsUsed(mock models.MockState) error {
	if mock.Name == "" {
		return fmt.Errorf("mock is empty")
	}
	m.consumedMu.Lock()
	if idx, exists := m.consumedIndex[mock.Name]; exists {
		m.consumedList[idx] = mock // update state, preserve position
	} else {
		m.consumedIndex[mock.Name] = len(m.consumedList)
		m.consumedList = append(m.consumedList, mock)
	}
	m.consumedMu.Unlock()
	return nil
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
