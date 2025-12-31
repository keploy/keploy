package mockreplay

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Service defines the interface for mock replay functionality
type Service interface {
	// Start runs the user's command with mock injection
	Start(ctx context.Context) error
}

// Instrumentation defines the interface for setting up and running the application
type Instrumentation interface {
	// Setup prepares the environment for mock injection
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) error
	// Run executes the user's command (blocking call)
	Run(ctx context.Context, opts models.RunOptions) models.AppError
	// StoreMocks sends mocks to the agent for injection
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
}

// MockDB defines the interface for accessing mock data
type MockDB interface {
	GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
}

// TestDB defines the interface for accessing test set information
type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
}

// Telemetry defines the interface for telemetry reporting
type Telemetry interface {
	MockTestRun(utilizedMocks int)
}
