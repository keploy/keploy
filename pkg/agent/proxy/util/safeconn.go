// Package util provides utility functions for the proxy package.
package util

import (
	"io"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
)

// SafeConn wraps a net.Conn so that integration parsers cannot Close(),
// set deadlines, or replace the underlying connection during record mode.
// Read and Write pass through to the real connection. The proxy layer
// retains the original connection and remains responsible for lifecycle
// management.
//
// SafeConn is the parser-facing wrapper used in this repo for record
// mode: Close and all deadline setters (SetDeadline, SetReadDeadline,
// SetWriteDeadline) are no-ops, and parsers must treat all of them as
// unavailable for lifecycle management. Some enterprise builds may
// provide a separate SimulatedConn type for other proxy modes that
// follows the same contract; that type is not defined in this repo,
// but where it exists it is expected to honour the same "Close and
// deadline setters are unavailable" rule so parsers behave identically
// regardless of which proxy mode is active.
//
// SafeConn satisfies net.Conn so it can be used wherever parsers expect
// a connection — including gRPC's singleConnListener and http2.Server.
type SafeConn struct {
	conn   net.Conn
	reader io.Reader
	logger *zap.Logger
	mu     sync.Mutex
}

// NewSafeConn wraps conn so that Close and the deadline setters
// (SetDeadline, SetReadDeadline, SetWriteDeadline) are no-ops.
func NewSafeConn(conn net.Conn, logger *zap.Logger) *SafeConn {
	return &SafeConn{
		conn:   conn,
		reader: conn,
		logger: logger,
	}
}

// NewSafeConnWithReader wraps conn with an overridden reader (e.g. an
// io.MultiReader that prepends buffered initial data).
func NewSafeConnWithReader(conn net.Conn, reader io.Reader, logger *zap.Logger) *SafeConn {
	return &SafeConn{
		conn:   conn,
		reader: reader,
		logger: logger,
	}
}

// Read delegates to the underlying reader. Thread-safe.
func (s *SafeConn) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reader.Read(p)
}

// Write delegates to the real connection.
func (s *SafeConn) Write(p []byte) (int, error) {
	return s.conn.Write(p)
}

// Close is a no-op. The proxy owns connection lifecycle.
func (s *SafeConn) Close() error {
	s.logger.Debug("SafeConn.Close called (no-op — proxy owns lifecycle)")
	return nil
}

// LocalAddr delegates to the real connection.
func (s *SafeConn) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

// RemoteAddr delegates to the real connection.
func (s *SafeConn) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}

// SetDeadline is a no-op in record mode.
func (s *SafeConn) SetDeadline(_ time.Time) error { return nil }

// SetReadDeadline is a no-op in record mode.
func (s *SafeConn) SetReadDeadline(_ time.Time) error { return nil }

// SetWriteDeadline is a no-op in record mode.
func (s *SafeConn) SetWriteDeadline(_ time.Time) error { return nil }

// Unwrap returns the real underlying net.Conn. This method is
// intentionally NOT part of the net.Conn interface. Only the proxy
// layer (which creates SafeConn) should call this — parsers must not.
func (s *SafeConn) Unwrap() net.Conn {
	return s.conn
}

// Compile-time check that SafeConn implements net.Conn.
var _ net.Conn = (*SafeConn)(nil)
