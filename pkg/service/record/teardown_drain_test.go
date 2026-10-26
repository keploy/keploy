package record

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/teardowntest"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// stoppingInstr is fakeInstr whose app (Run), and whose agent's setup
// goroutine (registered on the setup's group, as the agent client registers
// its own), are each, when set, a stop that moves the recording's teardown
// at each step the test gives it once the recording stops.
type stoppingInstr struct {
	*fakeInstr
	app, setup *teardowntest.Stop
	running    chan struct{}
	once       sync.Once
}

func (s *stoppingInstr) Setup(ctx context.Context, _ string, _ models.SetupOptions) error {
	if s.setup != nil {
		g := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
		g.Go(func() error { s.setup.Run(ctx); return nil })
	}
	return nil
}

func (s *stoppingInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	s.once.Do(func() { close(s.running) })
	if s.app != nil {
		s.app.Run(ctx)
	} else {
		<-ctx.Done()
	}
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

// stoppingRecording starts a recording on instr, stops it once its app runs,
// and returns the clock its stop's drains measure a stall on, its logs, and
// a channel closed once Start has returned.
func stoppingRecording(t *testing.T, instr *stoppingInstr) (*stall.ManualClock, *observer.ObservedLogs, <-chan struct{}) {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	// On a failure, what the recording said: the order its stop's drains
	// ran in.
	t.Cleanup(func() {
		if t.Failed() {
			for _, e := range logs.All() {
				t.Logf("%s %s %v", e.Time.Format("15:04:05.000"), e.Message, e.ContextMap())
			}
		}
	})
	r := exitCodeRecorder(instr)
	r.logger = zap.New(core)
	clk := &stall.ManualClock{}
	r.drainClock = clk
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = startForExitCode(t, ctx, r)
	}()
	select {
	case <-instr.running:
	case <-ended:
		t.Fatal("the recording ended before its app ran")
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the recording's app never ran")
	}
	cancel() // the stop
	return clk, logs, ended
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

// The stop's drain of the app's goroutine ("record-app") waits on the app's
// stop while the recording's teardown moves, past any time, and gives one
// that stops moving up on its bound of keploy's runnable time. It gave up at
// a fixed 30 s of wall time.
func TestTheAppsDrainWaitsWhileTheRecordingsTeardownMoves(t *testing.T) {
	t.Run("while the app's stop moves", func(t *testing.T) {
		stop := teardowntest.NewStop()
		t.Cleanup(stop.Release)
		clk, logs, ended := stoppingRecording(t, &stoppingInstr{fakeInstr: newFakeInstr(), app: stop, running: make(chan struct{})})
		teardowntest.CheckWaitsWhileItMoves(t, clk, teardownDrainStall, stop, gaveUp(logs, "record-app"), ended)
	})
	t.Run("once it stops moving", func(t *testing.T) {
		stop := teardowntest.NewStop()
		t.Cleanup(stop.Release)
		clk, logs, ended := stoppingRecording(t, &stoppingInstr{fakeInstr: newFakeInstr(), app: stop, running: make(chan struct{})})
		teardowntest.CheckGivesUpOnTheBound(t, clk, teardownDrainStall, stop, gaveUp(logs, "record-app"), ended)
	})
}

// The stop's drain of the agent's setup goroutines ("record-setup") waits on
// the agent's stop while the recording's teardown moves, past any time, and
// gives one that stops moving up on its bound of keploy's runnable time.
func TestTheSetupsDrainWaitsWhileTheRecordingsTeardownMoves(t *testing.T) {
	t.Run("while the agent's stop moves", func(t *testing.T) {
		stop := teardowntest.NewStop()
		t.Cleanup(stop.Release)
		clk, logs, ended := stoppingRecording(t, &stoppingInstr{fakeInstr: newFakeInstr(), setup: stop, running: make(chan struct{})})
		teardowntest.CheckWaitsWhileItMoves(t, clk, teardownDrainStall, stop, gaveUp(logs, "record-setup"), ended)
	})
	t.Run("once it stops moving", func(t *testing.T) {
		stop := teardowntest.NewStop()
		t.Cleanup(stop.Release)
		clk, logs, ended := stoppingRecording(t, &stoppingInstr{fakeInstr: newFakeInstr(), setup: stop, running: make(chan struct{})})
		teardowntest.CheckGivesUpOnTheBound(t, clk, teardownDrainStall, stop, gaveUp(logs, "record-setup"), ended)
	})
}
