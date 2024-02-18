package integrations

import (
	"context"
	"go.keploy.io/server/v2/pkg/core"
	"go.uber.org/zap"
	"net"
)

type Initializer func(logger *zap.Logger, opts core.ProxyOptions) Integrations

var Registered = make(map[string]Initializer)

type Integrations interface {
	OutgoingType(ctx context.Context, buffer []byte) bool //Change the name of the function
	ProcessOutgoing(ctx context.Context, buffer []byte, conn net.Conn, dst net.Conn) error
}

func Register(name string, i Initializer) {
	Registered[name] = i
}
