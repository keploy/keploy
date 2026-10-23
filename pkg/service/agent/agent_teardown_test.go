package agent

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// busyAtLoad loads as loadedHooks does, and keeps whether a stage of teardown
// work was in progress while it loaded (utils.TeardownBusy).
type busyAtLoad struct {
	loadedHooks
	busy *bool
}

func (h busyAtLoad) Load(context.Context, coreAgent.HookCfg, config.Agent) error {
	*h.busy = utils.TeardownBusy()
	return nil
}

// busyAtStart starts as startedProxy does, and keeps whether a stage of
// teardown work was in progress while it started.
type busyAtStart struct {
	startedProxy
	busy *bool
}

func (p *busyAtStart) StartProxy(ctx context.Context, opts coreAgent.ProxyOptions) error {
	*p.busy = utils.TeardownBusy()
	return p.startedProxy.StartProxy(ctx, opts)
}

// Hook's start -- the hooks' load, the proxy's start and its CA install -- is
// teardown work while a stop waits for it, and only where the agent sees its
// progress (utils.TeardownProgressSeen): a stop during it is then kept from
// killing the agent part way while the start makes progress, and a start that
// counted as progress throughout, where none can be seen, would keep a
// wedged one from ever being killed. The stage ends with the start, before
// Hook returns: a stage left open would count as teardown work in progress
// for the rest of the agent's life.
func TestHooksStartIsTeardownWorkOnlyWhereItsProgressIsSeen(t *testing.T) {
	if utils.TeardownBusy() {
		t.Fatal("a stage of teardown work is in progress before the agent starts")
	}
	var atLoad, atStart bool
	a := &Agent{logger: zap.NewNop(), Hooks: busyAtLoad{busy: &atLoad}, Proxy: &busyAtStart{busy: &atStart}, config: &config.Config{}}
	grp, ctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, grp))
	defer func() {
		cancel()
		_ = grp.Wait()
	}()
	if err := a.Hook(ctx, models.HookOptions{Mode: models.MODE_TEST}); err != nil {
		t.Fatalf("the agent did not start: %v", err)
	}
	seen := utils.TeardownProgressSeen()
	if atLoad != seen {
		t.Fatalf("the hooks' load counted as teardown work: %v; want %v, as the agent sees its progress: %v", atLoad, seen, seen)
	}
	if atStart != seen {
		t.Fatalf("the proxy's start counted as teardown work: %v; want %v, as the agent sees its progress: %v", atStart, seen, seen)
	}
	if utils.TeardownBusy() {
		t.Fatal("the start's stage of teardown work is still in progress once Hook has returned")
	}
}

// heldStage is a stage of the agent's teardown that the test holds open, as a
// CA tool rewriting the trust store, or the kernel removing a uprobe, can
// take long: it runs on the group its ctx carries (models.ErrGroupKey), as the
// proxy's and the hooks' goroutines do, begins once that ctx is done, and
// ends once released.
type heldStage struct {
	release chan struct{}
	once    sync.Once
	done    atomic.Bool
}

func newHeldStage(t *testing.T) *heldStage {
	s := &heldStage{release: make(chan struct{})}
	t.Cleanup(s.end) // a failed test leaves no goroutine held
	return s
}

func (s *heldStage) end() { s.once.Do(func() { close(s.release) }) }

func (s *heldStage) start(ctx context.Context) {
	ctx.Value(models.ErrGroupKey).(*errgroup.Group).Go(func() error {
		<-ctx.Done()
		<-s.release
		s.done.Store(true)
		return nil
	})
}

// heldStartProxy starts a held stage as its start (the proxy's start(), whose
// teardown removes the CA), and then fails when
// told to, as StartProxy does when an auxiliary hook fails (AfterStart) with
// start() running and the CA installed. started is closed once it has.
type heldStartProxy struct {
	coreAgent.Proxy // embedded: nil, so any other call panics loudly
	stage           *heldStage
	fail            error
	started         chan struct{}
}

func (p *heldStartProxy) StartProxy(ctx context.Context, _ coreAgent.ProxyOptions) error {
	p.stage.start(ctx)
	close(p.started)
	return p.fail
}

func (p *heldStartProxy) SetAuxiliaryHook(coreAgent.AuxiliaryProxyHook) {}

// stoppedWhileLoading loads hooks as the Linux hooks do: the stop comes while
// they load, and their unload -- a held stage -- joins its group only once
// the load is done (hooks/linux Load adds its unload goroutine after
// h.load). Here the load ends once the teardown has begun.
type stoppedWhileLoading struct {
	coreAgent.Hooks // embedded: nil, so any other call panics loudly
	stage           *heldStage
	stop            context.CancelFunc
}

func (h stoppedWhileLoading) Load(ctx context.Context, _ coreAgent.HookCfg, _ config.Agent) error {
	before := utils.TeardownSteps()
	h.stop()
	for utils.TeardownSteps() == before { // the teardown has begun
		runtime.Gosched()
	}
	h.stage.start(ctx)
	return nil
}

// stoppedWhileStarting is a proxy the stop reaches while it starts: start(),
// a held stage, joins its group only once the stop has cancelled the proxy's
// context, as StartProxy launches start() after its CA install, which the
// stop cuts short. started is closed once StartProxy is entered.
type stoppedWhileStarting struct {
	coreAgent.Proxy // embedded: nil, so any other call panics loudly
	stage           *heldStage
	started         chan struct{}
}

func (p *stoppedWhileStarting) StartProxy(ctx context.Context, _ coreAgent.ProxyOptions) error {
	close(p.started)
	<-ctx.Done()
	p.stage.start(ctx)
	return ctx.Err()
}

func (p *stoppedWhileStarting) SetAuxiliaryHook(coreAgent.AuxiliaryProxyHook) {}

// setupParked reports whether a goroutine is parked in Agent.Setup's wait on
// its errgroup -- Setup waiting for its teardown -- by the state and stack of
// every goroutine. Parked, not passing through: a wait on a group with
// nothing left in it does not park.
func setupParked() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range bytes.Split(buf, []byte("\n\n")) {
		header, _, _ := bytes.Cut(g, []byte("\n"))
		parked := bytes.Contains(header, []byte("semacquire")) || bytes.Contains(header, []byte("WaitGroup.Wait"))
		if parked && bytes.Contains(g, []byte("errgroup.(*Group).Wait")) && bytes.Contains(g, []byte(".(*Agent).Setup")) {
			return true
		}
	}
	return false
}

// Setup returns only once what it started has been torn down, however it
// returns: the agent process exits as soon as it does (main), and an exit
// cuts short the proxy's teardown -- the CA's removal from the trust store --
// and the hooks' unload. A stop that came before
// the port handoff, or while the hooks loaded or the proxy started, returned
// at once; so did a proxy that failed after it had started. And Hook's
// teardown waited on the proxy's and the hooks' groups before Hook had added
// to them what a stop during the load or the start leaves there.
func TestSetupReturnsOnlyOnceItsTeardownIsDone(t *testing.T) {
	defer func(f string) { agentReadyFile = f }(agentReadyFile)
	agentReadyFile = filepath.Join(t.TempDir(), "agent.ready")
	for _, tc := range []struct {
		name string
		// arrange returns the agent, and runs the stop (when the case has
		// one).
		arrange func(t *testing.T, stage *heldStage, cancel context.CancelFunc) *Agent
		// wantErr says Setup reports a failure.
		wantErr bool
	}{
		{
			name: "stopped before the port handoff",
			arrange: func(t *testing.T, stage *heldStage, cancel context.CancelFunc) *Agent {
				p := &heldStartProxy{stage: stage, started: make(chan struct{})}
				go func() { <-p.started; cancel() }()
				return &Agent{Hooks: loadedHooks{}, Proxy: p}
			},
		},
		{
			name: "stopped while the hooks load",
			arrange: func(t *testing.T, stage *heldStage, cancel context.CancelFunc) *Agent {
				return &Agent{Hooks: stoppedWhileLoading{stage: stage, stop: cancel}}
			},
			wantErr: true,
		},
		{
			name: "stopped while the proxy starts",
			arrange: func(t *testing.T, stage *heldStage, cancel context.CancelFunc) *Agent {
				p := &stoppedWhileStarting{stage: stage, started: make(chan struct{})}
				go func() { <-p.started; cancel() }()
				return &Agent{Hooks: loadedHooks{}, Proxy: p}
			},
			wantErr: true,
		},
		{
			name: "the proxy failed after it started",
			arrange: func(t *testing.T, stage *heldStage, cancel context.CancelFunc) *Agent {
				p := &heldStartProxy{stage: stage, started: make(chan struct{}), fail: errors.New("auxiliary proxy hook failed: the BPF verifier rejected the program")}
				return &Agent{Hooks: loadedHooks{}, Proxy: p}
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage := newHeldStage(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := tc.arrange(t, stage, cancel)
			a.logger = zap.NewNop()
			a.config = &config.Config{}

			type result struct {
				err          error
				doneAtReturn bool
			}
			returned := make(chan result, 1)
			go func() {
				err := a.Setup(ctx, make(chan int)) // nothing takes the port
				returned <- result{err, stage.done.Load()}
			}()

			// Setup returns (it did not wait), or parks waiting for its
			// teardown, which the held stage holds.
			for !setupParked() {
				select {
				case r := <-returned:
					t.Fatalf("Setup returned (%v) while the teardown it started was still running: the agent process exits on that return and cuts the teardown short", r.err)
				default:
				}
				runtime.Gosched()
			}
			stage.end()
			r := <-returned
			if !r.doneAtReturn {
				t.Fatal("Setup returned before its teardown had finished")
			}
			if (r.err != nil) != tc.wantErr {
				t.Fatalf("Setup returned %v; want an error: %v", r.err, tc.wantErr)
			}
		})
	}
}
