package record

import (
	"context"
	"go.keploy.io/server/pkg/models"
)

type Instrumentation interface {
	// Run is blocking call and will execute until error
	// if hook is false then application will just be started but not instrumented.
	Run(ctx context.Context, cmd string, hook bool) error
	GetIncoming(ctx context.Context) (chan models.Frame, error)
	GetOutgoing(ctx context.Context) (chan models.Frame, error)
}

type Service interface {
}

type TestDB interface {
}

type MockDB interface {
}

type Telemetry interface {
}
