package record

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// exitingInstr is fakeInstr whose application ends the way the test says, as
// soon as the recording is armed.
type exitingInstr struct {
	*fakeInstr
	appErr models.AppError
}

func (e *exitingInstr) Run(context.Context, models.RunOptions) models.AppError { return e.appErr }

func newExitTestRecorder(instr Instrumentation, cfg *config.Config) *Recorder {
	return &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{},
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          cfg,
	}
}

func newSilentFakeInstr() *fakeInstr {
	return &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
}

// startWithin runs Start and fails the test if it does not return in time: a
// wedged teardown must surface as a named failure, not a CI timeout.
func startWithin(t *testing.T, ctx context.Context, r *Recorder) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return")
		return nil
	}
}

// realExitError is the *exec.ExitError a real process exiting with code
// produces -- the thing app.Run wraps into the AppError it reports.
func realExitError(t *testing.T, code int) error {
	t.Helper()
	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/c"
	}
	err := exec.Command(shell, flag, fmt.Sprintf("exit %d", code)).Run()
	var ee *exec.ExitError
	require.ErrorAs(t, err, &ee)
	return err
}

// When the user's application exits, Start's error has to still BE that exit:
// the CLI turns it into `keploy record`'s own exit code, which needs the
// AppError's type and the application's code, and a caller reading the raw
// process status needs the *exec.ExitError. It used to be fmt.Errorf("%s",
// stopReason), which kept the words and threw all of that away.
//
// The words themselves do not change -- they are what the log line and every
// wrapper that prints Start's error show.
func TestStart_AnAppThatExitsIsReturnedAsItsAppError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       models.AppErrorType
		stopReason string
	}{
		{"exited while recording", models.ErrUnExpected,
			"user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected"},
		{"failed to start", models.ErrCommandError,
			"error in running the user application, hence stopping keploy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exitErr := realExitError(t, 3)
			instr := &exitingInstr{fakeInstr: newSilentFakeInstr(), appErr: models.AppError{
				AppErrorType: tc.kind,
				Err:          exitErr,
				ExitCode:     3,
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			err := startWithin(t, ctx, newExitTestRecorder(instr, &config.Config{}))
			require.Error(t, err)
			require.Equal(t, tc.stopReason, err.Error(), "the message Start returns changed")

			var byValue models.AppError
			require.ErrorAs(t, err, &byValue, "a models.AppError target cannot reach the application's error")
			require.Equal(t, tc.kind, byValue.AppErrorType)
			require.Equal(t, 3, byValue.ExitCode)

			// AppError's Error() has a value receiver, so *models.AppError is
			// an error too, and a caller may well ask for that instead.
			var byPointer *models.AppError
			require.ErrorAs(t, err, &byPointer, "a *models.AppError target cannot reach the application's error")
			require.Equal(t, tc.kind, byPointer.AppErrorType)
			require.Equal(t, 3, byPointer.ExitCode)

			var ee *exec.ExitError
			require.ErrorAs(t, err, &ee, "the application's *exec.ExitError is no longer reachable")
			require.Equal(t, 3, ee.ExitCode())
		})
	}
}

// A Keploy-side failure carrying a tag keeps it through the same error: the
// exit code the CLI derives from it (utils.ExitCodeFor) must not fall back to
// a generic 1 because the recorder rewrapped the cause. So does an application
// that could not be started for a reason Keploy tagged: with no code of the
// application's to mirror, `keploy record` exits with that tag's.
func TestStart_AnInternalAppErrorKeepsItsTag(t *testing.T) {
	for _, tc := range []struct {
		name       string
		app        models.AppError
		stopReason string
	}{
		{"internal", models.AppError{
			AppErrorType: models.ErrInternal,
			Err:          fmt.Errorf("%w: neither debugfs nor tracefs are mounted", utils.ErrEnvironmentUnsupported),
		}, "internal error occurred while hooking into the application, hence stopping keploy"},
		{"application could not be started", models.AppError{
			AppErrorType: models.ErrCommandError,
			ExitCode:     -1,
			Err:          fmt.Errorf("failed to start the replacement container: %w", utils.ErrEnvironmentUnsupported),
		}, "error in running the user application, hence stopping keploy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instr := &exitingInstr{fakeInstr: newSilentFakeInstr(), appErr: tc.app}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			err := startWithin(t, ctx, newExitTestRecorder(instr, &config.Config{}))
			require.Error(t, err)
			require.Equal(t, tc.stopReason, err.Error())
			require.Equal(t, utils.ExitEnvironmentUnsupported, utils.ExitCodeFor(err))
			var app models.AppError
			require.ErrorAs(t, err, &app)
			require.Equal(t, tc.app.AppErrorType, app.AppErrorType)
			require.Equal(t, tc.app.ExitCode, app.ExitCode)
		})
	}
}

// cancellingOutgoingInstr cancels the recording's context from the last call
// GetTestAndMockChans makes before it returns, so Start sees the stop right
// after the agent started streaming -- the window between the capture being
// armed and its consumers starting.
type cancellingOutgoingInstr struct {
	*blockingInstr
	stop context.CancelFunc
}

func (c *cancellingOutgoingInstr) GetOutgoing(ctx context.Context, o models.OutgoingOptions) (<-chan *models.Mock, error) {
	c.stop()
	return c.fakeInstr.GetOutgoing(ctx, o)
}

// cancellingIncomingInstr is stopped while it asks the agent for the capture,
// and the request fails -- with an error GetTestAndMockChans does not already
// read as shutdown (utils.IsShutdownError), so it reaches Start as a failure.
type cancellingIncomingInstr struct {
	*blockingInstr
	stop context.CancelFunc
}

func (c *cancellingIncomingInstr) GetIncoming(context.Context, models.IncomingOptions) (<-chan *models.TestCase, error) {
	c.stop()
	return nil, errors.New("keploy-agent responded 503 Service Unavailable")
}

// A recording that is STOPPED -- Ctrl+C, SIGTERM, --record-timer, all of
// which cancel the context Start runs on -- is not a failed one, so Start
// returns nil for it, and `keploy record` keeps exiting 0. Start already did
// that on most paths; on these three it returned an error for it, which
// the CLI, now that it fails the run on any error Start returns, would have
// turned into exit 1 for a user pressing Ctrl+C.
func TestStart_AStoppedRecordingIsNotAnError(t *testing.T) {
	t.Run("stopped as the capture is armed", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		instr := &cancellingOutgoingInstr{blockingInstr: &blockingInstr{newSilentFakeInstr()}, stop: cancel}
		require.NoError(t, startWithin(t, ctx, newExitTestRecorder(instr, &config.Config{})))
	})

	t.Run("stopped while the capture is requested", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		instr := &cancellingIncomingInstr{blockingInstr: &blockingInstr{newSilentFakeInstr()}, stop: cancel}
		require.NoError(t, startWithin(t, ctx, newExitTestRecorder(instr, &config.Config{})))
	})

	t.Run("stopped while waiting for the compose agent", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cfg := &config.Config{CommandType: string(utils.DockerCompose)}
		// Nothing listens here, so the agent never reports ready and Start
		// waits until the stop arrives.
		cfg.Agent.AgentURI = "http://127.0.0.1:1"
		time.AfterFunc(200*time.Millisecond, cancel)
		require.NoError(t, startWithin(t, ctx, newExitTestRecorder(&blockingInstr{newSilentFakeInstr()}, cfg)))
	})

	t.Run("stopped while the application runs", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(200*time.Millisecond, cancel)
		require.NoError(t, startWithin(t, ctx, newExitTestRecorder(&blockingInstr{newSilentFakeInstr()}, &config.Config{})))
	})
}

// cancellingSetupInstr is stopped while it sets the environment up, and the
// setup still succeeds: App.SetupCompose takes no context, so a Ctrl+C during
// a docker compose setup reaches Start as a setup that worked, with the
// recording's context already cancelled.
type cancellingSetupInstr struct {
	*blockingInstr
	stop context.CancelFunc
}

func (c *cancellingSetupInstr) Setup(context.Context, string, models.SetupOptions) error {
	c.stop()
	return nil
}

// A stop that lands during setup is a stop, however the setup ended. Under
// docker compose Start then waits for the keploy-agent on a context derived
// from the cancelled one, so the stop, the expired wait and the ticker closing
// its channel are all ready at once -- and Start used to take whichever the
// select picked: "keploy-agent did not become ready in time" (a failure, exit 1
// from `keploy record`) about half the time. Run repeatedly, because one run
// passing proves nothing about a coin flip.
func TestStart_AStopDuringSetupIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func() *config.Config
	}{
		{"native", func() *config.Config { return &config.Config{} }},
		{"docker compose", func() *config.Config {
			cfg := &config.Config{CommandType: string(utils.DockerCompose)}
			// Nothing listens here: the agent can never report ready.
			cfg.Agent.AgentURI = "http://127.0.0.1:1"
			return cfg
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 40; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				instr := &cancellingSetupInstr{blockingInstr: &blockingInstr{newSilentFakeInstr()}, stop: cancel}
				err := startWithin(t, ctx, newExitTestRecorder(instr, tc.cfg()))
				cancel()
				if err != nil {
					t.Fatalf("run %d: a recording stopped during setup returned %q", i+1, err)
				}
			}
		})
	}
}

// capturingInstr reports when the recording asks the agent for its capture:
// the point past the wait for the agent.
type capturingInstr struct {
	*blockingInstr
	capturing chan struct{}
	once      sync.Once
}

func (c *capturingInstr) GetIncoming(ctx context.Context, o models.IncomingOptions) (<-chan *models.TestCase, error) {
	c.once.Do(func() { close(c.capturing) })
	return c.fakeInstr.GetIncoming(ctx, o)
}

// A docker compose recording whose agent comes up records -- and is then
// stopped like any other.
func TestStart_AComposeRecordingStartsOnceTheAgentIsReady(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer agent.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &config.Config{CommandType: string(utils.DockerCompose)}
	cfg.Agent.AgentURI = agent.URL
	instr := &capturingInstr{blockingInstr: &blockingInstr{newSilentFakeInstr()}, capturing: make(chan struct{})}
	go func() {
		select {
		case <-instr.capturing:
		case <-time.After(30 * time.Second):
		}
		cancel()
	}()

	err := startWithin(t, ctx, newExitTestRecorder(instr, cfg))

	select {
	case <-instr.capturing:
	default:
		t.Fatalf("the recording never started capturing (Start returned %v)", err)
	}
	require.NoError(t, err)
}

// ...while an agent that never comes up, with nobody stopping anything, is
// still the failure it always was.
func TestStart_AComposeAgentThatNeverComesUpIsAnError(t *testing.T) {
	t.Setenv("KEPLOY_AGENT_READY_TIMEOUT", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &config.Config{CommandType: string(utils.DockerCompose)}
	cfg.Agent.AgentURI = "http://127.0.0.1:1"
	err := startWithin(t, ctx, newExitTestRecorder(&blockingInstr{newSilentFakeInstr()}, cfg))
	require.EqualError(t, err, "keploy-agent did not become ready in time")
}

// A recording whose application finished cleanly is a finished recording.
func TestStart_AnAppThatExitsZeroIsNotAnError(t *testing.T) {
	instr := &exitingInstr{fakeInstr: newSilentFakeInstr(), appErr: models.AppError{AppErrorType: models.ErrAppStopped}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, startWithin(t, ctx, newExitTestRecorder(instr, &config.Config{})))
}
