package replay

import (
	"context"
	"go.keploy.io/server/pkg/models"
)

type Instrumentation interface {
	// Run is blocking call and will execute until error
	// if hook is false then application will just be started but not instrumented.
	Run(cmd string, hook bool) error
	// Mock is a blocking call and will execute until error
	// or ctx is done.
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
