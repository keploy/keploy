package relay

import (
	"context"
	"errors"
	"fmt"
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
	stashedC2D []stashedPayload
	stashedD2C []stashedPayload

	// forwarded counts chunks successfully written to a real peer. The
	// half-close grace watches it so the bound is on IDLE time rather
	// than total time: a peer that is still streaming a response must
	// never be cut off mid-body.
	forwarded atomic.Uint64

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

	// holdClient is true while a [Config.HoldClientWrites] hold is up:
	// the C2D forwarder reads and tees but does not write to the real
	// destination, parking the client's bytes in stashedC2D instead.
	// Armed by [Relay.Run] when the config asks for it, cleared by the
	// KindReleaseClient or KindUpgradeTLS handler (or by the cap
	// breach in noteHeldClientBytes).
	//
	// Separate from preDispatchActive because the two answer different
	// questions: preDispatchActive is a property of a PAUSE WINDOW and
	// dies with it, while a hold spans the parser's whole decision —
	// across the server greeting's round trip, which for a
	// server-speaks-first protocol happens before the client has said
	// anything worth deciding on.
	holdClient atomic.Bool

	// heldClientBytes counts the bytes currently held by holdClient, so
	// the forwarder can enforce [Config.ClientHoldCap] without taking
	// stashMu on its hot path.
	heldClientBytes atomic.Int64

	// holdMu serialises hold releases so a claim-then-write pair is
	// atomic with respect to any other releaser.
	//
	// It is NOT redundant with the pause barrier. waitForwardersParked
	// gives up when `stopping` closes, and `stopping` closes as soon as
	// the FIRST forwarder exits — so from the moment D2C sees EOF, a
	// directive handler proceeds while C2D is still live inside the hold
	// block. Without this lock the handler's take+write and the
	// forwarder's cap-breach take+write can interleave and reach the
	// upstream socket out of order.
	holdMu sync.Mutex

	// preDispatchActive is true between [Relay.Run]'s pre-spawn
	// [installPreDispatchPause] call (gated on
	// [Config.PreDispatchPause]) and the
	// [directive.KindResumePreDispatch] handler. While set, the
	// forward loop:
	//
	//   - SKIPS the pre-Read park (so the first Read on each direction
	//     proceeds and produces a chunk for the parser to inspect via
	//     its FakeConn), and
	//   - on the post-Read pause check, TEES the chunk in addition to
	//     stashing — exposing the bytes to the parser even though they
	//     have not yet been written to the real peer.
	//
	// The parser ends the window by sending KindResumePreDispatch
	// (drain stash to real peer, clear flag, endPause) or by sending
	// KindUpgradeTLS (the existing TLS upgrade handler consumes the
	// stash via takeStashedPrefix; it then clears the flag on its own
	// endPause path).
	preDispatchActive atomic.Bool
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

	// Armed here rather than in run(): the caller registers its abort
	// hook between New and Run, and an abort in that window must find a
	// hold it can actually release. See the note in run().
	if cfg.HoldClientWrites {
		r.holdClient.Store(true)
	}

	r.teeC2D = newTee(
		fakeconn.FromClient,
		cfg.PerConnCap,
		cfg.TeeChanBuf,
		cfg.ConsumerStallGrace,
		cfg.MemoryGuardCheck,
		cfg.OnMarkMockIncomplete,
		cfg.Logger,
	)
	r.teeD2C = newTee(
		fakeconn.FromDest,
		cfg.PerConnCap,
		cfg.TeeChanBuf,
		cfg.ConsumerStallGrace,
		cfg.MemoryGuardCheck,
		cfg.OnMarkMockIncomplete,
		cfg.Logger,
	)

	// Assigned after construction rather than threaded through newTee's
	// positional signature, which is already at seven arguments.
	r.teeC2D.onDesync = cfg.OnCaptureDesync
	r.teeD2C.onDesync = cfg.OnCaptureDesync

	// Whether a desynced tee keeps feeding its parser. Assigned here, before
	// Run spawns any forwarder, so push can read it without synchronisation.
	// See [Config.ParserCanResyncAfterGap] for why the default is "cannot".
	r.teeC2D.parserCanResync = cfg.ParserCanResyncAfterGap
	r.teeD2C.parserCanResync = cfg.ParserCanResyncAfterGap

	var localAddr, remoteAddr net.Addr
	if src != nil {
		localAddr = src.LocalAddr()
		remoteAddr = src.RemoteAddr()
	}
	r.clientStream = fakeconn.New(r.teeC2D.readCh(), localAddr, remoteAddr)
	// The tee may only abandon a queued chunk once this stream is closed,
	// so its drain cannot start until the stream exists.
	r.teeC2D.start(r.clientStream.Done())
	var destLocal, destRemote net.Addr
	if dst != nil {
		destLocal = dst.LocalAddr()
		destRemote = dst.RemoteAddr()
	}
	r.destStream = fakeconn.New(r.teeD2C.readCh(), destLocal, destRemote)
	r.teeD2C.start(r.destStream.Done())

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
//
// This also ENDS any client write hold, flushing what it held. The
// coupling is not a convenience: this function's own contract is that
// every incoming byte still reaches its peer, and a hold that outlives
// its parser falsifies exactly that — the client direction stays
// blackholed for the rest of the connection. "The parser is dead" and
// "the parser can still decide what to do with the bytes we are holding
// for it" cannot both be true, so the release belongs here rather than
// at the call site, where forgetting it is a silent user-traffic
// outage rather than a compile error.
func (r *Relay) PauseTees() {
	r.teeC2D.setPaused(true)
	r.teeD2C.setPaused(true)
	if err := r.releaseClientHold(); err != nil && r.cfg.Logger != nil {
		r.cfg.Logger.Debug("relay: flushing the client hold while pausing tees failed",
			zap.Error(err),
		)
	}
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

	// Pre-dispatch pause: install the pause barrier BEFORE spawning
	// forwarder goroutines so the very first byte each direction
	// produces lands in the post-Read pause path (where the
	// preDispatchActive flag below makes the forwarder tee AND stash
	// instead of writing). This closes the postgres SSL preamble
	// race (keploy/enterprise#2012) — without it, by the time the
	// parser sees SSLRequest on its FakeConn the server may already
	// have replied with 'S' and the client may already have written
	// TLS bytes that the C2D forwarder forwarded as plaintext.
	//
	// installPreDispatchPause is the no-deadline-kick variant of
	// beginPause: there are no in-flight Reads yet (forwarders are
	// not running), so the deadline kick beginPause uses to wake a
	// blocked Read would just leave the read deadlines stuck in the
	// past — making the forwarder Read return EAGAIN forever in a
	// tight loop until endPause clears it. The pause channel and the
	// parker tracking are set up identically.
	if r.cfg.PreDispatchPause {
		r.installPreDispatchPause()
		r.preDispatchActive.Store(true)
	}

	// Client write hold: armed before the forwarders start so it covers
	// the connection from its first byte. Unlike the pre-dispatch pause
	// this installs no pause barrier — the forwarders must keep reading
	// (the parser needs to SEE the client's bytes to decide what to do
	// about them) and the destination→client direction must keep
	// flowing (a server-speaks-first protocol greets before the client
	// says anything). Only the C2D Write is deferred. See
	// [Config.HoldClientWrites].
	// The hold itself is armed in New(), NOT here. Arming inside this
	// goroutine loses a race with the caller: proxy_v2.go does
	// relay.New, then `go r.Run(...)`, then registers SessionOnAbort. An
	// abort firing before this goroutine got scheduled found holdClient
	// still false, so PauseTees released nothing — and then Run armed a
	// hold with no releaser left. On the FallthroughToPassthrough path
	// the relay deliberately keeps running, so the client direction
	// stayed blackholed until peer close: exactly the silent
	// user-traffic outage PauseTees exists to prevent.
	//
	// withDefaults has already refused any HoldClientWrites +
	// PreDispatchPause combination, so the block above cannot have
	// installed a pause barrier that this hold would strand.

	// When one forwarder exits, signal the other to exit too via the
	// existing stopping/nudgeDeadline machinery. Without this signalling,
	// if e.g. the upstream sends FIN → FromDest reads EOF and exits, the
	// FromClient forwarder is still parked in src.Read(client) with no
	// event to wake it; wgForward.Wait() blocks until the application's
	// HTTP client times out on its own (~60s for botocore default
	// read_timeout). The recipe used here mirrors what cancelNudge does
	// for ctx.Done(): close stopping (idempotent via sync.Once so the
	// late close below stays safe) AND nudge the peer's read deadline so
	// its in-flight Read returns Timeout(); the forwarder's existing
	// timeout-loop branch re-checks the top-of-loop select, observes
	// stopping closed, and exits cleanly. Crucially, the relay still
	// does NOT close the real sockets — that ownership stays with
	// proxy.go::handleConnection per the contract in doc.go.
	var stopOnce sync.Once
	closeStopping := func() { stopOnce.Do(func() { close(stopping) }) }

	// bothDone lets a half-close grace timer stop as soon as the other
	// forwarder has returned, instead of always running to term.
	bothDone := make(chan struct{})
	var graceWG sync.WaitGroup

	// onForwarderExit decides what a finished forwarder does to its
	// peer, and it is where TCP half-close is honoured.
	//
	// A clean EOF means "this side has finished WRITING", not "this
	// connection is over". A client that does shutdown(SHUT_WR) —
	// Node's socket.end(data), Python's sock.shutdown(SHUT_WR), any
	// request/EOF/response protocol — then waits for the reply. Tearing
	// the other direction down here discards that reply, and the
	// application sees its connection close before the answer arrives.
	// That is a silent, protocol-level data loss for every such app.
	//
	// So on EOF we forward the FIN (CloseWrite on the conn this
	// direction was writing to) and let the opposite direction keep
	// copying. The peer learns the request is complete, answers, and
	// closes; the other forwarder then reads its own EOF and the
	// connection winds down naturally.
	//
	// The grace timer is not optional. If the peer answers neither with
	// data nor with a FIN, the surviving forwarder stays parked in Read
	// forever — which is the ~60s hang the unconditional teardown was
	// added to prevent in the first place. Bounding the wait keeps that
	// protection while giving a well-behaved peer time to reply.
	//
	// Anything that is NOT a clean EOF (a read error, a write error, a
	// dead peer) keeps the original immediate teardown: there is no
	// half-open state to preserve when the connection is already broken.
	onForwarderExit := func(err error, writeSide, nudgeSide *atomic.Pointer[net.Conn]) {
		if r.cfg.HalfCloseGrace < 0 || !errors.Is(err, io.EOF) || !halfCloseWrite(writeSide.Load()) {
			closeStopping()
			r.nudgeDeadline(nudgeSide.Load())
			return
		}
		if log := r.cfg.Logger; log != nil {
			log.Debug("relay: peer half-closed; forwarding FIN and draining the opposite direction",
				zap.Duration("grace", r.cfg.HalfCloseGrace),
			)
		}
		graceWG.Add(1)
		go func() {
			defer graceWG.Done()
			r.awaitHalfCloseIdle(ctx, stopping, bothDone)
			closeStopping()
			r.nudgeDeadline(nudgeSide.Load())
		}()
	}

	// Forwarder src → dst (Dir=FromClient).
	wgForward.Add(1)
	go func() {
		defer wgForward.Done()
		err := r.forward(ctx, stopping, fakeconn.FromClient, &r.src, &r.dst, r.teeC2D, &r.seqC2D)
		recordErr(err)
		onForwarderExit(err, &r.dst, &r.dst)
	}()

	// Forwarder dst → src (Dir=FromDest).
	wgForward.Add(1)
	go func() {
		defer wgForward.Done()
		err := r.forward(ctx, stopping, fakeconn.FromDest, &r.dst, &r.src, r.teeD2C, &r.seqD2C)
		recordErr(err)
		onForwarderExit(err, &r.src, &r.src)
	}()

	// Block until both forwarders exit.
	wgForward.Wait()
	close(bothDone)
	graceWG.Wait()

	// Both forwarders are gone, so nothing can add to the hold stash
	// any more. Anything still in it was read from the client and never
	// delivered; flush it so "the relay forwards every byte it read"
	// holds even when the parser never ended the hold. Errors are
	// expected here (the peer is usually already gone) and are logged
	// rather than surfaced — the connection is ending either way.
	if err := r.releaseClientHold(); err != nil && r.cfg.Logger != nil {
		r.cfg.Logger.Debug("relay: flushing the client hold at teardown failed",
			zap.Error(err),
		)
	}

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

	// Signal the directive processor to exit and wait for it. Use the
	// idempotent closer because either forwarder's defer may have
	// already closed stopping during its own exit (see closeStopping
	// above) — closing a closed channel panics.
	closeStopping()
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
		//
		// Pre-dispatch pause is the exception: the pause channel is
		// already open at first entry (installed by run() before
		// spawning forwarders), but we must NOT park here — the
		// parser needs the first Read on each direction to produce a
		// chunk it can inspect via its FakeConn. The forwarder reads,
		// hits the post-Read recheck below, and parks there with the
		// chunk both teed (so the parser sees it) and stashed (so the
		// real-peer Write is deferred). The flag is cleared by
		// KindResumePreDispatch (or by the TLS upgrade handler's
		// endPause), so subsequent iterations behave as today.
		if pc := r.currentPauseCh(); pc != nil && !r.preDispatchActive.Load() {
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
		if n > 0 {
			readAt = observedReadAt(src, readAt)
		}

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
				r.stashInflightFromPause(dir, stash, readAt)
				if log != nil {
					log.Debug("relay: stashing in-flight bytes across pause boundary",
						zap.String("dir", dir.String()),
						zap.Int("stashed_bytes", n),
						zap.Bool("pre_dispatch", r.preDispatchActive.Load()),
					)
				}
				// In pre-dispatch mode the parser needs to SEE the
				// client's first message via its FakeConn — that's
				// how it inspects e.g. postgres SSLRequest before
				// deciding between ResumePreDispatch (plain) and
				// UpgradeTLS (SSL). The TLS-upgrade handler consumes
				// the server's preamble reply (the SSLResponse byte)
				// from the stash directly via takeStashedPrefix on
				// the FromDest direction, so a D2C tee during pre-
				// dispatch would leave that byte sitting in the
				// DestStream FakeConn buffer — where the post-
				// handshake captureSessionV2 would read it AS IF it
				// were the first post-auth message, mis-parse it,
				// and corrupt the session mock. Restrict the tee to
				// FromClient so the parser sees what it asked for
				// (the client's first chunk) and nothing it didn't
				// (the server's preamble reply belongs to the
				// directive handler, not the parser).
				if r.preDispatchActive.Load() && dir == fakeconn.FromClient {
					chunk := fakeconn.Chunk{
						Dir:    dir,
						Bytes:  stash,
						ReadAt: readAt,
						SeqNo:  seq.Add(1),
					}
					teed := t.push(chunk)
					if teed && r.cfg.OnClientChunkTeed != nil {
						r.cfg.OnClientChunkTeed()
					}
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

		// Client write hold. The bytes go to the parser but not to the
		// real destination: they are teed and stashed, and the
		// destination Write waits for the parser to say what should
		// happen to them (KindReleaseClient flushes all of it,
		// KindUpgradeTLS flushes a byte-exact prefix).
		//
		// This sits AFTER the pause recheck on purpose. Once a pause is
		// up the directive handler owns the sockets and the stash, and
		// the block above has already routed the bytes there; holding
		// them a second time here would double-stash them.
		if n > 0 && dir == fakeconn.FromClient && r.holdClient.Load() {
			// holdMu, and re-check under it. Without the lock a
			// releaser can clear the flag and start writing the stash
			// to the upstream socket while this goroutine is deciding
			// to stash-or-write, and the two writes reach the server
			// out of order — observed 25/25 with a slow flush, which is
			// exactly the case an abort correlates with (a full
			// upstream socket buffer). Held chunks only occur during
			// the pre-handshake window, so this is not a hot path.
			r.holdMu.Lock()
			if !r.holdClient.Load() {
				// A release won the race; fall through and forward
				// normally, after the flush it just completed.
				r.holdMu.Unlock()
			} else {
				payload := make([]byte, n)
				copy(payload, buf[:n])

				// Stash BEFORE teeing, and the order is load-bearing.
				//
				// The parser's decision is what ends the hold, so everything
				// downstream synchronises on having SEEN a chunk — a test
				// waits for it on the FakeConn, and in production the parser
				// issues its directive the moment it has decoded enough.
				// Teeing first makes "the parser saw it" true while "it is in
				// the stash" is not yet, so a release can run against an
				// empty stash and strand the very bytes it was meant to
				// deliver. Stashing first makes the visible event the LATER
				// of the two, so anyone who observed the chunk is guaranteed
				// the stash already holds it.
				//
				// The reverse hazard does not exist: if a release claims the
				// stash between these two statements, the bytes go upstream
				// and are teed immediately after, so the parser still sees
				// every byte the connection carried.
				r.stashInflightFromPause(dir, payload, readAt)
				chunk := fakeconn.Chunk{
					Dir:    dir,
					Bytes:  payload,
					ReadAt: readAt,
					SeqNo:  seq.Add(1),
				}
				teed := t.push(chunk)
				if teed && r.cfg.OnClientChunkTeed != nil {
					r.cfg.OnClientChunkTeed()
				}
				// The connection IS carrying traffic even though nothing
				// reaches the peer yet. Without this the only bump on this
				// path is the one OnClientChunkTeed makes, so a held
				// connection whose tee is dropping chunks (per-conn cap,
				// memory pressure) stops bumping altogether and reads as
				// idle to the hang watchdog while it is anything but.
				if r.cfg.BumpActivity != nil {
					r.cfg.BumpActivity()
				}
				if log != nil {
					log.Debug("relay: holding client bytes from the destination",
						zap.Int("held_bytes", n),
						zap.Bool("teed", teed),
					)
				}
				capBreached := r.noteHeldClientBytes(int64(n))
				// Drop the lock BEFORE releasing: releaseClientHold
				// takes holdMu itself, so calling it here would
				// self-deadlock.
				r.holdMu.Unlock()

				// A hold that outgrows its cap is a parser that is not
				// coming back. Release rather than keep buffering: the
				// client is blocked on a reply it cannot get while we
				// sit on its request. See [Config.ClientHoldCap].
				if capBreached {
					if werr := r.releaseClientHold(); werr != nil {
						if log != nil {
							log.Debug("relay: client hold cap release failed",
								zap.Error(werr),
							)
						}
						return werr
					}
				}
				// Suppress the forward below; the bytes are accounted for.
				n = 0
			}
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
			r.forwarded.Add(1)
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
	pc := r.installPauseChLocked()
	r.pauseMu.Unlock()
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

// installPreDispatchPause installs the pause channel + parker
// tracking WITHOUT touching the live sockets' read deadlines. Use
// before any forwarder has started reading — the deadline-kick the
// regular beginPause does would otherwise leave the sockets in a
// permanent timeout state until endPause cleared it, starving the
// forwarders out of their pause-aware Read loop. Acquires pauseMu
// internally; callers must NOT already hold it.
func (r *Relay) installPreDispatchPause() {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	r.installPauseChLocked()
}

// installPauseChLocked is the shared body of beginPause and
// installPreDispatchPause. Returns the active pause channel and
// guarantees the per-direction park signals are armed. Caller must
// hold pauseMu.
func (r *Relay) installPauseChLocked() chan struct{} {
	if r.pauseCh == nil {
		r.pauseCh = make(chan struct{})
	}
	if r.parkedC2D == nil {
		r.parkedC2D = make(chan struct{})
	}
	if r.parkedD2C == nil {
		r.parkedD2C = make(chan struct{})
	}
	return r.pauseCh
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
	// Always clear the pre-dispatch flag when a pause window ends.
	// This covers both ResumePreDispatch (the explicit no-TLS resume
	// path, which also clears it directly before calling endPause as
	// defence-in-depth) and the TLS-upgrade path (handleUpgradeTLS
	// transitions from pre-dispatch into its own pause + upgrade and
	// then calls endPause; without this clear the flag would leak
	// past the TLS upgrade and corrupt the semantics of any future
	// pause on the connection — pre-Read park would stay skipped
	// and post-Read pauses would tee into the parser FakeConn even
	// when they shouldn't).
	r.preDispatchActive.Store(false)
}

// awaitHalfCloseIdle blocks until the surviving direction has been
// IDLE for HalfCloseGrace — or until the connection ends by itself.
//
// The bound is on idle time, not total time, and that distinction is
// the whole safety of this feature. A total bound cuts the surviving
// direction off mid-response: measured on an upstream streaming 1 KiB
// every 50ms against a 500ms bound, the client received half its body.
// Worse, it does so SILENTLY — proxy.go closes the client socket once
// Run returns, so an EOF-delimited protocol (exactly the shape that
// motivated half-close support) sees a clean EOF on a truncated body
// and believes it complete, and keploy records the truncation as a
// mock. That converts a loud failure into corrupt user traffic and a
// corrupt recording, which is far worse than the bug being fixed.
//
// Watching Relay.forwarded keeps the protection that matters — a peer
// that says nothing at all is still bounded — while letting a peer that
// is actively answering take as long as it needs.
func (r *Relay) awaitHalfCloseIdle(ctx context.Context, stopping <-chan struct{}, bothDone <-chan struct{}) {
	grace := r.cfg.HalfCloseGrace
	// Poll well inside the grace so a burst of progress cannot be
	// missed between ticks, but never so fast that a long quiet wait
	// spins.
	tick := grace / 4
	if tick < 25*time.Millisecond {
		tick = 25 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()

	last := r.forwarded.Load()
	idle := time.Duration(0)
	for {
		select {
		case <-bothDone:
			return
		case <-ctx.Done():
			return
		case <-stopping:
			return
		case <-t.C:
			if now := r.forwarded.Load(); now != last {
				last = now
				idle = 0 // the peer is answering; give it room
				continue
			}
			if idle += tick; idle >= grace {
				return
			}
		}
	}
}

// closeWriter is the half-close capability. *net.TCPConn, *tls.Conn and
// *net.UnixConn all implement it; a wrapper that does not is simply not
// half-closable and falls back to the original teardown.
type closeWriter interface {
	CloseWrite() error
}

// halfCloseWrite forwards a FIN on c — telling the peer "no more data
// from this side" while leaving the peer free to keep sending. Reports
// whether the half-close actually happened; false means the caller must
// fall back to tearing the connection down.
//
// Unwrapping matters: the relay is handed conns that may be wrapped for
// safety or accounting, and a wrapper that forwards Read/Write but not
// CloseWrite would silently cost every connection its half-close. Any
// wrapper exposing NetConn() (the convention *tls.Conn established) is
// followed to the conn underneath.
func halfCloseWrite(c *net.Conn) bool {
	if c == nil || *c == nil {
		return false
	}
	conn := *c
	for i := 0; i < 4; i++ { // bounded: wrappers nest, cycles must not hang
		if cw, ok := conn.(closeWriter); ok {
			return cw.CloseWrite() == nil
		}
		u, ok := conn.(interface{ NetConn() net.Conn })
		if !ok {
			return false
		}
		inner := u.NetConn()
		if inner == nil || inner == conn {
			return false
		}
		conn = inner
	}
	return false
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
func (r *Relay) stashInflightFromPause(dir fakeconn.Direction, payload []byte, readAt time.Time) {
	if len(payload) == 0 {
		return
	}
	stashed := stashedPayload{
		bytes:  append([]byte(nil), payload...),
		readAt: readAt,
	}
	r.stashMu.Lock()
	defer r.stashMu.Unlock()
	switch dir {
	case fakeconn.FromClient:
		r.stashedC2D = append(r.stashedC2D, stashed)
	case fakeconn.FromDest:
		r.stashedD2C = append(r.stashedD2C, stashed)
	}
}

// noteHeldClientBytes adds n to the held-byte count and reports
// whether the hold has now exceeded [Config.ClientHoldCap]. A
// non-positive cap disables the bound. Reports true at most once per
// hold: the caller releases on true, and releaseClientHold zeroes the
// counter, so a hold cannot trip the cap twice.
func (r *Relay) noteHeldClientBytes(n int64) bool {
	total := r.heldClientBytes.Add(n)
	limit := r.cfg.ClientHoldCap
	if limit <= 0 || total <= limit {
		return false
	}
	if r.cfg.Logger != nil {
		r.cfg.Logger.Warn("relay: client write hold exceeded its cap; releasing",
			zap.Int64("held_bytes", total),
			zap.Int64("cap_bytes", limit),
		)
	}
	if r.cfg.OnMarkMockIncomplete != nil {
		r.cfg.OnMarkMockIncomplete("client_hold_cap")
	}
	return true
}

// releaseClientHold ends a client write hold: everything the C2D
// forwarder held is written to the real destination in read order and
// normal forwarding resumes. No-op when no hold is up.
//
// holdMu is held across the whole claim-then-write so two releasers
// cannot interleave their writes onto the upstream socket. The hold
// flag is cleared first so that anything the forwarder reads after this
// point it forwards itself rather than adding to a stash nobody will
// drain again.
func (r *Relay) releaseClientHold() error {
	r.holdMu.Lock()
	defer r.holdMu.Unlock()

	if !r.holdClient.Swap(false) {
		return nil
	}
	r.heldClientBytes.Store(0)
	held := r.takeStashed(fakeconn.FromClient)
	if held.len() == 0 {
		return nil
	}
	dstPtr := r.dst.Load()
	if dstPtr == nil {
		return fmt.Errorf("relay: client hold release: no destination to flush %d held bytes to", held.len())
	}
	if err := writeHeldBytes(*dstPtr, held.bytes); err != nil {
		return err
	}
	return nil
}

// heldFlushWriteTimeout bounds a hold flush's Write.
//
// Without it the flush is an unbounded blocking Write on the abort path.
// [Relay.PauseTees] is called from the supervisor's SessionOnAbort, whose
// contract is explicitly that callbacks are NON-BLOCKING — they run where
// further errors have nowhere to propagate to, and a blocked one wedges
// the very teardown that was supposed to recover the connection. A held
// payload is a MySQL handshake message, hundreds of bytes against a
// socket buffer measured in tens of kilobytes, so this can only fire when
// the peer has genuinely stopped reading — a connection already lost.
const heldFlushWriteTimeout = 5 * time.Second

// writeHeldBytes writes a claimed hold payload to dst under a bounded
// deadline, clearing the deadline afterwards so the forwarders' own
// writes are not left capped.
//
// SetWriteDeadline, never SetReadDeadline: the forwarders' Reads drive
// the pause machinery and must not be disturbed by a flush.
func writeHeldBytes(dst net.Conn, payload []byte) error {
	if dst == nil {
		return errors.New("relay: no destination to flush held bytes to")
	}
	_ = dst.SetWriteDeadline(time.Now().Add(heldFlushWriteTimeout))
	defer func() { _ = dst.SetWriteDeadline(time.Time{}) }()

	wn, err := dst.Write(payload)
	if err != nil {
		return err
	}
	if wn != len(payload) {
		// A short write on a blocking Write is a net.Conn contract
		// violation, and here it means the server holds half a message.
		// Surface it rather than reporting a clean flush.
		return fmt.Errorf("relay: short write %d of %d held bytes", wn, len(payload))
	}
	return nil
}

// stashedLen reports how many bytes the given direction's stash holds,
// without claiming any of them. Callers that must validate a request
// before consuming it use this; takeStashedPrefix mutates.
func (r *Relay) stashedLen(dir fakeconn.Direction) int {
	r.stashMu.Lock()
	defer r.stashMu.Unlock()
	var parts []stashedPayload
	switch dir {
	case fakeconn.FromClient:
		parts = r.stashedC2D
	case fakeconn.FromDest:
		parts = r.stashedD2C
	}
	total := 0
	for _, p := range parts {
		total += p.len()
	}
	return total
}

// endUpgradePause ends the TLS-upgrade pause, first delivering any
// client bytes still held if the client-side handshake did NOT take
// them.
//
// heldConsumedByHandshake, not "the client side ended up behind TLS":
// the two differ on exactly one path. Under [Config.ClientTLSFirst] the
// client handshake runs FIRST and claims the held remainder, and a
// dest-side handshake that then fails would report "not upgraded" for a
// connection whose held bytes are already spent. Both halves of this
// function want the same question answered — "is the remainder still
// ours to deliver, and is a copy of it still stranded in the parser's
// stream" — so the parameter asks that one.
//
// This is the whole reason handleUpgradeTLS does not call endPause
// directly. endPause discards both stashes unconditionally, which is
// right when the handshake consumed them — the held remainder IS the
// client's ClientHello, and the client-side handshake reads it through
// a prepending conn. It is wrong on every other exit. A preamble
// mismatch (the documented "server declined TLS, record the cleartext
// path" outcome, which acks OK=true), a failed handshake, a refused
// flush: in all of those the connection stays cleartext, the server has
// already received whatever prefix we flushed, and the bytes behind it
// are the rest of a message it is still waiting for. Dropping them
// truncates the client's request on the wire and then lets passthrough
// feed the server the continuation of a message whose head is missing.
//
// So: flush first, end the pause second.
//
// The other half of the job is the parser's view of the stream, and it
// is the mirror image. When the client side DID end up behind TLS, the
// held remainder was consumed by keploy's own handshake — but a copy of
// it was teed to the parser on the way in, because the forwarder had it
// long before the parser had decoded enough to ask for the upgrade.
// Left there, the parser's next read after the upgrade returns
// `16 03 01 ...` where a post-TLS message should be, mis-frames it as a
// packet header (a MySQL header reads that as payloadLength=66326) and
// blocks until the hang watchdog retires it — a connection that records
// zero mocks. barrierTeeOffset is the length of the parser's stream at
// the pause barrier, so discarding below it removes everything teed
// before the barrier that the parser had not yet read. That equals "the
// bytes the handshake took" for a parser that read exactly its own flush
// prefix — which is what ClientFlushBytes requires it to have measured —
// rather than being a property of the mechanism on its own.
//
// Only under a hold. The pre-dispatch path reaches this function too,
// but there the C2D stash is FLUSHED UPSTREAM whole before the
// handshakes run, so its prepend is normally empty and what the parser
// saw is what the server got — consistent, and not ours to change here.
func (r *Relay) endUpgradePause(heldAtEntry, heldConsumedByHandshake bool, barrierTeeOffset int64) {
	if heldAtEntry && heldConsumedByHandshake {
		if pending := r.clientStream.DiscardBefore(barrierTeeOffset); pending > 0 && r.cfg.Logger != nil {
			r.cfg.Logger.Debug("relay: dropping handshake bytes from the parser's client stream",
				zap.Int64("discard_bytes", pending),
				zap.Int64("stream_offset", barrierTeeOffset),
			)
		}
	}
	if heldAtEntry && !heldConsumedByHandshake {
		// releaseClientHold, not flushHeldRemainder: the hold must be
		// CLEARED here, not merely drained once. handleUpgradeTLS clears
		// the flag itself only inside its hold branch, and the exits
		// above that branch — no TLSUpgradeFn, a rejected flush request
		// — never reach it. Draining without clearing leaves the
		// forwarder holding every byte that arrives next, on a
		// connection whose parser has just been told the upgrade
		// failed. releaseClientHold is a no-op when the flag is already
		// down, so the normal path is unaffected.
		if err := r.releaseClientHold(); err != nil && r.cfg.Logger != nil {
			r.cfg.Logger.Debug("relay: flushing the held remainder after a non-upgrade exit failed",
				zap.Error(err),
			)
		}
		// The hold branch clears the flag before flushing its prefix, so
		// by the time we get here on THAT path releaseClientHold above
		// found the flag down and did nothing. Drain what its prefix
		// flush left behind.
		if err := r.flushHeldRemainder(); err != nil && r.cfg.Logger != nil {
			r.cfg.Logger.Debug("relay: flushing the held remainder after a non-upgrade exit failed",
				zap.Error(err),
			)
		}
	}
	r.endPause()
}

// flushHeldRemainder writes whatever is left of the client hold stash
// to the real destination. Unlike releaseClientHold it does not require
// the hold flag to still be set: handleUpgradeTLS clears the flag early
// so no failure path can strand the connection, and the bytes still
// need delivering afterwards.
func (r *Relay) flushHeldRemainder() error {
	r.holdMu.Lock()
	defer r.holdMu.Unlock()

	r.heldClientBytes.Store(0)
	held := r.takeStashed(fakeconn.FromClient)
	if held.len() == 0 {
		return nil
	}
	dstPtr := r.dst.Load()
	if dstPtr == nil {
		return fmt.Errorf("relay: no destination to flush %d held bytes to", held.len())
	}
	return writeHeldBytes(*dstPtr, held.bytes)
}

// takeStashed returns and clears the stash for the given direction.
// Used by the directive handler under the pause barrier to claim
// bytes the forwarder captured at the moment the barrier was raised.
// Returns a zero payload if there is no stash; takeStashed is safe to
// call before any forwarder Read has fired.
func (r *Relay) takeStashed(dir fakeconn.Direction) stashedPayload {
	r.stashMu.Lock()
	defer r.stashMu.Unlock()
	var parts []stashedPayload
	switch dir {
	case fakeconn.FromClient:
		parts = r.stashedC2D
		r.stashedC2D = nil
	case fakeconn.FromDest:
		parts = r.stashedD2C
		r.stashedD2C = nil
	}
	return joinStashed(parts)
}

// takeStashedPrefix returns up to n bytes from the given direction's
// stash, leaving any surplus bytes queued for a later takeStashed call.
func (r *Relay) takeStashedPrefix(dir fakeconn.Direction, n int) stashedPayload {
	if n <= 0 {
		return stashedPayload{}
	}
	r.stashMu.Lock()
	defer r.stashMu.Unlock()

	switch dir {
	case fakeconn.FromClient:
		prefix, rest := splitStashedPrefix(r.stashedC2D, n)
		r.stashedC2D = rest
		return prefix
	case fakeconn.FromDest:
		prefix, rest := splitStashedPrefix(r.stashedD2C, n)
		r.stashedD2C = rest
		return prefix
	default:
		return stashedPayload{}
	}
}

func splitStashedPrefix(parts []stashedPayload, n int) (stashedPayload, []stashedPayload) {
	if n <= 0 || len(parts) == 0 {
		return stashedPayload{}, parts
	}
	out := make([]byte, 0, n)
	var outReadAt time.Time
	rest := parts
	for n > 0 && len(rest) > 0 {
		part := rest[0]
		if len(part.bytes) == 0 {
			rest = rest[1:]
			continue
		}
		if outReadAt.IsZero() && !part.readAt.IsZero() {
			outReadAt = part.readAt
		}
		take := len(part.bytes)
		if take > n {
			take = n
		}
		out = append(out, part.bytes[:take]...)
		n -= take
		if take == len(part.bytes) {
			rest = rest[1:]
			continue
		}
		remainder := stashedPayload{
			bytes:  append([]byte(nil), part.bytes[take:]...),
			readAt: part.readAt,
		}
		newRest := make([]stashedPayload, 0, len(rest))
		newRest = append(newRest, remainder)
		newRest = append(newRest, rest[1:]...)
		rest = newRest
		break
	}
	if len(out) == 0 {
		return stashedPayload{}, rest
	}
	return stashedPayload{bytes: out, readAt: outReadAt}, rest
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
