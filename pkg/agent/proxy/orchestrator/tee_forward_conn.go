// Package orchestrator provides async I/O handling for parsers.
package orchestrator

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// ringBuf is a lock-free(ish) single-producer / single-consumer ring buffer
// optimised for the TeeForwardConn use-case.  The writer (forwarder goroutine)
// never blocks — if the buffer is full it sets the overflow flag.  The reader
// (parser goroutine) blocks via sync.Cond when the buffer is empty.
//
// All data lives in a single pre-allocated byte slice → zero per-chunk
// allocations after construction.
type ringBuf struct {
	buf  []byte
	size int
	r    int // read cursor
	w    int // write cursor
	full bool
	mu   sync.Mutex
	cond *sync.Cond

	// closed is set by the writer when it is done.
	closed   bool
	overflow bool // set if the writer couldn't fit data
}

func newRingBuf(size int) *ringBuf {
	rb := &ringBuf{
		buf:  make([]byte, size),
		size: size,
	}
	rb.cond = sync.NewCond(&rb.mu)
	return rb
}

// available returns the number of readable bytes (caller must hold mu).
func (rb *ringBuf) available() int {
	if rb.full {
		return rb.size
	}
	if rb.w >= rb.r {
		return rb.w - rb.r
	}
	return rb.size - rb.r + rb.w
}

// free returns the number of bytes that can be written (caller must hold mu).
func (rb *ringBuf) free() int {
	return rb.size - rb.available()
}

// Write appends p to the ring buffer.  Returns n written.
// If the buffer cannot hold len(p), it writes as much as possible
// and sets the overflow flag.  The writer never blocks.
func (rb *ringBuf) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	rb.mu.Lock()
	f := rb.free()
	n := len(p)
	if n > f {
		n = f
		rb.overflow = true
	}
	if n > 0 {
		// Write may wrap around the end of the buffer.
		end := rb.w + n
		if end <= rb.size {
			copy(rb.buf[rb.w:end], p[:n])
		} else {
			first := rb.size - rb.w
			copy(rb.buf[rb.w:], p[:first])
			copy(rb.buf[:end-rb.size], p[first:n])
		}
		rb.w = (rb.w + n) % rb.size
		if rb.w == rb.r {
			rb.full = true
		}
	}
	rb.mu.Unlock()
	rb.cond.Signal() // wake reader
	return n, nil
}

// Read copies available data into p.  Blocks if the buffer is empty and
// not yet closed.  Returns io.EOF when closed and drained.
func (rb *ringBuf) Read(p []byte) (int, error) {
	rb.mu.Lock()
	for rb.available() == 0 && !rb.closed {
		rb.cond.Wait()
	}
	avail := rb.available()
	if avail == 0 {
		rb.mu.Unlock()
		return 0, io.EOF
	}
	n := len(p)
	if n > avail {
		n = avail
	}
	end := rb.r + n
	if end <= rb.size {
		copy(p[:n], rb.buf[rb.r:end])
	} else {
		first := rb.size - rb.r
		copy(p[:first], rb.buf[rb.r:])
		copy(p[first:n], rb.buf[:end-rb.size])
	}
	rb.r = (rb.r + n) % rb.size
	rb.full = false
	rb.mu.Unlock()
	return n, nil
}

// Close marks the buffer as closed (no more writes).  A subsequent Read
// will drain remaining data and then return io.EOF.
func (rb *ringBuf) Close() {
	rb.mu.Lock()
	rb.closed = true
	rb.mu.Unlock()
	rb.cond.Signal()
}

// TeeForwardConn reads from a source connection and immediately forwards the data
// to a destination connection, while also buffering it for the parser to read at
// its own pace. This decouples forwarding speed from parsing speed — the
// client/server receives data as fast as the network allows, while the parser
// processes it asynchronously.
//
// Data is buffered in a pre-allocated ring buffer (default 4 MB) — zero
// per-read allocations after construction.  The forwarding goroutine reuses
// a single 32 KB read buffer from a sync.Pool.
//
// Read()  → returns data from the ring buffer (already forwarded)
// Write() → returns ErrWriteNotAllowed (read-only from parser's perspective)
type TeeForwardConn struct {
	src      net.Conn  // Source connection to read from
	dest     net.Conn  // Destination to forward reads to
	reader   io.Reader // Reader for the forwarding goroutine (defaults to src)
	ring     *ringBuf
	disabled int32 // atomic: 1 = recording disabled (buffer overflow)
	err      error // terminal error from forwarder
	errMu    sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	logger   *zap.Logger
}

// Pool of 32 KB read buffers for the forwarding goroutine.
var teeReadPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 32*1024)
		return &b
	},
}

const defaultRingSize = 1 * 1024 * 1024 // 1 MB ring buffer per connection

// NewTeeForwardConn creates a connection that reads from src, immediately
// forwards to dest, and buffers the data for the caller to read. The forwarding
// goroutine starts immediately.
func NewTeeForwardConn(ctx context.Context, logger *zap.Logger, src, dest net.Conn) *TeeForwardConn {
	setTCPNoDelay(dest)
	setTCPQuickACK(src)
	setTCPQuickACK(dest)
	ctx, cancel := context.WithCancel(ctx)
	t := &TeeForwardConn{
		src:    src,
		dest:   dest,
		reader: src,
		ring:   newRingBuf(defaultRingSize),
		ctx:    ctx,
		cancel: cancel,
		logger: logger,
	}
	t.startForwarding()
	return t
}

// NewTeeForwardConnWithReader creates a TeeForwardConn with a custom reader
// for the forwarding goroutine. This is useful when initial data has been
// pre-read from src and needs to be prepended (e.g., using io.MultiReader).
// The forwarding goroutine reads from the provided reader instead of src directly.
func NewTeeForwardConnWithReader(ctx context.Context, logger *zap.Logger, src, dest net.Conn, reader io.Reader) *TeeForwardConn {
	setTCPNoDelay(dest)
	setTCPQuickACK(src)
	setTCPQuickACK(dest)
	ctx, cancel := context.WithCancel(ctx)
	t := &TeeForwardConn{
		src:    src,
		dest:   dest,
		reader: reader,
		ring:   newRingBuf(defaultRingSize),
		ctx:    ctx,
		cancel: cancel,
		logger: logger,
	}
	t.startForwarding()
	return t
}

func (t *TeeForwardConn) startForwarding() {
	t.once.Do(func() {
		go func() {
			defer t.ring.Close()

			// Borrow one read buffer for the lifetime of this goroutine.
			bufPtr := teeReadPool.Get().(*[]byte)
			defer teeReadPool.Put(bufPtr)
			readBuf := *bufPtr

			for {
				select {
				case <-t.ctx.Done():
					t.setErr(t.ctx.Err())
					return
				default:
				}

				n, err := t.reader.Read(readBuf)
				if n > 0 {
					// Forward to dest immediately — use the pool buffer directly,
					// avoiding any copy before the write.
					if _, werr := t.dest.Write(readBuf[:n]); werr != nil {
						t.setErr(werr)
						return
					}

					// Buffer for parser — write into ring buffer (zero alloc).
					if atomic.LoadInt32(&t.disabled) == 0 {
						written, _ := t.ring.Write(readBuf[:n])
						if written < n {
							// Ring buffer full — disable recording to prevent
							// blocking network forwarding.
							atomic.StoreInt32(&t.disabled, 1)
							if t.logger != nil {
								t.logger.Warn("TeeForwardConn ring buffer full, disabling recording for this connection",
									zap.String("src", t.src.RemoteAddr().String()))
							}
						}
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
	t.errMu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.errMu.Unlock()
}

func (t *TeeForwardConn) getErr() error {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	return t.err
}

// setTCPNoDelay enables TCP_NODELAY on the connection if it is a TCP connection.
// This eliminates Nagle's algorithm delay (~40ms) on small writes, which is
// critical for forwarding performance.
func setTCPNoDelay(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
}

// setTCPQuickACK disables delayed ACKs to reduce read-side latency.
// Delayed ACKs can add up to 40ms on Linux when the proxy reads from
// one connection and writes to another (the piggyback ACK path never fires).
func setTCPQuickACK(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	rawConn, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = rawConn.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)
	})
}

// Read returns buffered data that was already forwarded to dest.
// It blocks until data is available or the forwarding goroutine closes.
func (t *TeeForwardConn) Read(p []byte) (int, error) {
	n, err := t.ring.Read(p)
	if err == io.EOF {
		// Ring drained — return the actual terminal error from the forwarder.
		if ferr := t.getErr(); ferr != nil {
			return 0, ferr
		}
		return 0, io.EOF
	}
	return n, err
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
