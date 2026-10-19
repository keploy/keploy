package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/internal/teardowntest"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	stallclock "go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// stoppingAgentInstr is an agent whose start registers its stop on the run's
// group, as the agent client registers its own, and then fails: the run goes
// straight to its end, whose drain of the group waits on that stop.
type stoppingAgentInstr struct {
	*composeInstr
	stop *teardowntest.Stop
}

func (s stoppingAgentInstr) Setup(ctx context.Context, _ string, _ models.SetupOptions) error {
	g := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	g.Go(func() error { s.stop.Run(ctx); return nil })
	return errors.New("the keploy agent could not start")
}

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

// The drain at a mock run's end ("mock-record", "mock-replay") waits on the
// agent's stop while the run's teardown moves, past any time, and gives one
// that stops moving up on its bound of keploy's runnable time. It gave up at
// a fixed 30 s of wall time.
func TestAMockRunsDrainWaitsWhileItsTeardownMoves(t *testing.T) {
	for _, run := range []struct {
		group string
		start func(Service, context.Context) error
	}{
		{"mock-record", func(s Service, ctx context.Context) error { return s.Record(ctx) }},
		{"mock-replay", func(s Service, ctx context.Context) error { return s.Replay(ctx) }},
	} {
		start := func(t *testing.T) (*teardowntest.Stop, *stallclock.ManualClock, *observer.ObservedLogs, <-chan struct{}) {
			t.Setenv(provenswitch.Env, "0") // this tests the paths with the proven switch off
			saved := utils.ErrCode
			t.Cleanup(func() { utils.ErrCode = saved })
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			if err := os.MkdirAll(filepath.Join(keployDir, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			stop := teardowntest.NewStop()
			t.Cleanup(stop.Release)
			inner := newInstr(t, agentUpFromSetup, false, models.AppError{})
			cfg := instrConfig(inner, utils.Native, "true")
			cfg.Path = keployDir
			cfg.Mock.OnMiss = "fail"
			core, logs := observer.New(zap.InfoLevel)
			svc := New(zap.New(core), stoppingAgentInstr{composeInstr: inner, stop: stop}, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg)
			clk := &stallclock.ManualClock{}
			svc.(*mockService).drainClock = clk
			ended := make(chan struct{})
			go func() {
				defer close(ended)
				_ = run.start(svc, context.Background())
			}()
			return stop, clk, logs, ended
		}
		t.Run(run.group+", while the agent's stop moves", func(t *testing.T) {
			stop, clk, logs, ended := start(t)
			teardowntest.CheckWaitsWhileItMoves(t, clk, teardownDrainStall, stop, gaveUp(logs, run.group), ended)
		})
		t.Run(run.group+", once it stops moving", func(t *testing.T) {
			stop, clk, logs, ended := start(t)
			teardowntest.CheckGivesUpOnTheBound(t, clk, teardownDrainStall, stop, gaveUp(logs, run.group), ended)
		})
	}
}
