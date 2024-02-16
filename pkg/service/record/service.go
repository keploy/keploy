package record

import (
	"context"
	"go.keploy.io/server/pkg/models"
)

type Instrumentation interface {
	// Run is blocking call and will execute until error
	Run(cmd string) error
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
