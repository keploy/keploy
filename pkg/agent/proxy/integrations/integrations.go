// Package integrations provides functionality for integrating different types of services.
package integrations

import (
	"context"
	"net"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type Initializer func(logger *zap.Logger) Integrations

type IntegrationType string

// constants for different types of integrations
const (
	HTTP        IntegrationType = "http"
	HTTP2       IntegrationType = "http2"
	GRPC        IntegrationType = "grpc"
	GENERIC     IntegrationType = "generic"
	MYSQL       IntegrationType = "mysql"
	POSTGRES_V1 IntegrationType = "postgres_v1"
	POSTGRES_V2 IntegrationType = "postgres_v2"
	MONGO_V1    IntegrationType = "mongo_v1"
	MONGO_V2    IntegrationType = "mongo_v2"
	REDIS       IntegrationType = "redis"
)

type Parsers struct {
	Initializer Initializer
	Priority    int
}

var Registered = make(map[IntegrationType]*Parsers)

type Integrations interface {
	MatchType(ctx context.Context, reqBuf []byte) bool
	RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error
	MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb MockMemDb, opts models.OutgoingOptions) error
}

func Register(name IntegrationType, p *Parsers) {
	Registered[name] = p
}

type MockMemDb interface {
	GetFilteredMocks() ([]*models.Mock, error)
	GetUnFilteredMocks() ([]*models.Mock, error)
	UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool
	DeleteFilteredMock(mock models.Mock) bool
	DeleteUnFilteredMock(mock models.Mock) bool
	GetMySQLCounts() (total, config, data int)
}
