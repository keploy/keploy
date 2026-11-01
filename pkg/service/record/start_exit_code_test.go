package record

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// exitCodeRecorder is a Recorder over instr, with stores that accept
// everything.
func exitCodeRecorder(instr Instrumentation) *Recorder {
	return &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{},
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
		frameQuiet:      5 * time.Millisecond,
	}
}

// startForExitCode runs Start to its end and returns its error and the exit
// code the process would end with. A Start that hangs is go test's -timeout
// to catch, with every goroutine's stack.
func startForExitCode(t *testing.T, ctx context.Context, r *Recorder) (error, int) {
	t.Helper()
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	err := r.Start(ctx)
	return err, utils.ErrCode
}

func newFakeInstr() *fakeInstr {
	return &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
}

// setupFailsInstr is an agent that never comes up: its Setup fails the way
// AgentClient.Setup does, after arming arm (0: nothing) as the agent client
// arms the agent's own exit code.
type setupFailsInstr struct {
	*fakeInstr
	err error
	arm int
}

func (s *setupFailsInstr) Setup(context.Context, string, models.SetupOptions) error {
	if s.arm != 0 {
		utils.SetExitCodeOnce(s.arm)
	}
	return s.err
}

// A recording whose agent could not start, or whose environment could not be
// set up, ends the process non-zero, with the exit code the failure is tagged
// with, and its error names the cause. It exited 0 (#4772): `keploy record`
// logged the error and returned nil, so a CI job whose agent never loaded its
// eBPF went green with nothing recorded.
func TestStart_AnAgentThatCannotStartFailsTheRun(t *testing.T) {
	exited := errors.New("exit status 1")
	for _, tc := range []struct {
		name  string
		err   error
		arm   int
		want  int
		names string
	}{
		{"the agent exited before it was ready", fmt.Errorf("the keploy agent exited before it became ready: %w", exited), 0, utils.ExitKeployError, "exit status 1"},
		{"the agent lacked privileges", fmt.Errorf("%w: the keploy agent could not start (exit status 3)", utils.ErrPrivilegeRequired), 0, utils.ExitPrivilegeRequired, "kernel privileges"},
		{"the environment lacked tracefs", fmt.Errorf("%w: the keploy agent could not start (exit status 6)", utils.ErrEnvironmentUnsupported), 0, utils.ExitEnvironmentUnsupported, "lacks something"},
		{"a code the agent client armed is kept", errors.New("the keploy agent could not start"), utils.ExitPrivilegeRequired, utils.ExitPrivilegeRequired, "could not start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := exitCodeRecorder(&setupFailsInstr{fakeInstr: newFakeInstr(), err: tc.err, arm: tc.arm})
			err, code := startForExitCode(t, context.Background(), r)
			if code != tc.want {
				t.Fatalf("exit code %d, want %d", code, tc.want)
			}
			if !errors.Is(err, tc.err) || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("Start returned %v; want it to carry the setup's error (%v), which names the cause", err, tc.err)
			}
		})
	}
}

// interruptAtFramesInstr is an agent that comes up, and the user's Ctrl+C
// lands as the recording opens its frame streams.
type interruptAtFramesInstr struct {
	*fakeInstr
	interrupt context.CancelFunc
}

func (i *interruptAtFramesInstr) GetIncoming(ctx context.Context, o models.IncomingOptions) (<-chan *models.TestCase, error) {
	i.interrupt()
	return i.fakeInstr.GetIncoming(ctx, o)
}

// interruptAtComposeWaitInstr is a docker compose stack whose agent never gets
// ready, and the user's Ctrl+C lands as the recording waits for it.
type interruptAtComposeWaitInstr struct {
	*fakeInstr
	interrupt context.CancelFunc
}

func (i *interruptAtComposeWaitInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	i.interrupt()
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

// interruptedSetupFailsInstr's Setup fails because the user stopped it.
type interruptedSetupFailsInstr struct {
	*fakeInstr
	interrupt context.CancelFunc
}

func (i *interruptedSetupFailsInstr) Setup(ctx context.Context, _ string, _ models.SetupOptions) error {
	i.interrupt()
	return fmt.Errorf("failed to start the agent: %w", context.Canceled)
}

// A recording the user stopped is not a failed one, even where Start returns
// an error for the stop: no exit code is armed. Start ends ctx itself as it
// returns, so the stop has to be told from a failure before that.
func TestStart_AStoppedRecordingIsNotAFailure(t *testing.T) {
	t.Run("stopped as it waited for its compose agent", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable) // never ready
		}))
		defer agent.Close()
		r := exitCodeRecorder(&interruptAtComposeWaitInstr{fakeInstr: newFakeInstr(), interrupt: cancel})
		r.config.CommandType = string(utils.DockerCompose)
		r.config.Agent.AgentURI = agent.URL
		err, code := startForExitCode(t, ctx, r)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start returned %v; the test needs the path that returns the stop as an error", err)
		}
		if code != 0 {
			t.Fatalf("exit code %d, want 0: the user stopped this recording", code)
		}
	})
	// Once the agent's frame streams are open, a stop drains what it
	// captured, as any stop does, and Start returns nil.
	t.Run("stopped as its frames opened", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := exitCodeRecorder(&interruptAtFramesInstr{fakeInstr: newFakeInstr(), interrupt: cancel})
		err, code := startForExitCode(t, ctx, r)
		if err != nil || code != 0 {
			t.Fatalf("Start returned %v with exit code %d, want nil and 0: the user stopped this recording", err, code)
		}
	})
	// Start returns nil for this stop and never reaches the arming; pinned so
	// that a stop during the setup keeps ending the run with no error and 0.
	t.Run("stopped during its setup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := exitCodeRecorder(&interruptedSetupFailsInstr{fakeInstr: newFakeInstr(), interrupt: cancel})
		err, code := startForExitCode(t, ctx, r)
		if err != nil || code != 0 {
			t.Fatalf("Start returned %v with exit code %d, want nil and 0: the user stopped this recording", err, code)
		}
	})
}

// appEndsInstr is an agent that comes up and an app that ends on its own,
// with appErr, once the recording runs. Its frame streams end when keploy says
// it is shutting down, as an agent's do once it is stopped, so the drain does
// not wait out its quiet grace.
type appEndsInstr struct {
	*fakeInstr
	appErr models.AppError
	once   sync.Once
}

func (a *appEndsInstr) Run(context.Context, models.RunOptions) models.AppError { return a.appErr }

func (a *appEndsInstr) NotifyGracefulShutdown(context.Context) error {
	a.once.Do(func() {
		close(a.mappings)
		close(a.incoming)
		close(a.outgoing)
	})
	return nil
}

// An app that died while keploy recorded it ends the recording as failed,
// with the app's own exit code (utils/exitcodes.go: the wrapped command's
// code, verbatim, as `keploy mock` mirrors it) and its cause named: a crash,
// its own non-zero code, SIGKILL or SIGSEGV (137, 139). An app whose end has
// no code of its own is a generic failure; one that exited 0, or was stopped
// by SIGTERM or SIGINT (143, 130: `docker stop`, `kill` or `kill -INT` on it),
// ends the recording normally, with no code.
func TestStart_AnAppThatDiesFailsTheRecording(t *testing.T) {
	for _, tc := range []struct {
		name  string
		app   models.AppError
		want  int
		names string
	}{
		{"it exited 2", models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 2"), ExitCode: 2}, 2, "exit status 2"},
		{"SIGKILL killed it", models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("signal: killed"), ExitCode: 137}, 137, "signal: killed"},
		{"it crashed (SIGSEGV)", models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("signal: segmentation fault"), ExitCode: 139}, 139, "segmentation fault"},
		{"its command did not run", models.AppError{AppErrorType: models.ErrCommandError, Err: errors.New("exit status 127"), ExitCode: 127}, 127, "exit status 127"},
		{"it ended with no code", models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("wait: no child processes"), ExitCode: -1}, utils.ExitKeployError, "no child processes"},
		// As the --from-container path reports a container that failed: no
		// code of its own, left 0.
		{"its container failed, with no code", models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("failed while waiting for the app container")}, utils.ExitKeployError, "waiting for the app container"},
		{"keploy could not hook it", models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("attach failed"), ExitCode: 2}, utils.ExitKeployError, "attach failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err, code := startForExitCode(t, context.Background(), exitCodeRecorder(&appEndsInstr{fakeInstr: newFakeInstr(), appErr: tc.app}))
			if code != tc.want {
				t.Fatalf("exit code %d, want %d", code, tc.want)
			}
			if err == nil || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("Start returned %v; want it to name how the app ended (%q)", err, tc.names)
			}
		})
	}

	for name, app := range map[string]models.AppError{
		"an app that exited 0":                             {AppErrorType: models.ErrAppStopped},
		"an app SIGTERM stopped":                           {AppErrorType: models.ErrUnExpected, Err: errors.New("signal: terminated"), ExitCode: 143},
		"an app SIGINT stopped":                            {AppErrorType: models.ErrUnExpected, Err: errors.New("signal: interrupt"), ExitCode: 130},
		"a docker run client that docker stop ended (143)": {AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 143"), ExitCode: 143},
	} {
		t.Run(name, func(t *testing.T) {
			err, code := startForExitCode(t, context.Background(), exitCodeRecorder(&appEndsInstr{fakeInstr: newFakeInstr(), appErr: app}))
			if err != nil || code != 0 {
				t.Fatalf("Start returned %v with exit code %d, want nil and 0: the recording ended normally", err, code)
			}
		})
	}
}

// startWatchingInstr is fakeInstr whose agent's start the watch gives up on
// (pkg.AgentStartWatcher): it says the start stopped making progress.
type startWatchingInstr struct {
	*fakeInstr
	watched atomic.Int32
}

func (s *startWatchingInstr) WatchAgentStart(ctx context.Context) <-chan error {
	s.watched.Add(1)
	out := make(chan error, 1)
	out <- fmt.Errorf("%w: its start took no step across 30 of its beats", pkg.ErrAgentNotReady)
	return out
}

// A docker compose recording whose agent's start stopped making progress
// before it answered its health check (pkg.WatchAgentStart) fails, saying so,
// and stops with that reason: utils.Stop refuses a stop with none. No budget
// of wall time ends the wait: the watch of the start does.
func TestStart_AComposeAgentWhoseStartStoppedFailsTheRun(t *testing.T) {
	// The process's stop, as utils.NewCtx sets it, so that utils.Stop gets as
	// far as the reason.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	utils.SetCancel(stop)
	t.Cleanup(func() { utils.SetCancel(nil) })
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	_ = closed.Close()
	instr := &startWatchingInstr{fakeInstr: newFakeInstr()}
	r := exitCodeRecorder(instr)
	r.config.CommandType = string(utils.DockerCompose)
	r.config.Agent.AgentURI = "http://" + addr + "/agent"
	core, logs := observer.New(zapcore.ErrorLevel)
	r.logger = zap.New(core)

	err, code := startForExitCode(t, ctx, r)
	if code != utils.ExitKeployError || !errors.Is(err, pkg.ErrAgentNotReady) || !strings.Contains(err.Error(), "did not come up") {
		t.Fatalf("Start returned %v with exit code %d; want the agent's stalled start, and 1", err, code)
	}
	if instr.watched.Load() != 1 {
		t.Fatalf("the agent's start was watched %d times, want once", instr.watched.Load())
	}
	if said := logs.FilterMessage("the keploy-agent docker compose started did not come up").Len(); said != 1 {
		t.Fatalf("said %d times; want the stop's reason said once", said)
	}
	if refused := logs.FilterMessage("failed stopping keploy").Len(); refused != 0 {
		t.Fatalf("the stop was refused %d times: it was given no reason", refused)
	}
	if ctx.Err() == nil {
		t.Fatal("the run was not stopped")
	}
}

// composeDiesInstr is a docker compose run whose project stops before the
// agent is ready: Run returns appErr, and agentFailure is what the agent's
// container says (nil: the agent was not what stopped). arm is the code the
// agent client arms for the agent's own exit (0: none).
type composeDiesInstr struct {
	*fakeInstr
	appErr       models.AppError
	agentFailure error
	arm          int
}

func (c *composeDiesInstr) Run(context.Context, models.RunOptions) models.AppError { return c.appErr }

func (c *composeDiesInstr) ComposeAgentFailure() error {
	if c.agentFailure != nil && c.arm != 0 {
		utils.SetExitCodeOnce(c.arm)
	}
	return c.agentFailure
}

// A docker compose recording whose project stops before the agent is ready
// fails at once: with the agent's own reason and code when the agent is what
// stopped (it could not start), else with the project's own code. It used to
// wait out the agent's whole readiness budget and fail as "not ready in
// time", naming neither.
func TestStart_AComposeProjectThatStopsBeforeTheAgentIsReadyFailsAtOnce(t *testing.T) {
	// A budget the run must not wait out: the test fails on the reason if it
	// does.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	_ = closed.Close()
	dependencyFailed := models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("dependency failed to start: container keploy-v3 exited (6)"), ExitCode: 1}
	for _, tc := range []struct {
		name  string
		instr *composeDiesInstr
		want  int
		names string
	}{
		{"the agent could not start", &composeDiesInstr{appErr: dependencyFailed, arm: utils.ExitEnvironmentUnsupported,
			agentFailure: fmt.Errorf("%w: neither debugfs nor tracefs are mounted", utils.ErrEnvironmentUnsupported)},
			utils.ExitEnvironmentUnsupported, "neither debugfs nor tracefs are mounted"},
		{"the project died on its own", &composeDiesInstr{appErr: models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 17"), ExitCode: 17}},
			17, "compose project stopped"},
		// The project stopped by SIGTERM (its compose or wrapper process
		// signalled): no failure, and no wait.
		{"the project was stopped", &composeDiesInstr{appErr: models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 143"), ExitCode: 143}},
			0, ""},
		// The agent's failure wins over a 143 compose gave the project
		// stopping it over the dead agent.
		{"the agent could not start, and compose stopped the project with 143", &composeDiesInstr{appErr: models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 143"), ExitCode: 143},
			arm: utils.ExitEnvironmentUnsupported, agentFailure: fmt.Errorf("%w: neither debugfs nor tracefs are mounted", utils.ErrEnvironmentUnsupported)},
			utils.ExitEnvironmentUnsupported, "neither debugfs nor tracefs are mounted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.instr.fakeInstr = newFakeInstr()
			r := exitCodeRecorder(tc.instr)
			r.config.CommandType = string(utils.DockerCompose)
			r.config.Agent.AgentURI = "http://" + addr + "/agent"
			err, code := startForExitCode(t, context.Background(), r)
			if tc.names == "" {
				if err != nil || code != 0 {
					t.Fatalf("Start returned %v with exit code %d; want a stopped project to end the run at once, with nil and 0", err, code)
				}
				return
			}
			if err == nil || strings.Contains(err.Error(), "did not become ready") || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("Start returned %v; want it to name %q at once, not wait out the agent's readiness budget", err, tc.names)
			}
			if code != tc.want {
				t.Fatalf("exit code %d, want %d", code, tc.want)
			}
		})
	}
}

// agentDiesInstr is a compose recording whose project's end, read after the
// app's (ComposeEndAfterReady), says the agent crashed after it was ready
// (died; nil when the app's own end stands -- composeEndAfterReady decides
// that). askedWith is the app's exit record passed.
type agentDiesInstr struct {
	*appEndsInstr
	died      error
	askedWith []int
}

func (a *agentDiesInstr) ComposeEndAfterReady(appExit int) error {
	a.askedWith = append(a.askedWith, appExit)
	return a.died
}

// An app compose stopped over a dead agent ends with 143 like an app that
// was stopped, but the run is the agent's failure: it fails, naming the
// agent, with keploy's own code, not 0 and not the app's code.
func TestStart_AnAgentThatDiesUnderComposeFailsTheRecording(t *testing.T) {
	for name, tc := range map[string]struct {
		app  models.AppError
		died string
	}{
		// Compose reports 0 for an app it never started, in a race: the
		// agent's crash must still fail the run, not end it as clean.
		"compose says 0 for an app it never started": {models.AppError{AppErrorType: models.ErrAppStopped}, "the keploy agent stopped before compose started the app: the keploy-agent container keploy-v3-x exited with code 2"},
		"compose stopped the app (143)":              {models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 143"), ExitCode: 143}, "the keploy agent stopped while the app was running: the keploy-agent container keploy-v3-x exited with code 2"},
		"the namespace took the app (137)":           {models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 137"), ExitCode: 137}, "the keploy agent stopped while the app was running: the keploy-agent container keploy-v3-x exited with code 2"},
	} {
		app := tc.app
		t.Run(name, func(t *testing.T) {
			died := errors.New(tc.died)
			instr := &agentDiesInstr{appEndsInstr: &appEndsInstr{fakeInstr: newFakeInstr(), appErr: app}, died: died}
			err, code := startForExitCode(t, context.Background(), exitCodeRecorder(instr))
			if err == nil || !errors.Is(err, died) {
				t.Fatalf("Start returned %v; want the agent's failure (%v)", err, died)
			}
			if code != utils.ExitKeployError {
				t.Fatalf("exit code %d, want %d: the agent's failure is keploy's", code, utils.ExitKeployError)
			}
			if len(instr.askedWith) != 1 || instr.askedWith[0] != app.ExitCode {
				t.Fatalf("asked whether the agent died with %v, want the app's exit %d", instr.askedWith, app.ExitCode)
			}
		})
	}

	// An app that ran and ended with a code of its own keeps it: the
	// project's end does not override it (died is nil).
	for name, tc := range map[string]struct {
		app  models.AppError
		want int
	}{
		"the app exited 42": {models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 42"), ExitCode: 42}, 42},
		"the app exited 0":  {models.AppError{AppErrorType: models.ErrAppStopped}, 0},
	} {
		t.Run(name+", and compose then killed a slow agent", func(t *testing.T) {
			instr := &agentDiesInstr{appEndsInstr: &appEndsInstr{fakeInstr: newFakeInstr(), appErr: tc.app}}
			_, code := startForExitCode(t, context.Background(), exitCodeRecorder(instr))
			if code != tc.want {
				t.Fatalf("exit code %d, want the app's own %d", code, tc.want)
			}
		})
	}
}

// agentDiesAfterReadyInstr is an agent this client started that comes up and
// then dies mid-recording, before the recording is stopped: exited is the
// channel its process's exit lands on (AgentExit), as the client's reaper
// sends it, and it is sent from GetIncoming, once the recording has wired
// its streams. The app runs on: nothing of its own ends the recording.
type agentDiesAfterReadyInstr struct {
	*fakeInstr
	exited  chan error
	exitErr error
	once    sync.Once
}

func (a *agentDiesAfterReadyInstr) AgentExit() <-chan error { return a.exited }

func (a *agentDiesAfterReadyInstr) GetIncoming(ctx context.Context, o models.IncomingOptions) (<-chan *models.TestCase, error) {
	a.once.Do(func() { a.exited <- a.exitErr })
	return a.fakeInstr.GetIncoming(ctx, o)
}

func (a *agentDiesAfterReadyInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

// taggedExit is an agent exit tagged with a code of its own, as the agent
// process arms it (utils/exitcodes.go): what an *exec.ExitError is to the
// real client.
type taggedExit struct{ code int }

func (e taggedExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e taggedExit) ExitCode() int { return e.code }

// An agent that dies after it became ready, before the recording is stopped,
// fails the recording: with a stop reason naming the agent's exit, and with
// the code the exit is tagged with (1 when it carries none). It used to keep
// going with nothing captured and end 0 on the user's stop (#4790).
func TestStart_AnAgentThatDiesAfterReadyFailsTheRecording(t *testing.T) {
	for _, tc := range []struct {
		name  string
		exit  error
		want  int
		names string
	}{
		{"it crashed", errors.New("exit status 2"), utils.ExitKeployError, "exit status 2"},
		{"SIGKILL killed it", errors.New("signal: killed"), utils.ExitKeployError, "signal: killed"},
		{"it lacked privileges", taggedExit{code: utils.ExitPrivilegeRequired}, utils.ExitPrivilegeRequired, "kernel privileges"},
		{"the environment could not run it", taggedExit{code: utils.ExitEnvironmentUnsupported}, utils.ExitEnvironmentUnsupported, "environment lacks"},
		{"it exited cleanly on its own", nil, utils.ExitKeployError, "it exited on its own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instr := &agentDiesAfterReadyInstr{fakeInstr: newFakeInstr(), exited: make(chan error, 1), exitErr: tc.exit}
			r := exitCodeRecorder(instr)
			r.config.CommandType = string(utils.Native)
			err, code := startForExitCode(t, context.Background(), r)
			if code != tc.want {
				t.Fatalf("exit code %d, want %d", code, tc.want)
			}
			if err == nil || !strings.Contains(err.Error(), "the keploy agent exited before the recording was stopped") || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("Start returned %v; want the agent's exit, named (%q)", err, tc.names)
			}
		})
	}
}

// agentDiesAsItStopsInstr is an agent whose death and the user's stop land
// together: its Run stops the recording first, then reports the exit.
type agentDiesAsItStopsInstr struct {
	*fakeInstr
	exited chan error
	stop   context.CancelFunc
}

func (a *agentDiesAsItStopsInstr) AgentExit() <-chan error { return a.exited }

func (a *agentDiesAsItStopsInstr) Run(context.Context, models.RunOptions) models.AppError {
	a.stop()
	a.exited <- errors.New("signal: killed")
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

// The agent dying as the recording is stopped is the stop, not a failure:
// the user's Ctrl+C ends the run, and it exits 0.
func TestStart_AnAgentThatDiesAsTheRecordingStopsIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	instr := &agentDiesAsItStopsInstr{fakeInstr: newFakeInstr(), exited: make(chan error, 1), stop: cancel}
	r := exitCodeRecorder(instr)
	r.config.CommandType = string(utils.Native)
	err, code := startForExitCode(t, ctx, r)
	if err != nil || code != 0 {
		t.Fatalf("Start returned %v with exit code %d, want nil and 0: the user stopped this recording", err, code)
	}
}

// dockerRunAgentDiesInstr is a docker run recording whose app dies and whose
// agent died with it (the app shares the agent container's PID namespace):
// exited carries the agent's exit, sent from GetIncoming, before the app's
// own end is read.
type dockerRunAgentDiesInstr struct {
	*appEndsInstr
	exited  chan error
	exitErr error
	once    sync.Once
}

func (a *dockerRunAgentDiesInstr) AgentExit() <-chan error { return a.exited }

func (a *dockerRunAgentDiesInstr) GetIncoming(ctx context.Context, o models.IncomingOptions) (<-chan *models.TestCase, error) {
	a.once.Do(func() { a.exited <- a.exitErr })
	return a.fakeInstr.GetIncoming(ctx, o)
}

// A docker run recording whose app was killed because the agent died names
// the agent, with keploy's own code: the app's 137 is the agent's death seen
// through the app. And an app SIGKILL killed on its own, agent alive, keeps
// the app's own code and blame.
func TestStart_ADockerRunAppKilledWithItsAgentBlamesTheAgent(t *testing.T) {
	killed := models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("signal: killed"), ExitCode: 137}
	t.Run("the agent died with it", func(t *testing.T) {
		instr := &dockerRunAgentDiesInstr{
			appEndsInstr: &appEndsInstr{fakeInstr: newFakeInstr(), appErr: killed},
			exited:       make(chan error, 1),
			exitErr:      errors.New("signal: killed"),
		}
		r := exitCodeRecorder(instr)
		r.config.CommandType = string(utils.DockerRun)
		err, code := startForExitCode(t, context.Background(), r)
		if code != utils.ExitKeployError {
			t.Fatalf("exit code %d, want %d: the agent's death is keploy's failure", code, utils.ExitKeployError)
		}
		if err == nil || !strings.Contains(err.Error(), "the keploy agent exited before the recording was stopped") {
			t.Fatalf("Start returned %v; want the agent named, not the app's 137", err)
		}
	})
	t.Run("the app was killed alone", func(t *testing.T) {
		instr := &appEndsInstr{fakeInstr: newFakeInstr(), appErr: killed}
		r := exitCodeRecorder(instr)
		r.config.CommandType = string(utils.DockerRun)
		err, code := startForExitCode(t, context.Background(), r)
		if code != 137 || err == nil || strings.Contains(err.Error(), "keploy agent") {
			t.Fatalf("Start returned %v with exit code %d; want the app's own 137", err, code)
		}
	})
}

// A mock.real keploy cannot read refuses the recording before anything runs
// (proven switch), and ends the process non-zero, as a recording whose agent
// cannot start does: never a green CI job that recorded nothing.
func TestStart_AnUnreadableMockRealFailsTheRun(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	cfg := &config.Config{}
	cfg.Mock.Real = []string{"http://db:5432"}
	r := &Recorder{logger: zap.NewNop(), config: cfg, instrumentation: newFakeInstr()}
	err, code := startForExitCode(t, context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "mock.real") {
		t.Fatalf("Start returned %v, want the refusal of mock.real", err)
	}
	if code == 0 {
		t.Fatal("a recording keploy refused for its mock.real exits 0")
	}
}

// closingMockDB is a mock store that writes in the background and is closed
// to write the rest (as the gob writer is): closeErr is what Close says.
type closingMockDB struct {
	recMockDB
	closeErr error
	mu       sync.Mutex
	closes   int
}

func (d *closingMockDB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closes++
	return d.closeErr
}

// storeClosedHooks notes whether the mock store was closed by the time the
// end-of-recording hook ran.
type storeClosedHooks struct {
	BaseRecordHooks
	store        *closingMockDB
	ran          bool
	closesAtHook int
}

func (h *storeClosedHooks) AfterRecordingComplete(context.Context, *RecordingCompleteContext) error {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.ran, h.closesAtHook = true, h.store.closes
	return nil
}

// A mock store that could not write mocks the recording captured (its
// writer stuck on a slow disk) leaves the recording incomplete: the run
// fails, as for any capture loss. It used to be a log line at the very end,
// and the run exited 0. The store writes the rest before the end-of-recording
// hook reads the mock file; one that could not skips the hook.
func TestStart_MocksTheStoreCouldNotWriteFailTheRecording(t *testing.T) {
	for name, tc := range map[string]struct {
		closeErr error
		want     int
	}{
		"written":     {nil, 0},
		"not written": {fmt.Errorf("%w: the gob mock writer made no progress", models.ErrMocksNotWritten), utils.ExitKeployError},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := exitCodeRecorder(&interruptAtFramesInstr{fakeInstr: newFakeInstr(), interrupt: cancel})
			store := &closingMockDB{recMockDB: recMockDB{unencodable: map[string]bool{}}, closeErr: tc.closeErr}
			r.mockDB = store
			hooks := &storeClosedHooks{store: store}
			r.hooks = hooks
			err, code := startForExitCode(t, ctx, r)
			if err != nil || code != tc.want {
				t.Fatalf("Start returned %v with exit code %d, want nil and %d", err, code, tc.want)
			}
			if store.closes == 0 {
				t.Fatal("the mock store was never closed")
			}
			if lost := r.captureLost.Load(); lost != (tc.closeErr != nil) {
				t.Fatalf("the recording is incomplete: %v, want %v", lost, tc.closeErr != nil)
			}
			// The summary, said before the deferred cleanup, says so.
			tel := r.telemetry.(*recTelemetry)
			wantStatus := map[bool]string{false: "completed", true: "incomplete"}[tc.closeErr != nil]
			if !tel.sessionRecorded || tel.sessionStatus != wantStatus {
				t.Fatalf("the session summary (said: %v) says %q, want %q", tel.sessionRecorded, tel.sessionStatus, wantStatus)
			}
			// The agent lost nothing: the reason is the store's loss alone.
			if want := "mocks the recording captured were not written to the mock file"; tc.closeErr != nil && tel.sessionReason != want {
				t.Fatalf("the session summary's reason is %q, want %q", tel.sessionReason, want)
			}
			if tc.closeErr == nil && (!hooks.ran || hooks.closesAtHook == 0) {
				t.Fatalf("the end-of-recording hook ran: %v, with the store closed %d times; want it run after the store wrote the rest", hooks.ran, hooks.closesAtHook)
			}
			if tc.closeErr != nil && hooks.ran {
				t.Fatal("the end-of-recording hook ran over a mock file the store could not finish")
			}
		})
	}
}

// A loss the deferred cleanup finds (the store closed only there, as on a
// record drain that did not join) fails the run too, after the summary.
func TestStart_AMockLossTheCleanupFindsFailsTheRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := exitCodeRecorder(&interruptAtFramesInstr{fakeInstr: newFakeInstr(), interrupt: cancel})
	r.RegisterCleanup(func() error {
		return fmt.Errorf("%w: the gob mock writer made no progress", models.ErrMocksNotWritten)
	})
	err, code := startForExitCode(t, ctx, r)
	if err != nil || code != utils.ExitKeployError {
		t.Fatalf("Start returned %v with exit code %d, want nil and %d", err, code, utils.ExitKeployError)
	}
}

// stuckAtStopInstr is an agent that, once the user's Ctrl+C has landed, says
// it still holds what it captured before the stop (BacklogBefore) and never
// moves on it: each frame drain stalls on that backlog (holdForBacklog), a
// loss of the agent's, whatever the scheduling.
type stuckAtStopInstr struct{ *interruptAtFramesInstr }

func (*stuckAtStopInstr) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return models.AgentBacklog{Verdict: models.BacklogPending}, nil
}

// A recording that lost both what the agent held and mocks the store could
// not write says both in its summary: neither loss hides the other.
func TestStart_TheSummaryNamesTheAgentsLossAndTheStores(t *testing.T) {
	const agents = "the agent's captures from before the stop were not all handed over"
	for name, tc := range map[string]struct {
		closeErr error
		want     string
	}{
		"the store wrote":         {nil, agents},
		"the store did not write": {fmt.Errorf("%w: the gob mock writer made no progress", models.ErrMocksNotWritten), agents + ", and mocks the recording captured were not written to the mock file"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := exitCodeRecorder(&stuckAtStopInstr{&interruptAtFramesInstr{fakeInstr: newFakeInstr(), interrupt: cancel}})
			r.mockDB = &closingMockDB{recMockDB: recMockDB{unencodable: map[string]bool{}}, closeErr: tc.closeErr}
			err, code := startForExitCode(t, ctx, r)
			if err != nil || code != utils.ExitKeployError {
				t.Fatalf("Start returned %v with exit code %d, want nil and %d", err, code, utils.ExitKeployError)
			}
			tel := r.telemetry.(*recTelemetry)
			if tel.sessionStatus != "incomplete" || tel.sessionReason != tc.want {
				t.Fatalf("the session summary says %q (%q), want %q (%q)", tel.sessionStatus, tel.sessionReason, "incomplete", tc.want)
			}
		})
	}
}

// The gob store is a mockStoreWriter: without it, the stop closes it as a
// plain io.Closer, silently, with no second-interrupt escape.
var _ mockStoreWriter = (*mockdb.MockYaml)(nil)

// slowStore is a mock store that writes in the background and never
// finishes until it is told to stop.
type slowStore struct {
	recMockDB
	left uint64
}

func (s *slowStore) CloseUnless(stop <-chan struct{}) error {
	<-stop
	return fmt.Errorf("%w: the stop was asked to end now", models.ErrMocksNotWritten)
}

func (s *slowStore) MocksLeft() uint64 { return s.left }

// Writing the mock store's rest has no fixed bound, so the stop says how
// many mocks are left and, once it has said so, a second interrupt ends it:
// the rest is not written, and is not waited for again at the cleanup.
func TestCloseMockStore_SaysWhatIsLeftAndASecondInterruptEndsIt(t *testing.T) {
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	core, logs := observer.New(zapcore.InfoLevel)
	r := &Recorder{logger: zap.New(core), mockDB: &slowStore{recMockDB: recMockDB{unencodable: map[string]bool{}}, left: 7}}
	utils.MarkInterrupted() // the stop
	done := make(chan error, 1)
	go func() { done <- r.closeMockStore(r.mockDB, 20*time.Millisecond) }()
	deadline := time.Now().Add(30 * time.Second) // bounds only a stop that never offers
	for logs.FilterMessageSnippet("press Ctrl+C again").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the stop never offered to end the writing: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	said := logs.FilterMessageSnippet("Still writing the recording's mocks").All()
	if len(said) == 0 || said[0].ContextMap()["mocksLeft"] != uint64(7) {
		t.Fatalf("the stop said %v, want how many mocks are left (7)", said)
	}
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the user's press after the offer did not end the writing")
	}
	if err := <-done; !errors.Is(err, models.ErrMocksNotWritten) {
		t.Fatalf("closeMockStore returned %v, want models.ErrMocksNotWritten", err)
	}
	if !r.mockWriteStopped.Load() {
		t.Fatal("the cleanup would wait for the writing the user stopped")
	}
}

// closingWriter is a closingMockDB that writes in the background
// (mockStoreWriter), as the gob store does: waits counts the closes that
// waited for it (CloseUnless); closes, those that did not.
type closingWriter struct {
	closingMockDB
	waits int
}

func (w *closingWriter) CloseUnless(<-chan struct{}) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.waits++
	return w.closeErr
}

func (w *closingWriter) MocksLeft() uint64 { return 0 }

// The cleanup closes the mock store, which writes what it still holds,
// unless the user stopped that writing at a second interrupt. A store that
// writes in the background, the one a second interrupt stops, is waited for.
func TestMockStoreCleanup_DoesNotWaitForWritingTheUserStopped(t *testing.T) {
	for stopped, want := range map[bool]int{false: 1, true: 0} {
		store := &closingMockDB{recMockDB: recMockDB{unencodable: map[string]bool{}}}
		r := &Recorder{logger: zap.NewNop()}
		r.mockWriteStopped.Store(stopped)
		if err := r.mockStoreCleanup(store)(); err != nil {
			t.Fatal(err)
		}
		if store.closes != want {
			t.Errorf("stopped by the user: %v; the cleanup closed the store %d times, want %d", stopped, store.closes, want)
		}
		w := &closingWriter{closingMockDB: closingMockDB{recMockDB: recMockDB{unencodable: map[string]bool{}}}}
		r = &Recorder{logger: zap.NewNop()}
		r.mockWriteStopped.Store(stopped)
		if err := r.mockStoreCleanup(w)(); err != nil {
			t.Fatal(err)
		}
		if w.waits != want || w.closes != 0 {
			t.Errorf("stopped by the user: %v; the cleanup waited for the writer %d times and closed it without waiting %d times, want %d and 0", stopped, w.waits, w.closes, want)
		}
	}
}

// slowClosingStore is a slowStore that must be closed by being waited for
// (CloseUnless): Close says it was not.
type slowClosingStore struct{ slowStore }

var errClosedUnwaited = errors.New("the cleanup closed a background writer without saying what is left")

func (s *slowClosingStore) Close() error { return errClosedUnwaited }

// The cleanup writes what a drain that did not join left to the store as the
// stop's early close does: it says what is left, and a second interrupt ends
// it.
func TestMockStoreCleanup_SaysWhatIsLeftAndASecondInterruptEndsIt(t *testing.T) {
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	core, logs := observer.New(zapcore.InfoLevel)
	store := &slowClosingStore{slowStore{recMockDB: recMockDB{unencodable: map[string]bool{}}, left: 9}}
	r := &Recorder{logger: zap.New(core)}
	utils.MarkInterrupted() // the stop
	done := make(chan error, 1)
	go func() { done <- r.mockStoreCleanup(store)() }()
	deadline := time.Now().Add(30 * time.Second) // bounds only a cleanup that never offers
	for logs.FilterMessageSnippet("press Ctrl+C again").Len() == 0 {
		select {
		case err := <-done:
			t.Fatalf("the cleanup ended (%v) without offering to end the writing: %v", err, logs.All())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cleanup never offered to end the writing: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	said := logs.FilterMessageSnippet("Still writing the recording's mocks").All()
	if len(said) == 0 || said[0].ContextMap()["mocksLeft"] != uint64(9) {
		t.Fatalf("the cleanup said %v, want how many mocks are left (9)", said)
	}
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the user's press after the offer did not end the writing")
	}
	if err := <-done; !errors.Is(err, models.ErrMocksNotWritten) {
		t.Fatalf("the cleanup returned %v, want models.ErrMocksNotWritten", err)
	}
}

// watchCtxInstr is fakeInstr whose agent's start the watch follows and says
// nothing of: it hands over the context the watch was given.
type watchCtxInstr struct {
	*fakeInstr
	ctxs chan context.Context
}

func (w *watchCtxInstr) WatchAgentStart(ctx context.Context) <-chan error {
	w.ctxs <- ctx
	return nil
}

// A docker compose recording stops watching its agent's start once the
// agent answers its health check, while the recording goes on: the watch
// looks into the agent's container (docker inspect, docker cp) every second,
// and has nothing left to judge.
func TestStart_AComposeRecordingStopsWatchingItsAgentOnceItAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)
	instr := &watchCtxInstr{fakeInstr: newFakeInstr(), ctxs: make(chan context.Context, 1)}
	r := exitCodeRecorder(instr)
	r.config.CommandType = string(utils.DockerCompose)
	r.config.Agent.AgentURI = srv.URL + "/agent"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	var watch context.Context
	select {
	case watch = <-instr.ctxs:
	case err := <-done:
		t.Fatalf("Start returned %v before watching its agent's start", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the agent's start was not watched")
	}
	select {
	case <-watch.Done():
	case err := <-done:
		t.Fatalf("Start returned %v before it stopped watching its agent", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the agent answered its health check, and its start was still watched")
	}
	select {
	case err := <-done:
		t.Fatalf("Start returned %v: the watch ended with the recording, not once the agent answered", err)
	default:
	}
	cancel()
	<-done
}
