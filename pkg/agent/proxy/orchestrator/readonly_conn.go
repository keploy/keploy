// Package orchestrator provides async I/O handling for parsers.
package orchestrator

import (
	"errors"
	"io"
	"net"
	"time"
)

// ErrWriteNotAllowed is returned when attempting to write to a read-only connection
var ErrWriteNotAllowed = errors.New("write not allowed on read-only connection")

// ReadOnlyConn wraps a net.Conn and prevents writes.
// Use this to provide parsers with read-only access to connections.
type ReadOnlyConn struct {
	inner  net.Conn
	reader io.Reader // Optional buffered reader
}

// NewReadOnlyConn creates a ReadOnlyConn from an existing connection
func NewReadOnlyConn(conn net.Conn) *ReadOnlyConn {
	return &ReadOnlyConn{inner: conn, reader: conn}
}

// NewReadOnlyConnWithReader creates a ReadOnlyConn with a custom reader
// Useful when you need to prepend buffered data
func NewReadOnlyConnWithReader(conn net.Conn, reader io.Reader) *ReadOnlyConn {
	return &ReadOnlyConn{inner: conn, reader: reader}
}

// Read reads data from the connection
func (c *ReadOnlyConn) Read(b []byte) (n int, err error) {
	return c.reader.Read(b)
}

// Write always returns ErrWriteNotAllowed
func (c *ReadOnlyConn) Write(_ []byte) (n int, err error) {
	return 0, ErrWriteNotAllowed
}

// Close closes the underlying connection
func (c *ReadOnlyConn) Close() error {
	return c.inner.Close()
}

// LocalAddr returns the local network address
func (c *ReadOnlyConn) LocalAddr() net.Addr {
	return c.inner.LocalAddr()
}

// RemoteAddr returns the remote network address
func (c *ReadOnlyConn) RemoteAddr() net.Addr {
	return c.inner.RemoteAddr()
}

// SetDeadline sets the read and write deadlines
func (c *ReadOnlyConn) SetDeadline(t time.Time) error {
	return c.inner.SetDeadline(t)
}

// SetReadDeadline sets the read deadline
func (c *ReadOnlyConn) SetReadDeadline(t time.Time) error {
	return c.inner.SetReadDeadline(t)
}

// SetWriteDeadline sets the write deadline (no-op for read-only)
func (c *ReadOnlyConn) SetWriteDeadline(t time.Time) error {
	return c.inner.SetWriteDeadline(t)
}

// Unwrap returns the underlying connection for controlled access
// Only the orchestrator should call this
func (c *ReadOnlyConn) Unwrap() net.Conn {
	return c.inner
}
