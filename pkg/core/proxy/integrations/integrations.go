// Package integrations provides functionality for integrating different types of services.
package integrations

import (
	"context"
	"crypto/tls"
	"net"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type Initializer func(logger *zap.Logger) Integrations

type integrationType string

// constants for different types of integrations
const (
	HTTP        integrationType = "http"
	GRPC        integrationType = "grpc"
	GENERIC     integrationType = "generic"
	MYSQL       integrationType = "mysql"
	POSTGRES_V1 integrationType = "postgres_v1"
	POSTGRES_V2 integrationType = "postgres_v2"
	MONGO       integrationType = "mongo"
)

var Registered = make(map[string]Initializer)

type ConditionalDstCfg struct {
	Addr   string // Destination Addr (ip:port)
	Port   uint
	TLSCfg *tls.Config
}

type Integrations interface {
	MatchType(ctx context.Context, reqBuf []byte) bool
	RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error
	MockOutgoing(ctx context.Context, src net.Conn, dstCfg *ConditionalDstCfg, mockDb MockMemDb, opts models.OutgoingOptions) error
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
	// Flag the mock as used which matches the external request from application in test mode
	FlagMockAsUsed(mock *models.Mock) error 
}
