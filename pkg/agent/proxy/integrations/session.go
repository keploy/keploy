package integrations

import (
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// RecordSession bundles all resources a parser needs during record
// mode. It replaces the previous pattern of passing individual
// net.Conn parameters plus smuggling errgroup, connection IDs, and
// other values through context.
//
// Connections are SafeConn wrappers where Close() and SetDeadline()
// are no-ops. The proxy retains the real connections and manages
// their lifecycle.
type RecordSession struct {
	// Ingress is the client-side connection (SafeConn).
	// Read: client requests. Write: server responses back to client.
	Ingress net.Conn

	// Egress is the destination-side connection (SafeConn).
	// Read: server responses. Write: client requests forwarded to server.
	Egress net.Conn

	// Mocks channel for sending recorded mock objects.
	Mocks chan<- *models.Mock

	// ErrGroup for managing parser goroutines. Previously passed via
	// context.Value(models.ErrGroupKey). Currently also injected into
	// the parser context via context.WithValue in proxy.go so that
	// ReadFromPeer and other utilities can retrieve it. Retained here
	// as part of the planned migration to pass it explicitly through
	// RecordSession rather than via context.
	ErrGroup *errgroup.Group

	// MemLimiter tracks memory usage across all proxy connections.
	// nil means unlimited. All MemoryLimiter methods are nil-safe,
	// so parsers never need to nil-check.
	MemLimiter *util.MemoryLimiter

	// TLSUpgrader provides controlled mid-stream TLS upgrade for
	// parsers that need it (PostgreSQL SSLRequest, MySQL CLIENT_SSL).
	// nil for parsers that don't do mid-stream TLS.
	TLSUpgrader models.TLSUpgrader

	// Logger is pre-configured with connection-scoped fields
	// (client conn ID, dest conn ID, addresses).
	Logger *zap.Logger

	// ClientConnID identifies the client connection. Previously
	// passed via context.Value(models.ClientConnectionIDKey).
	ClientConnID string

	// DestConnID identifies the destination connection. Previously
	// passed via context.Value(models.DestConnectionIDKey).
	DestConnID string

	// Opts contains protocol-specific options (bypass rules,
	// passwords, TLS keys, noise config, etc.).
	Opts models.OutgoingOptions
}
