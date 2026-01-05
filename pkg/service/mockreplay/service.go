package mockreplay

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

// Service defines the interface for replaying recorded mocks.
type Service interface {
	Replay(ctx context.Context, opts models.ReplayOptions) (*models.ReplayResult, error)
}

// AgentService defines the agent interactions required for replaying mocks.
type AgentService interface {
	Setup(ctx context.Context, startCh chan int) error
	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error
	SetMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
}

// MockDB defines the storage interface for loading mocks.
type MockDB interface {
	LoadMocks(ctx context.Context, path string) ([]*models.Mock, error)
}
