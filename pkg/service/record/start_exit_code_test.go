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
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
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

// A docker compose recording whose agent never answered its health check
// fails, saying so, and stops with that reason: utils.Stop refuses a stop
// with none.
func TestStart_AComposeAgentThatIsNeverReadyFailsTheRun(t *testing.T) {
	t.Setenv("KEPLOY_AGENT_READY_TIMEOUT", "1")
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
	r := exitCodeRecorder(newFakeInstr())
	r.config.CommandType = string(utils.DockerCompose)
	r.config.Agent.AgentURI = "http://" + addr + "/agent"
	core, logs := observer.New(zapcore.ErrorLevel)
	r.logger = zap.New(core)

	err, code := startForExitCode(t, ctx, r)
	if code != utils.ExitKeployError || err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("Start returned %v with exit code %d; want the agent's not being ready, and 1", err, code)
	}
	if said := logs.FilterMessage("keploy-agent did not become ready in time").Len(); said != 1 {
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
	t.Setenv("KEPLOY_AGENT_READY_TIMEOUT", "60")
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
