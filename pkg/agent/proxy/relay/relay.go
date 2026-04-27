package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// Relay forwards bytes between a real client and real destination
// net.Conn, teeing timestamped copies to parser-facing FakeConns and
// honouring control directives from the parser.
//
// Construct with [New]; start with [Run]. Relay does not own the
// lifecycle of the real sockets: callers close src/dst at connection
// end. Run returns when both forwarders have stopped (either EOF,
// an error, or ctx cancellation).
//
// A Relay is single-shot — allocate a new one per accepted connection.
type Relay struct {
	cfg Config

	// src is the real client-side socket. Held behind an atomic
	// pointer so the directive processor can swap it for a TLS-
	// wrapped version without synchronising on the forwarder's hot
	// path. The forwarder reloads on every iteration.
	src atomic.Pointer[net.Conn]
	// dst is the real destination-side socket; same treatment as src.
	dst atomic.Pointer[net.Conn]

	// teeC2D handles chunks flowing client → destination (Dir=FromClient).
	teeC2D *tee
	// teeD2C handles chunks flowing destination → client (Dir=FromDest).
	teeD2C *tee

	// clientStream is the FakeConn parsers read to consume
	// client-produced bytes.
	clientStream *fakeconn.FakeConn
	// destStream is the FakeConn parsers read to consume
	// destination-produced bytes.
	destStream *fakeconn.FakeConn

	// directives is the parser-writable directive channel. The
	// processor goroutine reads from here.
	directives chan directive.Directive
	// acks is the parser-readable ack channel.
	acks chan directive.Ack

	// pauseMu protects pauseCh. pauseCh is non-nil while forwarders
	// are paused (directive processor mid-TLS-upgrade). Forwarders
	// snapshot pauseCh at the top of each loop iteration and, if
	// non-nil, block on it until it is closed.
	//
	// parkedCh is a per-pause-window channel each forwarder closes
	// (atomically via parkedC2D/parkedD2C) the first time it observes
	// the pause barrier and parks on pauseCh. The directive handler
	// uses these to wait for both forwarders to actually be parked
	// before it claims any stashed bytes — without that wait there is
	// a TOCTOU race where the directive handler can call takeStashed
	// before the forwarder's deadline-driven Read returned (which is
	// what the forwarder needs to do before it can stash anything),
	// see the empty stash, fall through to readFullPreamble on the
	// live socket, and then deadlock because the byte was already
	// consumed by the very Read that woke the forwarder out of its
	// pre-pause block.
	pauseMu   sync.Mutex
	pauseCh   chan struct{}
	parkedC2D chan struct{}
	parkedD2C chan struct{}

	// stashedC2D and stashedD2C hold bytes a forwarder Read returned
	// AFTER the pause barrier was raised but BEFORE the forwarder
	// could check it. The forwarder transfers in-flight bytes here
	// (rather than writing them to the live socket and corrupting
	// the upstream wire across the upgrade boundary) when it
	// observes the pause set on its post-Read recheck. The directive
	// handler can claim them via takeStashed; on resume, anything
	// still stashed is silently discarded — the forwarder no longer
	// owns those bytes and the upgraded socket has no way to consume
	// them sensibly.
	stashMu    sync.Mutex
	stashedC2D []byte
	stashedD2C []byte

	// seqC2D and seqD2C are the per-direction monotonic Chunk sequence
	// numbers, scoped to this connection.
	seqC2D atomic.Uint64
	seqD2C atomic.Uint64

	// runOnce ensures Run's one-time startup path executes exactly once.
	runOnce sync.Once
	// runErr stores the error returned from the first Run. Relay is
	// single-shot; subsequent Run calls return this cached error via
	// ErrRelayAlreadyRun.
	runErr atomic.Pointer[error]
}

// ErrRelayAlreadyRun is returned from a second [Relay.Run] call.
var ErrRelayAlreadyRun = errors.New("relay: Run called more than once")

// ErrNoTLSUpgrader is wrapped in the Ack.Err when a KindUpgradeTLS
// directive arrives but no [Config.TLSUpgradeFn] is configured.
var ErrNoTLSUpgrader = errors.New("relay: no TLSUpgradeFn configured for KindUpgradeTLS directive")

// New returns a Relay bound to the given real sockets. Ownership of
// src and dst is shared with the relay for the duration of Run: the
// relay Reads and Writes them but does NOT Close them. Callers close
// on their own schedule.
//
// src is the real CLIENT-side socket (the incoming TCP connection
// from the user's app); dst is the real DESTINATION-side socket (the
// outbound TCP connection keploy opened to the upstream service).
// The direction convention follows [fakeconn.Direction]:
//
//	bytes read from src  → Chunk{Dir: FromClient} → written to dst
//	bytes read from dst  → Chunk{Dir: FromDest}   → written to src
func New(cfg Config, src, dst net.Conn) *Relay {
	cfg = cfg.withDefaults()

	r := &Relay{
		cfg:        cfg,
		directives: make(chan directive.Directive, 8),
		acks:       make(chan directive.Ack, 8),
	}
	r.src.Store(&src)
	r.dst.Store(&dst)

	r.teeC2D = newTee(
		fakeconn.FromClient,
		cfg.PerConnCap,
		cfg.TeeChanBuf,
		cfg.MemoryGuardCheck,
		cfg.OnMarkMockIncomplete,
		cfg.Logger,
	)
	r.teeD2C = newTee(
		fakeconn.FromDest,
		cfg.PerConnCap,
		cfg.TeeChanBuf,
		cfg.MemoryGuardCheck,
		cfg.OnMarkMockIncomplete,
		cfg.Logger,
	)

	var localAddr, remoteAddr net.Addr
	if src != nil {
		localAddr = src.LocalAddr()
		remoteAddr = src.RemoteAddr()
	}
	r.clientStream = fakeconn.New(r.teeC2D.readCh(), localAddr, remoteAddr)
	var destLocal, destRemote net.Addr
	if dst != nil {
		destLocal = dst.LocalAddr()
		destRemote = dst.RemoteAddr()
	}
	r.destStream = fakeconn.New(r.teeD2C.readCh(), destLocal, destRemote)

	return r
}

// ClientStream returns the parser-facing FakeConn populated with
// chunks read from the real client (bytes flowing client → dest).
// Safe to call before or after Run starts.
func (r *Relay) ClientStream() *fakeconn.FakeConn { return r.clientStream }

// DestStream returns the parser-facing FakeConn populated with chunks
// read from the real destination (bytes flowing dest → client). Safe
// to call before or after Run starts.
func (r *Relay) DestStream() *fakeconn.FakeConn { return r.destStream }

// Directives returns the parser-writable directive channel. Parsers
// send [directive.Directive] values here; the relay's directive
// processor goroutine handles them.
func (r *Relay) Directives() chan<- directive.Directive { return r.directives }

// Acks returns the parser-readable ack channel. Parsers drain this in
// response to directives they sent.
func (r *Relay) Acks() <-chan directive.Ack { return r.acks }

// DropCounts returns the (client→dest, dest→client) tee drop counts.
// Provided for diagnostics; tests use this to assert that a directive
// or memory-guard pressure actually caused drops.
func (r *Relay) DropCounts() (c2d, d2c uint64) {
	return r.teeC2D.dropCount(), r.teeD2C.dropCount()
}

// PauseTees suspends further chunk delivery into the parser-facing
// FakeConns without stopping the forwarders — every incoming byte
// still reaches its peer on the real sockets. Used by the supervisor
// abort path: once the parser is dead (panic / hang / mem-cap), we
// don't want the tees to spend capacity (or spam DropXxx debug logs)
// on a parser that will never read again. setPaused is cheap: an
// atomic-bool swap on the hot push path, and the channel buffer
// already has its chunks which a final close() will later GC.
//
// Idempotent; calling after Run has returned is a no-op because the
// tees are already closed.
func (r *Relay) PauseTees() {
	r.teeC2D.setPaused(true)
	r.teeD2C.setPaused(true)
}

// Run starts the forwarder, drain, and directive-processor goroutines
// and blocks until both forwarders exit. Exits happen on:
//
//   - EOF or any read error from either real socket,
//   - ctx cancellation (which interrupts in-flight reads via
//     SetReadDeadline on the source socket),
//   - an error in the opposite-direction forwarder that propagates
//     by closing the shared stopping signal.
//
// Run returns the first non-EOF error observed, or nil on clean close.
// Run does NOT close the real sockets; the caller is responsible.
//
// Run is single-shot; calling twice returns [ErrRelayAlreadyRun].
func (r *Relay) Run(ctx context.Context) error {
	var started bool
	r.runOnce.Do(func() {
		started = true
		err := r.run(ctx)
		r.runErr.Store(&err)
	})
	if !started {
		return ErrRelayAlreadyRun
	}
	e := r.runErr.Load()
	if e == nil {
		return nil
	}
	return *e
}

func (r *Relay) run(ctx context.Context) error {
	// Two waitgroups so we can wait on forwarders first (to know
	// it is safe to close tees) and only then on the directive
	// processor. The processor is woken up by closing stopping.
	var wgForward sync.WaitGroup
	var wgDirective sync.WaitGroup
	stopping := make(chan struct{})

	// Interlock ctx-cancel with in-flight Read calls on the real
	// sockets by nudging their read deadlines into the past. This
	// is best effort — net.Conn implementations that don't honour
	// deadlines simply won't unblock until the caller closes the
	// conn after Run returns. net.Pipe, *net.TCPConn, and *tls.Conn
	// all support it.
	cancelNudge := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			r.nudgeDeadline(r.src.Load())
			r.nudgeDeadline(r.dst.Load())
		case <-cancelNudge:
		}
	}()
	defer close(cancelNudge)

	var firstErrMu sync.Mutex
	var firstErr error
	recordErr := func(e error) {
		if e == nil || errors.Is(e, io.EOF) || isBenignNetErr(e) {
			return
		}
		firstErrMu.Lock()
		defer firstErrMu.Unlock()
		if firstErr == nil {
			firstErr = e
		}
	}

	// Directive processor.
	wgDirective.Add(1)
	go func() {
		defer wgDirective.Done()
		r.processDirectives(ctx, stopping)
	}()

	// Forwarder src → dst (Dir=FromClient).
	wgForward.Add(1)
	go func() {
		defer wgForward.Done()
		recordErr(r.forward(ctx, stopping, fakeconn.FromClient, &r.src, &r.dst, r.teeC2D, &r.seqC2D))
	}()

	// Forwarder dst → src (Dir=FromDest).
	wgForward.Add(1)
	go func() {
		defer wgForward.Done()
		recordErr(r.forward(ctx, stopping, fakeconn.FromDest, &r.dst, &r.src, r.teeD2C, &r.seqD2C))
	}()

	// Block until both forwarders exit.
	wgForward.Wait()

	// Now it is safe to stop the tees: no more push() calls will
	// fire. Close staging channels and wait for drain goroutines to
	// flush what they already had buffered.
	r.teeC2D.close()
	r.teeD2C.close()
	r.teeC2D.waitDone()
	r.teeD2C.waitDone()

	// Close the FakeConns so any blocked parser Read returns ErrClosed.
	_ = r.clientStream.Close()
	_ = r.destStream.Close()

	// Signal the directive processor to exit and wait for it.
	close(stopping)
	wgDirective.Wait()

	// After the processor has returned, no one else can send on acks:
	// close it so parsers blocked on <-Acks get the zero value and
	// can return.
	close(r.acks)

	return firstErr
}

// forward is one forwarder goroutine. src is the read side, dst is
// the write side, t is the tee to push chunks into, seq is the
// per-direction sequence counter.
//
// Each iteration:
//  1. Check for pause; block on pauseCh if set.
//  2. Read up to ForwardBuf bytes from src. Stamp readAt = time.Now().
//  3. Write to dst; stamp writtenAt = time.Now() after Write returns.
//  4. Build Chunk and push into the tee (non-blocking).
//  5. Bump activity.
//
// Returns the first read or write error encountered. io.EOF is
// returned verbatim; callers filter it out in their error accounting.
func (r *Relay) forward(
	ctx context.Context,
	stopping <-chan struct{},
	dir fakeconn.Direction,
	srcPtr *atomic.Pointer[net.Conn],
	dstPtr *atomic.Pointer[net.Conn],
	t *tee,
	seq *atomic.Uint64,
) error {
	bufSize := r.cfg.ForwardBuf
	buf := make([]byte, bufSize)
	log := r.cfg.Logger

	for {
		// Early exit if the outer run is tearing down. We check both
		// ctx and stopping; ctx.Done fires on external cancel, stopping
		// fires after run's cleanup.
		select {
		case <-ctx.Done():
			return nil
		case <-stopping:
			return nil
		default:
		}

		// Respect the pause barrier if the directive processor is
		// mid-TLS-upgrade. The barrier is a chan that closes when
		// resumed.
		if pc := r.currentPauseCh(); pc != nil {
			r.markForwarderParked(dir)
			select {
			case <-pc:
			case <-ctx.Done():
				return nil
			case <-stopping:
				return nil
			}
		}

		src := *srcPtr.Load()
		n, err := src.Read(buf)
		readAt := time.Now()

		// Re-check the pause barrier AFTER Read returns and BEFORE
		// any Write/Tee. Without this, a directive issued while the
		// forwarder was blocked in Read can land at the top of the
		// next loop iteration too late: the bytes already read here
		// would be Written to the live cleartext peer first. For the
		// Postgres SSL choreography that means a TLS ClientHello
		// from the real client gets forwarded to the real server
		// AS IF IT WERE PLAINTEXT, corrupting the upstream TCP
		// stream — which the directive handler then fights with by
		// trying to start its own TLS handshake on the same socket.
		//
		// The pre-pause Read here may have captured bytes the
		// directive handler still wants — the Postgres SSLResponse
		// 'S' byte being the canonical example. Stash them onto the
		// relay so the directive handler can claim them via
		// r.takeStashed(dir); whatever it leaves behind is dropped
		// when the pause lifts (in r.endPause). The forwarder does
		// NOT write these bytes to the live destination because the
		// upgraded socket has no protocol context to consume them
		// (see the upstream-corruption analysis above).
		if pc := r.currentPauseCh(); pc != nil {
			if n > 0 {
				stash := make([]byte, n)
				copy(stash, buf[:n])
				r.stashInflightFromPause(dir, stash)
				if log != nil {
					log.Debug("relay: stashing in-flight bytes across pause boundary",
						zap.String("dir", dir.String()),
						zap.Int("stashed_bytes", n),
					)
				}
				n = 0
			}
			// Now that the post-Read stash is committed (or there
			// were no in-flight bytes to stash), signal the directive
			// handler that this forwarder is parked. This MUST happen
			// after stashInflightFromPause so the directive handler's
			// takeStashed is guaranteed to observe the stash; before
			// adding markForwarderParked here, the handler could race
			// past an empty stash and fall into readFullPreamble,
			// blocking forever on bytes that the forwarder later
			// stashed (and that the upgraded socket will never re-
			// deliver).
			r.markForwarderParked(dir)
			select {
			case <-pc:
			case <-ctx.Done():
				return nil
			case <-stopping:
				return nil
			}
			// Pause has lifted. err is preserved so a closed-conn
			// condition still tears the forwarder down on the next
			// loop trip.
		}

		if n > 0 {
			// Copy into an owned slice so the forwarder can reuse
			// the scratch buffer on the next iteration without the
			// parser observing a torn read. This copy is unavoidable
			// given the chunk has to outlive the Read buffer.
			payload := make([]byte, n)
			copy(payload, buf[:n])

			dst := *dstPtr.Load()
			wn, werr := dst.Write(payload)
			writtenAt := time.Now()

			// Tee regardless of Write outcome: the bytes were
			// observed, the parser gets to see them. If Write failed
			// the mock is still incomplete because the real peer did
			// not receive them, so flag it.
			chunk := fakeconn.Chunk{
				Dir:       dir,
				Bytes:     payload,
				ReadAt:    readAt,
				WrittenAt: writtenAt,
				SeqNo:     seq.Add(1),
			}
			teed := t.push(chunk)
			// On a successful client→dest tee, signal the supervisor
			// that a request chunk is in flight and awaiting a mock
			// emission. This is the "pending work" signal the hang
			// watchdog needs to distinguish an idle connection (no
			// pending requests) from a parser that received bytes
			// but isn't making progress. Drops on the tee don't
			// trigger the signal — they already mark the mock
			// incomplete via OnMarkMockIncomplete.
			if teed && dir == fakeconn.FromClient && r.cfg.OnClientChunkTeed != nil {
				r.cfg.OnClientChunkTeed()
			}

			if werr != nil {
				// The write failure means the opposite peer has
				// gone. Flag and return: the other forwarder will
				// see EOF/error too.
				if log != nil {
					log.Debug("relay: forward write error",
						zap.String("dir", dir.String()),
						zap.Int("read_bytes", n),
						zap.Int("written_bytes", wn),
						zap.Error(werr),
					)
				}
				if r.cfg.OnMarkMockIncomplete != nil {
					r.cfg.OnMarkMockIncomplete("write_error")
				}
				return werr
			}
			if wn != n {
				// Short write on a blocking Write is a net.Conn
				// contract violation, but handle it anyway.
				short := errors.New("relay: short write on destination socket")
				if r.cfg.OnMarkMockIncomplete != nil {
					r.cfg.OnMarkMockIncomplete("short_write")
				}
				return short
			}

			r.cfg.BumpActivity()
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.EOF
			}
			// A SetReadDeadline-driven timeout fired by beginPause
			// (and cleared by endPause once the pause lifts) is the
			// expected wake-up path that lets a forwarder blocked
			// in Read observe a freshly-installed pause barrier.
			// Treat it as "loop, re-check pause, then read again"
			// rather than a terminal error: returning here would
			// take the relay down at the same moment the upgrade
			// completed, defeating the point of the pause. The
			// re-check at the top of the loop will block on the
			// pause channel; once endPause clears the deadline and
			// closes the channel, Read returns to its normal
			// blocking semantics.
			//
			// This branch is also tolerant of zero-byte timeouts —
			// some net.Conn implementations return n=0 with a
			// timeout-flagged error and the caller is expected to
			// re-arm. Continuing the loop is the right thing in
			// either case.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
	}
}

// currentPauseCh snapshots the current pause channel under the mutex.
// Returns nil if no pause is active. The forwarder uses the result to
// decide whether to block.
func (r *Relay) currentPauseCh() chan struct{} {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	return r.pauseCh
}

// beginPause installs a pause barrier. Subsequent [Relay.currentPauseCh]
// returns the new channel until [Relay.endPause] is called. Calling
// beginPause while a pause is already active returns the existing
// channel so concurrent directives don't clobber each other; this
// shouldn't happen in practice because the directive processor is
// single-threaded.
//
// We also nudge SetReadDeadline on both real sockets so any
// forwarder blocked in Read wakes up promptly and can observe the
// pause + (in the post-Read recheck path) stash any in-flight
// payload onto the relay. Without this kick, a forwarder blocked on
// a quiet socket would never see the pause until the next byte
// arrived from the peer — which could be a genuine application read
// the directive handler races against. The deadline is reset to
// time.Time{} (no deadline) just before endPause completes; until
// then the deadline-exceeded errors returned by Read are treated as
// "loop and re-check pause" rather than "tear down the relay".
func (r *Relay) beginPause() chan struct{} {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	if r.pauseCh == nil {
		r.pauseCh = make(chan struct{})
	}
	if r.parkedC2D == nil {
		r.parkedC2D = make(chan struct{})
	}
	if r.parkedD2C == nil {
		r.parkedD2C = make(chan struct{})
	}
	pc := r.pauseCh
	// Nudge both sockets out of any blocking Read. A past timestamp
	// fires immediately; the forwarder Read returns with a deadline
	// error which the post-Read pause recheck handles (returns to
	// the top of the loop, blocks on pc). The directive handler is
	// then the sole reader on the live socket until endPause restores
	// the no-deadline state.
	r.nudgeDeadline(r.dst.Load())
	r.nudgeDeadline(r.src.Load())
	return pc
}

// waitForwardersParked blocks until both forwarders have observed the
// active pause barrier, completed any in-flight Read, stashed any
// captured bytes, and parked on the pause channel. Returns once both
// per-direction park signals fire OR ctx cancels OR the relay is
// stopping. Used by the directive handler to close the TOCTOU race
// between beginPause and takeStashed: without this wait, the directive
// handler can claim an empty stash before the forwarder's deadline-
// driven Read has even returned, then fall through to readFullPreamble
// on the live socket and deadlock — the forwarder will then read the
// preamble byte (or part of it) and stash it, but by then the
// directive handler is committed to a Read that will never see those
// bytes.
//
// Caller must already hold the pause via [beginPause]; this only
// observes the state, it does not install one.
func (r *Relay) waitForwardersParked(ctx context.Context, stopping <-chan struct{}) {
	r.pauseMu.Lock()
	c2d := r.parkedC2D
	d2c := r.parkedD2C
	r.pauseMu.Unlock()
	if c2d != nil {
		select {
		case <-c2d:
		case <-ctx.Done():
			return
		case <-stopping:
			return
		}
	}
	if d2c != nil {
		select {
		case <-d2c:
		case <-ctx.Done():
			return
		case <-stopping:
			return
		}
	}
}

// markForwarderParked is called by a forwarder the first time it
// blocks on the pause channel within a given pause window. It closes
// the per-direction park signal so the directive handler's
// waitForwardersParked can observe both forwarders are now off the
// live sockets.
//
// Idempotent within a window: subsequent calls are no-ops because the
// forwarder's local first-park flag is reset only when endPause runs.
func (r *Relay) markForwarderParked(dir fakeconn.Direction) {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	switch dir {
	case fakeconn.FromClient:
		if r.parkedC2D != nil {
			select {
			case <-r.parkedC2D:
				// already closed
			default:
				close(r.parkedC2D)
			}
		}
	case fakeconn.FromDest:
		if r.parkedD2C != nil {
			select {
			case <-r.parkedD2C:
				// already closed
			default:
				close(r.parkedD2C)
			}
		}
	}
}

// endPause releases the pause barrier. No-op if no pause is active.
// Any bytes still in the per-direction stash buffers are dropped: the
// forwarders parked on the pause did not write them to the live
// peer, and the live peer has now (potentially) been TLS-upgraded so
// there is no way to deliver them sensibly. The directive handler is
// expected to have claimed any bytes it cared about via takeStashed
// while the pause was held.
//
// We also clear the SetReadDeadline that beginPause installed so the
// forwarders' next Read on the (possibly upgraded) sockets blocks
// normally. The clear happens BEFORE the pause channel is closed so
// the forwarder cannot observe a "pause lifted but deadline still
// past" window where its Read would return an immediate deadline
// error, hit the post-Read pause recheck (now nil), and write to the
// upgraded socket using stashed bytes that no longer apply.
func (r *Relay) endPause() {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	if r.pauseCh != nil {
		// Restore deadlines on whichever conns are now live (post-
		// upgrade these may be different *tls.Conn values from the
		// ones beginPause nudged). SetReadDeadline(time.Time{})
		// disables the deadline.
		clearDeadline(r.dst.Load())
		clearDeadline(r.src.Load())
		close(r.pauseCh)
		r.pauseCh = nil
	}
	// Reset the park signals to nil so the next pause window starts
	// with a fresh pair. We don't close any unfired park channels here
	// — they may have already been closed by markForwarderParked. If
	// they weren't (a forwarder never parked because it was already in
	// the post-stop branch), letting them be GC'd is fine; nothing
	// observes them after the pauseCh is gone.
	r.parkedC2D = nil
	r.parkedD2C = nil
	r.stashMu.Lock()
	r.stashedC2D = nil
	r.stashedD2C = nil
	r.stashMu.Unlock()
}

// clearDeadline drops any read deadline previously installed on the
// conn. Mirror of nudgeDeadline; errors are swallowed for the same
// reason (not every conn honours deadlines, conn may be torn down).
func clearDeadline(c *net.Conn) {
	if c == nil || *c == nil {
		return
	}
	_ = (*c).SetReadDeadline(time.Time{})
}

// stashInflightFromPause is called by the forwarder when its post-Read
// pause recheck observed the barrier set: the bytes were read from the
// real source socket but never written to the real destination, so
// they belong to the directive handler now (it may need them as a
// protocol preamble it's about to consume — Postgres SSLResponse
// being the canonical case). The dir argument identifies which
// direction's stash to populate.
//
// Bytes are appended to any existing stash so two consecutive Read+
// pause-recheck iterations don't lose data; in practice the forwarder
// only ever reaches this path once per pause window.
func (r *Relay) stashInflightFromPause(dir fakeconn.Direction, payload []byte) {
	if len(payload) == 0 {
		return
	}
	r.stashMu.Lock()
	defer r.stashMu.Unlock()
	switch dir {
	case fakeconn.FromClient:
		r.stashedC2D = append(r.stashedC2D, payload...)
	case fakeconn.FromDest:
		r.stashedD2C = append(r.stashedD2C, payload...)
	}
}

// takeStashed returns and clears the stash for the given direction.
// Used by the directive handler under the pause barrier to claim
// bytes the forwarder captured at the moment the barrier was raised.
// Returns nil if there is no stash; takeStashed is safe to call
// before any forwarder Read has fired.
func (r *Relay) takeStashed(dir fakeconn.Direction) []byte {
	r.stashMu.Lock()
	defer r.stashMu.Unlock()
	var out []byte
	switch dir {
	case fakeconn.FromClient:
		out = r.stashedC2D
		r.stashedC2D = nil
	case fakeconn.FromDest:
		out = r.stashedD2C
		r.stashedD2C = nil
	}
	return out
}

// nudgeDeadline sets a deadline in the past on the conn if non-nil.
// Errors are swallowed because (a) not every net.Conn honours
// deadlines, and (b) the conn may already be in teardown.
func (r *Relay) nudgeDeadline(c *net.Conn) {
	if c == nil || *c == nil {
		return
	}
	_ = (*c).SetReadDeadline(time.Unix(1, 0))
}

// isBenignNetErr returns true for errors that are expected during
// normal connection teardown and should not be surfaced as the relay's
// return value. That includes io.ErrClosedPipe, the "io: read/write
// on closed pipe" text produced by net.Pipe, the "use of closed
// network connection" text produced by the stdlib net package's
// unexported errors, and net.Error with Timeout()==true produced by
// our own ctx-cancel nudge.
func isBenignNetErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	// Timeout() == true means ctx was cancelled and we nudged the
	// read deadline; the caller is doing teardown, not reporting a
	// genuine protocol error.
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	// String fallback for errors that predate net.ErrClosed (and for
	// net.Pipe's io.ErrClosedPipe-equivalent message).
	msg := err.Error()
	if strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "io: read/write on closed pipe") {
		return true
	}
	return false
}
