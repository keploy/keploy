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
	KAFKA       IntegrationType = "kafka"
)

type Parsers struct {
	Initializer Initializer
	Priority    int
}

var Registered = make(map[IntegrationType]*Parsers)

// Integrations interface for protocol parsers.
// Parsers receive net.Conn values that represent plaintext byte streams.
// Any TLS termination or connection wrapping (e.g., to prepend buffered bytes)
// is handled at the proxy layer before calling these methods.
type Integrations interface {
	// MatchType checks if the initial bytes match this protocol
	MatchType(ctx context.Context, reqBuf []byte) bool

	// RecordOutgoing records the outgoing request/response to mocks.
	// src is the client connection (app -> proxy)
	// dst is the destination connection (proxy -> real server), may be nil if proxy handles it
	RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error

	// MockOutgoing replays recorded responses to the client.
	// src is the client connection (app -> proxy)
	// dstCfg contains destination configuration (for dial-on-demand in some parsers)
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
	MarkMockAsUsed(mock models.Mock) bool
}
