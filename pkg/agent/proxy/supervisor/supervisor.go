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
	// before declaring the parser hung. Default 60s.
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

	// Activity bookkeeping. lastProgressNano holds a UnixNano stamp
	// updated on every BumpActivity call. pending toggles whether
	// the watchdog is armed.
	lastProgressNano atomic.Int64
	pending          atomic.Bool

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

	rootCtx, cancel := context.WithCancel(context.Background())
	s := &Supervisor{
		cfg:        cfg,
		rootCtx:    rootCtx,
		rootCancel: cancel,
		hungCh:     make(chan struct{}),
		wdStop:     make(chan struct{}),
		wdDone:     make(chan struct{}),
	}
	s.lastProgressNano.Store(time.Now().UnixNano())
	go s.watchdogLoop()
	return s
}

// BumpActivity is called by the relay whenever a chunk is forwarded
// to a FakeConn or an Ack is delivered. It resets the watchdog
// timer. Cheap; a single atomic store.
func (s *Supervisor) BumpActivity() {
	s.lastProgressNano.Store(time.Now().UnixNano())
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
			zap.String("next_step", "raise supervisor.Config.HangBudget for slow-but-legitimate workloads (long LLM replies, pg_sleep), or set KEPLOY_NEW_RELAY=off to force the legacy parser path"),
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
			zap.String("next_step", "the supervisor is falling through to raw passthrough so user traffic continues unaffected; file the panic with the parser owner using the captured stack, and set KEPLOY_NEW_RELAY=off to force the legacy path for this parser, or KEPLOY_DISABLE_PARSING=1 / SIGUSR1 to disable parser dispatch entirely until the root cause is fixed"),
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

// watchdogLoop polls the activity clock. Closes hungCh when budget
// exceeded while pending work is outstanding.
func (s *Supervisor) watchdogLoop() {
	defer close(s.wdDone)

	tick := s.cfg.HangBudget / 4
	if tick < minHangTick {
		tick = minHangTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()

	for {
		select {
		case <-s.wdStop:
			return
		case <-t.C:
			if !s.pending.Load() {
				continue
			}
			last := s.lastProgressNano.Load()
			if last == 0 {
				continue
			}
			if time.Since(time.Unix(0, last)) > s.cfg.HangBudget {
				s.hungOnce.Do(func() { close(s.hungCh) })
				return
			}
		}
	}
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
