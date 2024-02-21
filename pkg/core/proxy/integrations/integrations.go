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
// TODO: Change the name of the struct
type ConditionalDstCfg struct {
	Addr   string // Destination Addr (ip:port)
	Port   uint
	TlsCfg *tls.Config
}

type Integrations interface {
	OutgoingType(ctx context.Context, reqBuf []byte) bool
	RecordOutgoing(ctx context.Context, reqBuf []byte, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error
	MockOutgoing(ctx context.Context, reqBuf []byte, src net.Conn, dstCfg *ConditionalDstCfg, mockDb MockMemDb, opts models.OutgoingOptions) error
}

func Register(name string, i Initializer) {
	Registered[name] = i
}

type MockMemDb interface {
	GetFilteredMocks() ([]*models.Mock, error)
	GetUnFilteredMocks() ([]*models.Mock, error)
	UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool
	DeleteFilteredMock(mock *models.Mock) bool
	DeleteUnFilteredMock(mock *models.Mock) bool
}
