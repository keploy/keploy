// Package orchestrator provides async I/O handling for parsers.
package orchestrator

import (
	"bufio"
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// TCP_QUICKACK is the Linux socket option to disable delayed ACKs.
// This constant is not in the standard syscall package for all platforms.
const TCP_QUICKACK = 12

// ringBuf is a lock-free single-producer / single-consumer (SPSC) ring buffer
// optimised for the TeeForwardConn use-case.
//
// The write path (forwarder goroutine / producer) is completely lock-free:
// it uses atomic cursor updates and never blocks.  The read path (parser
// goroutine / consumer) is lock-free when data is available; it falls back
// to sync.Cond only when the buffer is empty and the reader must sleep.
//
// Cursors are ever-increasing int64 values.  The actual index into the
// backing slice is obtained via cursor & mask (power-of-two size).
// This avoids the ambiguity between "empty" and "full" that plagues
// traditional modular cursors.
//
// All data lives in a single pre-allocated byte slice → zero per-chunk
// allocations after construction.
type ringBuf struct {
	buf  []byte
	size int64
	mask int64 // size - 1, for fast modular indexing

	// ── cache-line-padded cursors ─────────────────────────────────
	// Separate cache lines for producer and consumer cursors to
	// avoid false sharing.  On x86-64, a cache line is 64 bytes.
	_pad0 [64]byte
	w     atomic.Int64 // write cursor (only written by producer)
	_pad1 [64]byte
	r     atomic.Int64 // read cursor  (only written by consumer)
	_pad2 [64]byte

	// closed is set by the producer when it is done.
	closed   atomic.Bool
	overflow atomic.Bool // set if the writer couldn't fit data

	// waiting is set by the consumer before it enters cond.Wait().
	// The producer checks this and only grabs mu + signals when set,
	// keeping the fast write path entirely lock-free.
	waiting atomic.Bool

	// mu + cond are used ONLY for the blocking-wait case (buffer empty).
	mu   sync.Mutex
	cond *sync.Cond
}

// nextPow2 returns the smallest power of 2 >= v.
func nextPow2(v int) int64 {
	p := int64(1)
	for p < int64(v) {
		p <<= 1
	}
	return p
}

func newRingBuf(size int) *ringBuf {
	sz := nextPow2(size)
	rb := &ringBuf{
		buf:  make([]byte, sz),
		size: sz,
		mask: sz - 1,
	}
	rb.cond = sync.NewCond(&rb.mu)
	return rb
}

// Write appends p to the ring buffer.  Returns n written.
// If the buffer cannot hold len(p), it writes as much as possible and
// sets the overflow flag.  The write path is completely lock-free.
func (rb *ringBuf) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	w := rb.w.Load()
	r := rb.r.Load()
	free := rb.size - (w - r)

	n := int64(len(p))
	if n > free {
		n = free
		rb.overflow.Store(true)
	}
	if n == 0 {
		return 0, nil
	}

	// Copy data into the ring, handling wrap-around.
	start := w & rb.mask
	end := start + n
	if end <= rb.size {
		copy(rb.buf[start:end], p[:n])
	} else {
		first := rb.size - start
		copy(rb.buf[start:], p[:first])
		copy(rb.buf[:end-rb.size], p[first:n])
	}

	// Publish the new write cursor.  This Store provides a release
	// barrier: the data written above is visible to any reader that
	// observes this new cursor value.
	rb.w.Store(w + n)

	// Wake the reader only if it is blocked.  The check-then-signal
	// pattern is safe: if the reader sets waiting=true AFTER we read
	// false here, the reader will re-check the cursors inside the
	// mu-protected loop and see data without sleeping.
	if rb.waiting.Load() {
		rb.mu.Lock()
		rb.cond.Signal()
		rb.mu.Unlock()
	}

	return int(n), nil
}

// Read copies available data into p.  The fast path (data available) is
// completely lock-free.  It blocks via sync.Cond only when the buffer is
// empty and not yet closed.  Returns io.EOF when closed and drained.
func (rb *ringBuf) Read(p []byte) (int, error) {
	for {
		r := rb.r.Load()
		w := rb.w.Load()
		avail := w - r

		if avail > 0 {
			// ── fast path: data available, no lock needed ──
			n := int64(len(p))
			if n > avail {
				n = avail
			}

			start := r & rb.mask
			end := start + n
			if end <= rb.size {
				copy(p[:n], rb.buf[start:end])
			} else {
				first := rb.size - start
				copy(p[:first], rb.buf[start:])
				copy(p[first:n], rb.buf[:end-rb.size])
			}

			// Publish the new read cursor.
			rb.r.Store(r + n)
			return int(n), nil
		}

		// Buffer empty — check if producer is done.
		if rb.closed.Load() {
			return 0, io.EOF
		}

		// ── slow path: block until data arrives or buffer closes ──
		rb.mu.Lock()
		rb.waiting.Store(true)
		for rb.w.Load()-rb.r.Load() == 0 && !rb.closed.Load() {
			rb.cond.Wait()
		}
		rb.waiting.Store(false)
		rb.mu.Unlock()
		// Loop back to the fast path to actually read the data.
	}
}

// Close marks the buffer as closed (no more writes).  A subsequent Read
// will drain remaining data and then return io.EOF.
func (rb *ringBuf) Close() {
	rb.closed.Store(true)
	rb.mu.Lock()
	rb.cond.Signal()
	rb.mu.Unlock()
}

// TeeForwardConn reads from a source connection and immediately forwards the data
// to a destination connection, while also buffering it for the parser to read at
// its own pace. This decouples forwarding speed from parsing speed — the
// client/server receives data as fast as the network allows, while the parser
// processes it asynchronously.
//
// Data is buffered in a pre-allocated ring buffer (default 2 MB) — zero
// per-read allocations after construction.  The forwarding goroutine reuses
// a single 64 KB read buffer from a sync.Pool.
//
// The parser reads through a bufio.Reader that wraps the ring buffer,
// batching many small ring.Read calls into fewer, larger ones and
// dramatically reducing the number of atomic operations per MySQL packet.
//
// Read()  → returns data from the ring buffer (already forwarded)
// Write() → no-op; returns success without writing (forwarding goroutine handles it)
type TeeForwardConn struct {
	src      net.Conn  // Source connection to read from
	dest     net.Conn  // Destination to forward reads to
	reader   io.Reader // Reader for the forwarding goroutine (defaults to src)
	ring     *ringBuf
	bufRing  *bufio.Reader // buffered wrapper around ring for parser reads
	disabled int32         // atomic: 1 = recording disabled (buffer overflow)
	err      error         // terminal error from forwarder
	errMu    sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	logger   *zap.Logger

	// Cached file descriptors for TCP_QUICKACK — avoids calling SyscallConn()
	// on every forwarded packet (saves ~2-5μs per call which adds up at scale).
	srcFD  int // -1 if not TCP
	destFD int // -1 if not TCP
}

// Pool of 64 KB read buffers for the forwarding goroutine.
// Larger buffers mean fewer Read() syscalls per forwarded response,
// which reduces context-switch overhead for bursty result sets.
var teeReadPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 64*1024)
		return &b
	},
}

const (
	defaultRingSize   = 2 * 1024 * 1024 // 2 MB ring buffer per connection
	bufRingReaderSize = 64 * 1024       // parser-side buffered reader size
)

// NewTeeForwardConn creates a connection that reads from src, immediately
// forwards to dest, and buffers the data for the caller to read. The forwarding
// goroutine starts immediately.
func NewTeeForwardConn(ctx context.Context, logger *zap.Logger, src, dest net.Conn) *TeeForwardConn {
	setTCPNoDelay(dest)
	setTCPQuickACK(src)
	setTCPQuickACK(dest)
	ctx, cancel := context.WithCancel(ctx)
	ring := newRingBuf(defaultRingSize)
	t := &TeeForwardConn{
		src:     src,
		dest:    dest,
		reader:  src,
		ring:    ring,
		bufRing: bufio.NewReaderSize(ring, bufRingReaderSize),
		ctx:     ctx,
		cancel:  cancel,
		logger:  logger,
		srcFD:   extractTCPfd(src),
		destFD:  extractTCPfd(dest),
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
	ring := newRingBuf(defaultRingSize)
	t := &TeeForwardConn{
		src:     src,
		dest:    dest,
		reader:  reader,
		ring:    ring,
		bufRing: bufio.NewReaderSize(ring, bufRingReaderSize),
		ctx:     ctx,
		cancel:  cancel,
		logger:  logger,
		srcFD:   extractTCPfd(src),
		destFD:  extractTCPfd(dest),
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

				// Re-enable TCP_QUICKACK right before Read so our kernel
				// immediately ACKs the incoming data (Linux resets the flag
				// after every ACK).  One syscall per iteration, not two —
				// the reverse-direction TeeForwardConn sets quickACK on
				// the dest side via its own srcFD.
				quickACKByFD(t.srcFD)

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

// SetTCPNoDelay enables TCP_NODELAY on the connection if it is a TCP connection.
// This eliminates Nagle's algorithm delay (~40ms) on small writes, which is
// critical for forwarding performance.
func SetTCPNoDelay(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
}

// setTCPNoDelay is the package-internal alias (used by constructors).
func setTCPNoDelay(conn net.Conn) { SetTCPNoDelay(conn) }

// SetTCPQuickACK disables delayed ACKs to reduce read-side latency.
// Delayed ACKs can add up to 40ms on Linux when the proxy reads from
// one connection and writes to another (the piggyback ACK path never fires).
// NOTE: Linux resets TCP_QUICKACK after every ACK, so this must be called
// repeatedly — ideally after every Write() that precedes a Read().
func SetTCPQuickACK(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	rawConn, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = rawConn.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, TCP_QUICKACK, 1)
	})
}

// setTCPQuickACK is the package-internal alias (used by constructors).
func setTCPQuickACK(conn net.Conn) { SetTCPQuickACK(conn) }

// extractTCPfd extracts and caches the file descriptor from a TCP connection.
// Returns -1 if the connection is not TCP. This avoids calling SyscallConn()
// on every forwarded packet in the hot path.
func extractTCPfd(conn net.Conn) int {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return -1
	}
	rawConn, err := tc.SyscallConn()
	if err != nil {
		return -1
	}
	fd := -1
	_ = rawConn.Control(func(f uintptr) {
		fd = int(f)
	})
	return fd
}

// quickACKByFD re-enables TCP_QUICKACK using a cached file descriptor.
// This is the hot-path version — avoids SyscallConn() + Control() overhead
// (~2-5μs saved per call, significant at thousands of packets/sec).
func quickACKByFD(fd int) {
	if fd < 0 {
		return
	}
	_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, TCP_QUICKACK, 1)
}

// Read returns buffered data that was already forwarded to dest.
// Reads are served through a bufio.Reader that wraps the ring buffer,
// batching many small reads into fewer ring.Read calls.  This cuts
// the number of atomic cursor operations per MySQL packet from ~4-6
// down to ~1, significantly reducing overhead for multi-packet responses.
func (t *TeeForwardConn) Read(p []byte) (int, error) {
	n, err := t.bufRing.Read(p)
	if err == io.EOF {
		// Ring drained — return the actual terminal error from the forwarder.
		if ferr := t.getErr(); ferr != nil {
			return 0, ferr
		}
		return 0, io.EOF
	}
	return n, err
}

// Write is a no-op on TeeForwardConn — the forwarding goroutine already
// copies all data from src to dest at wire speed.  Returning success lets
// the same protocol-handling code work transparently with both raw connections
// (during the serial SSL-detection phase) and TeeForwardConns (during the
// auth + command phases) without any code changes.
func (t *TeeForwardConn) Write(p []byte) (int, error) {
	return len(p), nil
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

// SetReadDeadline is a no-op on TeeForwardConn.
// The parser reads from the ring buffer, not the underlying connection.
// Setting a deadline on src would cause the forwarding goroutine's Read()
// to timeout, terminating forwarding — which is never the caller's intent.
// Callers that need timeout behavior should use context cancellation instead.
func (t *TeeForwardConn) SetReadDeadline(_ time.Time) error {
	return nil
}

// SetWriteDeadline is a no-op since this is read-only.
func (t *TeeForwardConn) SetWriteDeadline(_ time.Time) error {
	return nil
}

// Ensure TeeForwardConn implements net.Conn.
var _ net.Conn = (*TeeForwardConn)(nil)
