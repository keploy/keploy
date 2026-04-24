package fakeconn

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ErrFakeConnNoWrite is returned by FakeConn.Write. Parsers are
// consumers of bytes, not producers; a parser calling Write is a
// bug and this error makes it loud rather than silent.
var ErrFakeConnNoWrite = errors.New("fakeconn: Write is not permitted; parsers must not write to real peers")

// ErrClosed is returned by Read/ReadChunk after Close.
var ErrClosed = errors.New("fakeconn: closed")

// deadlineError implements net.Error with Timeout()=true and
// Temporary()=true so callers that inspect via net.Error can treat
// Read deadline hits like real socket deadline hits.
type deadlineError struct{}

func (deadlineError) Error() string   { return "fakeconn: read deadline exceeded" }
func (deadlineError) Timeout() bool   { return true }
func (deadlineError) Temporary() bool { return true }

// ErrDeadlineExceeded is the sentinel returned when a read deadline passes.
var ErrDeadlineExceeded net.Error = deadlineError{}

// FakeConn is a read-only net.Conn driven by a Chunk channel owned
// by the proxy relay. Reads drain an internal buffer first, then
// fetch the next Chunk from the channel. Writes always fail with
// [ErrFakeConnNoWrite]. Close marks the FakeConn closed but does
// not touch any real socket — the relay owns those.
//
// FakeConn is safe for a single reader goroutine concurrent with
// calls to Close and SetReadDeadline. Concurrent Read callers are
// not supported; parsers are single-consumer by construction.
//
// Satisfies [net.Conn] so that parsers coded against net.Conn can
// consume it unchanged. Note that [FakeConn.Write] always returns
// an error — callers that do not check Write's error will silently
// drop their output and this is intentional: we want that bug to
// surface loudly during testing, not silently in production.
type FakeConn struct {
	ch     <-chan Chunk
	logger logger

	mu              sync.Mutex
	buf             bytes.Buffer
	bufReadAt       time.Time // source ReadAt of bytes currently in buf
	bufWrittenAt    time.Time // source WrittenAt of bytes currently in buf
	bufDir          Direction // source direction of bytes currently in buf
	lastReadNano    atomic.Int64
	lastWrittenNano atomic.Int64
	closed          atomic.Bool
	closeCh         chan struct{}

	deadlineMu        sync.Mutex
	deadline          time.Time
	deadlineCh        chan struct{}
	deadlineT         *time.Timer
	deadlineChangedCh chan struct{} // closed each time SetReadDeadline updates state

	local  net.Addr
	remote net.Addr
}

// logger is the minimal surface FakeConn needs from the outside
// world. Zero-value-safe — nil is treated as no-op.
type logger interface {
	// Debug is called on parser-side protocol violations (e.g.
	// Write attempts). The returned error is the primary signal;
	// the log just records the misuse site. Debug-level is
	// appropriate because callers that don't check Write's error
	// already have a loud bug, and this codebase reserves Warn for
	// operator-actionable conditions.
	Debug(msg string, kv ...any)
}

type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}

// New constructs a FakeConn. ch is the relay-owned Chunk channel;
// localAddr and remoteAddr are returned from LocalAddr/RemoteAddr
// (nil values are replaced with a "fakeconn" placeholder).
func New(ch <-chan Chunk, localAddr, remoteAddr net.Addr) *FakeConn {
	return newWithLogger(ch, localAddr, remoteAddr, nopLogger{})
}

// NewWithLogger is New with a caller-supplied logger for diagnostic
// messages (e.g. rejected Write attempts). Pass nil for no logging.
func NewWithLogger(ch <-chan Chunk, localAddr, remoteAddr net.Addr, log logger) *FakeConn {
	if log == nil {
		log = nopLogger{}
	}
	return newWithLogger(ch, localAddr, remoteAddr, log)
}

func newWithLogger(ch <-chan Chunk, localAddr, remoteAddr net.Addr, log logger) *FakeConn {
	if localAddr == nil {
		localAddr = placeholderAddr{label: "fakeconn-local"}
	}
	if remoteAddr == nil {
		remoteAddr = placeholderAddr{label: "fakeconn-remote"}
	}
	return &FakeConn{
		ch:      ch,
		logger:  log,
		closeCh: make(chan struct{}),
		local:   localAddr,
		remote:  remoteAddr,
	}
}

// Read implements io.Reader / net.Conn. It first drains any bytes
// left over from a previous Chunk, then blocks for the next Chunk
// (subject to read deadline and Close).
func (f *FakeConn) Read(p []byte) (int, error) {
	if f.closed.Load() {
		return 0, ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}

	f.mu.Lock()
	if f.buf.Len() > 0 {
		n, err := f.buf.Read(p)
		f.mu.Unlock()
		return n, err
	}
	f.mu.Unlock()

	chunk, err := f.readChunkLocked()
	if err != nil {
		return 0, err
	}

	// Copy as much as fits; stash the remainder in the buffer for
	// the next Read. The source chunk's timestamps are captured so
	// a later ReadChunk call that drains the stash returns them
	// intact (documented contract on ReadChunk).
	n := copy(p, chunk.Bytes)
	if n < len(chunk.Bytes) {
		f.mu.Lock()
		f.buf.Write(chunk.Bytes[n:])
		f.bufReadAt = chunk.ReadAt
		f.bufWrittenAt = chunk.WrittenAt
		f.bufDir = chunk.Dir
		f.mu.Unlock()
	}
	return n, nil
}

// ReadChunk returns the next Chunk from the underlying channel with
// timestamps intact. Bytes are returned without being copied into a
// caller buffer; parsers that want chunk-level timestamps (e.g.
// HTTP/2 frame parsers that care about per-frame arrival time) use
// this instead of Read.
//
// ReadChunk returns [io.EOF] when the channel is closed and no
// further chunks are available. It returns [ErrClosed] if Close
// has been called. It returns a net.Error with Timeout()=true if
// a read deadline was set and has passed.
//
// ReadChunk drains any residual bytes left in the internal buffer
// by a previous Read into a synthetic Chunk first. Those synthetic
// chunks carry the ReadAt/WrittenAt timestamps of the Chunk they
// were drained from; callers should typically use Read XOR ReadChunk
// on a single FakeConn to avoid mixing the two.
func (f *FakeConn) ReadChunk() (Chunk, error) {
	if f.closed.Load() {
		return Chunk{}, ErrClosed
	}
	return f.readChunkLocked()
}

func (f *FakeConn) readChunkLocked() (Chunk, error) {
	// First, drain any residual bytes stashed by a prior Read into a
	// synthetic Chunk carrying the source chunk's timestamps. This
	// preserves the documented ReadChunk contract when the caller
	// mixes Read and ReadChunk on the same FakeConn.
	f.mu.Lock()
	if f.buf.Len() > 0 {
		out := make([]byte, f.buf.Len())
		_, _ = f.buf.Read(out)
		c := Chunk{
			Dir:       f.bufDir,
			Bytes:     out,
			ReadAt:    f.bufReadAt,
			WrittenAt: f.bufWrittenAt,
		}
		// Clear stash metadata so a subsequent stash overwrites cleanly.
		f.bufReadAt = time.Time{}
		f.bufWrittenAt = time.Time{}
		f.bufDir = 0
		f.mu.Unlock()
		if !c.ReadAt.IsZero() {
			f.lastReadNano.Store(c.ReadAt.UnixNano())
		}
		if !c.WrittenAt.IsZero() {
			f.lastWrittenNano.Store(c.WrittenAt.UnixNano())
		}
		return c, nil
	}
	f.mu.Unlock()

	// Re-fetch the deadline channel on each iteration so concurrent
	// SetReadDeadline calls take effect on this in-flight read. The
	// changed-notification channel (closed by SetReadDeadline) wakes
	// the select; we then loop and reload both channels.
	for {
		dlCh, changedCh := f.currentDeadlineChans()
		select {
		case c, ok := <-f.ch:
			if !ok {
				return Chunk{}, io.EOF
			}
			f.lastReadNano.Store(c.ReadAt.UnixNano())
			if !c.WrittenAt.IsZero() {
				f.lastWrittenNano.Store(c.WrittenAt.UnixNano())
			}
			return c, nil
		case <-f.closeCh:
			return Chunk{}, ErrClosed
		case <-dlCh:
			return Chunk{}, ErrDeadlineExceeded
		case <-changedCh:
			// deadline changed; loop and re-fetch.
		}
	}
}

// Write always returns (0, [ErrFakeConnNoWrite]). It exists solely
// to satisfy io.Writer / net.Conn interface shapes that parsers
// consume. Parsers must not call it. The returned error is the
// primary "this should never happen" signal; a Debug-level log
// accompanies it so operators grepping for parser misuse can find
// the site, but Warn-level would be overkill because the error
// return is already loud during testing.
func (f *FakeConn) Write(p []byte) (int, error) {
	f.logger.Debug("fakeconn: Write attempted by parser", "bytes", len(p))
	return 0, ErrFakeConnNoWrite
}

// LastReadTime returns the ReadAt timestamp of the most recently
// delivered Chunk, or the zero time if no Chunk has been delivered.
func (f *FakeConn) LastReadTime() time.Time {
	n := f.lastReadNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// LastWrittenTime returns the WrittenAt timestamp of the most recently
// delivered Chunk, or the zero time if no Chunk has been delivered or
// no chunk carried a non-zero WrittenAt. Parsers that need response-
// side semantics (time the relay handed the byte off to the real peer)
// should prefer this over LastReadTime for consistency with other V2
// recorders that anchor ResTimestampMock to chunk.WrittenAt.
func (f *FakeConn) LastWrittenTime() time.Time {
	n := f.lastWrittenNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Close marks the FakeConn closed. All in-flight and future Reads
// return [ErrClosed]. Close does NOT close the underlying channel
// (the relay owns that) and does NOT affect any real socket.
// Idempotent; calling twice returns nil on the second call.
func (f *FakeConn) Close() error {
	if f.closed.Swap(true) {
		return nil
	}
	close(f.closeCh)
	f.deadlineMu.Lock()
	if f.deadlineT != nil {
		f.deadlineT.Stop()
		f.deadlineT = nil
	}
	f.deadlineMu.Unlock()
	return nil
}

// LocalAddr returns the address configured at construction, or a
// placeholder with network "fakeconn" if none was supplied.
func (f *FakeConn) LocalAddr() net.Addr { return f.local }

// RemoteAddr returns the address configured at construction, or a
// placeholder with network "fakeconn" if none was supplied.
func (f *FakeConn) RemoteAddr() net.Addr { return f.remote }

// SetDeadline sets both the read and write deadlines. The write
// deadline is ignored (Write always errors); the read deadline
// controls when Read/ReadChunk unblock with [ErrDeadlineExceeded].
// A zero t clears the deadline.
func (f *FakeConn) SetDeadline(t time.Time) error {
	return f.SetReadDeadline(t)
}

// SetReadDeadline sets the deadline for future Read/ReadChunk calls.
// A zero t clears the deadline. Safe to call from a different goroutine
// than the reader — a blocked Read/ReadChunk picks up the new deadline
// on the next loop iteration via the deadlineChangedCh broadcast below.
func (f *FakeConn) SetReadDeadline(t time.Time) error {
	f.deadlineMu.Lock()
	defer f.deadlineMu.Unlock()

	if f.deadlineT != nil {
		f.deadlineT.Stop()
		f.deadlineT = nil
	}
	f.deadline = t

	// Notify any in-flight Read/ReadChunk that the deadline changed
	// so it can re-fetch deadlineCh on the next loop iteration. The
	// previous channel is closed (not nil'd) to atomically unblock
	// every waiter; a fresh channel replaces it for subsequent waiters.
	if f.deadlineChangedCh != nil {
		close(f.deadlineChangedCh)
	}
	f.deadlineChangedCh = make(chan struct{})

	if t.IsZero() {
		f.deadlineCh = nil
		return nil
	}

	ch := make(chan struct{})
	f.deadlineCh = ch
	d := time.Until(t)
	if d <= 0 {
		close(ch)
		return nil
	}
	f.deadlineT = time.AfterFunc(d, func() { close(ch) })
	return nil
}

// SetWriteDeadline is a no-op; Write always errors.
func (f *FakeConn) SetWriteDeadline(_ time.Time) error { return nil }

// currentDeadlineChans returns both the active deadline channel
// (nil if no deadline) and the change-notification channel that
// fires the next time SetReadDeadline updates state. Callers should
// re-invoke after the changed channel closes so the new deadline
// takes effect on already-blocked Read/ReadChunk calls.
func (f *FakeConn) currentDeadlineChans() (deadline, changed <-chan struct{}) {
	f.deadlineMu.Lock()
	defer f.deadlineMu.Unlock()
	if f.deadlineChangedCh == nil {
		f.deadlineChangedCh = make(chan struct{})
	}
	return f.deadlineCh, f.deadlineChangedCh
}

// placeholderAddr is returned from LocalAddr/RemoteAddr when no
// real address is configured. Parsers that log the address get
// something readable rather than a nil panic.
type placeholderAddr struct{ label string }

func (p placeholderAddr) Network() string { return "fakeconn" }
func (p placeholderAddr) String() string  { return p.label }

// Compile-time check that FakeConn implements net.Conn.
var _ net.Conn = (*FakeConn)(nil)
