package relay

import (
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

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
	// Check the per-conn byte cap before attempting to send. The
	// accounting is lazy: t.bytes is only incremented after the
	// staging-send succeeds (see t.bytes.Add(n) below), so there is
	// nothing to "undo" on the drop paths. The load-then-compare is
	// not atomic w.r.t. the drain goroutine, but the worst case is
	// an over- or under-count of one chunk; the cap is a soft limit
	// by contract.
	if t.bytes.Load()+n > t.cap {
		t.drop(DropPerConnCap)
		return false
	}

	// Hold the read lock only around the send: close takes the write
	// lock before closing staging, so this keeps send and close
	// mutually exclusive without serialising pushes against each
	// other. Re-check closed under the lock; close may have fired
	// between the fast-path load and acquiring the lock.
	t.closeMu.RLock()
	if t.closed.Load() {
		t.closeMu.RUnlock()
		return false
	}
	select {
	case t.staging <- c:
		t.bytes.Add(n)
		t.closeMu.RUnlock()
		return true
	default:
		t.closeMu.RUnlock()
		t.drop(DropChannelFull)
		return false
	}
}

// drop bumps counters and notifies. Kept small so push stays inlineable.
//
// Logging cadence is exponential (drops_total ∈ {1, 2, 4, 8, ...}) at Warn
// so the first drop on a connection always surfaces (the signal an
// operator wants when investigating "incomplete mock") while sustained
// backpressure produces O(log N) warnings instead of O(N). Subsequent
// drops still increment drops_total and fire onDrop unchanged — only the
// log emission is rate-limited.
func (t *tee) drop(reason string) {
	n := t.drops.Add(1)
	if t.onDrop != nil {
		t.onDrop(reason)
	}
	if t.logger != nil && n&(n-1) == 0 {
		// n is a power of two (1, 2, 4, 8, ...) — log this drop.
		t.logger.Warn("relay: tee drop",
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
		select {
		case t.out <- c:
		case <-t.shutdown:
			// Consumer stopped reading before we could deliver;
			// drop the chunk. The mock is being abandoned either
			// way (close implies teardown), so suppress the usual
			// onDrop notification to avoid double-counting.
			t.drops.Add(1)
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
