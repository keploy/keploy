package relay

import (
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// KNOWN LIMITATION: the byte budget counts PAYLOAD only, so the real heap a
// connection can retain is a MULTIPLE of the configured cap. Each queued entry
// also costs a fakeconn.Chunk (88 B by unsafe.Sizeof: a slice header, two
// time.Times, a direction and a sequence number), and that is not charged.
//
// Measured retained heap at a saturated 64 MiB budget, by chunk size:
//
//	  16 B ->  449.5 MiB  (7.0x)
//	  64 B ->  165   MiB  (2.6x)
//	 256 B ->   90   MiB  (1.4x)
//	1448 B ->   72   MiB  (1.1x)   // MTU
//
// So maxMemoryPerConnection: 67108864 can retain ~450 MiB per direction per
// connection against a small-chunk peer. The fixed-slot channel this replaced
// bounded entries implicitly at 1024, so this is a real narrowing.
//
// It is left as-is deliberately, but note the honest reason: the overhead is
// ~10% at MTU and only falls under 1% above roughly 8.8 KB per chunk, so this
// is NOT negligible for small chunks — and small chunks are exactly the boot
// burst this rewrite exists to stop dropping. Charging per-entry overhead was
// implemented and reverted because it broke byte-precise cap enforcement at
// small caps (two existing cap tests failed) and the empty-queue exemption
// needed to compensate made the cap unenforceable in the other direction.
// A real fix needs both halves plus re-baselined cap tests.

// Drop-reason constants. Kept as exported strings so callers can
// branch on them in tests and assertions without importing internal
// types. Also forwarded verbatim into OnMarkMockIncomplete.
const (
	// DropMemoryPressure — [Config.MemoryGuardCheck] returned true
	// at tee time.
	DropMemoryPressure = "memory_pressure"

	// DropPerConnCap — the per-connection byte cap would be exceeded
	// by admitting this chunk. This is the only backpressure drop the
	// tee reports: the queue is bounded by bytes, not by slot count.
	DropPerConnCap = "per_conn_cap"

	// DropChannelFull is no longer emitted. The tee used to stage chunks
	// in a fixed-slot channel and drop when it filled, which lost response
	// chunks during a boot burst and stranded their mocks. The queue is now
	// bounded solely by the per-connection byte cap, so a burst that fits
	// the cap is never dropped and one that does not is reported as
	// [DropPerConnCap]. Retained so existing callers and dashboards that
	// reference the string still compile.
	//
	// Deprecated: no longer produced; see [DropPerConnCap].
	DropChannelFull = "channel_full"

	// DropPaused — the direction was paused via KindPauseDir or a
	// TLS upgrade was in flight.
	DropPaused = "paused"

	// DropConsumerGone — the parser stopped reading its FakeConn, so
	// chunks already recorded could not be handed over. Reported once per
	// connection with the whole undelivered remainder, rather than silently,
	// so a lost tail is visible the way a BPF ring buffer's lost-sample
	// count is.
	DropConsumerGone = "consumer_gone"
)

// tee is the accounting buffer between the forwarder goroutines and the
// FakeConn. For each direction the forwarder calls push(c), which appends
// to an in-memory queue and returns immediately; a drain goroutine pops
// from that queue and delivers to the FakeConn-facing channel.
//
// The indirection exists because fakeconn.FakeConn is the sole consumer
// of its read channel and we cannot instrument its Read path from here;
// the drain goroutine is the point where we learn that a chunk has
// moved out of relay-owned memory and into the FakeConn's internal
// buffer.
//
// # Why a queue and not a channel
//
// A buffered channel bounds by SLOT COUNT, but the resource that actually
// needs bounding is BYTES, and the two disagree badly: a boot burst of many
// small handshake chunks exhausts slots while using almost no memory, and a
// single large response does the reverse. When slots ran out the tee had no
// choice but to drop, and a dropped RESPONSE chunk strands its request so the
// mock is never emitted — the non-deterministic boot "no_mocks" loss.
//
// A single byte-bounded queue removes that failure mode outright rather than
// absorbing it in a second buffer: there is one FIFO, so wire order holds by
// construction; one counter, so the cap is exact rather than approximate; and
// one reason a chunk can ever be refused (the cap). push still never blocks,
// which is the load-bearing invariant for I1 — the forwarder must be free to
// return to Read immediately, since real traffic must never wait on recording.
//
// Lifetime: created by [newTee], started by [tee.start], stopped by
// [tee.close]. close is idempotent; callers typically defer it in Run.
type tee struct {
	dir      fakeconn.Direction
	logger   *zap.Logger
	cap      int64
	memCheck func() bool
	onDrop   func(reason string)

	// mu guards q, qBytes and closed. It is never held across a send on
	// out, so a stalled consumer cannot block push.
	mu sync.Mutex
	// q is the pending-chunk FIFO. Order is the order push saw, which is
	// wire order.
	q []fakeconn.Chunk
	// qBytes is the exact number of bytes queued. Compared against cap
	// under mu, so unlike a lazily-updated counter the cap is a hard limit
	// rather than one that can be overshot by a chunk.
	qBytes int64
	// closed is set by close(); push refuses afterwards and drain exits
	// once q is empty.
	closed bool

	// wake is a capacity-1 doorbell. push signals it non-blockingly after
	// appending; drain waits on it when q is empty. A signal can be
	// coalesced or arrive spuriously — drain always re-reads q under mu,
	// so neither loses a chunk.
	wake chan struct{}

	// out is the FakeConn-facing channel. The drain goroutine sends
	// into it; the FakeConn reads from it.
	out chan fakeconn.Chunk

	// stallGrace bounds how long drain waits on a FULL out channel before
	// concluding the consumer is gone. Supplied by the caller via
	// [Config.ConsumerStallGrace]; see DefaultConsumerStallGrace for why it
	// bounds stalled time rather than total teardown time.
	stallGrace time.Duration

	// consumerGone is the FakeConn's close signal, supplied to [tee.start].
	// It fires only when the parser itself abandons the connection, which
	// is the sole condition under which a fully recorded chunk may be
	// thrown away.
	consumerGone <-chan struct{}

	// paused is set by the directive processor to suspend tee delivery
	// while still forwarding real bytes.
	paused atomic.Bool

	// drops counts dropped chunks for this direction. Exposed via
	// [tee.dropCount] for diagnostics and tests.
	drops atomic.Uint64

	// closedFlag mirrors closed for a lock-free fast path in push.
	closedFlag atomic.Bool
	// closeOnce guards the single shutdown.
	closeOnce sync.Once
	// done is closed once the drain goroutine has exited so tests and the
	// relay can wait for it deterministically.
	done chan struct{}
	// shutdown is closed by close() to wake a drain goroutine parked on an
	// empty queue. It is deliberately NOT consulted when delivering a
	// chunk: it is closed at teardown, so selecting on it there would pick
	// at random between delivering and discarding an already-recorded
	// chunk.
	shutdown chan struct{}
}

// newTee builds a tee. The drain goroutine does not run until [tee.start]
// is called with the consumer's close signal — the FakeConn that consumes
// readCh cannot be constructed until this tee exists, so binding the two
// is necessarily a second step.
func newTee(dir fakeconn.Direction, capBytes int64, chanBuf int, stallGrace time.Duration, memCheck func() bool, onDrop func(reason string), logger *zap.Logger) *tee {
	return &tee{
		dir:        dir,
		logger:     logger,
		cap:        capBytes,
		stallGrace: stallGrace,
		memCheck:   memCheck,
		onDrop:     onDrop,
		out:        make(chan fakeconn.Chunk, chanBuf),
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		shutdown:   make(chan struct{}),
	}
}

// start launches the drain goroutine. consumerGone must be the close
// signal of the FakeConn reading [tee.readCh] (see fakeconn.FakeConn.Done);
// it is the only thing that permits drain to abandon a queued chunk, so a
// nil value means "deliver unconditionally" and a stopped consumer would
// then park the drain goroutine until close. Call exactly once.
func (t *tee) start(consumerGone <-chan struct{}) {
	t.consumerGone = consumerGone
	go t.drain()
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

// push queues a chunk. Returns true on success, false on drop. A drop
// invokes onDrop with the reason string and does not alter the byte
// counter.
//
// push never blocks. It takes mu only to append, and mu is never held
// across a send on out, so a parser that has stopped reading cannot stall
// the forwarder — it can only fill the queue until the per-connection cap
// refuses further chunks.
//
// Once the tee has been closed push silently returns false without
// invoking onDrop: the mock is already abandoned at that point, so
// additional "drop" notifications would just add noise. The same applies
// after [tee.abandon] has given up on the connection — it reports the loss
// once, with the total, and marks the tee closed so later pushes are
// refused rather than accepted into a queue no drain is left to serve.
// Returning false (not true) is what matters there: the caller uses it to
// decide whether the chunk was teed, and a false "yes" would both lose the
// chunk silently and keep re-arming the supervisor's pending-work watchdog.
func (t *tee) push(c fakeconn.Chunk) bool {
	if t.closedFlag.Load() {
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

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false
	}
	if t.qBytes+n > t.cap {
		t.mu.Unlock()
		t.drop(DropPerConnCap)
		return false
	}
	t.q = append(t.q, c)
	t.qBytes += n
	t.mu.Unlock()

	// Doorbell. Non-blocking: a pending token already tells drain there is
	// work, and drain re-checks the queue under mu, so coalescing is safe.
	select {
	case t.wake <- struct{}{}:
	default:
	}
	return true
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

// drain moves chunks from the queue to the FakeConn-facing channel in FIFO
// order, then closes out and signals done.
//
// Delivery is a blocking send. That is the point: a parser that is merely
// slow — the boot burst this path exists for, or a node under load — must
// not cost a recorded chunk, and no deadline can tell "slow" apart from
// "gone".
//
// There are exactly two escapes, and they differ by phase:
//
//   - consumerGone, always. It fires when the parser closes its FakeConn,
//     i.e. when it has genuinely stopped reading, so delivery is pointless.
//     This is the precise signal and the only one available mid-connection.
//   - the stall timer, ONLY after close(). Mid-connection there is no bound
//     at all: the parser is alive, the relay keeps forwarding real bytes
//     regardless, and backpressure is already expressed by the byte cap.
//     Bounding a live connection would throw its recording away and close
//     out under a reader that is still going, handing it a premature EOF.
//     After close() something must terminate the drain, because relay
//     teardown waits on waitDone — hence the bound, and hence its scope.
//
// Either escape is a verdict about the CONNECTION, not the chunk in hand, so
// both go through abandon: it drops the whole remaining queue, reports the
// loss once with the total, and marks the tee closed so no later push is
// accepted into a queue that no drain is left to serve.
func (t *tee) drain() {
	defer close(t.done)
	defer close(t.out)
	for {
		t.mu.Lock()
		if len(t.q) == 0 {
			if t.closed {
				t.mu.Unlock()
				return
			}
			t.mu.Unlock()
			select {
			case <-t.wake:
			case <-t.shutdown:
			}
			continue
		}
		c := t.q[0]
		t.q[0] = fakeconn.Chunk{} // release the reference for GC
		t.q = t.q[1:]
		t.qBytes -= int64(len(c.Bytes))
		if len(t.q) == 0 {
			// Drop the backing array so a connection that burst once does
			// not pin its high-water-mark allocation for its whole life.
			t.q = nil
		}
		t.mu.Unlock()

		// Fast path: deliver without blocking whenever out has room. This
		// is the overwhelmingly common case, and doing it first means the
		// escapes below are only ever reached when out is genuinely full —
		// so a chunk we could have delivered is never abandoned in a race
		// against a teardown signal.
		select {
		case t.out <- c:
			continue
		default:
		}

		// out is full, so the consumer is behind. Wait for it.
		//
		// The bound here is a STALL bound, not a flush budget: the timer is
		// per chunk, and the send completes the moment the consumer frees a
		// single slot. So a consumer that is merely slow — the boot burst
		// this path exists for, or a loaded node — always gets every chunk
		// no matter how long the connection's teardown takes in total; only
		// one that frees NO slot for the whole window is abandoned. Bounding
		// the whole flush instead, as a single deadline across all remaining
		// chunks would, is what makes a timeout fire under stress and drop
		// exactly the data this path exists to keep.
		//
		// consumerGone is the precise signal and is preferred whenever it is
		// available: a parser that closed its FakeConn is released instantly
		// rather than after a stall window. The timer only covers the case
		// consumerGone cannot — a parser that stopped reading WITHOUT
		// closing, where the relay is already blocked in waitDone and will
		// not reach its own stream Close. Without it that is a deadlock.
		// Mid-connection, blocking here is the CORRECT answer and the only
		// safe one. The parser is alive, just behind; the relay keeps
		// forwarding real bytes regardless, and backpressure is already
		// expressed by the byte cap — push keeps queueing until PerConnCap
		// and then refuses with DropPerConnCap. Abandoning here instead would
		// throw away a live connection's recording AND close out underneath a
		// parser that is still reading, handing it a premature io.EOF.
		//
		// So the stall bound applies ONLY once close() has been called, where
		// something must eventually terminate the drain or relay teardown
		// (which waits on waitDone) would hang.
	deliver:
		for {
			var stallT *time.Timer
			var stallC <-chan time.Time
			var closing <-chan struct{}
			if t.closedFlag.Load() {
				stallT = time.NewTimer(t.stallGrace)
				stallC = stallT.C
			} else {
				// Still open, so no bound — but a drain parked here must still
				// learn that close() happened, or it would block forever and
				// hang the relay teardown that waits on waitDone.
				closing = t.shutdown
			}

			select {
			case t.out <- c:
				if stallT != nil {
					stallT.Stop()
				}
				break deliver

			case <-t.consumerGone:
				// The parser closed its FakeConn: nothing will read this, and
				// the verdict covers the whole connection, not just the chunk
				// in hand.
				if stallT != nil {
					stallT.Stop()
				}
				t.abandon(c)
				return

			case <-stallC:
				// Frozen with out full for a whole window after close().
				// Conclude once and act on it — re-deciding per chunk would pay
				// the window N times over and turn teardown into minutes, which
				// is what made bounding the whole flush tempting in the first
				// place. Deciding once gets both: a progressing consumer never
				// trips the timer, a stopped one costs one window total.
				t.abandon(c)
				return

			case <-closing:
				// close() fired while we were parked. Loop once to re-arm with
				// the stall bound now that one is required. At most two
				// iterations (open -> closed), so this cannot spin.
			}
		}
	}
}

// abandon gives up on a chunk that could not be delivered plus everything
// still queued behind it, and reports the loss.
//
// Loss must be visible: the kernel's BPF ring buffer exports a lost-sample
// count for exactly this reason, and a capture pipeline that discards
// silently is indistinguishable from one that recorded nothing. So this
// reports through onDrop — once, with the total — rather than only bumping
// an internal counter.
func (t *tee) abandon(held fakeconn.Chunk) {
	_ = held // counted below along with the queue; named for the reader

	t.mu.Lock()
	lost := len(t.q) + 1
	// Dropping the slice releases the whole backing array; zeroing each entry
	// first would only add an O(n) pass under the same mutex push contends on.
	t.q = nil
	t.qBytes = 0
	// Mark the tee dead. drain is about to return, so from here on NOTHING
	// will deliver a queued chunk — a push that kept succeeding would append
	// to a queue with no consumer and report success for a chunk that is
	// already lost, which is worse than the leak this whole path fixes. It
	// would also keep arming the supervisor's pending-work watchdog, whose
	// eventual hang verdict abandons the connection's recording wholesale.
	t.closed = true
	t.mu.Unlock()
	t.closedFlag.Store(true)

	t.drops.Add(uint64(lost))
	if t.onDrop != nil {
		t.onDrop(DropConsumerGone)
	}
	if t.logger != nil {
		t.logger.Debug("relay: tee abandoned undelivered chunks",
			zap.String("dir", t.dir.String()),
			zap.Int("chunks", lost),
		)
	}
}

// close stops the tee. After close returns push is a no-op (returns
// false) and the drain goroutine finishes once the queue is empty.
// close is idempotent.
func (t *tee) close() {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		t.closedFlag.Store(true)
		// Wake a drain parked on an empty queue so it can observe closed.
		close(t.shutdown)
	})
}

// waitDone blocks until the drain goroutine has exited. Used by the relay
// teardown and by tests.
func (t *tee) waitDone() { <-t.done }
