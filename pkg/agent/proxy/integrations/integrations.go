// Package integrations provides functionality for integrating different types of services.
package integrations

import (
	"context"
	"net"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type Initializer func(logger *zap.Logger) Integrations

type IntegrationType string

// constants for different types of integrations
const (
	HTTP        IntegrationType = "http"
	HTTP2       IntegrationType = "http2"
	GRPC        IntegrationType = "grpc"
	GENERIC     IntegrationType = "generic"
	MYSQL       IntegrationType = "mysql"
	POSTGRES_V1 IntegrationType = "postgres_v1"
	POSTGRES_V2 IntegrationType = "postgres_v2"
	POSTGRES_V3 IntegrationType = "postgres_v3"
	MONGO_V1    IntegrationType = "mongo_v1"
	MONGO_V2    IntegrationType = "mongo_v2"
	AEROSPIKE   IntegrationType = "aerospike"
)

type Parsers struct {
	Initializer Initializer
	Priority    int
}

var Registered = make(map[IntegrationType]*Parsers)

type Integrations interface {
	MatchType(ctx context.Context, reqBuf []byte) bool
	RecordOutgoing(ctx context.Context, session *RecordSession) error
	MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb MockMemDb, opts models.OutgoingOptions) error
}

// IntegrationsV2 is the optional capability interface implemented by
// parsers that have been migrated to the supervisor + relay + FakeConn
// architecture (see PLAN.md at the repository root).
//
// The dispatcher in handleConnection performs a type assertion against
// this interface. Parsers that satisfy it and return true from IsV2()
// are run under the supervisor with a supervisor.Session attached via
// RecordSession.V2; their RecordOutgoing must consume V2.ClientStream,
// V2.DestStream, and V2.Directives rather than the legacy Ingress /
// Egress / TLSUpgrader fields.
//
// Parsers that do not satisfy this interface continue on the legacy
// path unchanged; migration is parser-by-parser and additive.
type IntegrationsV2 interface {
	Integrations
	// IsV2 reports that this parser consumes RecordSession.V2 and is
	// safe to run under supervisor.Run. A parser may return false
	// dynamically (e.g. if a required config is missing) to opt out
	// on a per-instance basis.
	IsV2() bool
}

func Register(name IntegrationType, p *Parsers) {
	Registered[name] = p
}

// MockMemDb is the runtime mock pool contract consumed by every parser's
// matcher. It is intentionally a narrow interface over the in-memory
// pool structure owned by the agent's MockManager (see
// pkg/agent/proxy/mockmanager.go).
//
// The surface is split into four focused facets — MockReader,
// MockWriter, MockConsumer, WindowAware — and MockMemDb itself is
// the composite of those four. The composite shape is unchanged from
// prior releases so every existing implementation (the agent's
// MockManager, test doubles like match_test.go's mockMemDb) continues
// to satisfy it without edits. Parsers that only read mocks can now
// type their argument as MockReader; matchers that only need the
// window accessors can take WindowAware; and so on. This is
// opt-in — the Integrations.MockOutgoing signature still passes the
// composite MockMemDb, so migrating a parser to a narrower facet is
// a local, mechanical change.
//
// Unification plan (see docs/explanation/mock-lifetimes.md):
//
//   - The primary, lifetime-aware methods are GetSessionMocks,
//     GetPerTestMocksInWindow, and GetConnectionMocks(connID). Parsers
//     should consume THESE going forward.
//
//   - The historical methods (GetFilteredMocks, GetUnFilteredMocks,
//     GetFilteredMocksInWindow) remain as forwarding aliases during the
//     per-parser migration. They will be removed in Phase 4 once every
//     in-tree parser migrates. Third-party parsers consuming MockMemDb
//     will see a deprecation window of at least one release.
//
// Lifetime semantics:
//
// Parsers should NOT assume a single universal "most specific first"
// ranking — the right order is protocol-dependent. Common pattern in
// the in-tree matchers (HTTP, MySQL):
//
//   - Start with per-test mocks via GetPerTestMocksInWindow. They are
//     scoped to the current test, are the most specific candidate,
//     and are consumed on match via DeleteFilteredMock.
//   - Merge in session mocks from GetSessionMocks. Session-scoped
//     traffic covers handshake, auth, heartbeats, gRPC reflection,
//     Kafka coordination, Redis HELLO/AUTH, etc. Reusable across
//     tests; not consumed on match.
//   - Consult connection-scoped mocks via GetConnectionMocks(connID)
//     for protocol state tied to a live connection (e.g. prepared-
//     statement PREPARE/EXECUTE pairing). These never window-filter
//     and are bounded by the connection's own lifecycle.
//
// Protocols with state ordering requirements (e.g. MySQL's EXEC after
// PREPARE) may need to consult the connection pool ahead of per-test
// — follow the existing matcher's shape when in doubt.
type MockMemDb interface {
	MockReader
	MockWriter
	MockConsumer
	WindowAware
}

// MockReader is the read-only facet of MockMemDb. Parsers that need
// to enumerate mocks from the in-memory pool but never mutate or
// consume on match should take this interface directly. Includes both
// the historical accessor API (GetFilteredMocks / GetUnFilteredMocks
// / GetFilteredMocksInWindow) and the unification API (GetSessionMocks
// / GetPerTestMocksInWindow / GetStartupMocks / GetConnectionMocks /
// GetSessionScopedMocks).
type MockReader interface {
	// --- Historical API (alias layer during Phase 2 migration). ---
	//
	// Deprecated: use GetPerTestMocksInWindow or GetSessionMocks.
	// GetFilteredMocks returns the current per-test-pool snapshot,
	// ignoring any active test window. Kept for compatibility with
	// parsers that have not yet migrated.
	GetFilteredMocks() ([]*models.Mock, error)
	// Deprecated: use GetSessionMocks.
	// GetUnFilteredMocks returns the current session-pool snapshot.
	GetUnFilteredMocks() ([]*models.Mock, error)

	GetMySQLCounts() (total, config, data int)

	// Deprecated: use GetPerTestMocksInWindow.
	GetFilteredMocksInWindow() ([]*models.Mock, error)

	// --- Unification API (primary going forward). ---

	// GetPerTestMocksInWindow returns only those per-test (Lifetime ==
	// LifetimePerTest) mocks whose Spec.ReqTimestampMock falls inside
	// the current window [start, end]. Mocks whose Spec.ResTimestampMock
	// is EARLIER than Spec.ReqTimestampMock are excluded as invalid.
	// Response timestamps after `end` are tolerated (async straggle).
	// If no window has been set (zero values), falls back to the full
	// per-test pool snapshot.
	//
	// Canonical name; GetFilteredMocksInWindow aliases this.
	GetPerTestMocksInWindow() ([]*models.Mock, error)

	// Deprecated: Wave 2 split startup-tier traffic out of the session
	// pool into its own storage tier. GetSessionMocks now returns the
	// UNION of startup + session so pre-wave-2 parsers keep working; new
	// parsers that want strict tier separation should call
	// GetStartupMocks and GetSessionScopedMocks directly.
	//
	// GetSessionMocks returns the session-scoped mock pool snapshot
	// (Lifetime == LifetimeSession) plus any startup-tier mocks (those
	// whose Spec.ReqTimestampMock predated the first test window).
	// Session mocks are reusable across every test — never
	// window-filtered, never consumed on match.
	GetSessionMocks() ([]*models.Mock, error)

	// GetStartupMocks returns the startup-tier mock pool snapshot —
	// app-bootstrap recordings whose Spec.ReqTimestampMock predates the
	// first HTTP test window (Flyway migrations, Hibernate metadata
	// boot, HikariCP pool warm-up). Strictly disjoint from
	// GetSessionScopedMocks, GetPerTestMocksInWindow, and
	// GetConnectionMocks; a mock lives in exactly one of the four
	// tiers.
	//
	// Wave 2 addition: tier-aware parsers build a dedicated startup
	// index from this pool to serve bootstrap traffic, then switch to
	// the session / per-test tiers as the runner advances into real
	// test windows.
	GetStartupMocks() ([]*models.Mock, error)

	// GetStartupMocksByKind returns startup-tier mocks matching the
	// given Kind. Symmetric counterpart to the session- / per-test-
	// tier by-kind accessors (GetUnFilteredMocksByKind,
	// GetFilteredMocksByKind). Parsers that build per-kind indices
	// over the startup tier should prefer this over filtering the
	// GetStartupMocks snapshot client-side.
	//
	// Returns (nil, nil) when the startup tree has never been
	// populated on the underlying manager (fresh manager, pre-first
	// SetMocksWithWindow call).
	GetStartupMocksByKind(kind models.Kind) ([]*models.Mock, error)

	// GetSessionScopedMocks returns the session-tier + connection-tagged
	// mocks strictly — startup-tier entries are NOT included (those are
	// in GetStartupMocks). Parsers opting into the Wave 2 tier-aware
	// routing call this in preference to the legacy GetSessionMocks
	// union shim.
	GetSessionScopedMocks() ([]*models.Mock, error)

	// GetConnectionMocks returns connection-scoped mock pool entries
	// (Lifetime == LifetimeConnection) associated with the given
	// connID (Spec.Metadata["connID"]). These are reusable for the
	// lifetime of that specific client connection only. Prepared-
	// statement setup mocks (Postgres Parse, MySQL COM_STMT_PREPARE)
	// use this pool so their executes still find them across test-
	// window boundaries while remaining isolated per connection.
	//
	// Returns an empty slice (no error) if connID has no associated
	// connection pool — the caller should then fall through to the
	// session / per-test pools.
	GetConnectionMocks(connID string) ([]*models.Mock, error)

	// SessionMockHitCounts returns per-mock atomic HitCount values for
	// session- and connection-scoped mocks. Used by replay summary
	// output and "which reusable mocks actually got reused?" telemetry.
	// Key is mock.Name; value is the atomic counter's current read.
	// Inherently racy as a snapshot — counters may increment during
	// iteration — but that's tolerable for observability.
	SessionMockHitCounts() map[string]uint64
}

// MockWriter is the in-place mutation facet of MockMemDb — narrow on
// purpose, because in-place mock updates are rare (the recorder writes
// through a different path). UpdateUnFilteredMock is used by parsers
// that rewrite a session-pool entry after augmenting it on the fly
// (e.g. stamping server-issued auth tokens onto a session mock so a
// later matcher sees the updated bytes).
type MockWriter interface {
	UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool
}

// MockConsumer is the match-time consumption facet of MockMemDb.
// Per-test mocks are consumed on match (DeleteFilteredMock), session
// and connection mocks are marked-used for telemetry
// (MarkMockAsUsed); DeleteUnFilteredMock exists for the rare consumer
// that eagerly evicts a session-pool entry (e.g. the MySQL prepared-
// statement teardown path on COM_STMT_CLOSE); DeleteStartupMock
// consumes a startup-tier mock matched during application bootstrap so
// the next identical query picks the next-recorded mock in chronological
// order (the boot path's analogue of DeleteFilteredMock for per-test).
type MockConsumer interface {
	DeleteFilteredMock(mock models.Mock) bool
	DeleteUnFilteredMock(mock models.Mock) bool
	// DeleteStartupMock removes a matched startup-tier mock from the
	// startup tree. Returns true if the mock was found and removed,
	// false otherwise (e.g. the mock wasn't classified into startup at
	// load time, or it has already been consumed by a prior match).
	//
	// Required for boot-phase replay correctness when an application
	// issues the same query repeatedly during recording while DB state
	// mutates: the recorder captures multiple same-shape mocks with
	// diverging responses, the boot-phase matcher (with the earliest-
	// wins tiebreaker) picks them in chronological order, and consumption
	// here advances through the recorded sequence so the booting app
	// sees the same response chain it saw at record time. Without this,
	// the matcher repeatedly picks the same earliest mock and the
	// booting app's follow-on revalidation queries fail.
	DeleteStartupMock(mock models.Mock) bool
	MarkMockAsUsed(mock models.Mock) bool
}

// WindowAware is the test-window facet of MockMemDb. Parsers that
// partition their index into per-test / session / startup tiers
// consult these accessors at dispatch time to pick the right tier
// for a live query. The pair (IsTestWindowActive, HasFirstTestFired)
// is NON-ATOMIC — callers that need a coherent point-in-time read of
// both bits MUST use WindowSnapshot.
type WindowAware interface {
	// SetCurrentTestWindow records the [start,end] timestamps of the outer
	// HTTP/gRPC test currently being replayed. Parsers use this window via
	// GetPerTestMocksInWindow to filter non-config mocks by their REQUEST
	// timestamp. Responses may legitimately straggle past `end` (downstream
	// async completion); mocks with an invalid timestamp ordering
	// (ResTimestampMock earlier than ReqTimestampMock) are dropped as a
	// sanity check, but response containment is NOT enforced against `end`.
	SetCurrentTestWindow(start, end time.Time)

	// IsTestWindowActive reports whether a non-zero test window has been
	// set via SetCurrentTestWindow or SetMocksWithWindow. Parsers that
	// partition their index into per-test and session tiers consult this
	// at dispatch time to decide which tier a live query should be served
	// from: true = inside a test-body window (route to per-test index),
	// false = session / connection-scoped traffic (route to session index).
	//
	// Inherently racy — a concurrent test-window flip could change the
	// answer between observation and use — but callers that need strict
	// window/pool atomicity go through GetPerTestMocksInWindow, which
	// snapshots both under the manager's swap lock.
	IsTestWindowActive() bool

	// HasFirstTestFired reports whether at least one real test window
	// has been set on the underlying MockManager via SetMocksWithWindow
	// (non-zero start that is not the BaseTime staging sentinel).
	// Sticky — once true, stays true.
	//
	// Parsers use this alongside IsTestWindowActive to distinguish
	// "app bootstrap" (before first test) from "between tests" (after
	// the first test fired but no test currently active):
	//
	//   IsTestWindowActive() == true                  → perTest tier
	//   !IsTestWindowActive() &&  HasFirstTestFired() → session tier
	//   !IsTestWindowActive() && !HasFirstTestFired() → startup tier
	//
	// NON-ATOMIC-PAIR WARNING: reading IsTestWindowActive and
	// HasFirstTestFired sequentially can observe the forbidden
	// intermediate state Active=true && FirstTestFired=false during a
	// SetCurrentTestWindow / SetMocksWithWindow transition because the
	// two bits are guarded by different locks on the underlying
	// MockManager. Callers that need the pair as a coherent point-in-
	// time read (the v3 dispatcher's routeTransactional and
	// TierIndex.orderForCurrentState) MUST use WindowSnapshot instead.
	HasFirstTestFired() bool

	// FirstTestWindowStart returns the earliest test window start observed
	// by the MockManager, or zero if no real test has fired yet (i.e. every
	// SetMocksWithWindow call so far was either absent or fired with the
	// models.BaseTime staging sentinel). Used by filter / strict-gate
	// callers to distinguish startup-init mocks (req_ts < firstWindowStart)
	// from stale previous-test mocks (firstWindowStart <= req_ts <
	// currentStart): the former are legitimate app-bootstrap traffic that
	// must be preserved, the latter are cross-test bleed that must be
	// dropped.
	FirstTestWindowStart() time.Time

	// WindowSnapshot returns the (IsTestWindowActive, HasFirstTestFired)
	// pair under one outer lock on the underlying MockManager, so
	// callers that read BOTH bits cannot observe a torn intermediate
	// state during a concurrent SetCurrentTestWindow /
	// SetMocksWithWindow transition.
	//
	// Required for any caller that routes based on the PAIR (the v3
	// Postgres dispatcher's routeTransactional and
	// types.TierIndex.orderForCurrentState). The individual bool
	// accessors are retained for legacy callers that read only one bit.
	WindowSnapshot() models.WindowSnapshot
}
