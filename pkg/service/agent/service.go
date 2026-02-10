package agent

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

type Service interface {
	Setup(ctx context.Context, startCh chan int) error
	StartIncomingProxy(ctx context.Context, opts models.IncomingOptions) (chan *models.TestCase, error) // Commenting out this for now need to move this and the instrument in the agent setup only
	GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error)
	GetMapping(ctx context.Context) (<-chan models.TestMockMapping, error)
	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error
	SetMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	UpdateMockParams(ctx context.Context, params models.MockFilterParams) error
	// SetGracefulShutdown sets a flag to indicate the application is shutting down gracefully.
	// When this flag is set, connection errors will be logged as debug instead of error.
	SetGracefulShutdown(ctx context.Context) error
	// SendKtInfo(ctx context.Context, tb models.TestBenchReq) error
}
