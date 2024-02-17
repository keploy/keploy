package replay

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
)

type Instrumentation interface {
	//Hook will load hooks and start the proxy server.
	Hook(ctx context.Context, opt models.HookOptions) error
	MockOutgoing(ctx context.Context, mocks []models.Frame, opts models.IncomingOptions) error
	SetMocks(ctx context.Context, mocks []models.Frame) error
	// Run is blocking call and will execute until error
	Run(ctx context.Context, cmd string) error
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
