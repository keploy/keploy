package supervisor

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Default knobs. Callers can override via Config.
const (
	defaultHangBudget = 60 * time.Second
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
	// before declaring the parser hung. Default 60s. The watchdog
	// checks it every HangBudget/4, but never more often than every
	// 5ms. A stall of the whole process (GC, SIGSTOP, a paused
	// container or VM) counts as one such interval however long it
	// lasts, and the first check after a stall never declares the hang
	// itself (see checkHang).
	HangBudget time.Duration

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

	// clock times the hang watchdog. See clock.
	clock clock

	// Activity bookkeeping. lastProgress holds the clock reading of
	// the latest BumpActivity call. pending toggles whether the
	// watchdog is armed.
	lastProgress atomic.Int64
	pending      atomic.Bool

	// wd is the watchdog's running measurement. After New only
	// checkHang touches it, and only the watchdog goroutine calls
	// checkHang (a test on a fake clock calls it instead: that
	// clock's ticker never ticks).
	wd watchdogState

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
	// parser's blocked reads unblock with ErrClosed.
	//
	// Set it before calling Run. The supervisor calls it synchronously
	// on the abort path, so the callback must not block.
	//
	// Go cannot forcibly kill a goroutine: the best we can do is
	// cancel its context and shut the FakeConns so I/O-bound code
	// returns. A parser stuck in a pure CPU loop with no I/O, no
	// ctx check, and no channel op will leak. The watchdog logs
	// this case; tests in supervisor_test.go demonstrate the bound
	// we can and cannot guarantee.
	SessionOnAbort func()

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

// watchdogState is what checkHang carries from one check to the next.
type watchdogState struct {
	// tick is the interval between checks, and so the most silence
	// a single check may charge.
	tick time.Duration
	// prev is the clock reading at the previous check.
	prev time.Duration
	// seen is the progress stamp the previous check read.
	seen time.Duration
	// silence is how long the pending request has gone without
	// progress, as far as the watchdog has seen.
	silence time.Duration
	// held is set when a late check found the budget spent and left
	// the verdict to the next check.
	held bool
}

// New constructs a Supervisor and starts its watchdog goroutine. The
// returned Supervisor owns internal resources until either Run
// returns or Close is called; Run calls Close on exit.
func New(cfg Config) *Supervisor {
	return newSupervisor(cfg, newMonoClock())
}

// newSupervisor is New with the watchdog's clock supplied.
func newSupervisor(cfg Config, clk clock) *Supervisor {
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.HangBudget <= 0 {
		cfg.HangBudget = defaultHangBudget
	}
	if cfg.MemCap <= 0 {
		cfg.MemCap = defaultMemCap
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	s := &Supervisor{
		cfg:        cfg,
		clock:      clk,
		rootCtx:    rootCtx,
		rootCancel: cancel,
		hungCh:     make(chan struct{}),
		wdStop:     make(chan struct{}),
		wdDone:     make(chan struct{}),
	}

	tick := cfg.HangBudget / 4
	if tick < minHangTick {
		tick = minHangTick
	}
	// Measure from here rather than from whenever the goroutine gets
	// to run: a late start is itself a stall.
	start := clk.now()
	s.lastProgress.Store(int64(start))
	s.wd = watchdogState{tick: tick, prev: start, seen: start}
	ticks, stopTicker := clk.newTicker(tick)
	go s.watchdogLoop(ticks, stopTicker)
	return s
}

// BumpActivity is called by the relay whenever a chunk is forwarded
// to a FakeConn or an Ack is delivered. It resets the watchdog
// timer. Cheap; a single atomic store.
func (s *Supervisor) BumpActivity() {
	s.lastProgress.Store(int64(s.clock.now()))
}

// MarkPendingWork indicates an in-flight request is awaiting a
// response. The watchdog only fires while pending. Calling it when
// already pending is harmless.
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
			if r := recover(); r != nil {
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
		// Debug-level: hang abort is a designed control-flow path —
		// the dispatcher's FallthroughToPassthrough handling picks it
		// up and the relay keeps forwarding bytes. Operators who want
		// to tune behaviour have explicit knobs (see next_step).
		s.cfg.Logger.Debug("parser hang detected; aborting",
			zap.Duration("hang_budget", s.cfg.HangBudget),
			zap.String("next_step", "raise supervisor.Config.HangBudget for slow-but-legitimate workloads (long LLM replies, pg_sleep), or set KEPLOY_DISABLE_PARSING=1 / SIGUSR1 to disable parser dispatch entirely (raw passthrough)"),
		)
		s.fireOnAbort()
		runCancel()
		return Result{
			Status:                   StatusHung,
			Err:                      fmt.Errorf("supervisor: parser hung beyond %s", s.cfg.HangBudget),
			FallthroughToPassthrough: true,
		}

	case <-ctx.Done():
		s.cfg.Logger.Debug("supervisor: outer ctx cancelled")
		runCancel()
		// Give the parser a short chance to return cleanly; if it
		// does we surface that. Otherwise we classify as canceled.
		select {
		case r := <-done:
			if r.panicked {
				if s.cfg.PanicReporter != nil {
					s.reportPanic(r.panicVal, r.stack)
				}
				s.fireOnAbort()
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
		case <-time.After(50 * time.Millisecond):
			// Parser did NOT return within the grace window. Most
			// likely it is parked in FakeConn.Read/ReadChunk, which
			// do not observe ctx — they only unblock on Close. Fire
			// SessionOnAbort now so the dispatcher's abort callback
			// (closes both FakeConns, pauses relay tees) runs and
			// the parser goroutine can exit. Without this, the
			// goroutine leaks for the life of the process and the
			// relay tees stay armed, so every subsequent chunk on
			// this connection falls through the channel-full drop
			// path logging at Debug.
			//
			// FallthroughToPassthrough is set so the outer caller
			// knows this connection should route raw; consistent
			// with the hang / panic paths above.
			s.fireOnAbort()
			return Result{
				Status:                   StatusCanceled,
				Err:                      ctx.Err(),
				FallthroughToPassthrough: true,
			}
		}
	}
}

// classifyReturn maps a parser's return (panic or error or nil) plus
// the supervisor's sticky abort flags to a Result.
func (s *Supervisor) classifyReturn(outerCtx context.Context, panicked bool, panicVal any, stack []byte, fnErr error) Result {
	if panicked {
		s.cfg.Logger.Error("parser panicked",
			zap.Any("panic", panicVal),
			zap.ByteString("stack", stack),
			zap.String("next_step", "the supervisor is falling through to raw passthrough so user traffic continues unaffected; file the panic with the parser owner using the captured stack, and set KEPLOY_DISABLE_PARSING=1 / SIGUSR1 to disable parser dispatch entirely until the root cause is fixed"),
		)
		s.reportPanic(panicVal, stack)
		s.fireOnAbort()
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

// watchdogLoop runs checkHang on every tick until the watchdog fires
// or the Supervisor is closed.
func (s *Supervisor) watchdogLoop(ticks <-chan time.Time, stopTicker func()) {
	defer close(s.wdDone)
	defer stopTicker()

	for {
		select {
		case <-s.wdStop:
			return
		case <-ticks:
			if s.checkHang() {
				return
			}
		}
	}
}

// checkHang charges the pending request with the silence since the
// previous check and closes hungCh once that exceeds the budget. It
// reports whether it did.
//
// A check charges at most one tick, however long it has been since
// the previous one. The ticker asks for a check every tick, so a check
// that arrives later than that means the watchdog was not running —
// and usually neither was the rest of the process: a GC pause, a
// SIGSTOP or debugger breakpoint, a paused container or VM, a host too
// loaded to schedule it. The relay was not forwarding in that time
// either, so its silence says nothing about the parser.
//
// Nor does a late check declare the hang. When the process resumes,
// the relay and the watchdog are both due to run, and the overdue
// check would otherwise abort a live parser before the relay has
// forwarded what reached its sockets during the stall. A late check
// that finds the budget spent holds the verdict to the next check,
// which declares the hang unless the relay made progress in between.
// That check decides even if it runs late too, so a watchdog that is
// always late still catches a hung parser.
//
// A hung parser is always caught, because every check charges
// something: at the first check past the budget, the next one if that
// check ran late, and later only when checks keep running late.
func (s *Supervisor) checkHang() bool {
	// Clock first, stamp second. A bump that lands in between is later
	// than now, so the silence restarts below zero by exactly that
	// much, and stays the time since the bump.
	now := s.clock.now()
	elapsed := now - s.wd.prev
	s.wd.prev = now
	step := min(elapsed, s.wd.tick)

	// Idle, or a poll-lane connection that is expected to make no byte
	// progress for a long time: nothing to charge. Re-arming bumps, so
	// the next armed check starts the silence afresh.
	if !s.pending.Load() || s.suspended.Load() {
		return false
	}

	last := time.Duration(s.lastProgress.Load())
	if last != s.wd.seen {
		// Progress since the previous check: the silence restarts at
		// the bump, and is charged no more than any other check.
		s.wd.seen = last
		s.wd.silence = min(now-last, step)
		s.wd.held = false
	} else {
		s.wd.silence += step
	}
	if s.wd.silence <= s.cfg.HangBudget {
		return false
	}
	if elapsed > s.wd.tick && !s.wd.held {
		s.wd.held = true
		return false
	}
	s.hungOnce.Do(func() { close(s.hungCh) })
	return true
}

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
