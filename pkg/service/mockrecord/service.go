// Package mockrecord provides functionality for recording outgoing calls from an application.
package mockrecord

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

// Service defines the interface for recording outgoing calls.
type Service interface {
	// Record starts recording outgoing calls for the given command.
	Record(ctx context.Context, opts models.RecordOptions) (*models.RecordResult, error)
}

// AgentService defines the interface for interacting with the Keploy agent.
type AgentService interface {
	// Setup prepares the agent for recording.
	Setup(ctx context.Context, startCh chan int) error
	// GetOutgoing returns a channel of recorded outgoing mocks.
	GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error)
	// StoreMocks stores the recorded mocks.
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
}

// MockDB defines the interface for mock database operations.
type MockDB interface {
	// InsertMock inserts a mock into the database.
	InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error
}
