// Package orchestrator provides async I/O handling for parsers.
package orchestrator

import (
	"bufio"
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Ensure ringSignal is used (defined in ring_signal_linux.go / ring_signal_others.go).
var _ = (*ringSignal)(nil)

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

	// sig provides platform-optimised goroutine wakeup.
	// On Linux this is backed by eventfd (sub-5μs wakeup).
	// On other platforms it falls back to a channel + 50μs timer.
	sig *ringSignal
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
		sig:  newRingSignal(),
	}
	return rb
}

// Write appends p to the ring buffer.  Returns n written.
// If the buffer cannot hold len(p), it writes as much as possible and
// sets the overflow flag.  The write path is completely lock-free and
// syscall-free in the common case (reader keeping up).
func (rb *ringBuf) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if rb.closed.Load() {
		return 0, io.ErrClosedPipe
	}

	w := rb.w.Load()
	r := rb.r.Load()
	free := rb.size - (w - r)

	n := int64(len(p))
	if n > free {
		// All-or-nothing: don't write partial data that could split a
		// MySQL packet in the middle, causing garbled framing for the
		// pipeline reader.  Return 0 so the caller knows nothing was
		// written and can close the ring for a clean EOF.
		rb.overflow.Store(true)
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

	// Publish the new write cursor (release barrier).
	rb.w.Store(w + n)

	// ── Conditional notify ──
	// Only issue the wakeup syscall when the reader is actually parked.
	// This keeps the forwarding goroutine's hot path syscall-free when
	// the parser is keeping up (which is the common case).  Cost when
	// reader is active: ~1ns (atomic load).  Cost when reader is parked:
	// ~200ns (eventfd write on Linux, channel send on others).
	if rb.sig.hasWaiter() {
		rb.sig.notify()
	}

	return int(n), nil
}

// Read copies available data into p.  The fast path (data available) is
// completely lock-free.  The slow path uses conditional signalling:
// the reader marks itself as waiting, re-checks the buffer, then parks.
// The producer only issues a wakeup syscall when it sees the waiting flag.
// Returns io.EOF when closed and drained.
func (rb *ringBuf) Read(p []byte) (int, error) {
	for {
		// ── Fast path: data available (no syscall, no lock) ──
		r := rb.r.Load()
		w := rb.w.Load()
		avail := w - r

		if avail > 0 {
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

			rb.r.Store(r + n)
			return int(n), nil
		}

		// Buffer empty — check if closed
		if rb.closed.Load() {
			return 0, io.EOF
		}

		// ── Slow path: mark waiting, re-check, then park ──
		// The setWaiting → re-check → wait pattern prevents missed wakeups:
		// if the producer writes between our first check and setWaiting,
		// the re-check catches it without blocking.
		rb.sig.setWaiting()

		// Re-check: data may have arrived between the first Load and setWaiting.
		if rb.w.Load() != w || rb.closed.Load() {
			rb.sig.clearWaiting()
			continue
		}

		// Park the goroutine.  On Linux: eventfd read via netpoller (~1-5μs wakeup).
		// On others: channel receive (immediate wakeup on notify).
		ok := rb.sig.wait()
		rb.sig.clearWaiting()
		if !ok {
			// Signal closed → loop back to drain remaining data.
			continue
		}
	}
}

// Close marks the buffer as closed (no more writes).  A subsequent Read
// will drain remaining data and then return io.EOF.
func (rb *ringBuf) Close() {
	rb.closed.Store(true)
	rb.sig.close()
}

// Stop stops the forwarding goroutine and closes the ring buffer.
// It returns any error that occurred during forwarding.
// This is useful when switching the underlying connection (e.g. SSL upgrade)
// and we need to discard the TeeForwardConn but keep the socket open.
// Note: the i/o timeout caused by SetReadDeadline(time.Now()) to unblock the
// reader is expected and is NOT returned as an error.
func (t *TeeForwardConn) Stop() error {
	t.cancel()
	// Force the reader to unblock if it is stuck in Read()
	if conn, ok := t.src.(net.Conn); ok {
		_ = conn.SetReadDeadline(time.Now())
	}
	t.ring.Close()
	t.wg.Wait()
	err := t.getErr()
	// The intentional deadline we set above causes an i/o timeout — filter it out.
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return nil
		}
	}
	return err
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
	wg       sync.WaitGroup
	logger   *zap.Logger

	// Cached file descriptors for TCP_QUICKACK — avoids calling SyscallConn()
	// on every forwarded packet (saves ~2-5μs per call which adds up at scale).
	srcFD  int // -1 if not TCP
	destFD int // -1 if not TCP
}

// Pool of 256 KB read buffers for the forwarding goroutine.
// On loopback, MySQL sends entire responses atomically into the socket
// receive buffer (set to 2 MB by tuneTCPConn).  A 256 KB read buffer
// lets us transfer a 256 KB response in ONE Read+Write iteration instead
// of four with a 64 KB buffer — reducing syscall count by 4× and
// eliminating 3 goroutine preemption points per large response.
var teeReadPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 256*1024)
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

func (t *TeeForwardConn) startForwarding() {
	t.once.Do(func() {
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			defer t.ring.Close()

			// Borrow one read buffer for the lifetime of this goroutine.
			bufPtr := teeReadPool.Get().(*[]byte)
			defer teeReadPool.Put(bufPtr)
			readBuf := *bufPtr

			// Check ctx.Done() every 64 iterations instead of every loop
			// to avoid the ~20-50ns channel-lock overhead per Read.  The
			// Read/Write syscalls themselves return errors promptly when
			// the underlying connection is closed, so cancellation is
			// still detected within a few milliseconds.
			var iter int
			for {
				iter++
				if iter&63 == 0 {
					select {
					case <-t.ctx.Done():
						t.setErr(t.ctx.Err())
						return
					default:
					}
				}

				n, err := t.reader.Read(readBuf)
				if n > 0 {
					// Forward to dest immediately — use the pool buffer directly,
					// avoiding any copy before the write.
					if _, werr := t.dest.Write(readBuf[:n]); werr != nil {
						t.setErr(werr)
						return
					}

					// Re-enable TCP_QUICKACK after every Write.
					// Linux resets TCP_QUICKACK after each ACK, so without
					// re-enabling it the kernel falls back to delayed ACKs
					// (~40ms timer). On the source side, a delayed ACK can
					// stall the next Read if the peer is waiting for the ACK
					// before sending more data. Uses cached FDs to avoid
					// SyscallConn() overhead (~2-5μs per call).
					//
					// Only srcFD needs quickACK — it controls our ACKs to the
					// sender. destFD quickACK is unnecessary since we're writing
					// to dest, not reading from it.
					quickACKByFD(t.srcFD)

					// Buffer for parser — write into ring buffer (zero alloc).
					// All-or-nothing: if the ring can't hold the full chunk,
					// drop it entirely and close the ring so the pipeline gets
					// a clean EOF instead of garbled mid-packet data.
					if atomic.LoadInt32(&t.disabled) == 0 {
						written, _ := t.ring.Write(readBuf[:n])
						if written < n {
							// Ring buffer full — disable recording and close
							// the ring to unblock the pipeline immediately.
							atomic.StoreInt32(&t.disabled, 1)
							t.ring.Close()
							if t.logger != nil {
								t.logger.Debug("TeeForwardConn ring buffer full, disabling recording for this connection",
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

// netConner is satisfied by *tls.Conn (and any other wrapper that exposes
// the underlying net.Conn via NetConn()). Using an interface avoids importing
// crypto/tls in this package.
type netConner interface {
	NetConn() net.Conn
}

// unwrapTCPConn traverses connection wrappers (TLS, custom middleware, etc.)
// until it finds the underlying *net.TCPConn, or returns nil.
func unwrapTCPConn(conn net.Conn) *net.TCPConn {
	for {
		switch c := conn.(type) {
		case *net.TCPConn:
			return c
		case netConner:
			conn = c.NetConn()
		default:
			return nil
		}
	}
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

// Peek returns the next n bytes without advancing the reader.
func (t *TeeForwardConn) Peek(n int) ([]byte, error) {
	return t.bufRing.Peek(n)
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
