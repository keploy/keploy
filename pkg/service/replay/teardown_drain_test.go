package replay

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/teardowntest"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// gaveUp reports whether logs say the drain of group gave it up.
func gaveUp(logs *observer.ObservedLogs, group string) func() bool {
	return func() bool {
		for _, e := range logs.FilterMessageSnippet("teardown drain made no progress").All() {
			if e.ContextMap()["group"] == group {
				return true
			}
		}
		return false
	}
}

// stoppingAgentInstr is an agent whose start registers its stop on the run's
// group, as the agent client's does, and then fails: the run goes straight
// to its teardown, whose drain of the group waits on that stop.
type stoppingAgentInstr struct {
	startInstrumentation
	stop *teardowntest.Stop
}

func (s stoppingAgentInstr) Setup(ctx context.Context, _ string, _ models.SetupOptions) error {
	g := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	g.Go(func() error { s.stop.Run(ctx); return nil })
	return errors.New("the keploy agent could not start")
}

// The run's drain of its goroutines ("replay") waits on a stop of the agent
// that keeps moving the run's teardown, past any time, and gives one that
// stops moving up on its bound of keploy's runnable time. It gave up at 30 s
// of wall time, with the agent's stop still going (E-C5 12051, grpc-linux).
func TestTheRunsDrainWaitsWhileItsTeardownMoves(t *testing.T) {
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })
	start := func(t *testing.T) (*teardowntest.Stop, *stall.ManualClock, *observer.ObservedLogs, <-chan struct{}) {
		core, logs := observer.New(zap.InfoLevel)
		stop := teardowntest.NewStop()
		t.Cleanup(stop.Release)
		sets := func(context.Context) ([]string, error) { return []string{"test-set-0"}, nil }
		r := NewReplayer(zap.New(core), startTestDB{list: sets}, nil, startReportDB{}, nil, nil, startTelemetry{},
			stoppingAgentInstr{stop: stop}, nil, &config.Config{Command: "true"}).(*Replayer)
		clk := &stall.ManualClock{}
		r.drainClock = clk
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			_ = r.Start(context.Background())
		}()
		return stop, clk, logs, ended
	}
	t.Run("while the agent's stop moves", func(t *testing.T) {
		stop, clk, logs, ended := start(t)
		teardowntest.CheckWaitsWhileItMoves(t, clk, teardownDrainStall, stop, gaveUp(logs, "replay"), ended)
	})
	t.Run("once it stops moving", func(t *testing.T) {
		stop, clk, logs, ended := start(t)
		teardowntest.CheckGivesUpOnTheBound(t, clk, teardownDrainStall, stop, gaveUp(logs, "replay"), ended)
	})
}

// stoppingAppInstr is the partial-run harness's agent whose application, once
// the set's context ends, is a stop that moves the set's teardown at each
// step the test gives it. onStop, set, is called as that stop begins.
type stoppingAppInstr struct {
	*prInstr
	stop   *teardowntest.Stop
	onStop func()
}

func (s stoppingAppInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	if s.onStop != nil {
		go func() {
			<-ctx.Done()
			s.onStop()
		}()
	}
	s.stop.Run(ctx)
	return models.AppError{AppErrorType: models.ErrCtxCanceled, ExitCode: -1}
}

// A test set's end waits on the stop of its application while the set's
// teardown moves, past any time, and gives one that stops moving up on its
// bound of keploy's runnable time, in either switch mode: the stop the set's
// end gives it before it reads the application's last calls (with the switch
// off, after the set's post-script; under it, before the set is judged)
// waits as the set's drain of its goroutines ("replay-testset") does, and a
// stop it gave up is not waited the bound again by that drain. Waited for
// without a bound, a wedged stop held keploy test before the set's report
// for good. One the run's user stops while it waits is the drain's to wait
// out, as before: the set's end stops waiting, and the drain waits while the
// stop moves.
func TestATestSetsDrainWaitsWhileItsTeardownMoves(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			start := func(t *testing.T, userStops bool) (*teardowntest.Stop, *stall.ManualClock, *observer.ObservedLogs, <-chan struct{}) {
				provenOn(t, proven)
				h := newPartialRunHarness(t, 2, 0)
				core, logs := observer.New(zap.InfoLevel)
				h.replayer.logger = zap.New(core)
				stop := teardowntest.NewStop()
				t.Cleanup(stop.Release)
				ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
				t.Cleanup(cancel)
				instr := stoppingAppInstr{prInstr: h.instr, stop: stop}
				if userStops {
					instr.onStop = cancel
				}
				h.replayer.instrumentation = instr
				clk := &stall.ManualClock{}
				h.replayer.drainClock = clk
				ended := make(chan struct{})
				go func() {
					defer close(ended)
					_, _ = h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
				}()
				return stop, clk, logs, ended
			}
			t.Run("while the app's stop moves", func(t *testing.T) {
				stop, clk, logs, ended := start(t, false)
				teardowntest.CheckWaitsWhileItMoves(t, clk, testSetDrainStall, stop, gaveUp(logs, "replay-testset"), ended)
			})
			t.Run("once it stops moving", func(t *testing.T) {
				stop, clk, logs, ended := start(t, false)
				teardowntest.CheckGivesUpOnTheBound(t, clk, testSetDrainStall, stop, gaveUp(logs, "replay-testset"), ended)
				if n := len(logs.FilterMessageSnippet("teardown drain made no progress").All()); n != 1 {
					t.Fatalf("the stop was given up %d times, want once", n)
				}
			})
			t.Run("while it moves after the run's user stopped the run", func(t *testing.T) {
				stop, clk, logs, ended := start(t, true)
				teardowntest.CheckWaitsWhileItMoves(t, clk, testSetDrainStall, stop, gaveUp(logs, "replay-testset"), ended)
			})
		})
	}
}

// awaitAppExit, the wait of a set's end (or the run's last set's, for the app
// --keep-app-alive keeps) for the exit of the application it stopped: true
// once it exited, after a stop that moved for as long as it took; false, said
// as the drain's give-up of its group, once the stop made no progress for the
// bound; false, and nothing said, once its ctx ended.
func TestAWaitForAnAppsExitIsBoundAsItsDrainIs(t *testing.T) {
	start := func(t *testing.T, ctx context.Context) (*teardowntest.Stop, *stall.ManualClock, *observer.ObservedLogs, <-chan struct{}, *atomic.Bool) {
		core, logs := observer.New(zap.InfoLevel)
		r := &Replayer{logger: zap.New(core)}
		clk := &stall.ManualClock{}
		r.drainClock = clk
		scopeCtx, scope := utils.NewTeardownScope(context.Background())
		appCtx, stopApp := context.WithCancel(scopeCtx)
		stop := teardowntest.NewStop()
		t.Cleanup(stop.Release)
		exited := make(chan struct{})
		go func() {
			defer close(exited)
			stop.Run(appCtx)
		}()
		stopApp()
		ended := make(chan struct{})
		var sawExit atomic.Bool
		go func() {
			defer close(ended)
			sawExit.Store(r.awaitAppExit(ctx, exited, scope, "the-group", time.Minute))
		}()
		return stop, clk, logs, ended, &sawExit
	}
	t.Run("an exit after a stop that moved", func(t *testing.T) {
		stop, clk, logs, ended, sawExit := start(t, context.Background())
		teardowntest.CheckWaitsWhileItMoves(t, clk, time.Minute, stop, gaveUp(logs, "the-group"), ended)
		if !sawExit.Load() {
			t.Fatal("the wait said the application did not exit; it did")
		}
	})
	t.Run("a stop that made no progress", func(t *testing.T) {
		stop, clk, logs, ended, sawExit := start(t, context.Background())
		teardowntest.CheckGivesUpOnTheBound(t, clk, time.Minute, stop, gaveUp(logs, "the-group"), ended)
		if sawExit.Load() {
			t.Fatal("the wait said the application exited; it was given up")
		}
	})
	t.Run("a ctx that ended", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, logs, ended, sawExit := start(t, ctx)
		select {
		case <-ended:
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("the wait went on after its ctx ended")
		}
		if sawExit.Load() || gaveUp(logs, "the-group")() {
			t.Fatalf("the wait said the application exited (%v) or gave it up (%v); its ctx ended first", sawExit.Load(), gaveUp(logs, "the-group")())
		}
	})
}
