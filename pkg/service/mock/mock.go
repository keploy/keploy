package mock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/ids"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	stallclock "go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// mockService implements Service for the `keploy mock record|replay` flow.
type mockService struct {
	ids *ids.Map
	// drainClock is the clock the run's end measures its drain's stall on
	// (teardownDrain): nil, keploy's own runnable time; a test drives its
	// own.
	drainClock stallclock.Clock
	// servedAnnounced remembers which mocks have already been reported as
	// served, so the poll loop and the end-of-run flush cannot announce the
	// same mock twice. Guarded because the two run on different goroutines.
	servedAnnouncedMu sync.Mutex
	servedAnnounced   map[string]struct{}

	logger          *zap.Logger
	instrumentation Instrumentation
	mockDB          MockDB
	mappingDB       MappingDB // may be nil (suite-level only)
	testDB          TestDB    // set only for --record-requests
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
	// claims are, under the proven switch, the run's claims (windows with
	// Claim set), which readWindows takes out of the windows it returns: a
	// record tags each pool mock with the key of the claim it was made
	// under (claimKeyOf).
	claims []models.ScopeWindow
	// refusals are, under the proven switch, what the run's wire refused
	// (windows with Refused set), which readWindows takes out of the
	// windows it returns: the replay's receipt names them.
	refusals []models.RunRefusal
	// live and mocked are, under the proven switch, where the run's calls
	// to local listeners went (windows with Live or Mocked set), which
	// readWindows takes out of the windows it returns: the per-test
	// receipts carry them to keploy's summary.
	live   []models.LiveRow
	mocked []models.MockedRow
	// realListed is, under the proven switch, the mock.real destinations
	// each test's recording lists (mappings.yaml real:), by test ID, read
	// as a replay pushes the tests' clocks: the replay's receipt carries
	// them, for the verdict's rule 4 and UNFROZEN rows.
	realListed map[string][]string
	// mockOwners is, under the proven switch, the tests that own each mock
	// of the per-test table the replay pushed (models.MockOwners), set by
	// pushProvenTable: the receipt names, on a test's miss that is another
	// test's recording, that test (withRecordedIn).
	mockOwners map[string][]string
	// skippedReadOf is, under the proven switch, each read of a test the
	// replay's gate skips that pushProvenTable moved to the worker scope,
	// with that test (skippedReads' moved): the receipt names, on a miss one
	// past such a read (models.RepeatNote.SkippedRead), its test
	// (withSkippedTest).
	skippedReadOf map[string]string
	// trails is, under the proven switch, each recording of the worker scope
	// a trail move put there (models.MockEntry.Trails), with the test it
	// trails, as pushProvenTable read the set's mappings: each is required
	// once per run of that test (unmadeTrails).
	trails map[string]string
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

// SetTestDB gives Record a store for the app's incoming requests (--record-requests).
func (m *mockService) SetTestDB(db TestDB) {
	m.testDB = db
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

// agentEpilogueTimeout bounds, with the proven switch off, every agent read
// made AFTER the wrapped runner has exited: the consumed/missed outcome, the
// PID-map count and the captured-on-miss drain (epilogueRead), and a
// record's per-test scope windows (agentWindows). A replay's windows are
// never bounded: whatever the switch, they carry the begins keploy refused,
// which fail the run (readWindows). Under the switch those reads are what
// keploy's verdict rests on, and none has a deadline (design rule 15): the
// agent's answer, the connection's close or the run's context ends each, and
// the user ends the epilogueRead ones at a second interrupt.
//
// Natively the agent is keploy's own sibling and is alive right through
// teardown, so those reads always answer. Under compose the runner exiting is
// what stops the whole project — the agent service included — so every one of
// them is fired at an agent that is dying in that instant. (The replay's
// outcome is the exception: the agent leaves it as it stops, and it is read
// back from the stopped container instead -- ComposeOutcomeReader.) The agent client
// holds a zero-value http.Client with no timeout of its own
// (pkg/platform/http/agent.go), and a SIGTERM'd server that still accepts but
// never answers would hang the run. (Its reads of capture losses alone carry a
// bound of their own: an agent that made no answer while keploy had
// stall.HangBudget of its own runnable time, or the interrupt that gives a
// stop's waits up, ends them; AgentClient.GetCaptureLosses.)
//
// With the switch off these are all best-effort epilogue reads — losing one
// costs a summary line or a mappings file, never a recorded mock — so a short
// bound is the right trade.
const agentEpilogueTimeout = 10 * time.Second

// refuseProvenDocker refuses a docker command (docker compose, docker run,
// docker start) under the proven switch, before anything starts. Inside a
// container keploy cannot tell which process runs the tests (a test
// process's claim is refused), and under compose the agent stops with the
// project, before a record can read what each test did: a run that could
// only end unjudged is refused up front instead.
func (m *mockService) refuseProvenDocker(verb string) error {
	if !provenswitch.On() || !utils.IsDockerCmd(utils.CmdType(m.config.CommandType)) {
		return nil
	}
	return fmt.Errorf("keploy mock %[1]s with %[2]s runs the test command natively in this release: inside a container keploy cannot tell which process runs the tests, or read what each of them did. "+
		"Run the test command itself through keploy (keploy mock %[1]s -c \"go test -p 1 -json ./...\"), or unset %[2]s", verb, provenswitch.Env)
}

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
//
// stopNoticeFrom, for a recording, is the user's context (Record's parent):
// the project then runs on runUntilStopNoted's context, so the user's stop is
// told to the agent before the project is torn down, as the native command's
// is. A replay passes nil: it records nothing, and the project runs on ctx.
func (m *mockService) startComposeApp(ctx context.Context, errGrp *errgroup.Group, phase string, stopNoticeFrom context.Context) (chan models.AppError, error) {
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
		if stopNoticeFrom != nil {
			exit = m.runStopNoted(stopNoticeFrom, ctx)
		} else {
			exit = m.instrumentation.Run(ctx, models.RunOptions{AppCommand: m.config.Command})
		}
		return nil
	})

	m.logger.Info("waiting for the keploy-agent compose service to come up",
		zap.String("agent-uri", pkg.ShownAgentURI(m.config.Agent.AgentURI)))

	// The wait follows the agent container's BOOT, which is exactly what this
	// waits on, as keploy record's same wait does: it lasts while the start
	// makes progress (and while the engine has not run the container yet:
	// compose starts it in its own time), and ends once the start fails or
	// stops making progress (pkg.WatchAgentStart). It is NOT the compose
	// ready-file healthcheck's budget; that has its own start_period
	// (agentHealthcheckStartPeriod in pkg/platform/docker).
	agentCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	agentReadyCh := make(chan bool, 1)
	go pkg.AgentHealthTicker(agentCtx, m.logger, m.config.Agent.AgentURI, agentReadyCh)
	agentStart := pkg.WatchAgentStart(agentCtx, m.instrumentation)

	select {
	case ready, ok := <-agentReadyCh:
		if ok && ready {
			return appExit, nil
		}
		// The ticker closes its channel unready only once ctx ends.
		return nil, ctx.Err()
	case err := <-agentStart:
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("the keploy-agent docker compose started did not come up: %w", err)
	case appErr := <-appExit:
		// The agent first. It is a service in the project, and when it is the
		// one that stopped -- it could not start: no tracefs on the machine
		// Docker runs on, a kernel that refused it eBPF -- compose aborts the
		// project over it ("dependency failed to start: container keploy-v3-…
		// exited (6)"). That exit is compose's, not the test command's, which
		// never ran: mirroring it as the runner's code told the user their
		// tests failed and lost the agent's own reason with it.
		if err := m.composeAgentFailure(); err != nil {
			return nil, err
		}
		// Otherwise the project died on its own — a bad compose file, a failed
		// build, a port already taken. Report that now rather than wait on for
		// an agent that is never going to start.
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
		if appErr.ExitCode > 0 {
			return nil, fmt.Errorf("the compose project exited with code %d while keploy was waiting for the keploy-agent to come up", appErr.ExitCode)
		}
		return nil, fmt.Errorf("the compose project stopped while keploy was waiting for the keploy-agent to come up: %w", appErr)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// composeAgentFailure is the keploy-agent compose service having stopped while
// the app still needed it, as keploy's own failure, or nil. See
// ComposeAgentFailureReader.
func (m *mockService) composeAgentFailure() error {
	reader, ok := m.instrumentation.(ComposeAgentFailureReader)
	if !ok || !m.isDockerCompose() {
		return nil
	}
	return reader.ComposeAgentFailure()
}

// composeEnd is what ended a compose recording when the test command's own
// end, appExit, does not stand, or nil. See ComposeEndReader.
func (m *mockService) composeEnd(appExit int) error {
	reader, ok := m.instrumentation.(ComposeEndReader)
	if !ok || !m.isDockerCompose() {
		return nil
	}
	return reader.ComposeEndAfterReady(appExit)
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
	// The release follows the agent, not a clock: it ends at once on the
	// latched CA-install failure the agent answers, and once the agent's
	// container has exited or its start has stopped making progress
	// (MakeAgentReadyForDockerCompose), so an agent that dies between the
	// health check and this call does not stall the run.
	return m.instrumentation.MakeAgentReadyForDockerCompose(ctx)
}

// stopBeganNotifier is an Instrumentation that can tell the agent a
// recording's stop has begun (the agent client's NotifyStopBegun).
type stopBeganNotifier interface {
	NotifyStopBegun(ctx context.Context) error
}

// runUntilStopNoted returns the context the recorded test command runs on,
// with ctx's values, and ran, which releases it once the command's run has
// returned. ctx is parent's child (the recording's errgroup context). A stop
// of the user's (parent ends: Ctrl+C, or the record timer's stop) ends the
// command only once the agent has been told the recording's stop has begun,
// so the calls the command still has in flight are cut by a stop the agent
// knows of, and record no fault; ended with the stop, the command was killed
// first, and a call it had open was recorded as the application's giving up.
// keploy's own failure (ctx ends, parent does not) ends the command at once,
// with no notice: no stop of the user's began.
//
// One callback on ctx's end decides both, in that order: parent's end ends
// ctx too, and a context's Err is set before its children end, so the
// callback reads whose end it is; the notice, when there is one, returns
// before the command's context ends, with no second callback to race it.
func (m *mockService) runUntilStopNoted(parent, ctx context.Context) (runCtx context.Context, ran func()) {
	runCtx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() {
		if parent.Err() != nil {
			m.notifyStopBegun()
		}
		stopRun()
	})
	return runCtx, func() {
		stop()
		stopRun()
	}
}

// runStopNoted runs the recorded test command until it exits, on
// runUntilStopNoted's context (parent: the user's; ctx: the recording's), and
// releases that context once the run has returned, so a stop of the user's
// after it tells the agent nothing: no call of the command's is left to cut.
func (m *mockService) runStopNoted(parent, ctx context.Context) models.AppError {
	runCtx, ran := m.runUntilStopNoted(parent, ctx)
	defer ran()
	return m.instrumentation.Run(runCtx, models.RunOptions{AppCommand: m.config.Command})
}

// notifyStopBegun tells the agent the recording's stop has begun, bounded so
// an agent that does not answer cannot hold the stop. One that did not take
// it is said at WARN: a call the stop cuts may then be recorded as a timeout
// fault the application never met.
func (m *mockService) notifyStopBegun() {
	n, ok := m.instrumentation.(stopBeganNotifier)
	if !ok {
		return
	}
	notifyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.NotifyStopBegun(notifyCtx); err != nil {
		m.logger.Warn("keploy could not tell the agent the recording is stopping: a call the stop cuts may be recorded as a timeout fault",
			zap.Error(err), zap.String("next_step", "check the mock's fault in the recording, or record again"))
	}
}

// notifyShutdown tells the agent the session is ending so connection errors are
// logged at debug level. The notification is bounded so an unresponsive agent
// can't hang teardown; with --from-container, restoring the user's container
// afterwards waits for the agent container to exit, or for a second Ctrl+C
// (AgentClient.RestoreFromContainer).
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
