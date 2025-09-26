package agent

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type Service interface {
	Setup(ctx context.Context, opts models.SetupOptions) error
	GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error)
	StartIncomingProxy(ctx context.Context, opts models.IncomingOptions) (chan *models.TestCase, error) // Commenting out this for now need to move this and the instrument in the agent setup only
	GetOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) (<-chan *models.Mock, error)
	MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error
	SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error
	GetConsumedMocks(ctx context.Context, id uint64) ([]models.MockState, error)
	RegisterClient(ctx context.Context, opts models.SetupOptions) error
	DeRegisterClient(ctx context.Context, opts models.UnregisterReq) error
	StoreMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error
	UpdateMockParams(ctx context.Context, id uint64, params models.MockFilterParams) error
	// SendKtInfo(ctx context.Context, tb models.TestBenchReq) error
}

type Options struct {
	// Platform    Platform
	Network     string
	Container   string
	SelfTesting bool
	Mode        models.Mode
}

// type Platform string

// var (
// 	linux   Platform = "linux"
// 	windows Platform = "windows"
// 	mac     Platform = "mac"
// 	docker  Platform = "docker"
// )
