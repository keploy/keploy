package record

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type Instrumentation interface {
	Hook(ctx context.Context, opt models.HookOptions) error
	GetIncoming(ctx context.Context, opts models.IncomingOptions) (chan models.Frame, chan models.IncomingError)
	GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (chan models.Frame, chan models.OutgoingError)
	Run(ctx context.Context, cmd string) models.AppError
}

type Service interface {
	Record(ctx context.Context) error
	MockRecord(ctx context.Context) error
}

type TestDB interface {
	GetAllTestSetIds(ctx context.Context) ([]string, error)
	InsertTestCase(ctx context.Context, tc models.Frame, testSetId string) error
}

type MockDB interface {
	InsertMock(ctx context.Context, mock models.Frame, testSetId string) error
}

type Telemetry interface {
}
