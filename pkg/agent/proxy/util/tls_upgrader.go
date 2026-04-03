package util

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"go.uber.org/zap"
)

// ClientTLSHandlerFunc is the signature of the function that performs
// TLS termination on the client side (e.g. pTls.HandleTLSConnection).
// It is injected as a dependency to avoid an import cycle between
// proxy/util and proxy/tls.
type ClientTLSHandlerFunc func(ctx context.Context, logger *zap.Logger, conn net.Conn, backdate time.Time) (net.Conn, bool, error)

// ConnTLSUpgrader is the concrete implementation of models.TLSUpgrader.
// It performs TLS upgrades on the real underlying connections and
// updates the proxy's references so that deferred-close still works
// on the upgraded connections.
type ConnTLSUpgrader struct {
	// srcConn and dstConn are pointers to the proxy's connection
	// variables. When an upgrade happens, the pointer target is
	// updated so the proxy's deferred close operates on the new
	// (upgraded) connection.
	srcConn *net.Conn
	dstConn *net.Conn
	logger  *zap.Logger

	// handleClientTLS is the function that terminates TLS on the
	// client side (typically pTls.HandleTLSConnection).
	handleClientTLS ClientTLSHandlerFunc
}

// NewConnTLSUpgrader creates a TLS upgrader. handleClientTLS is
// typically pTls.HandleTLSConnection.
func NewConnTLSUpgrader(srcConn, dstConn *net.Conn, logger *zap.Logger, handleClientTLS ClientTLSHandlerFunc) *ConnTLSUpgrader {
	return &ConnTLSUpgrader{
		srcConn:         srcConn,
		dstConn:         dstConn,
		logger:          logger,
		handleClientTLS: handleClientTLS,
	}
}

// UpgradeClientTLS terminates TLS on the client side. It unwraps the
// SafeConn, performs the TLS handshake, updates the proxy's reference,
// and returns a new SafeConn wrapping the TLS connection.
func (u *ConnTLSUpgrader) UpgradeClientTLS(ctx context.Context, backdate time.Time) (net.Conn, bool, error) {
	realConn := unwrapSafe(*u.srcConn)

	tlsConn, isMTLS, err := u.handleClientTLS(ctx, u.logger, realConn, backdate)
	if err != nil {
		return nil, false, err
	}

	// Update proxy's reference so deferred close works on upgraded conn.
	*u.srcConn = tlsConn

	// Return a new SafeConn wrapping the TLS connection.
	safe := NewSafeConn(tlsConn, u.logger)
	return safe, isMTLS, nil
}

// UpgradeDestTLS upgrades the destination connection to TLS. It
// unwraps the SafeConn, performs the TLS handshake, updates the
// proxy's reference, and returns a new SafeConn.
func (u *ConnTLSUpgrader) UpgradeDestTLS(cfg *tls.Config) (net.Conn, error) {
	realConn := unwrapSafe(*u.dstConn)

	tlsConn := tls.Client(realConn, cfg)
	if err := tlsConn.Handshake(); err != nil {
		return nil, err
	}

	// Update proxy's reference.
	*u.dstConn = tlsConn

	safe := NewSafeConn(tlsConn, u.logger)
	return safe, nil
}

// UnwrapClientForPeek returns the real underlying client connection
// for peeking (TLS detection). The caller must NOT store, close, or
// write to the returned connection.
func (u *ConnTLSUpgrader) UnwrapClientForPeek() net.Conn {
	return unwrapSafe(*u.srcConn)
}

// unwrapSafe extracts the real net.Conn from a SafeConn wrapper. If
// the connection is not a SafeConn, it is returned as-is.
func unwrapSafe(conn net.Conn) net.Conn {
	if sc, ok := conn.(*SafeConn); ok {
		return sc.Unwrap()
	}
	return conn
}
