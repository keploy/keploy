package replay

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
)

type Instrumentation interface {
	//Setup prepares the environment for the recording
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) (int, error)
	//Hook will load hooks and start the proxy server.
	Hook(ctx context.Context, id int, opts models.HookOptions) error
	MockOutgoing(ctx context.Context, id int, mocks []models.Frame, opts models.IncomingOptions) error
	SetMocks(ctx context.Context, id int, mocks []models.Frame) error
	// Run is blocking call and will execute until error
	Run(ctx context.Context, id int, opts models.RunOptions) error
}

type Service interface {
}

// TestDB will only be readonly
type TestDB interface {
}

// MockDB will only be readonly
type MockDB interface {
}

type ReportDB interface {
}

type Telemetry interface {
}
