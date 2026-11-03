package mock

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

// composeStopNoticeInstr is a compose project held open until it is torn down
// (composeInstr with runBlocks), whose agent takes the stop's notice.
type composeStopNoticeInstr struct {
	*composeInstr
	runCtx     atomic.Pointer[context.Context]
	notices    atomic.Int32
	endedFirst atomic.Bool
}

func (c *composeStopNoticeInstr) Run(ctx context.Context, o models.RunOptions) models.AppError {
	c.runCtx.Store(&ctx)
	return c.composeInstr.Run(ctx, o)
}

func (c *composeStopNoticeInstr) NotifyStopBegun(context.Context) error {
	if p := c.runCtx.Load(); p != nil && (*p).Err() != nil {
		c.endedFirst.Store(true)
	}
	c.notices.Add(1)
	return nil
}

// Under docker compose, keploy mock record stopped by the user tells the agent
// the recording's stop has begun before it tears the project down, as it does
// for a native command: the app runs in the project, and a call it had in
// flight, cut by the stop, records no fault. A replay stopped the same way
// tells the agent nothing: it records nothing.
func TestCompose_AStopTellsTheAgentBeforeItTearsTheProjectDown(t *testing.T) {
	for _, tc := range []struct {
		name        string
		run         func(Service, context.Context) error
		wantNotices int32
	}{
		{"record", func(s Service, ctx context.Context) error { return s.Record(ctx) }, 1},
		{"replay", func(s Service, ctx context.Context) error { return s.Replay(ctx) }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instr := newInstr(t, agentUpAfterRun, true, models.AppError{})
			inst := &composeStopNoticeInstr{composeInstr: instr}
			svc := New(zap.NewNop(), inst, stubMockDB{}, nil, nil, nil, instrConfig(instr, utils.DockerCompose, "docker compose up"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.run(svc, ctx) }()
			waitForEvent(t, instr, "ready", done, 60*time.Second)
			cancel() // the user's Ctrl+C, or the record timer's stop
			select {
			case <-done:
			case <-time.After(2 * time.Minute):
				t.Fatalf("keploy mock %s did not return after the stop", tc.name)
			}
			if inst.endedFirst.Load() {
				t.Fatal("the compose project was torn down before the agent was told the recording's stop had begun")
			}
			if got := inst.notices.Load(); got != tc.wantNotices {
				t.Fatalf("the agent was told %d times that the stop began, want %d", got, tc.wantNotices)
			}
		})
	}
}

// heldNotifier is an agent that takes the stop's notice and answers only once
// the row lets it (hold), so the row reads the command's context while the
// notice is still on its way.
type heldNotifier struct {
	Instrumentation
	hold    chan struct{}
	notices atomic.Int32
}

// Run is a test command that exits at once.
func (h *heldNotifier) Run(context.Context, models.RunOptions) models.AppError {
	return models.AppError{}
}

func (h *heldNotifier) NotifyStopBegun(context.Context) error {
	h.notices.Add(1)
	<-h.hold
	return nil
}

// runUntilStopNoted, step by step, in a synctest bubble: synctest.Wait returns
// once every goroutine the contexts' ends started has run as far as it can, so
// each read below follows every effect of the end before it, by event.
//   - The user's stop: the agent is told, and the command's context stays live
//     for as long as the notice is unanswered; it ends once the agent answered.
//   - keploy's own failure (the recording's context ends, the user's does not):
//     the command's context ends at once, and the agent is told nothing.
//   - A user's stop after the command's run returned (runStopNoted) tells the
//     agent nothing: no call of the command's is left to cut.
func TestRunUntilStopNoted_EndsTheCommandOnlyAfterAUsersStopIsTold(t *testing.T) {
	newRun := func() (*mockService, *heldNotifier, context.Context, context.CancelFunc, context.Context, context.CancelFunc) {
		n := &heldNotifier{hold: make(chan struct{})}
		m := &mockService{logger: zap.NewNop(), instrumentation: n, config: &config.Config{}}
		parent, stop := context.WithCancel(context.Background())
		ctx, fail := context.WithCancel(parent)
		return m, n, parent, stop, ctx, fail
	}
	t.Run("the user's stop", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m, n, parent, stop, ctx, fail := newRun()
			defer fail()
			runCtx, ran := m.runUntilStopNoted(parent, ctx)
			defer ran()
			stop()
			synctest.Wait()
			if got := n.notices.Load(); got != 1 {
				t.Fatalf("the user's stop told the agent %d times, want once", got)
			}
			if runCtx.Err() != nil {
				t.Fatal("the test command's context ended while the stop's notice was still unanswered")
			}
			close(n.hold)
			synctest.Wait()
			if runCtx.Err() == nil {
				t.Fatal("the test command's context did not end once the agent had taken the stop's notice")
			}
		})
	})
	t.Run("keploy's own failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m, n, parent, stop, ctx, fail := newRun()
			defer stop()
			close(n.hold)
			runCtx, ran := m.runUntilStopNoted(parent, ctx)
			defer ran()
			fail()
			synctest.Wait()
			if runCtx.Err() == nil {
				t.Fatal("keploy's own failure did not end the test command")
			}
			if got := n.notices.Load(); got != 0 {
				t.Fatalf("keploy's own failure told the agent %d times that a stop began", got)
			}
		})
	})
	t.Run("a stop after the run returned", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m, n, parent, stop, ctx, fail := newRun()
			defer fail()
			close(n.hold)
			m.runStopNoted(parent, ctx) // the command exits on its own
			stop()
			synctest.Wait()
			if got := n.notices.Load(); got != 0 {
				t.Fatalf("a stop after the command's run returned told the agent %d times", got)
			}
		})
	})
}
