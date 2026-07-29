package relay

import (
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// teardownFlushGrace bounds how long the drain will block delivering a
// single overflow-tail chunk to out at teardown before abandoning it. out
// is still open and the per-connection decoder normally drains it, so the
// non-blocking fast path usually delivers immediately; this grace only
// applies if the consumer momentarily lags, and the cap prevents an
// exited-early consumer from hanging the drain goroutine forever.
const teardownFlushGrace = 2 * time.Second

// Drop-reason constants. Kept as exported strings so callers can
// branch on them in tests and assertions without importing internal
// types. Also forwarded verbatim into OnMarkMockIncomplete.
const (
	// DropMemoryPressure — [Config.MemoryGuardCheck] returned true
	// at tee time.
	DropMemoryPressure = "memory_pressure"

	// DropPerConnCap — the per-connection byte cap would be exceeded
	// by admitting this chunk.
	DropPerConnCap = "per_conn_cap"

	// DropChannelFull — the internal staging channel was full.
	DropChannelFull = "channel_full"

	// DropPaused — the direction was paused via KindPauseDir or a
	// TLS upgrade was in flight.
	DropPaused = "paused"
)

// tee is the accounting wrapper around the internal staging channel
// between the forwarder goroutines and the FakeConn. For each direction
// the forwarder calls push(c); that either enqueues the chunk onto an
// internal channel (non-blocking) or drops it. A drain goroutine pulls
// from the internal channel, decrements the byte counter, and forwards
// to the FakeConn-facing channel.
//
// The indirection exists because fakeconn.FakeConn is the sole consumer
// of its read channel and we cannot instrument its Read path from here;
// the drain goroutine is the point where we learn that a chunk has
// moved out of relay-owned memory and into the FakeConn's internal
// buffer.
//
// Lifetime: created by [newTee] and stopped by [tee.close]. close is
// idempotent; callers typically defer it in Run.
type tee struct {
	dir      fakeconn.Direction
	logger   *zap.Logger
	cap      int64
	memCheck func() bool
	onDrop   func(reason string)

	// staging is the internal buffered channel. Forwarders push into
	// it; the drain goroutine pulls out.
	staging chan fakeconn.Chunk

	// out is the FakeConn-facing channel. The drain goroutine sends
	// into it; the FakeConn reads from it.
	out chan fakeconn.Chunk

	// bytes is the running count of bytes sitting in staging (read
	// from the socket, not yet consumed by the parser). Updated with
	// atomic ops: add on successful push, subtract on drain.
	bytes atomic.Int64

	// paused is set by the directive processor to suspend tee delivery
	// while still forwarding real bytes.
	paused atomic.Bool

	// drops counts dropped chunks for this direction. Exposed via
	// [tee.dropCount] for diagnostics and tests.
	drops atomic.Uint64

	// overflow is an order-preserving spill buffer used when the staging
	// channel is momentarily full (the parser is behind, e.g. during a
	// concurrent boot burst — many small createIndexes/handshake chunks
	// arriving faster than the mongo decoder drains, or a large response
	// exceeding the 1024-chunk channel). Without it a full staging channel
	// forces push to DROP the chunk (DropChannelFull); a dropped RESPONSE
	// chunk then strands its request and the mock is never emitted — the
	// non-deterministic boot "no_mocks" loss. Spilling here instead keeps
	// push non-blocking (invariant I1: the forwarder returns to Read
	// immediately) AND lossless up to the per-conn cap. push flushes
	// overflow → staging opportunistically and drain flushes any remaining
	// tail at teardown, both preserving wire order (overflow chunks always
	// follow the chunks already in staging). overflowMu guards overflow +
	// overflowBytes; it is only ever taken by push and by drain's teardown
	// flush, never held across a blocking op.
	overflowMu    sync.Mutex
	overflow      []fakeconn.Chunk
	overflowBytes int64

	// closeMu is held in read mode during a push's channel send and
	// in write mode by close. This ensures no in-flight send is racing
	// the channel-close step, eliminating "send on closed channel"
	// panics without requiring the forwarder to block on a Mutex.
	closeMu sync.RWMutex
	// closeOnce guards the single close of staging.
	closeOnce sync.Once
	// closed is set by close so concurrent push callers short-circuit
	// before touching the channel.
	closed atomic.Bool
	// done is closed once the drain goroutine has exited so tests can
	// wait for it deterministically.
	done chan struct{}
	// shutdown is closed by close() to unblock a drain goroutine
	// stuck sending to a FakeConn that has stopped reading.
	shutdown chan struct{}
}

// newTee wires a staging channel, an out channel, and a drain
// goroutine that moves chunks from staging to out while maintaining
// the bytes counter.
func newTee(dir fakeconn.Direction, capBytes int64, chanBuf int, memCheck func() bool, onDrop func(reason string), logger *zap.Logger) *tee {
	t := &tee{
		dir:      dir,
		logger:   logger,
		cap:      capBytes,
		memCheck: memCheck,
		onDrop:   onDrop,
		staging:  make(chan fakeconn.Chunk, chanBuf),
		out:      make(chan fakeconn.Chunk, chanBuf),
		done:     make(chan struct{}),
		shutdown: make(chan struct{}),
	}
	go t.drain()
	return t
}

// readCh returns the FakeConn-facing receive channel. Exposed so the
// relay can wrap it in a [fakeconn.FakeConn].
func (t *tee) readCh() <-chan fakeconn.Chunk { return t.out }

// dropCount returns the number of chunks dropped since construction.
// Safe to call concurrently.
func (t *tee) dropCount() uint64 { return t.drops.Load() }

// setPaused toggles delivery. When paused, push immediately drops with
// reason [DropPaused] without consuming capacity.
func (t *tee) setPaused(p bool) { t.paused.Store(p) }

// push admits a chunk into staging. Returns true on success, false on
// drop. A drop invokes onDrop with the reason string and does not
// alter the byte counter.
//
// push never blocks: channel-full and cap-exceeded cases are reported
// as drops. This is the load-bearing invariant for I1 — the caller
// (forwarder goroutine) must be free to return to Read immediately.
//
// Once the tee has been closed push silently returns false without
// invoking onDrop: the mock is already abandoned at that point, so
// additional "drop" notifications would just add noise.
func (t *tee) push(c fakeconn.Chunk) bool {
	if t.closed.Load() {
		return false
	}
	if t.paused.Load() {
		t.drop(DropPaused)
		return false
	}
	if t.memCheck != nil && t.memCheck() {
		t.drop(DropMemoryPressure)
		return false
	}
	n := int64(len(c.Bytes))

	// overflowMu serialises the staging/overflow decision so wire order is
	// preserved: a concurrent pair of pushes cannot interleave one onto
	// staging and the other onto overflow out of order.
	t.overflowMu.Lock()
	// Check the per-conn byte cap. It now bounds staging + overflow
	// TOGETHER, so the spill below can never let one connection exceed the
	// cap. The accounting is lazy: t.bytes is incremented after a staging
	// send succeeds and overflowBytes after a spill; both are decremented
	// as the chunk drains, so there is nothing to "undo" on the drop path.
	if t.bytes.Load()+t.overflowBytes+n > t.cap {
		t.overflowMu.Unlock()
		t.drop(DropPerConnCap)
		return false
	}
	// Drain any existing backlog into staging first, so the incoming chunk
	// is only ever ordered AFTER chunks already buffered.
	t.flushOverflowLocked()
	if len(t.overflow) == 0 {
		// No backlog — try the fast path straight onto staging. Hold the
		// read lock only around the send: close takes the write lock
		// before closing staging, keeping send and close mutually
		// exclusive. Re-check closed under the lock; close may have fired.
		t.closeMu.RLock()
		if t.closed.Load() {
			t.closeMu.RUnlock()
			t.overflowMu.Unlock()
			return false
		}
		select {
		case t.staging <- c:
			t.bytes.Add(n)
			t.closeMu.RUnlock()
			t.overflowMu.Unlock()
			return true
		default:
			// staging channel full — fall through to spill.
			t.closeMu.RUnlock()
		}
	}
	// staging is full (or a backlog already exists): SPILL onto overflow
	// instead of dropping. This is the fix for the boot "no_mocks" loss — a
	// dropped response chunk strands its request and the mock is never
	// emitted. push stays non-blocking (invariant I1) and the per-conn cap
	// checked above bounds the memory, so this cannot grow unbounded.
	t.overflow = append(t.overflow, c)
	t.overflowBytes += n
	t.flushOverflowLocked() // opportunistically push some through now
	t.overflowMu.Unlock()
	return true
}

// flushOverflowLocked moves as many buffered overflow chunks as will fit
// onto the staging channel, oldest first — preserving wire order. The
// caller MUST hold overflowMu. It is non-blocking: it stops at the first
// send that would block on a full staging channel, and short-circuits if
// the tee has been closed (the drain teardown then delivers the tail).
func (t *tee) flushOverflowLocked() {
	for len(t.overflow) > 0 {
		c := t.overflow[0]
		t.closeMu.RLock()
		if t.closed.Load() {
			t.closeMu.RUnlock()
			return
		}
		select {
		case t.staging <- c:
			t.closeMu.RUnlock()
			n := int64(len(c.Bytes))
			t.bytes.Add(n)
			t.overflowBytes -= n
			t.overflow[0] = fakeconn.Chunk{} // release the reference
			t.overflow = t.overflow[1:]
		default:
			t.closeMu.RUnlock()
			return
		}
	}
	// Overflow fully drained: release the backing array so a long-lived
	// connection that spilled once doesn't pin it forever.
	t.overflow = nil
	t.overflowBytes = 0
}

// drop bumps counters and notifies. Kept small so push stays inlineable.
//
// Logging cadence is exponential (drops_total ∈ {1, 2, 4, 8, ...}) at
// Debug so the first drop on a connection surfaces when --debug is on
// while sustained backpressure produces O(log N) emissions instead of
// O(N). Drops are an internal-pipeline signal, not a user-facing
// problem (the forward path is unaffected; only mock recording is
// impacted), so the level is Debug — operators investigating
// "incomplete mock" enable --debug to see them, normal runs stay
// quiet. Subsequent drops still increment drops_total and fire onDrop
// unchanged — only the log emission is rate-limited.
func (t *tee) drop(reason string) {
	n := t.drops.Add(1)
	if t.onDrop != nil {
		t.onDrop(reason)
	}
	if t.logger != nil && n&(n-1) == 0 {
		// n is a power of two (1, 2, 4, 8, ...) — log this drop.
		t.logger.Debug("relay: tee drop",
			zap.String("dir", t.dir.String()),
			zap.String("reason", reason),
			zap.Uint64("drops_total", n),
		)
	}
}

// drain moves chunks from staging to the FakeConn-facing channel,
// decrementing the byte counter as it goes. When staging is closed —
// either because the relay is shutting down or because close was
// called explicitly — drain forwards any remaining buffered chunks
// non-blockingly, then closes out and signals done.
//
// The FakeConn reader is the only receiver on out; under normal
// operation it drains at roughly parser speed and sends complete
// quickly. If the parser stops consuming, staging fills and push()
// starts reporting DropChannelFull — real traffic is never blocked.
// The chunk currently held by drain is accounted for in bytes until
// it is delivered or discarded, so the counter will over-count by at
// most one chunk during normal operation.
//
// Correctness under teardown: the staging send in push is guarded by
// closeMu, so after close() returns no new values can be enqueued.
// drain's send to out selects on shutdown to avoid a deadlock when
// the FakeConn consumer has already stopped reading.
func (t *tee) drain() {
	defer close(t.done)
	defer close(t.out)
	for c := range t.staging {
		t.bytes.Add(-int64(len(c.Bytes)))
		// Prefer delivery. A non-blocking send always succeeds while
		// out has buffer room, which it does for every chunk that was
		// admitted to staging: out and staging share the same capacity
		// and close() stops further pushes, so the bounded tail left in
		// staging at teardown fits in out. This matters for teardown
		// correctness: when close() fires shutdown, a plain
		// `select { case out<-c: case <-shutdown: drop }` would pick
		// randomly between delivering and dropping a fully-recorded
		// chunk — that coin flip is the "server closed before response"
		// mock-drop race for Connection: close traffic (e.g. the boot
		// startup mock). Only fall back to the shutdown escape when out
		// is genuinely full and the consumer has stopped reading, which
		// is the deadlock-avoidance case the escape was built for.
		select {
		case t.out <- c:
			continue
		default:
		}
		select {
		case t.out <- c:
		case <-t.shutdown:
			// Consumer stopped reading and out is full before we could
			// deliver; drop the chunk. The mock is being abandoned
			// either way (close implies teardown), so suppress the usual
			// onDrop notification to avoid double-counting.
			t.drops.Add(1)
		}
	}

	// staging is closed and fully drained. Deliver any tail still buffered
	// in overflow — chunks that never made it into staging (e.g. the final
	// bytes of a boot response on a connection that spilled and then closed
	// before push could flush them). Nothing is stranded past this snapshot:
	// a push only appends to overflow when overflow is non-empty AFTER its
	// flush (tee.go's append path); if overflow is empty it takes the fast
	// path, which re-checks closed under closeMu and returns without
	// appending once close() has fired. A non-empty overflow and this
	// snapshot are therefore mutually exclusive under overflowMu, so any push
	// that appends does so BEFORE the snapshot and is captured in tail; after
	// the snapshot nils overflow, the next push sees len==0 → the closed
	// re-check → returns. Order is preserved: overflow chunks always follow
	// every chunk that passed through staging.
	t.overflowMu.Lock()
	tail := t.overflow
	t.overflow = nil
	t.overflowBytes = 0
	t.overflowMu.Unlock()
	// out is still open here (close(t.out) is deferred and runs after this
	// loop) and the per-connection decoder keeps draining it until it
	// closes, so these blocking sends normally complete at once. Do NOT
	// select on t.shutdown: it is already closed by teardown, so it would
	// fire at random and drop deliverable chunks. Instead a single deadline
	// bounds the WHOLE flush: a live consumer receives every chunk well
	// within it; a stopped consumer (out stays full) trips the deadline once
	// and the residual is abandoned, so a gone consumer cannot wedge the
	// drain goroutine — and we never pay the grace per chunk.
	deadline := time.NewTimer(teardownFlushGrace)
	defer deadline.Stop()
	for i, c := range tail {
		select {
		case t.out <- c:
		case <-deadline.C:
			t.drops.Add(uint64(len(tail) - i)) // abandon the rest
			return
		}
	}
}

// close stops the tee. After close returns, push is a no-op (returns
// false) and the drain goroutine will finish once staging is drained.
// close is idempotent.
func (t *tee) close() {
	t.closeOnce.Do(func() {
		// Write-lock fences any in-flight push send; setting closed
		// first plus the fence means "new sends see closed=true,
		// ongoing sends have finished".
		t.closeMu.Lock()
		t.closed.Store(true)
		close(t.staging)
		close(t.shutdown)
		t.closeMu.Unlock()
	})
}

// waitDone blocks until the drain goroutine has exited. Used by tests.
func (t *tee) waitDone() { <-t.done }
