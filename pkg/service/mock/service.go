// Package mock implements the `keploy mock record|replay` flow: using Keploy as
// a framework-agnostic mocking layer for a user's own test runner (pytest, go
// test, jest/playwright, mobile UI tests). Unlike record/test it captures ONLY
// outgoing dependency calls (no incoming test cases become test cases) into a
// single named mock set, and on replay serves that set back to the wrapped
// runner while propagating the runner's own exit code.
//
// The same *http.AgentClient instrumentation, RecordHooks and app hooks that
// `record`/`test` use flow through here unchanged, so enterprise runtime
// features (LD_PRELOAD / java-agent / go-binpatch TLS shims, time-freeze,
// secret obfuscation, registry upload) apply to `keploy mock` with no
// mock-specific wiring.
package mock

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Service is the mock record/replay flow. Record captures outgoing calls into
// the configured named set; Replay serves that set to the wrapped runner.
// Both return the wrapped runner's exit code (or an error only for setup
// failures) and set utils.ErrCode so the keploy process mirrors the runner.
type Service interface {
	Record(ctx context.Context) error
	Replay(ctx context.Context) error
}

// Instrumentation is the subset of the agent/proxy control surface the mock
// flow needs. It is satisfied verbatim by *pkg/platform/http.AgentClient — the
// same client record/replay use — so the mock flow inherits every runtime
// behaviour that client applies to the wrapped command.
type Instrumentation interface {
	// Setup prepares the environment: starts the agent, loads hooks and the
	// proxy, and prepares the wrapped command. opts.MockMode must be set so
	// the agent does NOT relocate the runner's listening ports.
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) error

	// GetOutgoing switches the proxy to record mode and streams every captured
	// outgoing call as a mock. Used by Record.
	GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error)

	// MockOutgoing switches the proxy to mock-serving mode. Used by Replay.
	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error

	// StoreMocks pushes the loaded set into the proxy. Used by Replay.
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error

	// UpdateMockParams selects which mocks the proxy serves (whole pool, or —
	// via MockMapping — a single scoped test's mocks). Used by Replay.
	UpdateMockParams(ctx context.Context, params models.MockFilterParams) error

	// GetConsumedMocks / GetMockErrors report which mocks were served and which
	// outgoing calls matched nothing, for the end-of-run summary and --strict.
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
	GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error)

	// Run runs the wrapped command and blocks until it exits, returning its
	// exit status in AppError.ExitCode.
	Run(ctx context.Context, opts models.RunOptions) models.AppError

	MakeAgentReadyForDockerCompose(ctx context.Context) error
	NotifyGracefulShutdown(ctx context.Context) error
}

// ScopeReader is an optional Instrumentation extension: the per-test scope
// windows the wrapped runner reported through the agent's /agent/scope/* API
// during a record session. Record uses it to write mappings.yaml so replay can
// serve mocks per test. When the instrumentation does not implement it (or the
// runner made no scope calls) the set is recorded suite-level and replay serves
// the whole pool — still correct, just without per-test isolation.
type ScopeReader interface {
	GetScopeWindows(ctx context.Context) ([]models.ScopeWindow, error)
}

// MissCapturer is an optional Instrumentation extension: under `--on-miss
// record`, Replay drains the calls the proxy served live-from-upstream on a miss
// and appends them to the set (VCR new_episodes).
type MissCapturer interface {
	DrainCapturedMocks(ctx context.Context) ([]*models.Mock, error)
}

// ScopePusher is an optional Instrumentation extension: Replay uses it to hand
// the agent the per-test name→mock-names table (from mappings.yaml) so the
// runner's /agent/scope/begin calls can restrict the served pool per test.
type ScopePusher interface {
	PushScopeTable(ctx context.Context, table map[string][]string) error
}

// MockDB reads and writes a named mock set on disk. It is exactly the surface
// the yaml mockdb already implements, so OSS wires the file store directly and
// enterprise wraps it with registry upload/download.
type MockDB interface {
	InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error
	DeleteMocksForSet(ctx context.Context, testSetID string) error
	GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error)
	GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error)
	ResetCounterID()
	// SetCounterID seeds the mock-name counter so the next InsertMock names its
	// mock "mock-<id+1>" — used to append without reusing existing names.
	SetCounterID(id int64)
}

// MappingDB persists and reads the per-test mock mapping for a set. Optional:
// nil disables per-test scoping (suite-level record/replay).
type MappingDB interface {
	UpsertBatch(ctx context.Context, testSetID string, byTest map[string][]models.MockEntry) error
	Get(ctx context.Context, testSetID string) (map[string][]models.MockEntry, bool, error)
}

// Store is the mock-set persistence backend. OSS uses FileStore (mocks live on
// disk, no remote sync). Enterprise plugs in a registry-backed store that
// uploads after record and downloads before replay. Registry-first by default;
// `--local` selects FileStore.
type Store interface {
	// Pull materialises the named set locally before replay. FileStore is a
	// no-op (the set is already on disk).
	Pull(ctx context.Context, name string) error
	// Push publishes the named set after a successful record. FileStore is a
	// no-op.
	Push(ctx context.Context, name string) error
}
