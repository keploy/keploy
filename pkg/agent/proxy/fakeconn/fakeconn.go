package fakeconn

import (
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

// ErrSkipMidChunk is returned by [FakeConn.SkipThrough] when the chunk the
// reader is partway through was captured after the point to skip through: the
// stream cannot be cut there at a chunk boundary.
var ErrSkipMidChunk = errors.New("fakeconn: the chunk being read was captured after the point to skip through")

// ErrUnplaced is returned by [FakeConn.SkipThrough] for a chunk whose capture
// time (ReadAt) does not place it before or after the point to skip through:
// it has none, or it is the point itself.
var ErrUnplaced = errors.New("fakeconn: a chunk's capture time does not place it before or after the point to skip through")

// deadlineError implements net.Error with Timeout()=true and
// Temporary()=true so callers that inspect via net.Error can treat
// Read deadline hits like real socket deadline hits.
type deadlineError struct{}

func (deadlineError) Error() string   { return "fakeconn: read deadline exceeded" }
func (deadlineError) Timeout() bool   { return true }
func (deadlineError) Temporary() bool { return true }

// ErrDeadlineExceeded is the sentinel returned when a read deadline passes.
var ErrDeadlineExceeded net.Error = deadlineError{}

// ErrUnnumbered is what a reader that orders a connection's two directions
// gets for a chunk its producer did not number (Chunk.ConnSeq 0): a broken
// producer contract, never a state of the traffic. Ordering by a number that
// is not there would pair a request with whatever answer came first, the very
// mispairing the number exists to prevent, so the chunk is refused instead.
var ErrUnnumbered = errors.New("fakeconn: a chunk without a capture sequence (ConnSeq 0): its producer must number every chunk (fakeconn.ConnSeqOf)")

// FakeConn is a read-only net.Conn driven by a Chunk channel owned
// by the proxy relay. Reads take what is left of the chunk the reader is
// in the middle of first, then fetch the next Chunk from the channel.
// Writes always fail with
// [ErrFakeConnNoWrite]. Close marks the FakeConn closed but does
// not touch any real socket — the relay owns those.
//
// FakeConn is safe for a single reader goroutine concurrent with
// calls to Close, SetReadDeadline and DiscardBefore. Concurrent Read
// callers are not supported; parsers are single-consumer by
// construction. DiscardBefore carries one extra rule that the mutex
// cannot enforce — it must not be called while the parser is inside a
// read; see its doc. DropBefore, SkipThrough and SettleStart are the reader's
// own.
//
// Satisfies [net.Conn] so that parsers coded against net.Conn can
// consume it unchanged. Note that [FakeConn.Write] always returns
// an error — callers that do not check Write's error will silently
// drop their output and this is intentional: we want that bug to
// surface loudly during testing, not silently in production.
type FakeConn struct {
	ch     <-chan Chunk
	logger logger

	mu sync.Mutex
	// pending is the reader's next bytes: what is left of the chunk it is in
	// the middle of, or the chunk Peek showed it and it has not taken
	// (hasPending, below, says pending holds one: a chunk may carry no
	// bytes). Its other fields are that chunk's, so the rest of a chunk
	// read in part keeps the chunk's times and numbers. It is the chunk the
	// producer sent, never a copy, and is let go once it is read whole: an
	// empty slice of it would keep its backing array, as large as the
	// largest read the producer made, for as long as the connection sits
	// idle.
	pending Chunk
	// floor is DropBefore's: a chunk captured before the chunk numbered
	// floor is dropped, not handed to the reader. 0 while there is none
	// (ConnSeq 0 is no number).
	floor uint32
	// runFirst, runFrom and runTo are the run of chunks the floor is
	// dropping (with what SkipThrough and SettleStart take of the stream's
	// start, which is the floor's to drop): the ConnSeq of its first chunk
	// (0 while there is no run), and the first and last capture time
	// (ReadAt, UnixNano) among them. reportRun is told of the run once it
	// ends (endRunLocked).
	runFirst       uint32
	runFrom, runTo int64
	reportRun      func(Dropped)
	// pos is the absolute offset, in bytes of this stream, of the next
	// byte the parser will be handed. Equivalently: how many bytes have
	// already left this FakeConn, counting bytes delivered to the parser,
	// bytes swallowed on its behalf by [FakeConn.DiscardBefore], and chunks
	// the floor dropped ([FakeConn.DropBefore]).
	//
	// pending therefore holds the bytes at offsets [pos, pos+len(pending)),
	// and pos+len(pending) is the number of bytes pulled off ch so far — the
	// quantity the producer counts on its side.
	pos int64
	// discardTo is the discard watermark set by [FakeConn.DiscardBefore]:
	// every byte below it is swallowed rather than delivered. Monotonic.
	discardTo int64
	// consumed mirrors pos, for Consumed.
	consumed atomic.Int64
	// waits counts the times the reader found nothing to take and blocked
	// for the next chunk; waiting is set while it is blocked so. See
	// Waiting.
	waits   atomic.Uint64
	waiting atomic.Bool
	// hasPending is pending's (guarded by mu), placed here, in the padding
	// after waiting, so the FakeConn is no larger than it was before it
	// kept the chunk in hand.
	hasPending bool
	// started is set (guarded by mu) once the stream's start is behind it.
	// The stream's start is the chunks captured before the reader's first
	// floor (first): a stream's chunks come off it in the order they were
	// captured, so they come before any other. It is behind once a chunk
	// comes off the stream that is not in it (startLocked), or the run the
	// floor dropped in it ends (endRunLocked). That run is Dropped.AtStart,
	// and no other is. In the same padding.
	started bool
	// partial is set (guarded by mu) while pending is what is left of a chunk
	// some of whose bytes have left the FakeConn (read, or swallowed for the
	// reader): the reader's next byte does not start a chunk
	// (AtChunkBoundary). In the same padding.
	partial         bool
	lastReadNano    atomic.Int64
	lastWrittenNano atomic.Int64
	closed          atomic.Bool
	// first is the first floor DropBefore was given (guarded by mu), 0 until
	// then: the stream's start is what was captured before it (started). In
	// the padding after closed, so a FakeConn stays in the 288 B size class.
	first   uint32
	closeCh chan struct{}

	deadlineMu        sync.Mutex
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

// Read implements io.Reader / net.Conn. It hands out what is left of the
// chunk the reader is in the middle of, or else blocks for the next Chunk
// (subject to read deadline and Close). A read never spans two chunks.
func (f *FakeConn) Read(p []byte) (int, error) {
	// A zero-length Read consumes nothing and must not block, so it is
	// answered before a pending discard can park it on the channel. Kept
	// above the closed check too, matching io.Reader's "Read(p) with
	// len(p)==0 returns 0, nil" convention for a stream that has not
	// otherwise failed.
	if len(p) == 0 {
		return 0, nil
	}
	if err := f.fill(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	n := copy(p, f.pending.Bytes)
	f.deliverLocked(f.pending, n)
	f.skipLocked(n)
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
// has been called and nothing already delivered is left. It returns a
// net.Error with Timeout()=true if a read deadline was set and has passed.
//
// After a Read that took part of a chunk, ReadChunk returns the rest of
// that chunk, with its times and numbers. Callers should typically use
// Read XOR ReadChunk on a single FakeConn to avoid mixing the two.
func (f *FakeConn) ReadChunk() (Chunk, error) {
	if err := f.fill(); err != nil {
		return Chunk{}, err
	}
	f.mu.Lock()
	c := f.pending
	f.deliverLocked(c, len(c.Bytes))
	f.pending, f.hasPending, f.partial = Chunk{}, false, false
	f.mu.Unlock()
	return c, nil
}

// Peek returns what ReadChunk would, without taking it: the next read hands
// out the same bytes. It blocks, and fails, as ReadChunk does. The chunk's
// Bytes are the FakeConn's: the caller must not change them. A peeked chunk is
// not delivered: it moves neither LastReadTime nor Consumed.
func (f *FakeConn) Peek() (Chunk, error) {
	if err := f.fill(); err != nil {
		return Chunk{}, err
	}
	f.mu.Lock()
	c := f.pending
	f.mu.Unlock()
	return c, nil
}

// skipLocked moves past the next n bytes of pending, and lets go of the chunk
// once none of it is left. What is left of it otherwise is a chunk in part
// (partial). Caller holds mu.
func (f *FakeConn) skipLocked(n int) {
	if n >= len(f.pending.Bytes) {
		f.pending, f.hasPending, f.partial = Chunk{}, false, false
		return
	}
	f.pending.Bytes = f.pending.Bytes[n:]
	f.partial = f.partial || n > 0
}

// advanceLocked counts n bytes as having left the FakeConn. Caller holds mu.
func (f *FakeConn) advanceLocked(n int) {
	f.pos += int64(n)
	f.consumed.Store(f.pos)
}

// deliverLocked hands the reader n bytes of c: they leave the FakeConn, and
// c's times are the last delivered (LastReadTime, LastWrittenTime). Caller
// holds mu.
func (f *FakeConn) deliverLocked(c Chunk, n int) {
	f.advanceLocked(n)
	if !c.ReadAt.IsZero() {
		f.lastReadNano.Store(c.ReadAt.UnixNano())
	}
	if !c.WrittenAt.IsZero() {
		f.lastWrittenNano.Store(c.WrittenAt.UnixNano())
	}
}

// next makes pending hold the stream's next bytes, as they were captured. It
// is the one way bytes come off ch: it swallows what the discard watermark
// covers (DiscardBefore), and pulls chunks off ch as it needs them (receive),
// each of which ends the stream's start unless it is in it (startLocked).
// It returns io.EOF at the end of the stream, ErrClosed after Close once
// nothing already delivered is left, and the deadline's error.
func (f *FakeConn) next() error {
	for {
		f.mu.Lock()
		if n := min(f.discardTo-f.pos, int64(len(f.pending.Bytes))); n > 0 {
			f.skipLocked(int(n))
			f.advanceLocked(int(n))
		}
		if f.hasPending && f.pos >= f.discardTo {
			f.mu.Unlock()
			return nil
		}
		f.mu.Unlock()

		c, err := f.receive()
		if err != nil {
			return err
		}
		f.mu.Lock()
		f.pending, f.hasPending, f.partial = c, true, false
		report, run := f.startLocked(c)
		f.mu.Unlock()
		report(run)
	}
}

// fill makes pending hold the reader's next bytes. It is the one way bytes
// reach the reader (Read, ReadChunk and Peek all go through it): it takes the
// stream's next bytes (next), and drops the chunks the floor covers
// (DropBefore). It returns next's errors, and ErrUnnumbered for an unnumbered
// chunk under a floor.
func (f *FakeConn) fill() error {
	for {
		if err := f.next(); err != nil {
			f.endRunAt(err)
			return err
		}
		f.mu.Lock()
		if f.floor != 0 && f.pending.ConnSeq == 0 {
			f.mu.Unlock()
			f.endRunAt(ErrUnnumbered)
			return ErrUnnumbered
		}
		if f.floorCoversLocked(f.pending) {
			// Not the reader's: captured before the floor, in whole or what
			// is left of it.
			f.dropPendingLocked()
			f.mu.Unlock()
			continue
		}
		// The reader's: the run the floor was dropping before it ends.
		report, run := f.endRunLocked()
		f.mu.Unlock()
		report(run)
		return nil
	}
}

// floorCoversLocked reports whether the floor drops c: there is a floor, and
// c, numbered, was captured before it. Caller holds mu.
func (f *FakeConn) floorCoversLocked(c Chunk) bool {
	return f.floor != 0 && c.ConnSeq != 0 && c.CapturedBefore(f.floor)
}

// dropPendingLocked drops the chunk in hand, or what is left of it, in the run
// the floor is dropping (dropLocked): its bytes leave the FakeConn, not handed
// to the reader. Caller holds mu.
func (f *FakeConn) dropPendingLocked() {
	f.dropLocked(f.pending)
	f.advanceLocked(len(f.pending.Bytes))
	f.pending, f.hasPending, f.partial = Chunk{}, false, false
}

// startLocked is told of each chunk as it comes off the stream (next). While
// the stream's start is not behind it (started), c is in the start when it was
// captured before the reader's first floor (first). The first chunk that is
// not ends the start, and the run the floor dropped in it (endRunLocked): it
// returns what endRunLocked does then, and noRun otherwise. Caller holds mu.
func (f *FakeConn) startLocked(c Chunk) (func(Dropped), Dropped) {
	if f.started || f.first != 0 && c.ConnSeq != 0 && c.CapturedBefore(f.first) {
		return noRun, Dropped{}
	}
	report, run := f.endRunLocked()
	f.started = true
	return report, run
}

// endRunAt ends the run the floor was dropping as a read (fill, SkipThrough,
// SettleStart) returns err, with no chunk handed: every such return ends it,
// but one at the read deadline, after which the reader can read on and the run
// with it. So a run is reported once, however the read that took its chunks
// ends: the stream's end, a refusal, or a skip that cannot cut. Caller does
// not hold mu.
func (f *FakeConn) endRunAt(err error) {
	if errors.Is(err, ErrDeadlineExceeded) {
		return
	}
	f.mu.Lock()
	report, run := f.endRunLocked()
	f.mu.Unlock()
	report(run)
}

// AtChunkBoundary reports whether the reader's next byte starts a chunk: no
// chunk is left in part, some of its bytes read (or swallowed for the reader)
// and the rest still to read. A chunk Peek showed and the reader has not taken
// is whole, so the reader is at its boundary. A parser taking up a stream
// again after a message it could not frame starts only where a message was
// sent on its own.
func (f *FakeConn) AtChunkBoundary() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.partial
}

// SkipThrough discards the bytes of this stream captured before t, and leaves
// the reader at the start of the first chunk captured after it.
//
// A parser that could not frame a response re-aligns with it. In a
// request/response protocol whose client sends a command only once it has
// read the whole answer to the one before (MySQL's, unpipelined), every byte
// of that answer was captured before the next command was, and every byte of
// the next answer after it. With t the capture time of the next command, what
// SkipThrough discards is the rest of the answer the parser could not frame,
// and the next Read starts the next answer: a capture never joins two reads
// into one chunk, so an answer the server sent only after the command starts a
// chunk of its own.
//
// What is left of the chunk the reader is partway through goes with it when
// that chunk was captured before t. When the reader has read from a chunk
// captured after t (partway through it, or the whole of it) the stream cannot
// be cut at t, and SkipThrough returns [ErrSkipMidChunk] having discarded
// nothing. A chunk with no capture time, or captured at t itself, cannot be
// told to be before or after it (a clock too coarse to order the command and
// its answer): SkipThrough returns [ErrUnplaced] before it. Otherwise it
// blocks for chunks until one captured after t arrives, and leaves it unread,
// LastReadTime still naming the last chunk read; or until the stream ends,
// with the error a Read gets there. It returns how many bytes it discarded,
// and the capture time of the last chunk it discarded from (zero when none).
//
// It cuts the stream as it was captured (next), before the floor (DropBefore):
// what it discards, and whether it can cut at all, do not depend on a floor.
// The floor then drops, at the reader's next read, what is left of the stream
// that was captured before it: of a capture whose numbers and times agree,
// nothing the skip has not discarded already.
//
// What it discards of the stream's start, the bytes captured before the
// reader's first floor, is in the floor's run there all the same
// (Dropped.AtStart), as what the floor drops of it is: the start is reported
// however it leaves the stream, the floor's read or this cut, and as nothing
// else. The first chunk the skip takes past it, discarded or left for the
// reader, ends that run, as the skip's return without one does (endRunAt). A
// chunk of the start it leaves for the reader (one stamped at t or after it,
// of a capture whose numbers and times disagree) is the floor's to drop at the
// next read, in the run, which goes on. A chunk it discards past the start is
// the reader's own (the rest of an answer it left out, which it reports
// itself), and is in no run.
//
// It is the reader's own call, as a Read is.
func (f *FakeConn) SkipThrough(t time.Time) (n int64, last time.Time, err error) {
	for {
		f.mu.Lock()
		if !f.hasPending && f.pos >= f.discardTo {
			// Nothing in hand: the last chunk read, all of it, was captured
			// at t or after it. (A chunk the skip discards was captured
			// before t, so this holds or not for the whole of the skip.)
			if read := f.lastReadNano.Load(); read != 0 && !time.Unix(0, read).Before(t) {
				f.mu.Unlock()
				err := ErrSkipMidChunk
				if time.Unix(0, read).Equal(t) {
					err = ErrUnplaced
				}
				f.endRunAt(err)
				return n, last, err
			}
		}
		f.mu.Unlock()
		if err := f.next(); err != nil {
			f.endRunAt(err)
			return n, last, err
		}
		f.mu.Lock()
		c := f.pending
		// In the stream's start (next has told startLocked of c): the floor's
		// to drop, in its run there, however it leaves the stream.
		inStart := !f.started
		report, run := noRun, Dropped{}
		if !inStart {
			// Past the start: whatever the floor was dropping before it is
			// done.
			report, run = f.endRunLocked()
		}
		if at := c.ReadAt; at.IsZero() || !at.Before(t) {
			// Left for the reader's next read: in its hand, as a chunk Peek
			// shows it is.
			partial := f.partial
			f.mu.Unlock()
			report(run)
			switch {
			case at.IsZero() || at.Equal(t):
				return n, last, ErrUnplaced
			case partial:
				return n, last, ErrSkipMidChunk
			}
			// A whole chunk captured after t: the next Read starts with it,
			// and LastReadTime names it once it does.
			return n, last, nil
		}
		// Discarded: its bytes leave the FakeConn, and its times are the last
		// read, as a chunk's that was handed over are.
		if inStart {
			f.dropLocked(c)
		}
		f.deliverLocked(c, len(c.Bytes))
		f.pending, f.hasPending, f.partial = Chunk{}, false, false
		f.mu.Unlock()
		report(run)
		n, last = n+int64(len(c.Bytes)), c.ReadAt
	}
}

// beforeWait, when set, runs on a reader's goroutine as it is about to wait:
// after Waiting reports it waiting, before the wait. It is for tests, which put
// what arrives in that window there every time instead of by chance; nil
// outside them. It is the package's, not a FakeConn field: a FakeConn is 288
// bytes, a size class, and one more field would put each in the 320 byte one.
var beforeWait atomic.Pointer[func(*FakeConn)]

// receive takes the next Chunk off ch. While the FakeConn is open it blocks
// for one (Waiting), subject to the read deadline and Close, and returns
// io.EOF once ch is closed. Once closed it does not block: Close means "no
// more blocking", NOT "discard bytes already delivered to me". The relay tee
// drains its buffered chunks into ch and then Close() fires during teardown; a
// chunk that already landed in ch is a fully recorded wire event and must
// still be readable, otherwise a response whose body arrived a hair before the
// connection was torn down is silently lost (the "server closed before
// response" mock-incomplete race on Connection: close traffic such as the
// boot-time startup mock). So it takes what is already on ch, and returns
// ErrClosed once nothing is. That holds however the reader learns of the
// Close, at its start or while it waits: a reader that found ch empty can be
// delivered a chunk and closed before it waits, and then waits with both
// ready, so the wait goes back to take what ch holds.
func (f *FakeConn) receive() (Chunk, error) {
	// Re-fetch the deadline channel on each iteration so concurrent
	// SetReadDeadline calls take effect on this in-flight read. The
	// changed-notification channel (closed by SetReadDeadline) wakes
	// the select; we then loop and reload both channels.
	for {
		if f.closed.Load() {
			select {
			case c, ok := <-f.ch:
				if ok {
					return c, nil
				}
			default:
			}
			return Chunk{}, ErrClosed
		}
		dlCh, changedCh := f.currentDeadlineChans()
		select {
		case c, ok := <-f.ch:
			if !ok {
				return Chunk{}, io.EOF
			}
			return c, nil
		default:
		}
		// Nothing to take: the reader is done with all it was handed, and
		// blocks for more (Waiting).
		f.waits.Add(1)
		f.waiting.Store(true)
		if h := beforeWait.Load(); h != nil {
			(*h)(f)
		}
		select {
		case c, ok := <-f.ch:
			f.waiting.Store(false)
			if !ok {
				return Chunk{}, io.EOF
			}
			return c, nil
		case <-f.closeCh:
			// Closed: loop to take what ch already holds.
			f.waiting.Store(false)
		case <-dlCh:
			f.waiting.Store(false)
			return Chunk{}, ErrDeadlineExceeded
		case <-changedCh:
			// deadline changed; loop and re-fetch.
			f.waiting.Store(false)
		}
	}
}

// Dropped is a run of chunks the floor dropped (FakeConn.DropBefore), reported
// once as it ends.
type Dropped struct {
	// First is the ConnSeq of the run's first chunk.
	First uint32
	// AtStart is set when the run is the stream's start: the chunks captured
	// before the reader's first floor, which come off the stream before any
	// other, since a stream's chunks come off it in the order they were
	// captured. It holds those of them that leave the stream by
	// SkipThrough's cut or SettleStart too, and nothing captured after that
	// floor: a run that is not the start's is not AtStart. A stream has at
	// most one.
	AtStart bool
	// From and To are the capture times (ReadAt) of the first and the last of
	// its chunks that had one.
	From, To time.Time
}

// noRun is the reporter of no run.
func noRun(Dropped) {}

// dropLocked counts c in the run the floor is dropping. c is numbered
// (floorCoversLocked): fill refuses an unnumbered chunk under a floor. Caller
// holds mu.
func (f *FakeConn) dropLocked(c Chunk) {
	if f.runFirst == 0 {
		f.runFirst = c.ConnSeq
	}
	if c.ReadAt.IsZero() {
		return
	}
	at := c.ReadAt.UnixNano()
	if f.runFrom == 0 {
		f.runFrom = at
	}
	f.runTo = at
}

// endRunLocked ends the run the floor was dropping: the reader is about to be
// handed the chunk after it (fill), a chunk comes off the stream past its
// start (startLocked, SkipThrough), or a read returns without one (endRunAt).
// A run that ends while the stream's start is not behind it is the start's
// (AtStart), and its end puts the start behind it (started): so the start is
// reported once, in one run, however its read ends. It returns the run's
// reporter (noRun when there is no run) and the run, for the caller to call
// once it has released mu. Caller holds mu.
func (f *FakeConn) endRunLocked() (func(Dropped), Dropped) {
	if f.runFirst == 0 {
		return noRun, Dropped{}
	}
	run := Dropped{First: f.runFirst, AtStart: !f.started}
	f.started = true
	if f.runFrom != 0 {
		run.From, run.To = time.Unix(0, f.runFrom), time.Unix(0, f.runTo)
	}
	f.runFirst, f.runFrom, f.runTo = 0, 0, 0
	return f.reportRun, run
}

// DropBefore sets the floor: from the reader's next read on, every chunk
// captured before the chunk numbered seq (Chunk.CapturedBefore) is dropped,
// however late it reaches the reader, and so is what is left of one the reader
// read in part. A request/response parser's session sets it on the server's
// stream with the ConnSeq of each request's first chunk
// (supervisor.Session.NextRequest): server bytes captured before a request are
// not its answer. Each run of chunks it drops is told to report once, as the
// run ends: when the reader is next handed a chunk, or its read returns
// without one, at the stream's end or a refusal (but not at the read deadline,
// after which the reader can read on). What it drops moves the stream on as
// delivered bytes do (Consumed), and moves neither LastReadTime nor
// LastWrittenTime.
//
// The first seq it is given is the reader's first floor: what was captured
// before it is the stream's start, and the run the floor drops there is
// Dropped.AtStart, which ends at the first chunk off the stream captured
// after that floor. A later floor moves the floor, not the start.
//
// With a floor, a chunk the producer did not number (ConnSeq 0) is not
// compared: the read that meets it fails with ErrUnnumbered. seq is a numbered
// chunk's ConnSeq, which the caller checks, and report is not nil.
//
// SkipThrough is not such a read: it cuts the stream by capture time before
// the floor, and the floor applies to what it leaves. What it discards of the
// stream's start is in the floor's run there (AtStart).
func (f *FakeConn) DropBefore(seq uint32, report func(Dropped)) {
	f.mu.Lock()
	if f.first == 0 {
		f.first = seq
	}
	f.floor, f.reportRun = seq, report
	f.mu.Unlock()
}

// SettleStart puts the stream's start behind it for a reader that will not
// read the stream again (supervisor.Session.EndExchanges). The run the floor
// drops in the start (Dropped.AtStart) is reported as it ends, which is when
// a read goes past the start. A reader that ends without reading the stream
// there would leave it unreported. SettleStart reads on for it, as fill does:
// it drops each chunk of the start, which the floor covers, in that run, and
// waits for the next as a read does, until a chunk captured after the first
// floor comes off the stream (left in hand, not read), or the read returns
// without one (endRunAt): the stream's end, or Close (at a read deadline the
// start stays as it is, as after any read). It returns at once when the start
// is already behind it, and when there is no floor: with none, the stream has
// no start.
//
// It is the reader's own call, as a Read is.
func (f *FakeConn) SettleStart() {
	for {
		f.mu.Lock()
		done := f.started || f.first == 0
		f.mu.Unlock()
		if done {
			return
		}
		if err := f.next(); err != nil {
			f.endRunAt(err)
			return
		}
		f.mu.Lock()
		if !f.started {
			// Of the start (startLocked): captured before the first floor,
			// so before the floor, which drops it.
			f.dropPendingLocked()
		}
		f.mu.Unlock()
	}
}

// Consumed is how many bytes of this stream have left the FakeConn: handed to
// the reader, or swallowed for it (DiscardBefore, DropBefore). With what the
// producer has put on the channel, it tells how much the reader has yet to
// take.
func (f *FakeConn) Consumed() int64 { return f.consumed.Load() }

// Waiting reports whether the reader is blocked for the next chunk with
// nothing buffered for it, and how many times it has blocked so. A reader that
// reads one message at a time and is blocked so is done with everything it
// was handed. A caller that sees the same count before and after checking
// other state knows the reader stayed blocked in between.
func (f *FakeConn) Waiting() (waiting bool, waits uint64) {
	return f.waiting.Load(), f.waits.Load()
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
// FakeConn), so bytes to be dropped can simultaneously be in the queue,
// in the drain goroutine's hand, in the channel, and pending here.
// Draining that from outside means racing the drain goroutine. Here there
// is no race at all: this FakeConn is single-consumer by construction, and
// every one of those places is reached by pulling FORWARD from the
// reader, which is what its next read does (fill).
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
// degrades to best effort: nothing more is coming, so the next read stops
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

// LastReadTime returns the ReadAt timestamp of the Chunk the reader was last
// handed bytes of (Read, ReadChunk; not Peek, nor a chunk the floor dropped),
// or the zero time if none carried one.
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
