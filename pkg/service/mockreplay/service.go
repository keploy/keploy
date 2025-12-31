package mockreplay

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

type Service interface {
	Start(ctx context.Context) error
}

type Instrumentation interface {
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) error
	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	UpdateMockParams(ctx context.Context, params models.MockFilterParams) error
	Run(ctx context.Context, opts models.RunOptions) models.AppError
	MakeAgentReadyForDockerCompose(ctx context.Context) error
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
}

type MockDB interface {
	GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
	GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error)
}

type Telemetry interface {
	MockTestRun(utilizedMocks int)
}
