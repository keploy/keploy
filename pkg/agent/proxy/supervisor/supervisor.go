package supervisor

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// Default knobs. Callers can override via Config.
const (
	defaultHangBudget = stall.HangBudget
	defaultMemCap     = 8 * 1024 * 1024 // 8 MiB
	// minHangTick bounds how often the watchdog checks its budget so
	// very short HangBudgets (used in tests) do not cause a ridiculous
	// polling rate. HangBudget/4 is the target; we never go below this.
	minHangTick = 5 * time.Millisecond
)

// Config tunes Supervisor behaviour. Zero values are replaced with
// documented defaults by New.
type Config struct {
	// Logger is used for recovery logs, hang diagnostics, and
	// telemetry breadcrumbs. nil is replaced with zap.NewNop().
	Logger *zap.Logger

	// HangBudget is the maximum time the supervisor tolerates no
	// progress (no BumpActivity call) while there is pending work
	// before declaring the parser hung. Default 60s. It is time the
	// process could run (Clock), not wall time: a parser starved of CPU
	// (an overloaded host, a CPU-throttled container, a frozen one) is
	// not hung, and its stall counts only as fast as it gets to run.
	HangBudget time.Duration

	// Clock measures HangBudget (utils/stall); nil is the process's own
	// runnable clock, held while there is pending work to measure (see
	// watchdogLoop). Tests set it.
	Clock stall.Clock

	// MemCap is the per-connection byte cap on parser-owned buffers.
	// Enforced by callers via MarkMemCapExceeded; exposed here as
	// the canonical value so the relay and supervisor agree. Default
	// 8 MiB.
	MemCap int64

	// PanicReporter, if non-nil, is called with the recovered panic
	// value and a captured stack on every parser panic. It runs
	// synchronously on the recovery path; reporters that may block
	// (sentry HTTP, etc.) should forward to a background worker.
	PanicReporter func(r any, stack []byte)
}

// ParserFunc is the signature of a migrated parser's record entry
// point. It replaces the current RecordOutgoing pattern once the new
// Session type is adopted.
type ParserFunc func(ctx context.Context, sess *Session) error

// Supervisor wraps a parser goroutine with panic recovery, a hang
// watchdog, and goroutine accounting. One instance per active
// connection.
//
// The watchdog is a single goroutine started in New and torn down
// when the Supervisor is closed. It re-arms on every BumpActivity
// and fires only while pending work is outstanding.
type Supervisor struct {
	cfg Config

	// Cancellation root for both the supervised parser and any
	// goroutines the parser registers.
	rootCtx    context.Context
	rootCancel context.CancelFunc

	// Activity bookkeeping. lastProgress holds the clock's runnable
	// time (ns) at the latest BumpActivity call. pending toggles
	// whether the watchdog is armed, until the stop: from then on the
	// watchdog measures stop instead (stopWatch).
	clk stall.Clock
	// holdClk holds the process clock (stall.Hold) while the watchdog has
	// pending work to measure; nil for a Config's Clock.
	holdClk      func() (release func())
	lastProgress atomic.Int64
	pending      atomic.Bool
	// stop is set as Run's stop wait begins (watchTheStop): the parser's
	// streams, which still hold what the relay captured for it.
	stop atomic.Pointer[stopWatch]

	// Abort path: hung is closed by the watchdog when the activity
	// budget is exceeded while pending work is outstanding.
	// memCapExceeded is set by callers that detect a memory-cap
	// violation.
	hungOnce       sync.Once
	hungCh         chan struct{}
	memCapExceeded atomic.Bool

	// SessionOnAbort is invoked exactly once when the supervisor
	// aborts the run (hang, panic, mem-cap, outer cancel). The relay
	// sets this to a closure that closes the FakeConns so the
	// parser's blocked reads unblock: with ErrClosed, however long the
	// connection goes on after (the relay leaves the stream of a FakeConn
	// closed before its end open), or io.EOF when the relay had ended the
	// stream before the close.
	//
	// Set it before calling Run. The supervisor calls it synchronously
	// on the abort path, so the callback must not block. It calls it
	// before it logs or reports why it aborts: the parser is dead or
	// retired from that moment, and what the connection carries while the
	// supervisor writes a panic's stack, say, is captured for no one.
	//
	// Go cannot forcibly kill a goroutine: the best we can do is
	// cancel its context and shut the FakeConns so I/O-bound code
	// returns. A parser stuck in a pure CPU loop with no I/O, no
	// ctx check, and no channel op will leak. The watchdog logs
	// this case; tests in supervisor_test.go demonstrate the bound
	// we can and cannot guarantee.
	SessionOnAbort func()

	// StopDeadline, when set before Run, is closed once the stop's own
	// overall deadline has passed. After the outer ctx ends (the recording's
	// stop) Run waits for its parser to return, to read both its streams to
	// their end, or to hang, and aborts it on none of those before this is
	// closed. nil: the stop has no deadline of its own here (the process's
	// end bounds it).
	StopDeadline <-chan struct{}

	// suspended, once set, permanently disarms the hang watchdog for
	// this connection (see SuspendWatchdog). Panic and mem-cap
	// protection are unaffected.
	suspended atomic.Bool

	// Watchdog lifecycle. wdStop is closed to signal the loop to
	// exit; wdDone is closed when the loop returns.
	wdOnce sync.Once
	wdStop chan struct{}
	wdDone chan struct{}

	// abortOnce guards SessionOnAbort invocation.
	abortOnce sync.Once

	// closed guards repeated Close calls.
	closed atomic.Bool
}

// New constructs a Supervisor and starts its watchdog goroutine. The
// returned Supervisor owns internal resources until either Run
// returns or Close is called; Run calls Close on exit.
func New(cfg Config) *Supervisor {
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.HangBudget <= 0 {
		cfg.HangBudget = defaultHangBudget
	}
	if cfg.MemCap <= 0 {
		cfg.MemCap = defaultMemCap
	}

	clk := cfg.Clock
	var holdClk func() func()
	if clk == nil {
		clk, holdClk = stall.ProcessClock(), holdProcessClock
	}
	rootCtx, cancel := context.WithCancel(context.Background())
	s := &Supervisor{
		cfg:        cfg,
		rootCtx:    rootCtx,
		rootCancel: cancel,
		clk:        clk,
		holdClk:    holdClk,
		hungCh:     make(chan struct{}),
		wdStop:     make(chan struct{}),
		wdDone:     make(chan struct{}),
	}
	s.BumpActivity()
	go s.watchdogLoop()
	return s
}

// BumpActivity is called by the relay whenever a chunk is forwarded
// to a FakeConn or an Ack is delivered. It resets the watchdog
// timer. Cheap: an atomic load of the clock and a store.
func (s *Supervisor) BumpActivity() {
	s.lastProgress.Store(int64(s.clk.Runnable()))
}

// MarkPendingWork indicates an in-flight request is awaiting a
// response. Until the stop, the watchdog only fires while pending; from
// the stop on it measures what the parser still has to read instead
// (watchTheStop). Calling it when already pending is harmless.
func (s *Supervisor) MarkPendingWork() {
	// Bump so the hang budget starts fresh at the moment the request
	// actually arrived, not from Supervisor construction.
	s.BumpActivity()
	s.pending.Store(true)
}

// ClearPendingWork declares the outstanding request complete. The
// watchdog disarms; subsequent inactivity is tolerated indefinitely
// (matches invariant: long-poll, LLM response, pg_sleep(45) are OK).
func (s *Supervisor) ClearPendingWork() {
	s.pending.Store(false)
}

// SuspendWatchdog permanently disarms the hang watchdog for this
// connection. The dispatcher calls it (via Session.SuspendWatchdog)
// once the in-flight request is matched to a long-poll async lane:
// such a request legitimately makes no byte progress for far longer
// than the hang budget while the server holds the connection open,
// which is indistinguishable from a hung parser at the byte level.
// Without this the watchdog would abort the parser and fall through
// to passthrough before the delivery arrives, so the poll's mock would
// never be recorded. Panic and mem-cap protection stay active; only
// no-progress hang detection is disabled. Idempotent and concurrency-safe.
//
// The disarm is permanent for the connection, not scoped to the in-flight
// request: on an HTTP/1.1 keep-alive connection that carried a poll, a
// later genuinely-hung request on the same connection is no longer
// hang-aborted. This is an accepted, bounded trade-off — the blast radius
// is one connection, user bytes keep flowing via the relay, and the parked
// read unblocks on connection close — because poll clients typically
// dedicate a connection to the poll loop.
func (s *Supervisor) SuspendWatchdog() {
	s.suspended.Store(true)
}

// MarkMemCapExceeded is called by the relay when the per-connection
// parser-owned byte cap is breached. Run treats it like a hang: the
// parser is aborted and the caller falls through to passthrough.
func (s *Supervisor) MarkMemCapExceeded() {
	s.memCapExceeded.Store(true)
	// Waking the parser and propagating the abort is enough; we do
	// not close hungCh here so Run can discriminate mem-cap from
	// hang by checking memCapExceeded.Load after fn returns.
	s.rootCancel()
	s.fireOnAbort()
}

// RegisterGoroutine hands back a context the caller should respect.
// All registered goroutines share the supervisor's root context, so
// cancelling Run cancels them.
//
// This replaces the legacy errgroup passed via RecordSession.ErrGroup.
// Unlike errgroup, the supervisor does not wait on these goroutines:
// waiting is the caller's job via its own WaitGroup or channel
// rendezvous. The supervisor's role is solely to cancel.
func (s *Supervisor) RegisterGoroutine() context.Context {
	return s.rootCtx
}

// Run wraps fn with panic recovery, a hang watchdog, and goroutine
// accounting. It does not touch real sockets; callers that need to
// fall through to passthrough inspect Result.FallthroughToPassthrough
// and invoke their own passthrough path.
//
// The supervisor owns sess.Ctx for the duration of Run; outer
// cancellation is honoured via a derived context. Run blocks until
// either fn returns, the outer ctx cancels, or the watchdog fires.
//
// After Run returns, the Supervisor is single-use: its root context
// is cancelled and its watchdog is torn down. Construct a new
// Supervisor per connection.
func (s *Supervisor) Run(ctx context.Context, fn ParserFunc, sess *Session) Result {
	defer s.Close()

	// Derive the parser's context from both the outer caller and
	// the supervisor root so abort paths (mem-cap, hang via its
	// own path) and outer cancellation both flow through.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	stopOnRoot := context.AfterFunc(s.rootCtx, runCancel)
	defer stopOnRoot()

	if sess != nil {
		sess.Ctx = runCtx
	}

	// Run the parser in its own goroutine so we can select on it
	// alongside watchdog and ctx signals.
	type fnReturn struct {
		err      error
		panicked bool
		panicVal any
		stack    []byte
	}
	done := make(chan fnReturn, 1)
	go func() {
		var ret fnReturn
		defer func() {
			r := recover()
			if r != nil || ret.err != nil {
				// The parser is gone, and nothing records what its
				// connection carries from here on: stamp the session's
				// stop (Session.StoppedAt) here, where the supervisor
				// learns it, before the stack is taken and the return
				// handed on to be logged. A parser that returns nil has
				// recorded what it read, and is not stopped here.
				sess.StoppedAt()
			}
			// The parser emits nothing more on its session: say, once for
			// the connection, what the request clamp cost it.
			sess.ReportClampedOut()
			if r != nil {
				ret = fnReturn{
					panicked: true,
					panicVal: r,
					stack:    debug.Stack(),
				}
			}
			done <- ret
		}()
		ret.err = fn(runCtx, sess)
	}()

	select {
	case r := <-done:
		return s.classifyReturn(ctx, r.panicked, r.panicVal, r.stack, r.err)

	case <-s.hungCh:
		return s.hung(sess, runCancel)

	case <-ctx.Done():
		s.cfg.Logger.Debug("supervisor: outer ctx cancelled")
		runCancel()
		// The stop. A parser behind its capture still holds what the relay
		// captured for it before the stop, and the relay's teardown ends its
		// streams once it has handed all of that on: the parser reads it
		// (it ends on its streams' end, not on its context's) and returns.
		// So the supervisor waits for an event, never for a time: a grace
		// measured on the wall clock decides nothing on a starved agent,
		// where the parser is not run within it, and aborting it then cut
		// short what it had not read yet. It aborts the parser only once
		// that costs nothing captured, or once the stop's own deadline says
		// the wait is over:
		//   - the parser has read both its streams to their end
		//     (fakeconn.FakeConn.ReadToEnd): whatever it still waits on,
		//     nothing of the capture is left for it to read;
		//   - the stop's overall deadline passed (StopDeadline): what the
		//     parser has not read by then is left out, with its span, as
		//     everything a retired parser never read is;
		//   - it hangs (the watchdog, on the time it could run): the hang
		//     path, as before the stop. From the stop, a hang is judged by
		//     what the parser still has to read (watchTheStop), not by the
		//     pending flag a recorded mock clears: a parser that records a
		//     mock and then stalls with captured bytes unread is hung, and
		//     is retired, never waited on silently.
		// The abort fires SessionOnAbort, whose closes unblock a parser
		// parked in a read of a stream the relay has not ended: the
		// goroutine does not leak, and the relay's tees do not stay armed.
		ends := streamEnds(sess)
		s.watchTheStop(sess)
	wait:
		for {
			var end <-chan struct{}
			if len(ends) > 0 {
				end = ends[0]
			}
			select {
			case r := <-done:
				if r.panicked {
					s.fireOnAbort()
					if s.cfg.PanicReporter != nil {
						s.reportPanic(r.panicVal, r.stack)
					}
					return Result{
						Status:                   StatusPanicked,
						Err:                      wrapPanic(r.panicVal),
						FallthroughToPassthrough: true,
					}
				}
				// Clean parser exit on outer-cancel: no blocked reads to
				// unstick, so we intentionally skip fireOnAbort. The
				// FakeConns will be GC'd on the normal return path.
				return Result{Status: StatusCanceled, Err: r.err}
			case <-s.hungCh:
				return s.hung(sess, runCancel)
			case <-end:
				if ends = ends[1:]; len(ends) == 0 {
					break wait
				}
			case <-s.StopDeadline:
				break wait
			}
		}
		// FallthroughToPassthrough is set so the outer caller knows this
		// connection should route raw; consistent with the hang / panic
		// paths above.
		s.fireOnAbort()
		return Result{
			Status:                   StatusCanceled,
			Err:                      ctx.Err(),
			FallthroughToPassthrough: true,
		}
	}
}

// hung retires a parser the watchdog found hung, and aborts it.
func (s *Supervisor) hung(sess *Session, runCancel context.CancelFunc) Result {
	// Abort first: the parser is retired from here, and the abort
	// stops its capture (SessionOnAbort). It goes on running, and the
	// abort closes its streams under it before its context is
	// cancelled below: retire its session first, so that nothing it
	// hands over from here, cut where its streams were closed, is
	// recorded (Session.retire).
	sess.retire(leftOutParserHung)
	s.fireOnAbort()
	// Debug-level: hang abort is a designed control-flow path —
	// the dispatcher's FallthroughToPassthrough handling picks it
	// up and the relay keeps forwarding bytes. Operators who want
	// to tune behaviour have explicit knobs (see next_step).
	s.cfg.Logger.Debug("parser hang detected; aborting",
		zap.Duration("hang_budget", s.cfg.HangBudget),
		zap.String("next_step", "raise supervisor.Config.HangBudget for slow-but-legitimate workloads (long LLM replies, pg_sleep), or set KEPLOY_DISABLE_PARSING=1 / SIGUSR1 to disable parser dispatch entirely (raw passthrough)"),
	)
	runCancel()
	return Result{
		Status:                   StatusHung,
		Err:                      fmt.Errorf("supervisor: parser hung beyond %s", s.cfg.HangBudget),
		FallthroughToPassthrough: true,
	}
}

// streamEnds is what says sess's parser has read both its streams to their
// end (fakeconn.FakeConn.ReadToEnd), nil for a session without both: then
// nothing says so.
func streamEnds(sess *Session) []<-chan struct{} {
	var ends []<-chan struct{}
	for _, f := range bothStreams(sess) {
		ends = append(ends, f.ReadToEnd())
	}
	return ends
}

// bothStreams is sess's two streams, nil for a session without both.
func bothStreams(sess *Session) []*fakeconn.FakeConn {
	if sess == nil || sess.ClientStream == nil || sess.DestStream == nil {
		return nil
	}
	return []*fakeconn.FakeConn{sess.ClientStream, sess.DestStream}
}

// classifyReturn maps a parser's return (panic or error or nil) plus
// the supervisor's sticky abort flags to a Result.
func (s *Supervisor) classifyReturn(outerCtx context.Context, panicked bool, panicVal any, stack []byte, fnErr error) Result {
	if panicked {
		// Abort first: the parser is dead, and the abort stops its capture
		// (SessionOnAbort). Logged first, the stack and the report took
		// their time while the connection's bytes were captured for no one.
		s.fireOnAbort()
		s.cfg.Logger.Error("parser panicked",
			zap.Any("panic", panicVal),
			zap.ByteString("stack", stack),
			zap.String("next_step", "the supervisor is falling through to raw passthrough so user traffic continues unaffected; file the panic with the parser owner using the captured stack, and set KEPLOY_DISABLE_PARSING=1 / SIGUSR1 to disable parser dispatch entirely until the root cause is fixed"),
		)
		s.reportPanic(panicVal, stack)
		return Result{
			Status:                   StatusPanicked,
			Err:                      wrapPanic(panicVal),
			FallthroughToPassthrough: true,
		}
	}

	// Sticky flags beat a "clean" return: if we already declared
	// the parser dead and it happened to return the exact moment we
	// cancelled it, surface the real reason.
	if s.memCapExceeded.Load() {
		return Result{
			Status:                   StatusMemCap,
			Err:                      fnErr,
			FallthroughToPassthrough: true,
		}
	}

	if fnErr == nil {
		return Result{Status: StatusOK}
	}
	if errors.Is(fnErr, context.Canceled) && outerCtx.Err() != nil {
		return Result{Status: StatusCanceled, Err: fnErr}
	}
	// G1 fix: a parser that returned a non-nil error on its own
	// (decode failure, malformed wire frame, decompression error,
	// etc.) is in the same situation as a panic from the user-traffic
	// perspective — the bytes have already been forwarded by the
	// relay, and the parser's failure to record a clean mock has no
	// bearing on whether the application's connection should survive.
	// Set FallthroughToPassthrough so the dispatcher leaves the relay
	// alone and bytes keep flowing until peer close. fireOnAbort is
	// invoked so the SessionOnAbort callback can pause the tees and
	// close the FakeConns; without it the tees keep accumulating
	// chunks for a parser that will never read them, eventually
	// dropping at DropChannelFull and spamming Debug logs.
	s.fireOnAbort()
	return Result{
		Status:                   StatusError,
		Err:                      fnErr,
		FallthroughToPassthrough: true,
	}
}

// reportPanic invokes cfg.PanicReporter guarded against reporter
// panics, so a buggy reporter cannot turn a recovered parser panic
// into a crash.
func (s *Supervisor) reportPanic(v any, stack []byte) {
	if s.cfg.PanicReporter == nil {
		return
	}
	defer func() {
		if rr := recover(); rr != nil {
			s.cfg.Logger.Error("panic reporter itself panicked",
				zap.Any("panic", rr),
				zap.String("next_step", "the configured PanicReporter must be non-blocking and must not panic; fix the reporter implementation, or unset it via supervisor.Config.PanicReporter=nil to fall back to no external reporting (the recovered parser panic is still logged at Error level)"),
			)
		}
	}()
	s.cfg.PanicReporter(v, stack)
}

// Close tears down the watchdog and cancels any registered goroutines.
// Run calls Close on exit; direct callers can call it to abandon a
// Supervisor without running anything. Idempotent.
func (s *Supervisor) Close() {
	if s.closed.Swap(true) {
		return
	}
	s.rootCancel()
	s.wdOnce.Do(func() { close(s.wdStop) })
	<-s.wdDone
}

// hangExceeded reports whether the no-progress budget has been exceeded, in
// the time the process could run since the latest activity, given the current
// pending/suspended state, or, from the stop on, since the parser last read
// (stopWatch.hung). It is the single decision the watchdog makes on each tick.
func (s *Supervisor) hangExceeded() bool {
	if w := s.stop.Load(); w != nil {
		return w.hung(s.clk.Runnable(), s.cfg.HangBudget)
	}
	if !s.pending.Load() {
		return false
	}
	// A poll-lane connection is expected to make no byte progress for a long
	// time; do not treat that as a hang.
	if s.suspended.Load() {
		return false
	}
	return s.clk.Runnable()-time.Duration(s.lastProgress.Load()) > s.cfg.HangBudget
}

// watchdogLoop looks at the activity clock every tick of wall time (a
// quarter of the budget). Closes hungCh when the budget is exceeded while
// pending work is outstanding, or, from the stop on, while bytes captured
// for the parser are unread (armed). A timer re-armed after each look: how late it
// fires does not matter, the clock says how long the process could run.
//
// The process clock is sampled only while held, so the loop holds it only
// while there is pending work to measure: an idle connection, the most of a
// pooled one's life, costs no sampling. Held from the first look that finds
// the work pending, the budget is measured from then, at most a tick after
// the work arrived when nothing else held the clock.
func (s *Supervisor) watchdogLoop() {
	defer close(s.wdDone)
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()

	tick := s.cfg.HangBudget / 4
	if tick < minHangTick {
		tick = minHangTick
	}
	for {
		if s.holdClk != nil {
			switch armed := s.armed(); {
			case armed && release == nil:
				release = s.holdClk()
			case !armed && release != nil:
				release()
				release = nil
			}
		}
		c, stop := s.clk.After(tick)
		select {
		case <-s.wdStop:
			stop()
			return
		case <-c:
			if s.hangExceeded() {
				s.hungOnce.Do(func() { close(s.hungCh) })
				return
			}
		}
	}
}

// armed reports whether the watchdog has work to measure: before the stop, a
// request pending (MarkPendingWork) on a connection whose watchdog is not
// suspended; from the stop on, bytes captured for the parser that it has not
// read (stopWatch.unread).
func (s *Supervisor) armed() bool {
	if w := s.stop.Load(); w != nil {
		return w.unread()
	}
	return s.pending.Load() && !s.suspended.Load()
}

// watchTheStop turns the watchdog, as Run's stop wait begins, from the pending
// flag to what sess's parser still has to read of its streams. The pending
// flag says a request awaits its answer, and a recorded mock clears it: after
// the stop nothing more arrives, and what is left is what the relay captured
// before it. A parser that records a mock and then stalls outside a read, its
// streams holding captured bytes it has not read, is hung all the same: the
// watchdog fires once it has not read for its budget of the time it could
// run, and Run retires it (hung), so that what it never read is left out and
// reported, not waited on for as long as it stalls. The relay's activity
// (BumpActivity) is no progress of the parser's, and counts for nothing from
// the stop. The parser's budget is measured from the stop: the time the
// process could run before it says nothing of the parser's reads after it.
// A suspended watchdog (SuspendWatchdog) is armed again: its parser
// waits in a read for an answer the network holds, and a parser blocked in a
// read is not hung (stopWatch.hung). A session without both streams keeps the
// pending flag: nothing says what its parser still has to read.
func (s *Supervisor) watchTheStop(sess *Session) {
	streams := bothStreams(sess)
	if len(streams) == 0 {
		return
	}
	w := &stopWatch{streams: streams, since: s.clk.Runnable()}
	w.read, w.waits, _ = w.mark()
	s.stop.Store(w)
}

// stopWatch is what the watchdog measures from the stop on (watchTheStop).
type stopWatch struct {
	streams []*fakeconn.FakeConn

	mu sync.Mutex
	// read and waits are the parser's reads at the latest look (mark), and
	// since the clock's runnable time when they last moved, or when the
	// parser was last seen blocked in a read.
	read  int64
	waits uint64
	since time.Duration
}

// unread reports whether some stream holds what the parser has not read: it
// has not read it to the end its producer gave it (fakeconn.FakeConn.ReadToEnd).
// The relay ends a stream only once it has handed on every chunk it captured
// for it.
func (w *stopWatch) unread() bool {
	for _, f := range w.streams {
		select {
		case <-f.ReadToEnd():
		default:
			return true
		}
	}
	return false
}

// mark is how far the parser has read its streams (fakeconn.FakeConn.Consumed),
// how many times it has blocked in a read of one, and whether it is blocked in
// one now (fakeconn.FakeConn.Waiting).
func (w *stopWatch) mark() (read int64, waits uint64, waiting bool) {
	for _, f := range w.streams {
		read += f.Consumed()
		wt, n := f.Waiting()
		waits += n
		waiting = waiting || wt
	}
	return read, waits, waiting
}

// hung reports whether the parser, with bytes captured for it unread, has not
// read for more than budget of the clock's runnable time, now. Its reads
// moving, or its blocking in a read, is progress: a parser blocked in a read
// has read all it was handed of that stream, and waits on the relay, not on
// itself (the relay ends the stream once it has handed on what it captured).
// A parser reading a long capture slowly is never hung, however long it
// takes.
func (w *stopWatch) hung(now, budget time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.unread() {
		return false
	}
	read, waits, waiting := w.mark()
	if waiting || read != w.read || waits != w.waits {
		w.read, w.waits, w.since = read, waits, now
		return false
	}
	return now-w.since > budget
}

// holdProcessClock holds the process clock (stall.Hold); tests count it.
var holdProcessClock = stall.Hold

// wrapPanic converts a recovered panic value into an error suitable
// for Result.Err. If the value is already an error, we preserve it
// so errors.Is / errors.As continue to work; otherwise we format.
func wrapPanic(v any) error {
	if err, ok := v.(error); ok {
		return fmt.Errorf("supervisor: parser panic: %w", err)
	}
	return fmt.Errorf("supervisor: parser panic: %v", v)
}

// fireOnAbort invokes SessionOnAbort at most once, guarded so parallel
// abort reasons (hang + outer cancel racing) do not invoke the
// callback twice and a panicking callback does not propagate.
func (s *Supervisor) fireOnAbort() {
	if s.SessionOnAbort == nil {
		return
	}
	s.abortOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				s.cfg.Logger.Error("SessionOnAbort callback panicked",
					zap.Any("panic", r),
					zap.String("next_step", "SessionOnAbort callbacks must be non-blocking and must not panic — they run on the supervisor's abort path where further errors have nowhere to propagate to; fix the callback (typical use is just closing FakeConns and pausing tees — see proxy_v2.go for the reference implementation), or unset it by constructing the Supervisor without SessionOnAbort if the caller can tolerate parser-side reads not unblocking promptly on abort"),
				)
			}
		}()
		s.SessionOnAbort()
	})
}
