package integrations

import (
	"context"
	"crypto/tls"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"net"
)

type Initializer func(logger *zap.Logger) Integrations

var Registered = make(map[string]Initializer)

// Used to establish destination connection in case of any passthrough.
type ConditionalDstCfg struct {
	Addr   string // Destination Addr (ip:port)
	Port   uint
	TlsCfg *tls.Config
}

type Integrations interface {
	OutgoingType(ctx context.Context, reqBuf []byte) bool //Change the name of the function
	RecordOutgoing(ctx context.Context, reqBuf []byte, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error
	MockOutgoing(ctx context.Context, reqBuf []byte, src net.Conn, dstCfg *ConditionalDstCfg, mocks []*models.Mock, opts models.OutgoingOptions) error
}

func Register(name string, i Initializer) {
	Registered[name] = i
}
