// Package mockreplay provides functionality for replaying recorded mocks during application testing.
package mockreplay

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Service defines the interface for replaying recorded mocks.
type Service interface {
	// Replay runs the app command with mocks from the specified file.
	Replay(ctx context.Context, opts models.ReplayOptions) (*models.ReplayResult, error)
}

// AgentService defines the interface for interacting with the Keploy agent.
type AgentService interface {
	// Setup prepares the agent for replay.
	Setup(ctx context.Context, startCh chan int) error
	// MockOutgoing enables mock mode for outgoing calls.
	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error
	// SetMocks sets the mocks to be replayed.
	SetMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	// GetConsumedMocks returns the mocks that were consumed during replay.
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
}

// MockDB defines the interface for mock database operations.
type MockDB interface {
	// GetFilteredMocks retrieves mocks from the database.
	GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	// GetUnFilteredMocks retrieves unfiltered mocks from the database.
	GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
}
