// Package orchestrator provides async I/O handling for parsers.
package orchestrator

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// TeeForwardConn reads from a source connection and immediately forwards the data
// to a destination connection, while also buffering it for the parser to read at
// its own pace. This decouples forwarding speed from parsing speed — the
// client/server receives data as fast as the network allows, while the parser
// processes it asynchronously.
//
// Key benefit: For large result sets (many rows), the forwarder goroutine
// continuously reads-and-forwards without waiting for the parser to decode each
// packet. The client gets the full response faster, and the parser catches up.
//
// Read()  → returns data from the internal buffer (already forwarded)
// Write() → returns ErrWriteNotAllowed (read-only from parser's perspective)
type TeeForwardConn struct {
	src    net.Conn // Source connection to read from
	dest   net.Conn // Destination to forward reads to
	dataCh chan []byte
	mu     sync.Mutex
	buf    []byte // leftover from previous Read
	err    error  // terminal error from forwarder
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

// NewTeeForwardConn creates a connection that reads from src, immediately
// forwards to dest, and buffers the data for the caller to read. The forwarding
// goroutine starts immediately.
func NewTeeForwardConn(ctx context.Context, src, dest net.Conn) *TeeForwardConn {
	ctx, cancel := context.WithCancel(ctx)
	t := &TeeForwardConn{
		src:    src,
		dest:   dest,
		dataCh: make(chan []byte, 128), // 128 chunks ≈ up to ~4MB buffered at 32KB each
		ctx:    ctx,
		cancel: cancel,
	}
	t.startForwarding()
	return t
}

func (t *TeeForwardConn) startForwarding() {
	t.once.Do(func() {
		go func() {
			defer close(t.dataCh)
			readBuf := make([]byte, 32*1024) // 32KB read buffer
			for {
				select {
				case <-t.ctx.Done():
					t.setErr(t.ctx.Err())
					return
				default:
				}

				n, err := t.src.Read(readBuf)
				if n > 0 {
					data := make([]byte, n)
					copy(data, readBuf[:n])

					// Forward to dest immediately
					if _, werr := t.dest.Write(data); werr != nil {
						t.setErr(werr)
						return
					}

					// Buffer for parser
					select {
					case t.dataCh <- data:
					case <-t.ctx.Done():
						t.setErr(t.ctx.Err())
						return
					}
				}
				if err != nil {
					t.setErr(err)
					return
				}
			}
		}()
	})
}

func (t *TeeForwardConn) setErr(err error) {
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.mu.Unlock()
}

func (t *TeeForwardConn) getErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Read returns buffered data that was already forwarded to dest.
// It blocks until data is available or the context is cancelled.
func (t *TeeForwardConn) Read(p []byte) (int, error) {
	// Return leftover data from previous reads first
	if len(t.buf) > 0 {
		n := copy(p, t.buf)
		t.buf = t.buf[n:]
		return n, nil
	}

	// Try non-blocking read first (prioritize data over ctx cancellation)
	select {
	case data, ok := <-t.dataCh:
		if !ok {
			// Channel closed — forwarder stopped. Return the terminal error.
			err := t.getErr()
			if err == nil {
				err = io.EOF
			}
			return 0, err
		}
		n := copy(p, data)
		if n < len(data) {
			t.buf = data[n:]
		}
		return n, nil
	default:
	}

	// Block until data arrives or context cancels
	select {
	case data, ok := <-t.dataCh:
		if !ok {
			err := t.getErr()
			if err == nil {
				err = io.EOF
			}
			return 0, err
		}
		n := copy(p, data)
		if n < len(data) {
			t.buf = data[n:]
		}
		return n, nil
	case <-t.ctx.Done():
		return 0, t.ctx.Err()
	}
}

// Write returns ErrWriteNotAllowed — parsers cannot write directly.
func (t *TeeForwardConn) Write(_ []byte) (int, error) {
	return 0, ErrWriteNotAllowed
}

// Close cancels the forwarding goroutine.
func (t *TeeForwardConn) Close() error {
	t.cancel()
	return t.src.Close()
}

// LocalAddr returns the source's local address.
func (t *TeeForwardConn) LocalAddr() net.Addr {
	return t.src.LocalAddr()
}

// RemoteAddr returns the source's remote address.
func (t *TeeForwardConn) RemoteAddr() net.Addr {
	return t.src.RemoteAddr()
}

// SetDeadline delegates to the source connection.
func (t *TeeForwardConn) SetDeadline(d time.Time) error {
	return t.src.SetDeadline(d)
}

// SetReadDeadline delegates to the source connection.
func (t *TeeForwardConn) SetReadDeadline(d time.Time) error {
	return t.src.SetReadDeadline(d)
}

// SetWriteDeadline is a no-op since this is read-only.
func (t *TeeForwardConn) SetWriteDeadline(_ time.Time) error {
	return nil
}

// Ensure TeeForwardConn implements net.Conn.
var _ net.Conn = (*TeeForwardConn)(nil)

// ErrReadOnlyConnection is returned when attempting to write to a read-only connection.
var ErrReadOnlyConnection = errors.New("write not allowed on read-only connection")
