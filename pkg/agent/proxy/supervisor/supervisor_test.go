package supervisor

import (
	"context"
	"errors"
	"fmt"
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

// fakeClock stands still until the test moves it, and its ticker never
// ticks: a Supervisor built on one checks for a hang only when the
// test calls checkHang, at a reading the test chose. Whether the
// watchdog fires is then decided by the test's steps alone, not by how
// the scheduler interleaved goroutines, and every check a test counts
// on is known to have run.
type fakeClock struct {
	t atomic.Int64
	// every is the interval the Supervisor asked its ticker for.
	every time.Duration
}

func (c *fakeClock) now() time.Duration { return time.Duration(c.t.Load()) }

// newTicker hands back a nil channel, which is never ready.
func (c *fakeClock) newTicker(d time.Duration) (<-chan time.Time, func()) {
	c.every = d
	return nil, func() {}
}

func (c *fakeClock) advance(d time.Duration) { c.t.Add(int64(d)) }

func (c *fakeClock) set(d time.Duration) { c.t.Store(int64(d)) }

// newFakeClockSupervisor returns a Supervisor with the given hang
// budget whose watchdog runs only when the test calls checkHang.
func newFakeClockSupervisor(t *testing.T, budget time.Duration) (*Supervisor, *fakeClock) {
	t.Helper()
	clk := &fakeClock{}
	s := newSupervisor(Config{Logger: zaptest.NewLogger(t), HangBudget: budget}, clk)
	t.Cleanup(s.Close)
	return s, clk
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
	// Parser-side decode/state errors must fall through to passthrough.
	// The bytes have already been forwarded by the relay; the parser's
	// inability to record a clean mock has no bearing on whether the
	// application's connection should survive. The dispatcher reads
	// FallthroughToPassthrough and skips relayCancel when true, leaving
	// the relay alive to forward subsequent traffic until peer close.
	// See pkg/agent/proxy/integrations/http/recordv2.go for the call
	// sites this contract protects (invalid Content-Length, gzip
	// decompression failure, malformed status line).
	if !res.FallthroughToPassthrough {
		t.Fatalf("fallthrough: got false, want true (parser errors must not tear down the application's connection)")
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

// With every check on time, the watchdog fires at the first check at
// which the pending request has gone longer than the budget without
// progress, and not a check sooner, wherever between two checks that
// progress happened.
func TestHangDetectedWhenPending(t *testing.T) {
	t.Parallel()
	const tick = 20 * time.Millisecond // an 80ms budget is checked every 20ms
	for _, tc := range []struct {
		name string
		// lastChunk is when the relay last forwarded a chunk; 0 means
		// only MarkPendingWork, when the request arrived.
		lastChunk time.Duration
		// firesAt is the first check more than 80ms after lastChunk.
		firesAt time.Duration
	}{
		{"no chunk after the request", 0, 100 * time.Millisecond},
		{"last chunk between checks", 25 * time.Millisecond, 120 * time.Millisecond},
		{"last chunk just before a check", 40 * time.Millisecond, 140 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			aborted := make(chan struct{})
			s, clk := newFakeClockSupervisor(t, 80*time.Millisecond)
			s.SessionOnAbort = func() { close(aborted) }

			// Arm pending work so the watchdog is eligible to fire.
			s.MarkPendingWork()

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

			// Walk the clock in 5ms steps, forwarding the last chunk when
			// its time comes and checking on every tick.
			for now := 5 * time.Millisecond; now <= tc.firesAt; now += 5 * time.Millisecond {
				clk.advance(5 * time.Millisecond)
				if now == tc.lastChunk {
					s.BumpActivity()
				}
				if now%tick != 0 {
					continue
				}
				if fired := s.checkHang(); fired != (now == tc.firesAt) {
					t.Fatalf("check at %v: fired=%v; want the first firing at %v, the first check more than 80ms after the chunk at %v",
						now, fired, tc.firesAt, tc.lastChunk)
				}
			}

			var res Result
			select {
			case res = <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("Run did not return within 5s of the watchdog firing")
			}
			if res.Status != StatusHung {
				t.Fatalf("status: got %s, want hung", res.Status)
			}
			if !res.FallthroughToPassthrough {
				t.Fatalf("fallthrough: got false, want true")
			}
			// Run invokes SessionOnAbort before it returns on the hang path.
			select {
			case <-aborted:
			default:
				t.Fatalf("SessionOnAbort was not invoked")
			}
		})
	}
}

// The watchdog checks every quarter of the budget, and charges a check
// at most that much, but never checks more often than minHangTick.
func TestWatchdogTicksEveryQuarterBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ budget, tick time.Duration }{
		{80 * time.Millisecond, 20 * time.Millisecond},
		{0, defaultHangBudget / 4},
		{12 * time.Millisecond, minHangTick},
	} {
		clk := &fakeClock{}
		s := newSupervisor(Config{Logger: zaptest.NewLogger(t), HangBudget: tc.budget}, clk)
		s.Close()
		if clk.every != tc.tick || s.wd.tick != tc.tick {
			t.Errorf("budget %v: ticker every %v, charging up to %v per check; want %v for both",
				tc.budget, clk.every, s.wd.tick, tc.tick)
		}
	}
}

// The fake-clock tests drive checkHang themselves. This one leaves the
// watchdog to New's own clock and ticker, the wiring production runs.
func TestHangDetectedOnRealClock(t *testing.T) {
	t.Parallel()
	s := New(shortCfg(t))
	s.MarkPendingWork()

	done := make(chan Result, 1)
	go func() {
		done <- s.Run(context.Background(),
			func(ctx context.Context, sess *Session) error {
				<-ctx.Done()
				return ctx.Err()
			},
			&Session{})
	}()

	// The watchdog should fire by 75ms: at the first check past the
	// 50ms budget, or the next one. The bound only catches a watchdog
	// that never fires, so it leaves a loaded runner a wide margin.
	select {
	case res := <-done:
		if res.Status != StatusHung {
			t.Fatalf("status: got %s, want hung", res.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("watchdog did not fire within 5s on a 50ms budget")
	}
}

func TestHangNotDetectedWhenIdle(t *testing.T) {
	t.Parallel()
	s, clk := newFakeClockSupervisor(t, 50*time.Millisecond) // a check every 12.5ms

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
	// Five budgets of checks; without MarkPendingWork none may fire.
	for i := 0; i < 20; i++ {
		clk.advance(12500 * time.Microsecond)
		if s.checkHang() {
			t.Fatalf("watchdog fired while idle, at %v", clk.now())
		}
	}

	// Cleanup: cancel via Close so the parser exits and the test finishes.
	s.Close()
	if r := <-done; r.Status == StatusHung {
		t.Fatalf("watchdog fired while idle: %+v", r)
	}
}

// A long-poll request makes no byte progress for far longer than the hang
// budget while the server holds the connection open. SuspendWatchdog disarms
// the hang detection for that connection so the parser is not aborted (which
// would fall through to passthrough and lose the mock), even though pending
// work is armed.
func TestHangNotDetectedWhenSuspended(t *testing.T) {
	t.Parallel()
	s, clk := newFakeClockSupervisor(t, 50*time.Millisecond) // a check every 12.5ms

	// Arm pending work (as the relay does when the request bytes are teed),
	// then suspend the watchdog (as the dispatcher does once the request is
	// matched to a poll lane).
	s.MarkPendingWork()
	s.SuspendWatchdog()

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
	// Pending is armed and five budgets of checks go by, but the
	// watchdog is suspended for this poll connection, so none may fire.
	for i := 0; i < 20; i++ {
		clk.advance(12500 * time.Microsecond)
		if s.checkHang() {
			t.Fatalf("watchdog fired while suspended, at %v", clk.now())
		}
	}

	s.Close()
	if r := <-done; r.Status == StatusHung {
		t.Fatalf("watchdog fired while suspended: %+v", r)
	}
}

func TestHangResetOnActivity(t *testing.T) {
	t.Parallel()
	s, clk := newFakeClockSupervisor(t, 80*time.Millisecond) // a check every 20ms
	s.MarkPendingWork()

	// The parser stays busy for 300ms, several budgets, while the relay
	// forwards a chunk between every two checks: the watchdog never sees
	// more than a tick of silence, so it must never fire.
	res := s.Run(context.Background(),
		func(ctx context.Context, sess *Session) error {
			for now := time.Duration(0); now < 300*time.Millisecond; now += 20 * time.Millisecond {
				clk.advance(10 * time.Millisecond)
				s.BumpActivity()
				clk.advance(10 * time.Millisecond)
				if s.checkHang() {
					return fmt.Errorf("watchdog fired at %v with a chunk every 20ms", clk.now())
				}
			}
			return nil
		},
		&Session{})

	if res.Status != StatusOK {
		t.Fatalf("expected OK with activity bumps, got %s (err=%v)", res.Status, res.Err)
	}
}

// processStall is a point at which a stall of the whole process — a GC
// pause, a SIGSTOP or debugger breakpoint, a paused container or VM, a
// host too loaded to run it — can catch a pending request under an 80ms
// budget, checked every 20ms.
type processStall struct {
	name string
	// lastChunk is when the relay last forwarded a chunk before the
	// stall; 0 means only MarkPendingWork, when the request arrived.
	lastChunk time.Duration
	// stallFrom is when the process stops running.
	stallFrom time.Duration
	// firesAfter is how many on-time checks after the stall it takes to
	// catch a parser that stays silent.
	firesAfter int
}

var processStalls = []processStall{
	// 20ms charged before the stall and 20ms for it; 60, 80, then 100ms.
	{"stall after a quiet tick", 0, 25 * time.Millisecond, 3},
	// The stall's check restarts the silence at 20ms; 40, 60, 80, 100ms.
	{"stall right after a chunk", 25 * time.Millisecond, 25 * time.Millisecond, 4},
	// 65ms charged before the stall, so the tick its check charges takes
	// the silence past the budget. That check holds the verdict; the
	// next one gives it.
	{"stall in the budget's last tick", 15 * time.Millisecond, 85 * time.Millisecond, 1},
}

// walkIntoStall takes s, pending, from the clock's reading through
// tc.stallFrom with a check every 20ms tick, then stalls the process
// for a second, twelve budgets, and runs the check that was overdue
// meanwhile. It fails the test if any of those checks fires.
func walkIntoStall(t *testing.T, s *Supervisor, clk *fakeClock, tc processStall) {
	t.Helper()
	base := clk.now()
	for d := 5 * time.Millisecond; d <= tc.stallFrom; d += 5 * time.Millisecond {
		clk.set(base + d)
		if d == tc.lastChunk {
			s.BumpActivity()
		}
		if d%(20*time.Millisecond) == 0 && s.checkHang() {
			t.Fatalf("fired %v in, before the stall", d)
		}
	}
	clk.advance(time.Second)
	if s.checkHang() {
		t.Fatalf("the first check after a 1s stall declared the parser hung")
	}
}

// When the process resumes, the watchdog's overdue check can run before
// the relay has forwarded what reached its sockets during the stall. So
// that check must neither charge the stall to the parser nor declare the
// hang; doing either aborts a paused agent's live parsers. Each case runs
// twice, so the second stall meets a watchdog already through the first.
func TestHangNotChargedForProcessStall(t *testing.T) {
	t.Parallel()
	for _, tc := range processStalls {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, clk := newFakeClockSupervisor(t, 80*time.Millisecond)
			s.MarkPendingWork()
			for range 2 {
				walkIntoStall(t, s, clk, tc)
				// The relay catches up with what arrived during the stall.
				s.BumpActivity()
			}
			clk.advance(20 * time.Millisecond)
			if s.checkHang() {
				t.Fatalf("fired a tick after the relay caught up")
			}
		})
	}
}

// Leaving a stall uncharged must not blind the watchdog: a parser still
// silent after the process resumes is caught once the checks after the
// stall have seen the rest of the budget go by.
func TestHangDetectedAfterProcessStall(t *testing.T) {
	t.Parallel()
	for _, tc := range processStalls {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, clk := newFakeClockSupervisor(t, 80*time.Millisecond)
			s.MarkPendingWork()
			walkIntoStall(t, s, clk, tc)
			for i := 1; i <= tc.firesAfter; i++ {
				clk.advance(20 * time.Millisecond)
				if fired := s.checkHang(); fired != (i == tc.firesAfter) {
					t.Fatalf("check %d after the stall: fired=%v; want the first firing at check %d",
						i, fired, tc.firesAfter)
				}
			}
		})
	}
}

// A watchdog that only ever runs late, on a host too starved to wake it
// on time, still catches a hung parser: each late check charges a tick,
// and the check after one that held the verdict gives it, late or not.
func TestHangDetectedWhenEveryCheckIsLate(t *testing.T) {
	t.Parallel()
	s, clk := newFakeClockSupervisor(t, 80*time.Millisecond) // a check every 20ms
	s.MarkPendingWork()

	// Checks 30ms apart, each charged 20ms: 20, 40, 60, 80ms are within
	// the budget, 100ms is past it but held, and 120ms declares the hang.
	for i, want := range []bool{false, false, false, false, false, true} {
		clk.advance(30 * time.Millisecond)
		if got := s.checkHang(); got != want {
			t.Fatalf("check %d: fired=%v, want %v", i+1, got, want)
		}
	}
}

// The ticker keeps to its schedule, so the check after a late one comes
// early. It must charge only the time since the late check: a full tick
// would declare a live parser hung before its budget ran out.
func TestHangCheckAfterALateOneChargesOnlyItsTime(t *testing.T) {
	t.Parallel()
	s, clk := newFakeClockSupervisor(t, 80*time.Millisecond) // a check every 20ms
	s.MarkPendingWork()

	clk.set(20 * time.Millisecond)
	if s.checkHang() {
		t.Fatalf("fired 20ms into an 80ms budget")
	}
	clk.set(45 * time.Millisecond)
	s.BumpActivity() // the last chunk

	// The check due at 40ms runs 18ms late, then the ticker is back on
	// its 20ms schedule. 120ms is 75ms after the chunk, within the
	// budget; 140ms is 95ms after it.
	for _, c := range []struct {
		at    time.Duration
		fires bool
	}{
		{58 * time.Millisecond, false},
		{60 * time.Millisecond, false},
		{80 * time.Millisecond, false},
		{100 * time.Millisecond, false},
		{120 * time.Millisecond, false},
		{140 * time.Millisecond, true},
	} {
		clk.set(c.at)
		if got := s.checkHang(); got != c.fires {
			t.Fatalf("check at %v: fired=%v, want %v (last chunk at 45ms, budget 80ms)", c.at, got, c.fires)
		}
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
// and OnPendingCleared must still fire. Under this test's setup —
// session not marked incomplete, RouteMocksViaSyncMock=true, Mocks
// non-nil — that combination identifies the syncMock branch. Note
// that the incomplete-mock drop path would also produce "Mocks
// untouched + OnPendingCleared fired" if the session were marked
// incomplete, so the invariant is conditional on the clean-session
// precondition this test sets up.
//
// Earlier iterations of this test rebound the package-singleton
// syncMock's outChan via SetOutputChannel(...). That touched a
// global visible across every test in the same test process, so
// parallel tests or t.Parallel() subtests in this package that
// also call SetOutputChannel could race on the outChan pointer
// and produce flaky timeouts. (Cross-package `go test ./...` runs
// each package in its own binary, so the race was strictly
// intra-package.) Asserting on the local Session.Mocks channel
// avoids rebinding that global outChan — the syncMock manager's
// other state (buffered mocks, firstReqSeen) is still touched
// because RouteMocksViaSyncMock=true routes through it, but none
// of that state is read by this test's assertions.
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

	// Parser blocks forever on ctx; the watchdog should fire by 75ms,
	// and the bound only catches one that never does.
	done := make(chan Result, 1)
	go func() {
		done <- s.Run(context.Background(),
			func(ctx context.Context, sess *Session) error {
				<-ctx.Done()
				return ctx.Err()
			},
			&Session{})
	}()
	select {
	case res := <-done:
		if res.Status != StatusHung {
			t.Fatalf("status: got %s, want hung", res.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("watchdog did not fire within 5s on a 50ms budget")
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

// SuspendWatchdog disables ONLY no-progress hang detection. A suspended
// (poll-lane) connection must still be aborted on a mem-cap breach — the
// per-connection buffer cap protects memory regardless of poll status.
func TestMemCapAbortsEvenWhenSuspended(t *testing.T) {
	t.Parallel()
	s := New(shortCfg(t))
	s.MarkPendingWork()
	s.SuspendWatchdog()

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
			t.Fatalf("status: got %s, want mem_cap (suspend must not disable mem-cap protection)", res.Status)
		}
		if !res.FallthroughToPassthrough {
			t.Fatalf("fallthrough: got false, want true")
		}
	case <-time.After(time.Second):
		t.Fatalf("mem cap did not abort a suspended parser")
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
	s, clk := newFakeClockSupervisor(t, 50*time.Millisecond) // a check every 12.5ms
	s.MarkPendingWork()
	// Immediately clear; subsequent quiet period should not fire.
	s.ClearPendingWork()

	res := s.Run(context.Background(),
		func(ctx context.Context, sess *Session) error {
			// Quiet for 4 budgets; should NOT be classified as hung.
			for i := 0; i < 16; i++ {
				clk.advance(12500 * time.Microsecond)
				if s.checkHang() {
					return fmt.Errorf("watchdog fired at %v with no pending work", clk.now())
				}
			}
			return nil
		},
		&Session{})

	if res.Status != StatusOK {
		t.Fatalf("status: got %s, want ok (err=%v)", res.Status, res.Err)
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
