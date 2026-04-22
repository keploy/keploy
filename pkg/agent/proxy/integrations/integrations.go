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
	REDIS       IntegrationType = "redis"
	KAFKA       IntegrationType = "kafka"
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

func Register(name IntegrationType, p *Parsers) {
	Registered[name] = p
}

// MockMemDb is the runtime mock pool contract consumed by every parser's
// matcher. It is intentionally a narrow interface over the in-memory
// pool structure owned by the agent's MockManager (see
// pkg/agent/proxy/mockmanager.go).
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

	UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool
	DeleteFilteredMock(mock models.Mock) bool
	DeleteUnFilteredMock(mock models.Mock) bool
	GetMySQLCounts() (total, config, data int)
	MarkMockAsUsed(mock models.Mock) bool

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

	// GetSessionScopedMocks returns the session-tier + connection-tagged
	// mocks strictly — startup-tier entries are NOT included (those are
	// in GetStartupMocks). Parsers opting into the Wave 2 tier-aware
	// routing call this in preference to the legacy GetSessionMocks
	// union shim.
	GetSessionScopedMocks() ([]*models.Mock, error)

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
