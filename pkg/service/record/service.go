package record

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
)

type Instrumentation interface {
	//Hook will load hooks and start the proxy server.
	Hook(ctx context.Context, opt models.HookOptions) error
	GetIncoming(ctx context.Context, opts models.IncomingOptions) (chan models.Frame, error)
	GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (chan models.Frame, error)
	// Run is blocking call and will execute until error
	Run(ctx context.Context, cmd string) error
}

type Service interface {
}

type TestDB interface {
}

type MockDB interface {
}

type Telemetry interface {
}
