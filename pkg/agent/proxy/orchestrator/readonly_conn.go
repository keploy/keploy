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

// ForwardingReadOnlyConn wraps a connection and automatically forwards all reads
// to a destination connection. From the parser's perspective, it's read-only:
// - Read() works normally AND forwards data to dest
// - Write() returns ErrWriteNotAllowed
//
// This enables parsers to read from connections without having direct write access,
// while the proxy transparently handles forwarding.
type ForwardingReadOnlyConn struct {
	src    net.Conn  // Source connection to read from
	dest   net.Conn  // Destination to forward reads to
	reader io.Reader // Optional buffered reader for prepending initial data
}

// NewForwardingReadOnlyConn creates a connection that reads from src and forwards to dest.
// The parser calls Read(), which reads from src and writes to dest automatically.
func NewForwardingReadOnlyConn(src, dest net.Conn) *ForwardingReadOnlyConn {
	return &ForwardingReadOnlyConn{src: src, dest: dest, reader: src}
}

// NewForwardingReadOnlyConnWithReader creates a forwarding conn with a custom reader.
// Useful for prepending already-read initial buffer data.
func NewForwardingReadOnlyConnWithReader(src, dest net.Conn, reader io.Reader) *ForwardingReadOnlyConn {
	return &ForwardingReadOnlyConn{src: src, dest: dest, reader: reader}
}

// Read reads from source and forwards to destination.
// This is the key method that makes the parser "read-only" while enabling forwarding.
func (c *ForwardingReadOnlyConn) Read(b []byte) (n int, err error) {
	n, err = c.reader.Read(b)
	if n > 0 && c.dest != nil {
		// Forward the read data to destination (fire-and-forget)
		// Errors here are intentionally ignored - the dest read goroutine will detect them
		_, _ = c.dest.Write(b[:n])
	}
	return n, err
}

// Write returns ErrWriteNotAllowed - parsers cannot write directly
func (c *ForwardingReadOnlyConn) Write(_ []byte) (n int, err error) {
	return 0, ErrWriteNotAllowed
}

// Close closes the source connection
func (c *ForwardingReadOnlyConn) Close() error {
	return c.src.Close()
}

// LocalAddr returns the source's local address
func (c *ForwardingReadOnlyConn) LocalAddr() net.Addr {
	return c.src.LocalAddr()
}

// RemoteAddr returns the source's remote address
func (c *ForwardingReadOnlyConn) RemoteAddr() net.Addr {
	return c.src.RemoteAddr()
}

// SetDeadline sets the deadline on the source connection
func (c *ForwardingReadOnlyConn) SetDeadline(t time.Time) error {
	return c.src.SetDeadline(t)
}

// SetReadDeadline sets the read deadline on the source connection
func (c *ForwardingReadOnlyConn) SetReadDeadline(t time.Time) error {
	return c.src.SetReadDeadline(t)
}

// SetWriteDeadline is a no-op since this is read-only
func (c *ForwardingReadOnlyConn) SetWriteDeadline(t time.Time) error {
	return nil
}

// UnwrapSrc returns the underlying source connection
func (c *ForwardingReadOnlyConn) UnwrapSrc() net.Conn {
	return c.src
}

// UnwrapDest returns the underlying destination connection
func (c *ForwardingReadOnlyConn) UnwrapDest() net.Conn {
	return c.dest
}

// SetReader updates the reader (useful after TLS upgrade)
func (c *ForwardingReadOnlyConn) SetReader(r io.Reader) {
	c.reader = r
}

// SetDest updates the destination connection (useful after TLS upgrade)
func (c *ForwardingReadOnlyConn) SetDest(dest net.Conn) {
	c.dest = dest
}
