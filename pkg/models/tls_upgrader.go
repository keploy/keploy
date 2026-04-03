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
	// UpgradeClientTLS terminates TLS on the client side of the
	// connection. It returns a new SafeConn wrapping the TLS
	// connection, and a boolean indicating whether mutual TLS (mTLS)
	// was negotiated.
	UpgradeClientTLS(ctx context.Context, backdate time.Time) (net.Conn, bool, error)

	// UpgradeDestTLS upgrades the destination (server) side of the
	// connection to TLS using the provided config. It returns a new
	// SafeConn wrapping the TLS connection.
	UpgradeDestTLS(cfg *tls.Config) (net.Conn, error)

	// UnwrapClientForPeek returns the underlying client connection
	// temporarily so the parser can peek bytes to detect whether the
	// client is actually sending a TLS ClientHello. Both MySQL and
	// PostgreSQL peek 5 bytes for this purpose.
	//
	// The returned conn must NOT be stored, closed, or written to.
	// It is valid only for the duration of the peek operation.
	UnwrapClientForPeek() net.Conn
}
