// Package integrations provides functionality for integrating different types of services.
package integrations

import (
	"context"
	"io"
	"net"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type Initializer func(logger *zap.Logger) Integrations

type IntegrationType string

// constants for different types of integrations
const (
	HTTP        IntegrationType = "http"
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

// StreamConn wraps a connection with a custom reader.
// This allows the proxy to prepend already-read bytes (like initial buffer)
// back onto the stream, so parsers can read from the beginning.
// After proxy-level TLS handling, the underlying Conn is the TLS connection
// but from the parser's perspective, it's just a plain byte stream.
type StreamConn struct {
	// Conn is the underlying connection (may be TLS-wrapped by proxy)
	Conn net.Conn
	// Reader is a custom reader that may include buffered/peeked data
	Reader io.Reader
}

// Read implements io.Reader, reading from the custom Reader
func (s *StreamConn) Read(p []byte) (n int, err error) {
	return s.Reader.Read(p)
}

// Write implements io.Writer, writing to the underlying Conn
func (s *StreamConn) Write(p []byte) (n int, err error) {
	return s.Conn.Write(p)
}

// Close closes the underlying connection
func (s *StreamConn) Close() error {
	return s.Conn.Close()
}

// RemoteAddr returns the remote address of the underlying connection
func (s *StreamConn) RemoteAddr() net.Addr {
	return s.Conn.RemoteAddr()
}

// LocalAddr returns the local address of the underlying connection
func (s *StreamConn) LocalAddr() net.Addr {
	return s.Conn.LocalAddr()
}

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
	UpdateFilteredMock(old *models.Mock, new *models.Mock) bool
	DeleteFilteredMock(mock models.Mock) bool
	DeleteUnFilteredMock(mock models.Mock) bool
	GetMySQLCounts() (total, config, data int)
}
