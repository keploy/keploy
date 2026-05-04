package mock

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Service is the interface for the mock service which only handles mock loading,
// without any test case or test set concerns.
type Service interface {
	// LoadMocks fetches the mocks that belong to the given testSetID (and
	// optionally a specific testCaseName) from the database, then pushes them
	// into the proxy so outgoing calls from the application under test are
	// intercepted and served from that mock list.
	//
	// If testCaseName is empty, all mocks for the test set are loaded.
	LoadMocks(ctx context.Context, testSetID string, testCaseName string) error
}

// Instrumentation is the subset of the agent/proxy capabilities that the mock
// service needs.
type Instrumentation interface {
	// Setup prepares the environment (loads eBPF hooks, starts the proxy, etc.).
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) error

	// MockOutgoing tells the proxy to start intercepting outgoing calls and
	// serve them from the stored mock list.
	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error

	// StoreMocks pushes filtered and unfiltered mocks into the proxy so they
	// can be matched against live outgoing traffic.
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error

	// UpdateMockParams sends filtering parameters to the agent so it knows
	// how to select mocks for incoming requests (mapping-based vs timestamp-based).
	UpdateMockParams(ctx context.Context, params models.MockFilterParams) error

	// NotifyGracefulShutdown signals the proxy that the session is ending so
	// connection errors are logged at debug level instead of error level.
	NotifyGracefulShutdown(ctx context.Context) error
	MakeAgentReadyForDockerCompose(ctx context.Context) error
}

// MockDB is the subset of storage operations the mock service needs to read
// mocks from the filesystem / database.
type MockDB interface {
	GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error)
	GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error)
}

// MappingDB gives the mock service access to the test-case → mock-name
// mapping table so it can apply the same mapping-based filtering strategy
// that the replayer uses.
type MappingDB interface {
	Get(ctx context.Context, testSetID string) (map[string][]models.MockEntry, bool, error)
}

// OutgoingConfig holds the knobs that control how the proxy intercepts and
// matches outgoing calls. These mirror the fields from models.OutgoingOptions
// that are relevant to the mock service (no test-case-specific timestamps, etc.).
type OutgoingConfig struct {
	// BypassRules lists host/port patterns that should be passed through
	// without being mocked.
	BypassRules []models.BypassRule

	// MongoPassword is used to decode encrypted Mongo wire traffic.
	MongoPassword string

	// SQLDelay mimics the application startup delay used during test runs.
	SQLDelay time.Duration

	// Mocking enables or disables mock interception entirely.
	Mocking bool
}
