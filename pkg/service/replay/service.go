package replay

import (
	"context"
	"go.keploy.io/server/pkg/models"
)

type Instrumentation interface {
	// Run is blocking call and will execute until error
	Run(ctx context.Context, cmd string) error
	// if pid==0, we use parent pid. i.e keploy pid
	//Hook will load hooks and start the proxy server.
	Hook(ctx context.Context, pid uint32, opt models.InstOptions) error
	Mock(ctx context.Context, frames []models.Frame) error
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
