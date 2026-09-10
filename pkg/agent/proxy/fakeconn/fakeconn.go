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
// calls to Close, SetReadDeadline and DiscardBefore. Concurrent Read
// callers are not supported; parsers are single-consumer by
// construction. DiscardBefore carries one extra rule that the mutex
// cannot enforce — it must not be called while the parser is inside a
// read; see its doc.
//
// Satisfies [net.Conn] so that parsers coded against net.Conn can
// consume it unchanged. Note that [FakeConn.Write] always returns
// an error — callers that do not check Write's error will silently
// drop their output and this is intentional: we want that bug to
// surface loudly during testing, not silently in production.
type FakeConn struct {
	ch     <-chan Chunk
	logger logger

	mu           sync.Mutex
	buf          bytes.Buffer
	bufReadAt    time.Time // source ReadAt of bytes currently in buf
	bufWrittenAt time.Time // source WrittenAt of bytes currently in buf
	bufDir       Direction // source direction of bytes currently in buf
	// pos is the absolute offset, in bytes of this stream, of the next
	// byte the parser will be handed. Equivalently: how many bytes have
	// already left this FakeConn, counting both bytes delivered to the
	// parser and bytes swallowed on its behalf by [FakeConn.DiscardBefore].
	//
	// buf therefore holds the bytes at offsets [pos, pos+buf.Len()), and
	// pos+buf.Len() is the number of bytes pulled off ch so far — the
	// quantity the producer counts on its side.
	pos int64
	// discardTo is the discard watermark set by [FakeConn.DiscardBefore]:
	// every byte below it is swallowed rather than delivered. Monotonic.
	discardTo       int64
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
	// A zero-length Read consumes nothing and must not block, so it is
	// answered before the discard below can park on the channel. Kept
	// above the closed check too, matching io.Reader's "Read(p) with
	// len(p)==0 returns 0, nil" convention for a stream that has not
	// otherwise failed.
	if len(p) == 0 {
		return 0, nil
	}
	// Honour any discard watermark BEFORE looking at the buffer: the
	// bytes it wants gone may be sitting in buf, in ch, or still
	// upstream in the relay's tee, and all three are reached by
	// pulling forward from here. See [FakeConn.DiscardBefore].
	if err := f.applyDiscard(); err != nil {
		return 0, err
	}
	if f.closed.Load() {
		// Close means "no more blocking", NOT "discard bytes already
		// delivered to me". The relay tee drains its buffered chunks
		// into f.ch and then Close() fires during teardown; a chunk
		// that already landed in f.ch (or the stash) is a fully
		// recorded wire event and must still be readable, otherwise a
		// response whose body arrived a hair before the connection was
		// torn down is silently lost (the "server closed before
		// response" mock-incomplete race on Connection: close traffic
		// such as the boot-time startup mock). Only return ErrClosed
		// once nothing buffered remains.
		if c, ok := f.drainBufferedLocked(); ok {
			n := copy(p, c.Bytes)
			f.mu.Lock()
			f.pos += int64(n)
			if n < len(c.Bytes) {
				f.buf.Write(c.Bytes[n:])
				f.bufReadAt = c.ReadAt
				f.bufWrittenAt = c.WrittenAt
				f.bufDir = c.Dir
			}
			f.mu.Unlock()
			return n, nil
		}
		return 0, ErrClosed
	}

	f.mu.Lock()
	if f.buf.Len() > 0 {
		n, err := f.buf.Read(p)
		f.pos += int64(n)
		f.mu.Unlock()
		// bytes.Buffer.Read returns io.EOF whenever it drains the
		// buffer to empty, even when we got bytes back on this call
		// and more chunks may still arrive on f.ch. Surfacing that
		// EOF to our caller (bufio.Reader / io.Copy / etc.) would
		// make them treat the stream as finished prematurely. Only
		// the channel-close path (below, via readChunkLocked) is a
		// real EOF. Mask the stash-exhaustion EOF whenever n > 0.
		if n > 0 && errors.Is(err, io.EOF) {
			err = nil
		}
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
	f.mu.Lock()
	f.pos += int64(n)
	if n < len(chunk.Bytes) {
		f.buf.Write(chunk.Bytes[n:])
		f.bufReadAt = chunk.ReadAt
		f.bufWrittenAt = chunk.WrittenAt
		f.bufDir = chunk.Dir
	}
	f.mu.Unlock()
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
	// See Read: the watermark is honoured before anything is handed
	// back, and it reaches bytes wherever they currently sit.
	if err := f.applyDiscard(); err != nil {
		return Chunk{}, err
	}
	if f.closed.Load() {
		// See Read: a chunk already delivered to f.ch (or stashed) is a
		// recorded wire event and must survive Close. Drain what is
		// buffered before reporting ErrClosed.
		if c, ok := f.drainBufferedLocked(); ok {
			f.mu.Lock()
			f.pos += int64(len(c.Bytes))
			f.mu.Unlock()
			return c, nil
		}
		return Chunk{}, ErrClosed
	}
	c, err := f.readChunkLocked()
	if err != nil {
		return c, err
	}
	f.mu.Lock()
	f.pos += int64(len(c.Bytes))
	f.mu.Unlock()
	return c, nil
}

// drainBufferedLocked returns the next chunk that is ALREADY available
// without blocking: first any residual bytes left in the internal stash
// by a prior Read, then a single non-blocking receive from f.ch. It is
// used only on the post-Close read paths so that bytes the relay already
// handed to this FakeConn are not discarded when Close races the final
// delivery. Returns ok=false when nothing is immediately available (the
// caller then returns ErrClosed). It updates the last-read/written
// timestamps exactly like readChunkLocked so downstream timestamp anchors
// stay consistent.
func (f *FakeConn) drainBufferedLocked() (Chunk, bool) {
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
		return c, true
	}
	f.mu.Unlock()

	select {
	case c, ok := <-f.ch:
		if !ok {
			return Chunk{}, false
		}
		f.lastReadNano.Store(c.ReadAt.UnixNano())
		if !c.WrittenAt.IsZero() {
			f.lastWrittenNano.Store(c.WrittenAt.UnixNano())
		}
		return c, true
	default:
		return Chunk{}, false
	}
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

	return f.recvChunk()
}

// recvChunk blocks for the next Chunk on f.ch, ignoring anything
// residual in buf. Split out of readChunkLocked because
// [FakeConn.applyDiscard] must pull the pipeline forward WITHOUT
// draining buf into a synthetic chunk — it consumes buf itself, and
// only as far as the watermark, so a discard can stop mid-chunk.
func (f *FakeConn) recvChunk() (Chunk, error) {
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

// DiscardBefore declares that the parser must never be handed a stream
// byte whose absolute offset is below `offset`, and that its next read
// resumes at `offset`. Returns how many bytes are still to be swallowed
// at the moment of the call (0 when the watermark is already behind the
// reader). The watermark is monotonic: a lower offset is ignored.
//
// # Why the consumer side, and why an absolute offset
//
// The relay sometimes CONSUMES bytes it has already teed here. The
// canonical case is MySQL's CLIENT_SSL upgrade: the client sends a
// 36-byte SSLRequest and, with no further server turn, immediately
// starts a TLS handshake on the same socket. The relay's forwarder is
// parked in Read, so it has both messages in hand long before the
// parser has decoded the SSLRequest and asked for the upgrade — and
// both get teed. The parser reads its 36 bytes; the ClientHello behind
// them is then consumed by keploy's own client-side handshake, but a
// copy of it is still sitting in this FakeConn, where the parser's next
// read finds `16 03 01 ...` in place of the post-TLS message it
// expects, mis-frames it as a packet header and hangs.
//
// Retracting those bytes on the PRODUCER side does not work: the tee is
// a pipeline (staging queue -> drain goroutine -> channel -> this
// buffer), so bytes to be dropped can simultaneously be in the queue,
// in the drain goroutine's hand, in the channel, and in buf. Draining
// that from outside means racing the drain goroutine. Here there is no
// race at all: this FakeConn is single-consumer by construction, and
// every one of those places is reached by pulling FORWARD from the
// reader, which is what applyDiscard does.
//
// The offset is absolute rather than a count of "whatever is pending
// now" for the same reason: the producer states a stream position, so
// bytes that have not been teed yet when the watermark is set are still
// covered, and bytes teed AFTER it (the post-upgrade plaintext) are
// above the watermark and pass through untouched. No quiescing of the
// pipeline is required, and arming the watermark before or after the
// forwarders resume gives the same result.
//
// Caller contract: call it while the parser is not reading. The relay
// does this under its directive pause, where the parser is blocked on
// the directive ack by construction. A watermark armed against a reader
// already blocked inside a channel receive cannot retract the chunk
// that receive is about to return.
//
// The guarantee is exact while the FakeConn is open. After Close it
// degrades to best effort: nothing more is coming, so applyDiscard stops
// pulling rather than blocking, and a chunk that lands on the channel in
// the window between that decision and the post-Close drain can still be
// handed back. That window only exists on a connection whose parser is
// already being retired.
func (f *FakeConn) DiscardBefore(offset int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if offset > f.discardTo {
		f.discardTo = offset
	}
	if n := f.discardTo - f.pos; n > 0 {
		return n
	}
	return 0
}

// applyDiscard swallows bytes until the reader has reached the discard
// watermark. It consumes buf first and then pulls chunks, so a
// watermark that falls in the middle of a chunk splits it exactly.
//
// Blocking is correct while the FakeConn is open: the bytes below the
// watermark were teed before it was set, so they are already on their
// way and the parser's next read must not overtake them. Once closed,
// nothing more is coming, so the pull is best-effort and non-blocking —
// otherwise a discard armed just before teardown would turn every
// subsequent read into an ErrClosed instead of letting the post-Close
// drain paths hand back what did arrive.
func (f *FakeConn) applyDiscard() error {
	for {
		f.mu.Lock()
		need := f.discardTo - f.pos
		if need <= 0 {
			f.mu.Unlock()
			return nil
		}
		if have := int64(f.buf.Len()); have > 0 {
			n := have
			if n > need {
				n = need
			}
			f.buf.Next(int(n))
			f.pos += n
			if f.buf.Len() == 0 {
				f.bufReadAt = time.Time{}
				f.bufWrittenAt = time.Time{}
				f.bufDir = 0
			}
			f.mu.Unlock()
			continue
		}
		f.mu.Unlock()

		var c Chunk
		if f.closed.Load() {
			var ok bool
			select {
			case c, ok = <-f.ch:
				if !ok {
					return nil
				}
			default:
				return nil
			}
		} else {
			var err error
			c, err = f.recvChunk()
			if err != nil {
				return err
			}
		}
		f.mu.Lock()
		f.buf.Write(c.Bytes)
		f.bufReadAt = c.ReadAt
		f.bufWrittenAt = c.WrittenAt
		f.bufDir = c.Dir
		f.mu.Unlock()
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

// Done returns a channel closed when this FakeConn is closed — i.e. when
// the parser has stopped reading it. The relay's tee selects on this while
// delivering: a consumer that is merely slow must never cost a recorded
// chunk, so delivery blocks, and this is the only signal that says the
// consumer is genuinely gone rather than behind.
func (f *FakeConn) Done() <-chan struct{} { return f.closeCh }

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
