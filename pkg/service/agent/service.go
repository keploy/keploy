package agent

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type Service interface {
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) error
	GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error)
	GetOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) (<-chan *models.Mock, error)
	MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error
	SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error
	GetConsumedMocks(ctx context.Context, id uint64) ([]string, error)
	RegisterClient(ctx context.Context, opts models.SetupOptions) error
	DeRegisterClient(ctx context.Context, opts models.UnregisterReq) error
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
