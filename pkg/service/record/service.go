package record

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
)

type Instrumentation interface {
	//Hook will load hooks and start the proxy server.
	Hook(ctx context.Context, opt models.HookOptions) error
	GetIncoming(ctx context.Context, opts models.IncomingOptions) (chan models.Frame, chan models.IncomingError)
	GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (chan models.Frame, chan models.OutgoingError)
		// Run is blocking call and will execute until error
	Run(ctx context.Context, cmd string) models.AppError

	// Run is blocking call and will execute until erro
}

type Service interface {
}

type TestDB interface {
	// SetTestCase(ctx context.Context, tc models.TestCase) error
	// GetTestCase(ctx context.Context, id string) (models.TestCase, error)
	// GetTestCases(ctx context.Context) ([]models.TestCase, error)
	// DeleteTestCase(ctx context.Context, id string)
	// UpdateTestCase(ctx context.Context, tc models.TestCase) error
}

type MockDB interface {
	// SetMock(ctx context.Context, mock models.Mock) error
	// GetMock(ctx context.Context, id string) (models.Mock, error)
	// GetMocks(ctx context.Context) ([]models.Mock, error)
	// DeleteMock(ctx context.Context, id string)
	// UpdateMock(ctx context.Context, mock models.Mock) error
}

type Telemetry interface {
}
