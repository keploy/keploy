package models

import (
	"context"
	"crypto/tls"
	"net"
	"time"
)

// TLSUpgrader provides controlled mid-stream TLS upgrade capability
// for parsers that need it (PostgreSQL SSLRequest, MySQL CLIENT_SSL).
//
// The proxy layer creates the concrete implementation and passes it
// via RecordSession.TLSUpgrader. Parsers call UpgradeClientTLS /
// UpgradeDestTLS instead of directly manipulating the underlying
// connection. This ensures:
//   - The proxy's deferred-close references are updated to the TLS conn
//   - Parsers never touch the raw conn directly
//   - TLS upgrade logic is centralized in the proxy layer
type TLSUpgrader interface {
	// UpgradeClientTLS peeks the client connection to detect TLS,
	// and if detected, performs the TLS termination. Returns
	// (conn, isTLS, isMTLS, error). If the client is not sending
	// TLS, returns the original conn with isTLS=false.
	UpgradeClientTLS(ctx context.Context, backdate time.Time) (conn net.Conn, isTLS bool, isMTLS bool, err error)

	// UpgradeDestTLS upgrades the destination (server) side of the
	// connection to TLS using the provided config. It returns a new
	// SafeConn wrapping the TLS connection.
	UpgradeDestTLS(cfg *tls.Config) (net.Conn, error)
}
