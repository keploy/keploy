package supervisor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// shortCfg returns a Config tuned for fast tests.
func shortCfg(t *testing.T) Config {
	t.Helper()
	return Config{
		Logger:     zaptest.NewLogger(t),
		HangBudget: 50 * time.Millisecond,
	}
}

func TestRunOK(t *testing.T) {
	t.Parallel()
	s := New(shortCfg(t))
	res := s.Run(context.Background(),
		func(ctx context.Context, sess *Session) error { return nil },
		&Session{})
	if res.Status != StatusOK {
		t.Fatalf("status: got %s, want ok", res.Status)
	}
	if res.Err != nil {
		t.Fatalf("err: got %v, want nil", res.Err)
	}
	if res.FallthroughToPassthrough {
		t.Fatalf("fallthrough: got true, want false")
	}
}

func TestRunError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("parser failure")
	s := New(shortCfg(t))
	res := s.Run(context.Background(),
		func(ctx context.Context, sess *Session) error { return sentinel },
		&Session{})
	if res.Status != StatusError {
		t.Fatalf("status: got %s, want error", res.Status)
	}
	if !errors.Is(res.Err, sentinel) {
		t.Fatalf("err: got %v, want %v", res.Err, sentinel)
	}
	if res.FallthroughToPassthrough {
		t.Fatalf("fallthrough: got true, want false (errors alone don't fall through)")
	}
}

func TestRunPanic(t *testing.T) {
	t.Parallel()
	var gotPanic any
	var gotStack []byte
	reporterCalled := make(chan struct{})

	cfg := shortCfg(t)
	cfg.PanicReporter = func(r any, stack []byte) {
		gotPanic = r
		gotStack = stack
		close(reporterCalled)
	}
	s := New(cfg)

	res := s.Run(context.Background(),
		func(ctx context.Context, sess *Session) error {
			panic("boom")
		},
		&Session{})

	if res.Status != StatusPanicked {
		t.Fatalf("status: got %s, want panicked", res.Status)
	}
	if !res.FallthroughToPassthrough {
		t.Fatalf("fallthrough: got false, want true")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "boom") {
		t.Fatalf("err should wrap panic value: got %v", res.Err)
	}

	select {
	case <-reporterCalled:
	case <-time.After(time.Second):
		t.Fatalf("PanicReporter was not called")
	}
	if gotPanic != "boom" {
		t.Fatalf("reporter panic val: got %v, want boom", gotPanic)
	}
	if len(gotStack) == 0 {
		t.Fatalf("reporter stack: empty")
	}
}

func TestRunPanicWithError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("real error")
	s := New(shortCfg(t))
	res := s.Run(context.Background(),
		func(ctx context.Context, sess *Session) error {
			panic(sentinel)
		},
		&Session{})
	if res.Status != StatusPanicked {
		t.Fatalf("status: got %s, want panicked", res.Status)
	}
	if !errors.Is(res.Err, sentinel) {
		t.Fatalf("err: got %v, should wrap %v", res.Err, sentinel)
	}
}

func TestHangDetectedWhenPending(t *testing.T) {
	t.Parallel()
	aborted := make(chan struct{})
	s := New(shortCfg(t))
	s.SessionOnAbort = func() { close(aborted) }

	// Arm pending work so the watchdog is eligible to fire.
	s.MarkPendingWork()

	start := time.Now()
	res := s.Run(context.Background(),
		func(ctx context.Context, sess *Session) error {
			<-ctx.Done()
			return ctx.Err()
		},
		&Session{})
	elapsed := time.Since(start)

	if res.Status != StatusHung {
		t.Fatalf("status: got %s, want hung", res.Status)
	}
	if !res.FallthroughToPassthrough {
		t.Fatalf("fallthrough: got false, want true")
	}
	// Budget is 50ms + up to one tick (12.5ms) + scheduling slop.
	// Fail if we wildly overshot — something is broken.
	if elapsed > time.Second {
		t.Fatalf("hang detection too slow: %v", elapsed)
	}
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Fatalf("SessionOnAbort was not invoked")
	}
}

func TestHangNotDetectedWhenIdle(t *testing.T) {
	t.Parallel()
	s := New(shortCfg(t)) // 50ms budget

	done := make(chan Result, 1)
	parserStarted := make(chan struct{})
	go func() {
		done <- s.Run(context.Background(),
			func(ctx context.Context, sess *Session) error {
				close(parserStarted)
				<-ctx.Done()
				return ctx.Err()
			},
			&Session{})
	}()

	<-parserStarted
	// Give the watchdog five budgets of wallclock time; without
	// MarkPendingWork it must not fire.
	select {
	case r := <-done:
		t.Fatalf("watchdog fired while idle: %+v", r)
	case <-time.After(250 * time.Millisecond):
	}

	// Cleanup: cancel via Close so the parser exits and the test finishes.
	s.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("parser did not exit after Close")
	}
}

func TestHangResetOnActivity(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Logger:     zaptest.NewLogger(t),
		HangBudget: 80 * time.Millisecond,
	}
	s := New(cfg)
	s.MarkPendingWork()

	// Drive activity well faster than the budget so the parser's
	// effective run time exceeds budget but the watchdog never fires.
	runFor := 300 * time.Millisecond
	stopBumping := make(chan struct{})
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				s.BumpActivity()
			case <-stopBumping:
				return
			}
		}
	}()

	res := s.Run(context.Background(),
		func(ctx context.Context, sess *Session) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(runFor):
				return nil
			}
		},
		&Session{})
	close(stopBumping)

	if res.Status != StatusOK {
		t.Fatalf("expected OK with activity bumps, got %s (err=%v)", res.Status, res.Err)
	}
}

func TestContextCancelCleanExit(t *testing.T) {
	t.Parallel()
	s := New(shortCfg(t))
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	res := s.Run(ctx,
		func(ctx context.Context, sess *Session) error {
			<-ctx.Done()
			return ctx.Err()
		},
		&Session{})

	if res.Status != StatusCanceled {
		t.Fatalf("status: got %s, want canceled", res.Status)
	}
}

func TestSessionEmitMockRespectsIncomplete(t *testing.T) {
	t.Parallel()
	ch := make(chan *models.Mock, 1)
	sess := &Session{
		Mocks:  ch,
		Ctx:    context.Background(),
		Logger: zaptest.NewLogger(t),
	}

	sess.MarkMockIncomplete("memory_pressure")
	if err := sess.EmitMock(&models.Mock{Name: "m1"}); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	select {
	case got := <-ch:
		t.Fatalf("incomplete mock leaked to channel: %v", got)
	default:
	}

	// After an incomplete emit, the flag is cleared so the next mock
	// sends normally.
	sess.MarkMockComplete()
	if err := sess.EmitMock(&models.Mock{Name: "m2"}); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	select {
	case got := <-ch:
		if got.Name != "m2" {
			t.Fatalf("wrong mock: %v", got.Name)
		}
	case <-time.After(time.Second):
		t.Fatalf("m2 was not delivered")
	}
}

// TestSessionEmitMockDropPathClearsPending pins the invariant that
// dropping a mock due to IsMockIncomplete() still calls
// OnPendingCleared. Otherwise the supervisor's hang watchdog stays
// armed after a benign drop (chunk gate, memory pressure, short
// write) and eventually fires a spurious abort after the connection
// goes idle.
func TestSessionEmitMockDropPathClearsPending(t *testing.T) {
	t.Parallel()
	var pendingCleared int
	sess := &Session{
		Mocks:            make(chan *models.Mock, 1),
		Ctx:              context.Background(),
		Logger:           zaptest.NewLogger(t),
		OnPendingCleared: func() { pendingCleared++ },
	}

	sess.MarkMockIncomplete("chunk_gate")
	if err := sess.EmitMock(&models.Mock{Name: "drop-me"}); err != nil {
		t.Fatalf("EmitMock returned err: %v", err)
	}
	if pendingCleared != 1 {
		t.Fatalf("OnPendingCleared calls on drop path = %d, want 1", pendingCleared)
	}

	// Sanity: the normal emit path also fires OnPendingCleared, so a
	// subsequent successful emit increments the counter.
	if err := sess.EmitMock(&models.Mock{Name: "kept"}); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	if pendingCleared != 2 {
		t.Fatalf("OnPendingCleared calls after successful emit = %d, want 2", pendingCleared)
	}
}

func TestSessionEmitMockHonorsCtxCancel(t *testing.T) {
	t.Parallel()
	// Unbuffered channel nobody reads → EmitMock would block forever
	// without ctx handling.
	ch := make(chan *models.Mock)
	ctx, cancel := context.WithCancel(context.Background())
	sess := &Session{
		Mocks:  ch,
		Ctx:    ctx,
		Logger: zaptest.NewLogger(t),
	}

	done := make(chan error, 1)
	go func() { done <- sess.EmitMock(&models.Mock{Name: "m"}) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err: got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("EmitMock did not return after ctx cancel (deadlock)")
	}
}

// TestSessionEmitMockRouteViaSyncMock_DirectChannelUntouched pins
// the RouteMocksViaSyncMock=true path by asserting the negative:
// when the flag is on, Session.Mocks must NOT receive anything,
// and OnPendingCleared must still fire. That combination can only
// be produced by the syncMock branch — the other branches either
// push to Session.Mocks or only fire OnPendingCleared when Mocks
// is nil.
//
// Earlier iterations of this test rebound the package-singleton
// syncMock's outChan via SetOutputChannel(...). That touched a
// global visible across every test in the same test process, so
// parallel tests or t.Parallel() subtests in this package that
// also call SetOutputChannel could race on the outChan pointer
// and produce flaky timeouts. (Cross-package `go test ./...` runs
// each package in its own binary, so the race was strictly
// intra-package.) Asserting on the local Session.Mocks channel
// keeps the test entirely within this Session instance.
func TestSessionEmitMockRouteViaSyncMock_DirectChannelUntouched(t *testing.T) {
	t.Parallel()

	directCh := make(chan *models.Mock, 1)

	var pendingCleared int32
	sess := &Session{
		Mocks:                 directCh,
		Logger:                zaptest.NewLogger(t),
		Ctx:                   context.Background(),
		RouteMocksViaSyncMock: true,
		OnPendingCleared: func() {
			atomic.AddInt32(&pendingCleared, 1)
		},
	}

	if err := sess.EmitMock(&models.Mock{Name: "via-syncmock"}); err != nil {
		t.Fatalf("EmitMock returned err: %v", err)
	}

	// Direct channel must remain empty — the syncMock route should
	// have returned before touching it. EmitMock is synchronous, so
	// a non-blocking receive is enough: anything it was going to
	// send on Mocks would already be buffered by the time EmitMock
	// returned.
	select {
	case got := <-directCh:
		t.Fatalf("mock leaked to Session.Mocks when RouteMocksViaSyncMock=true: %+v", got)
	default:
		// expected: nothing delivered.
	}

	if c := atomic.LoadInt32(&pendingCleared); c != 1 {
		t.Fatalf("OnPendingCleared calls = %d, want 1 (only the syncMock branch fires it without also sending on Mocks)", c)
	}
}

// TestSessionEmitMockRouteViaSyncMock_HonorsCtx pins the ctx-cancel
// behaviour on the syncMock routing path. Without the pre-check,
// EmitMock would call mgr.AddMock (which does not observe s.Ctx)
// and return nil — silently violating the documented contract
// that EmitMock returns ctx.Err() when the parser's context has
// been cancelled.
//
// Uses the local-direct-channel pattern (no singleton rebind) so
// concurrent test packages that call SetOutputChannel can't
// interfere.
func TestSessionEmitMockRouteViaSyncMock_HonorsCtx(t *testing.T) {
	t.Parallel()

	directCh := make(chan *models.Mock, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before EmitMock is called

	sess := &Session{
		Mocks:                 directCh,
		Logger:                zaptest.NewLogger(t),
		Ctx:                   ctx,
		RouteMocksViaSyncMock: true,
	}
	err := sess.EmitMock(&models.Mock{Name: "should-be-cancelled"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Non-blocking receive: EmitMock is synchronous, so if it had
	// written to directCh it would already be readable.
	select {
	case got := <-directCh:
		t.Fatalf("mock leaked to Session.Mocks after ctx cancel: %+v", got)
	default:
		// expected: no delivery.
	}
}

func TestAddPostRecordHookChains(t *testing.T) {
	t.Parallel()
	ch := make(chan *models.Mock, 1)
	sess := &Session{
		Mocks:  ch,
		Ctx:    context.Background(),
		Logger: zaptest.NewLogger(t),
	}

	var order []string
	// The "first" hook is added first but becomes the outer hook.
	sess.AddPostRecordHook(func(m *models.Mock) { order = append(order, "first") })
	// The "second" hook is then added in front, so it runs first.
	sess.AddPostRecordHook(func(m *models.Mock) { order = append(order, "second") })

	if err := sess.EmitMock(&models.Mock{Name: "x"}); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	<-ch // drain

	// Front-added runs first: "second" (added last, front-of-chain)
	// then "first".
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Fatalf("hook order: got %v, want [second first]", order)
	}
}

func TestAddPostRecordHookNilSafe(t *testing.T) {
	t.Parallel()
	var s *Session
	// Must not panic.
	s.AddPostRecordHook(func(*models.Mock) {})

	sess := &Session{}
	sess.AddPostRecordHook(nil)
	if sess.OnMockRecorded != nil {
		t.Fatalf("nil hook should not have been installed")
	}
}

func TestRegisterGoroutine(t *testing.T) {
	t.Parallel()
	s := New(shortCfg(t))
	s.MarkPendingWork()

	helperCtx := s.RegisterGoroutine()
	helperDone := make(chan struct{})

	go func() {
		<-helperCtx.Done()
		close(helperDone)
	}()

	// Parser blocks forever on ctx; watchdog fires after 50ms.
	res := s.Run(context.Background(),
		func(ctx context.Context, sess *Session) error {
			<-ctx.Done()
			return ctx.Err()
		},
		&Session{})

	if res.Status != StatusHung {
		t.Fatalf("status: got %s, want hung", res.Status)
	}

	select {
	case <-helperDone:
	case <-time.After(time.Second):
		t.Fatalf("helper goroutine's ctx was not cancelled after Run")
	}
}

func TestMemCapAborts(t *testing.T) {
	t.Parallel()
	s := New(shortCfg(t))

	parserStarted := make(chan struct{})
	done := make(chan Result, 1)
	go func() {
		done <- s.Run(context.Background(),
			func(ctx context.Context, sess *Session) error {
				close(parserStarted)
				<-ctx.Done()
				return ctx.Err()
			},
			&Session{})
	}()

	<-parserStarted
	s.MarkMemCapExceeded()

	select {
	case res := <-done:
		if res.Status != StatusMemCap {
			t.Fatalf("status: got %s, want mem_cap", res.Status)
		}
		if !res.FallthroughToPassthrough {
			t.Fatalf("fallthrough: got false, want true")
		}
	case <-time.After(time.Second):
		t.Fatalf("mem cap did not abort parser")
	}
}

func TestBumpActivityIsCheap(t *testing.T) {
	t.Parallel()
	s := New(shortCfg(t))
	defer s.Close()

	// Smoke test: 100k bumps should complete in a sane time and not
	// race the watchdog.
	for i := 0; i < 100_000; i++ {
		s.BumpActivity()
	}
}

func TestClearPendingDisarmsWatchdog(t *testing.T) {
	t.Parallel()
	s := New(shortCfg(t))
	s.MarkPendingWork()
	// Immediately clear; subsequent quiet period should not fire.
	s.ClearPendingWork()

	done := make(chan Result, 1)
	go func() {
		done <- s.Run(context.Background(),
			func(ctx context.Context, sess *Session) error {
				// Quiet for 4 budgets; should NOT be classified as hung.
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(200 * time.Millisecond):
					return nil
				}
			},
			&Session{})
	}()

	select {
	case r := <-done:
		if r.Status != StatusOK {
			t.Fatalf("status: got %s, want ok", r.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("parser did not finish")
	}
}

func TestPanicReporterPanicIsContained(t *testing.T) {
	t.Parallel()
	cfg := shortCfg(t)
	cfg.PanicReporter = func(r any, stack []byte) { panic("reporter blew up") }
	s := New(cfg)

	res := s.Run(context.Background(),
		func(ctx context.Context, sess *Session) error {
			panic("parser blew up")
		},
		&Session{})
	// The parser panic is still reported as StatusPanicked; the
	// reporter's own panic is swallowed.
	if res.Status != StatusPanicked {
		t.Fatalf("status: got %s, want panicked", res.Status)
	}
}

func TestSessionOnAbortRunsAtMostOnce(t *testing.T) {
	t.Parallel()
	var count atomic.Int32
	s := New(shortCfg(t))
	s.SessionOnAbort = func() { count.Add(1) }
	s.MarkPendingWork()

	// Double trigger: mem cap + hang. The sync.Once guards a single call.
	s.MarkMemCapExceeded()

	_ = s.Run(context.Background(),
		func(ctx context.Context, sess *Session) error {
			<-ctx.Done()
			return ctx.Err()
		},
		&Session{})

	if got := count.Load(); got != 1 {
		t.Fatalf("SessionOnAbort call count: got %d, want 1", got)
	}
}

func TestNilSessionEmitIsNoop(t *testing.T) {
	t.Parallel()
	var sess *Session
	if err := sess.EmitMock(&models.Mock{}); err != nil {
		t.Fatalf("nil session EmitMock: %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	t.Parallel()
	s := New(Config{})
	defer s.Close()
	if s.cfg.Logger == nil {
		t.Fatalf("nil logger should be replaced")
	}
	if s.cfg.HangBudget != defaultHangBudget {
		t.Fatalf("hang budget default: got %s, want %s", s.cfg.HangBudget, defaultHangBudget)
	}
	if s.cfg.MemCap != defaultMemCap {
		t.Fatalf("mem cap default: got %d, want %d", s.cfg.MemCap, defaultMemCap)
	}
}

func TestConcurrentBumpAndPending(t *testing.T) {
	t.Parallel()
	// Stress test: parallel BumpActivity + MarkPendingWork should
	// not race (run under -race to catch). Not asserting results,
	// just that nothing explodes.
	s := New(Config{
		Logger:     zap.NewNop(),
		HangBudget: 5 * time.Second,
	})
	defer s.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.BumpActivity()
					s.MarkPendingWork()
					s.ClearPendingWork()
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
