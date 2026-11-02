package mock

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/utils"
)

// stopNoticeInstrumentation is a test command that runs until its context
// ends (held, as a stop finds a call in flight), or returns at once (exits),
// and an agent that takes the stop's notice.
type stopNoticeInstrumentation struct {
	sendsInstrumentation
	held    bool
	running chan struct{}
	notices atomic.Int32
	// runCtx is the context the command runs on; endedFirst: it had ended
	// when the notice reached the agent.
	runCtx     context.Context
	endedFirst atomic.Bool
}

func (s *stopNoticeInstrumentation) Run(ctx context.Context, o models.RunOptions) models.AppError {
	s.runCtx = ctx
	close(s.running)
	if s.held {
		<-ctx.Done()
	}
	return s.sendsInstrumentation.Run(ctx, o)
}

func (s *stopNoticeInstrumentation) NotifyStopBegun(context.Context) error {
	if s.runCtx != nil && s.runCtx.Err() != nil {
		s.endedFirst.Store(true)
	}
	s.notices.Add(1)
	return nil
}

func recordWithin(ctx context.Context, t *testing.T, inst *stopNoticeInstrumentation, as ...Instrumentation) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Mock.Name = "stopped"
	cfg.Path = t.TempDir()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	db := mockdb.New(zap.NewNop(), cfg.Path, "mocks")
	returned := make(chan error, 1)
	var run Instrumentation = inst
	if len(as) > 0 {
		run = as[0]
	}
	go func() { returned <- New(zap.NewNop(), run, db, nil, FileStore{}, nil, cfg).Record(ctx) }()
	select {
	case <-returned:
	case <-time.After(2 * time.Minute):
		t.Fatal("keploy mock record did not return")
	}
}

// keploy mock record stopped by the user (or by its record timer, which stops
// the same context) tells the agent the recording's stop has begun before it
// ends the test command, so a call the command still had in flight, cut by
// the stop, records no fault. It ended the command first, and the agent read
// the cut call as the application's giving up.
func TestRecord_AStopTellsTheAgentBeforeItEndsTheTestCommand(t *testing.T) {
	inst := &stopNoticeInstrumentation{held: true, running: make(chan struct{}),
		sendsInstrumentation: sendsInstrumentation{out: make(chan *models.Mock)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-inst.running
		cancel() // the user's Ctrl+C, or the record timer's stop
	}()
	recordWithin(ctx, t, inst)
	if inst.endedFirst.Load() {
		t.Fatal("the test command was ended before the agent was told the recording's stop had begun")
	}
	if got := inst.notices.Load(); got != 1 {
		t.Fatalf("the agent was told %d times that the stop began, want once", got)
	}
}

// A test command that exits on its own ends the run with no stop to tell the
// agent of: keploy's teardown after it does not say the stop began (the agent
// reads the command's exit itself).
func TestRecord_ACommandThatExitsOnItsOwnTellsTheAgentNoStop(t *testing.T) {
	inst := &stopNoticeInstrumentation{running: make(chan struct{}),
		sendsInstrumentation: sendsInstrumentation{out: make(chan *models.Mock)}}
	recordWithin(context.Background(), t, inst)
	if got := inst.notices.Load(); got != 0 {
		t.Fatalf("a command that exited on its own told the agent %d times that a stop began", got)
	}
}

// failingAgentInstrumentation is stopNoticeInstrumentation whose agent fails
// while the command runs: the recording's errgroup ends, the user's context
// does not.
type failingAgentInstrumentation struct{ stopNoticeInstrumentation }

func (f *failingAgentInstrumentation) Setup(ctx context.Context, _ string, _ models.SetupOptions) error {
	if g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group); ok {
		g.Go(func() error {
			<-f.running
			return errors.New("the agent exited")
		})
	}
	return nil
}

// keploy's own failure while the command runs (the agent exited) ends the
// command at once, with no stop notice: no stop of the user's began, and a
// command held open would otherwise outlive the failed recording.
func TestRecord_KeploysOwnFailureEndsTheTestCommandWithNoNotice(t *testing.T) {
	inst := &failingAgentInstrumentation{stopNoticeInstrumentation{held: true, running: make(chan struct{}),
		sendsInstrumentation: sendsInstrumentation{out: make(chan *models.Mock)}}}
	recordWithin(context.Background(), t, &inst.stopNoticeInstrumentation, inst)
	if got := inst.notices.Load(); got != 0 {
		t.Fatalf("keploy's own failure told the agent %d times that a stop began", got)
	}
}
