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
	OutgoingType(buffer []byte) bool
	ProcessOutgoing(buffer []byte, conn net.Conn, dst net.Conn, ctx context.Context)
}

func Register(name string, i Initializer) {
	Registered[name] = i
}
