package replay

import (
	"context"
	"errors"
	"testing"

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
// step the test gives it.
type stoppingAppInstr struct {
	*prInstr
	stop *teardowntest.Stop
}

func (s stoppingAppInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	s.stop.Run(ctx)
	return models.AppError{AppErrorType: models.ErrCtxCanceled, ExitCode: -1}
}

// A test set's drain of its goroutines ("replay-testset") waits on the stop
// of its application while the set's teardown moves, past any time, and
// gives one that stops moving up on its bound of keploy's runnable time.
func TestATestSetsDrainWaitsWhileItsTeardownMoves(t *testing.T) {
	start := func(t *testing.T) (*teardowntest.Stop, *stall.ManualClock, *observer.ObservedLogs, <-chan struct{}) {
		h := newPartialRunHarness(t, 2, 0)
		core, logs := observer.New(zap.InfoLevel)
		h.replayer.logger = zap.New(core)
		stop := teardowntest.NewStop()
		t.Cleanup(stop.Release)
		h.replayer.instrumentation = stoppingAppInstr{prInstr: h.instr, stop: stop}
		clk := &stall.ManualClock{}
		h.replayer.drainClock = clk
		ended := make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
		t.Cleanup(cancel)
		go func() {
			defer close(ended)
			_, _ = h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
		}()
		return stop, clk, logs, ended
	}
	t.Run("while the app's stop moves", func(t *testing.T) {
		stop, clk, logs, ended := start(t)
		teardowntest.CheckWaitsWhileItMoves(t, clk, testSetDrainStall, stop, gaveUp(logs, "replay-testset"), ended)
	})
	t.Run("once it stops moving", func(t *testing.T) {
		stop, clk, logs, ended := start(t)
		teardowntest.CheckGivesUpOnTheBound(t, clk, testSetDrainStall, stop, gaveUp(logs, "replay-testset"), ended)
	})
}
