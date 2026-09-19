package mock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// mockService implements Service for the `keploy mock record|replay` flow.
type mockService struct {
	// servedAnnounced remembers which mocks have already been reported as
	// served, so the poll loop and the end-of-run flush cannot announce the
	// same mock twice. Guarded because the two run on different goroutines.
	servedAnnouncedMu sync.Mutex
	servedAnnounced   map[string]struct{}

	logger          *zap.Logger
	instrumentation Instrumentation
	mockDB          MockDB
	mappingDB       MappingDB // may be nil (suite-level only)
	store           Store
	hooks           record.RecordHooks // reused so enterprise obfuscation/encryption applies on record
	config          *config.Config
	// userCommand is the test command exactly as the user gave it. A build
	// that instruments natively rewrites config.Command after construction --
	// on macOS it becomes `DYLD_INSERT_LIBRARIES='/tmp/keploy-native-<pid>/…'
	// KEPLOY_SHIM_CTRL=… <command>` -- and that launch line is not the user's
	// command: recorded in the replay receipt it leaked a temp path, and
	// `keploy mock status` handed it back to agents as the command to run.
	userCommand string
}

// New constructs the mock record/replay service. mappingDB and hooks may be nil
// (a nil hooks becomes a no-op; a nil mappingDB disables per-test scoping).
// store must be non-nil — pass FileStore for OSS.
func New(
	logger *zap.Logger,
	instrumentation Instrumentation,
	mockDB MockDB,
	mappingDB MappingDB,
	store Store,
	hooks record.RecordHooks,
	cfg *config.Config,
) Service {
	if hooks == nil {
		hooks = record.BaseRecordHooks{}
	}
	if store == nil {
		store = FileStore{}
	}
	return &mockService{
		logger:          logger,
		instrumentation: instrumentation,
		mockDB:          mockDB,
		mappingDB:       mappingDB,
		store:           store,
		hooks:           hooks,
		config:          cfg,
		userCommand:     cfg.Command,
	}
}

// Overridable lets a downstream build (enterprise) swap the store and record
// hooks on a constructed mock service — the same post-construction override
// pattern Recorder.SetRecordHooks uses. The CLI's mock command type-asserts to
// this so enterprise can inject a registry-backed store and secret-obfuscation
// hooks without a mock-specific constructor.
type Overridable interface {
	SetStore(store Store)
	SetRecordHooks(hooks record.RecordHooks)
}

// SetStore replaces the mock-set store (e.g. enterprise's registry-backed store).
func (m *mockService) SetStore(store Store) {
	if store != nil {
		m.store = store
	}
}

// SetRecordHooks replaces the record hooks (e.g. enterprise secret obfuscation).
func (m *mockService) SetRecordHooks(hooks record.RecordHooks) {
	if hooks != nil {
		m.hooks = hooks
	}
}

// setName returns the configured mock-set name, defaulting to "default".
func (m *mockService) setName() string {
	name := m.config.Mock.Name
	if name == "" {
		return "default"
	}
	return name
}

// agentEpilogueTimeout bounds every agent read made AFTER the wrapped runner
// has exited: the consumed/missed outcome, the per-test scope windows, and the
// captured-on-miss drain.
//
// Natively the agent is keploy's own sibling and is alive right through
// teardown, so those reads always answer. Under compose the runner exiting is
// what stops the whole project — the agent service included — so every one of
// them is fired at an agent that is dying in that instant. The agent client
// holds a zero-value http.Client with no timeout of its own
// (pkg/platform/http/agent.go), and a SIGTERM'd server that still accepts but
// never answers would hang the run: forever on the record side, whose reads sit
// on a context.WithoutCancel that not even Ctrl+C reaches.
//
// These are all best-effort epilogue reads — losing one costs a summary line or
// a mappings file, never a recorded mock — so a short bound is the right trade.
const agentEpilogueTimeout = 10 * time.Second

// composeReleaseTimeout bounds the /agent/ready POST that releases the app.
// MakeAgentReadyForDockerCompose retries for the whole of pkg.AgentReadyTimeout
// (5m30s) because its other callers use it as a bring-up wait. Here the agent
// answered /agent/health seconds ago, so there is nothing left to wait out: the
// only 503 still reachable is a latched CA-install failure, which never clears.
// Without a bound, an agent that dies between the two calls stalls the run for
// five and a half minutes.
const composeReleaseTimeout = 30 * time.Second

// isDockerCompose reports whether the wrapped command is a compose project.
// Compose inverts the usual startup order — see startComposeApp.
func (m *mockService) isDockerCompose() bool {
	return utils.CmdType(m.config.CommandType) == utils.DockerCompose
}

// startComposeApp resolves the ordering circularity compose introduces. It
// returns a non-nil channel — the one the project's eventual exit arrives on —
// exactly when it returns a nil error and the command is a compose one; the
// caller otherwise keeps the normal order and runs the app last.
//
// Under compose the keploy agent is itself a service in the compose project
// keploy generates, so it does not exist until the wrapped `docker compose up`
// runs. Yet arming the proxy needs a live agent, and natively the app is the
// last thing started. So the project is brought up here, concurrently, and the
// caller receives its exit off the returned channel where it would otherwise
// have called Run.
//
// That would leave the app racing the proxy, which is what releaseComposeApp
// exists to prevent: the app service is gated behind the agent service's
// healthcheck, and that healthcheck only passes once keploy posts /agent/ready.
// This function therefore waits only for the agent to become REACHABLE; the
// app stays parked at its healthcheck until the caller has armed the proxy.
func (m *mockService) startComposeApp(ctx context.Context, errGrp *errgroup.Group, phase string) (chan models.AppError, error) {
	if !m.isDockerCompose() {
		return nil, nil
	}

	appExit := make(chan models.AppError, 1)
	errGrp.Go(func() error {
		// The receiver has no other way to learn this goroutine is done, so the
		// send has to survive a panic in Run. Two things make it do so, and
		// both are load-bearing:
		//
		// The send is DEFERRED, so a panic still delivers it on the way out —
		// an inline send after Run is simply skipped, and the receiver then
		// blocks for good, ahead of the deferred teardown that would otherwise
		// unstick a Ctrl+C. (utils.Recover does cancel the root context on its
		// way through, but a bare receive has no ctx arm to notice.)
		//
		// And the value it sends is SEEDED with a failure, because a panic
		// leaves it untouched: a zero AppError would reach propagateExit as
		// "nothing to report" and exit keploy cleanly on a crashed runner.
		exit := models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("the app runner panicked")}
		defer utils.Recover(m.logger)
		defer func() { appExit <- exit }()
		exit = m.instrumentation.Run(ctx, models.RunOptions{AppCommand: m.config.Command})
		return nil
	})

	m.logger.Info("waiting for the keploy-agent compose service to come up",
		zap.String("agent-uri", m.config.Agent.AgentURI))

	// The budget for agent container BOOT, which is exactly what this waits on
	// — the same constant pkg/service/record/record.go gives the same wait, and
	// tunable through KEPLOY_AGENT_READY_TIMEOUT. It is NOT the compose
	// ready-file healthcheck's budget; that has its own, longer start_period
	// (agentHealthcheckStartPeriod in pkg/platform/docker).
	agentCtx, cancel := context.WithTimeout(ctx, pkg.AgentReadyTimeout())
	defer cancel()
	agentReadyCh := make(chan bool, 1)
	go pkg.AgentHealthTicker(agentCtx, m.logger, m.config.Agent.AgentURI, agentReadyCh, time.Second)

	select {
	case ready, ok := <-agentReadyCh:
		if ok && ready {
			return appExit, nil
		}
		// The ticker closes its channel when agentCtx expires.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("keploy-agent did not become ready within %s", pkg.AgentReadyTimeout())
	case appErr := <-appExit:
		// The project died on its own — a bad compose file, a failed build, a
		// port already taken. Report that now rather than sitting out the full
		// agent budget waiting for an agent that is never going to start.
		//
		// Mirror its exit code on the way out. `keploy mock` promises to
		// propagate the wrapped runner's code, and that promise was kept only
		// when the runner got as far as step 8: a project that died during
		// agent bring-up reached the caller as a plain error, so keploy exited
		// a generic 1 and the runner's own code was lost. Which of the two
		// paths a dying project takes is a race — the same crash reported two
		// different exit codes depending on whether the agent's health poll
		// landed first.
		m.propagateExit(appErr, phase)
		reason := string(appErr.AppErrorType)
		if reason == "" {
			reason = "exited"
		}
		return nil, fmt.Errorf("the compose project %s while keploy was waiting for the keploy-agent to come up (exit code %d)", reason, appErr.ExitCode)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// releaseComposeApp releases the app service, which has been parked at its
// healthcheck ever since startComposeApp brought the project up.
//
// POST /agent/ready is not a readiness probe: it writes the file the agent
// service's compose healthcheck reads (pkg/agent/routes/record.go), and the
// app service depends_on that health. So it must be posted only once the proxy
// is armed — any earlier and the app's first dependency calls go out
// unintercepted. All three other callers post it at that same point: after the
// recorder is installed (Recorder.Start) or after the mocks are stored and
// staged (Replayer.RunTestSet, Runner.setupTestSet). Only Recorder.Start also
// gates it on the command type, as this does; the two replay-side callers post
// it for every command type, where it writes a file nothing but the generated
// compose healthcheck ever reads.
func (m *mockService) releaseComposeApp(ctx context.Context) error {
	if !m.isDockerCompose() {
		return nil
	}
	m.logger.Debug("marking the keploy-agent ready; the compose app service is released now")
	releaseCtx, cancel := context.WithTimeout(ctx, composeReleaseTimeout)
	defer cancel()
	return m.instrumentation.MakeAgentReadyForDockerCompose(releaseCtx)
}

// notifyShutdown tells the agent the session is ending so connection errors are
// logged at debug level. Bounded so an unresponsive agent can't hang teardown.
func (m *mockService) notifyShutdown() {
	notifyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.instrumentation.NotifyGracefulShutdown(notifyCtx); err != nil {
		m.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
	}
}

// propagateExit mirrors the wrapped runner's exit status onto keploy's own
// process exit code (utils.ErrCode), so a wrapped `pytest`/`go test` failure
// fails the keploy process — the contract every CI job depends on. A clean
// runner exit leaves ErrCode at 0. phase is "record" or "replay" for logging.
func (m *mockService) propagateExit(appErr models.AppError, phase string) {
	switch appErr.AppErrorType {
	case models.ErrAppStopped:
		// Clean exit (code 0). Success.
		m.logger.Info("test command finished successfully", zap.String("phase", phase))
	case models.ErrCtxCanceled, "":
		// User interrupt or nothing to report — leave ErrCode untouched.
	case models.ErrUnExpected, models.ErrCommandError:
		code := appErr.ExitCode
		if code <= 0 {
			code = 1
		}
		utils.ErrCode = code
		m.logger.Info("test command exited non-zero; mirroring its exit code",
			zap.String("phase", phase), zap.Int("exitCode", code))
	default:
		utils.ErrCode = 1
		m.logger.Info("test command did not complete cleanly", zap.String("phase", phase), zap.String("reason", string(appErr.AppErrorType)))
	}
}
