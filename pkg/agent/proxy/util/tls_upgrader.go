package util

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
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

// isTLSClientHello checks if the first 5 bytes look like a TLS ClientHello.
// Inlined here to avoid an import cycle with proxy/tls.
func isTLSClientHello(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 &&
		(data[2] == 0x00 || data[2] == 0x01 || data[2] == 0x02 || data[2] == 0x03)
}

// UpgradeClientTLS peeks the client connection to detect TLS, and if
// detected, performs the TLS termination. Returns (conn, isTLS, isMTLS, error).
// If the client is not sending TLS, returns the original conn with isTLS=false.
func (u *ConnTLSUpgrader) UpgradeClientTLS(ctx context.Context, backdate time.Time) (net.Conn, bool, bool, error) {
	realConn := unwrapSafe(*u.srcConn)

	// Peek 5 bytes to detect TLS ClientHello.
	reader := bufio.NewReader(realConn)
	testBuffer, err := reader.Peek(5)
	if err != nil {
		if err == io.EOF && len(testBuffer) == 0 {
			u.logger.Debug("UpgradeClientTLS: received EOF during peek, no TLS")
			// Return the original conn wrapped with the reader to replay any buffered bytes.
			safe := NewSafeConnWithReader(*u.srcConn, io.MultiReader(reader, realConn), u.logger)
			return safe, false, false, nil
		}
		return nil, false, false, err
	}

	if !isTLSClientHello(testBuffer) {
		// Not TLS — wrap the connection with a MultiReader so the peeked
		// bytes are replayed on subsequent reads.
		safe := NewSafeConnWithReader(*u.srcConn, io.MultiReader(reader, realConn), u.logger)
		return safe, false, false, nil
	}

	// TLS detected. The bufio.Reader may have buffered more than 5 bytes.
	// Create a MultiReader that replays the buffered data before reading
	// from the raw conn, then perform the TLS handshake on that combined reader.
	replayConn := &Conn{
		Conn:   realConn,
		Reader: io.MultiReader(reader, realConn),
		Logger: u.logger,
	}

	tlsConn, isMTLS, err := u.handleClientTLS(ctx, u.logger, replayConn, backdate)
	if err != nil {
		return nil, false, false, err
	}

	// Update proxy's reference so deferred close works on upgraded conn.
	*u.srcConn = tlsConn

	// Return a new SafeConn wrapping the TLS connection.
	safe := NewSafeConn(tlsConn, u.logger)
	return safe, true, isMTLS, nil
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

// unwrapSafe extracts the real net.Conn from a SafeConn wrapper. If
// the connection is not a SafeConn, it is returned as-is.
func unwrapSafe(conn net.Conn) net.Conn {
	if sc, ok := conn.(*SafeConn); ok {
		return sc.Unwrap()
	}
	return conn
}
