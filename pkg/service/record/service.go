package record

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type Instrumentation interface {
	//Setup prepares the environment for the recording
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) (int, error)
	//Hook will load hooks and start the proxy server.
	Hook(ctx context.Context, id int, opts models.HookOptions) error
	GetIncoming(ctx context.Context, id int, opts models.IncomingOptions) (chan models.Frame, chan models.IncomingError)
	GetOutgoing(ctx context.Context, id int, opts models.OutgoingOptions) (chan models.Frame, chan models.OutgoingError)
	// Run is blocking call and will execute until error
	Run(ctx context.Context, id int, opts models.RunOptions) models.AppError
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
