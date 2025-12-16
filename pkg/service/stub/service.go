// Package stub provides functionality for recording and replaying stubs/mocks for external tests.
package stub

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Service interface defines the stub service operations
type Service interface {
	// Record captures outgoing calls as mocks while running external tests
	Record(ctx context.Context) error
	// Replay serves recorded mocks while running external tests
	Replay(ctx context.Context) error
}

// Instrumentation interface for proxy setup and mock interception
type Instrumentation interface {
	// Setup prepares the environment for recording or replaying
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) error
	// GetOutgoing returns a channel of captured outgoing mocks (for recording)
	GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error)
	// MockOutgoing sets up mock responses for outgoing calls (for replay)
	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error
	// StoreMocks stores mocks for serving during replay
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	// UpdateMockParams updates mock filtering parameters
	UpdateMockParams(ctx context.Context, params models.MockFilterParams) error
	// Run executes the test command
	Run(ctx context.Context, opts models.RunOptions) models.AppError
}

// MockDB interface for mock storage operations
type MockDB interface {
	InsertMock(ctx context.Context, mock *models.Mock, stubSetID string) error
	GetFilteredMocks(ctx context.Context, stubSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	GetUnFilteredMocks(ctx context.Context, stubSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	GetCurrMockID() int64
	ResetCounterID()
}

// Telemetry interface for recording metrics
type Telemetry interface {
	RecordedMocks(mockTotal map[string]int)
}
