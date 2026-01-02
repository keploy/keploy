// Package mock provides functionality for recording and replaying mocks for external dependencies.
package mock

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Instrumentation interface for interacting with the proxy layer for mocking.
type Instrumentation interface {
	// Setup prepares the environment for recording/replaying mocks
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) error
	// GetOutgoing returns a channel of mocks captured from outgoing calls
	GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error)
	// MockOutgoing sets up the mock matching for outgoing calls
	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error
	// Run is blocking call and will execute until error
	Run(ctx context.Context, opts models.RunOptions) models.AppError
	// StoreMocks stores mocks for replay mode
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	// UpdateMockParams updates the mock filter parameters
	UpdateMockParams(ctx context.Context, params models.MockFilterParams) error
}

// Service interface for the mock service.
type Service interface {
	// Record starts recording mocks from outgoing calls
	Record(ctx context.Context) error
	// Replay starts replaying mocks for outgoing calls
	Replay(ctx context.Context) error
}

// MockDB interface for mock storage.
type MockDB interface {
	InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error
	DeleteMocksForSet(ctx context.Context, testSetID string) error
	GetCurrMockID() int64
	ResetCounterID()
	GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	GetAllMockSetIDs(ctx context.Context) ([]string, error)
}

// Telemetry interface for tracking mock operations.
type Telemetry interface {
	RecordedMocks(mockTotal map[string]int)
}
