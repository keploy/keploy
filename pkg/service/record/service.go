package record

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
