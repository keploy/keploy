package mock

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/ids"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/coverage/report"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// Replay runs the wrapped test command with the configured named mock set
// served in place of the real dependencies. It needs no incoming test cases —
// the runner drives the requests, Keploy answers the outgoing calls from the
// set. On a miss it applies the configured policy (fail / passthrough /
// record). It propagates the runner's exit code, and with --strict also exits
// non-zero when a call missed its recorded mock (one that counts against a
// test, models.UnmatchedCall.CountsAgainstTest). It returns an error for a
// failure of keploy's own -- one that stopped the run, or left --strict unable
// to verify it -- which a failing test command never is.
func (m *mockService) Replay(ctx context.Context) (err error) {
	// The replay's own teardown scope: what keploy sees its stops do moves
	// it, and the drain below waits while it moves (teardownDrain).
	ctx, teardown := utils.NewTeardownScope(ctx)
	m.ids = nil
	name := m.setName()
	requests := m.requests()
	started := time.Now()
	parent := ctx
	// A replay that could not start leaves a receipt saying so. Without it
	// the PREVIOUS receipt -- green, isolated -- stayed in place while keploy
	// exited 1, and every reader reported a proof this run had just failed to
	// repeat. A user interrupt is not a failure and leaves the last receipt,
	// which still truthfully describes the last completed run.
	finished := false
	// ran is the test command's own exit once it ran (step 8): a failure
	// after it is keploy's, never "the test command never ran". nil before.
	var ran *int
	defer func() {
		if finished || err == nil || parent.Err() != nil {
			return
		}
		// The exit this process is about to make. A compose project that died
		// during bring-up has already mirrored ITS code into utils.ErrCode, so
		// writing a flat 1 here would have the receipt contradict the exit the
		// shell sees -- and claim the runner never ran when it had just
		// exited 7. Keploy's OWN specific code lands there too -- an agent
		// that could not start for want of privileges (3) or of something in
		// the environment (6) -- and that one is not the runner's: the test
		// command never started.
		code, runner, note := utils.ErrCode, -1, "the test command never ran"
		failed := FailedBySetup
		switch {
		case ran != nil:
			// The test command ran, and keploy could not read, or judge,
			// what it did (its agent's windows, or what the run saw of
			// mock.real): keploy's failure, after the command, whose own
			// exit is kept.
			runner, failed = *ran, FailedByKeploy
			note = "the test command ran; keploy could not read what it did"
			if *ran < 0 {
				// Keploy failed under the command (its agent died).
				note = "the test command ran; keploy did not complete the run"
			}
			if code == 0 {
				code = 1
			}
		case code == 0:
			code = 1
		case code != utils.ExitKeployError && code == utils.ExitCodeFor(err):
			// Keploy's own, armed for the failure err describes.
		default:
			runner, failed = code, FailedByRunner
			note = "the test command exited before keploy finished starting"
		}
		writeReceipt(m.logger, m.config.Path, Receipt{
			Set:            name,
			At:             started.UTC().Truncate(time.Second),
			Command:        m.userCommand,
			OnMiss:         m.config.Mock.OnMiss,
			Strict:         m.config.Mock.Strict,
			ExitCode:       code,
			RunnerExitCode: runner,
			FailedBy:       failed,
			Error:          err.Error(),
			Loaded:         -1,
			Consumed:       -1,
			Missed:         -1,
			MissedData:     provenCount(-1),
			IsolationNote:  note,
			MocksDigest:    MocksDigest(m.config.Path, name),
			MinCoverage:    m.config.Mock.MinCoverage,
			Version:        utils.Version,
		})
		// No test ran, or keploy could not read what they did: an earlier
		// run's per-test results must not stand next to this receipt as
		// though they were its.
		writeTestsReceipt(m.logger, m.config.Path, TestsReceipt{Set: name, At: started.UTC().Truncate(time.Second)})
	}()
	// A replay the user stopped before keploy judged it did not verify the
	// suite, wherever the stop came: while the agent started, while the test
	// command ran, or after it exited, while keploy read what the run did.
	// It exits 128+N for the signal (SIGINT 130, SIGHUP 129, SIGTERM 143),
	// never the test command's code -- 0 for a suite that passed -- and it is
	// not keploy's failure, so it returns nil, and the last receipt stands
	// (above). Every return before the verdict ends here, so no stop can slip
	// past it: one that came while keploy read the windows or mock.real
	// returned the read's error, which the CLI drops on a cancelled context,
	// and the replay exited 0, unjudged. While Replay runs only NewCtx's
	// handler cancels parent (the CLI's own ExecCancel comes after it
	// returns). A code already armed is kept (SetExitCodeOnce): the runner's
	// own, or keploy's 3 or 6, is non-zero too. A cancel that is no signal's
	// arms nothing (InterruptExitCode is 0).
	defer func() {
		if finished || parent.Err() == nil {
			return
		}
		if code := utils.InterruptExitCode(); code != 0 {
			utils.SetExitCodeOnce(code)
		}
		err = nil
	}()
	// Coverage is read from a report the runner writes during THIS run. What
	// the report files looked like beforehand is what tells that report from
	// one an earlier run left behind.
	workDir, _ := os.Getwd()
	coverageBefore := report.Snapshot(workDir, m.config.Mock.CoverageReport, m.userCommand)

	if provenswitch.On() && !replaysJudged.Load() {
		return fmt.Errorf("keploy mock replay under %s needs a keploy build that judges each test (keploy enterprise): this one exits with the test command's code, so a test keploy failed after it ended, or a call no test made that got no recorded answer, would pass", provenswitch.Env)
	}
	// 0. Materialise the set locally (registry download in enterprise; no-op on files).
	if err := m.store.Pull(ctx, name); err != nil {
		utils.LogError(m.logger, err, "failed to fetch the mock set", zap.String("mock-set", name))
		return err
	}

	if provenswitch.On() && len(m.config.Mock.RunOnly) > 0 {
		return errors.New("--run-only is not used with KEPLOY_PROVEN: select the tests with your own test command; keploy skips only the tests verdicts.yaml quarantines")
	}
	if provenswitch.On() && m.config.Mock.Strict {
		return errors.New("--strict is not used with KEPLOY_PROVEN: keploy's verdict fails a run on every call the recording did not answer, charged to a test or not")
	}
	if err := m.refuseProvenDocker("replay"); err != nil {
		return err
	}
	if err := m.checkRealDeps(); err != nil {
		return err
	}
	policy := models.MissPolicy(m.config.Mock.OnMiss)
	if !policy.Valid() {
		return fmt.Errorf("invalid --on-miss value %q: allowed values are fail, passthrough, record", m.config.Mock.OnMiss)
	}
	if provenswitch.On() && policy != models.MissFail {
		// A call served live is not a miss, so it would reach no test's
		// evidence and no verdict: a test that called real infra would pass.
		return fmt.Errorf("--on-miss %s is not used with %s: keploy judges each test by the calls the recording answers, so a miss must fail it (--on-miss fail)", policy, provenswitch.Env)
	}

	m.logger.Info("Replaying mocks for your test command",
		zap.String("mock-set", name),
		zap.String("command", m.userCommand),
		zap.String("on-miss", string(policy)))

	errGrp, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)
	ctx, cancel := context.WithCancel(ctx)

	defer func() {
		m.notifyShutdown()
		// Cancel so agent-monitor / app-runner goroutines unwind before drain.
		cancel()
		if err := m.teardownDrain("mock-replay", errGrp, teardown); err != nil {
			utils.LogError(m.logger, err, "failed to drain mock-replay goroutines")
		}
	}()

	// 1. Instrument in mock (test) mode — no ingress port relocation.
	//    A start the agent stops making progress in is given up on
	//    (pkg.ErrAgentNotReady) and retried with a fresh agent, as record and
	//    test retry it (pkg.RetryAgentSetup). Any other failure is not
	//    retried.
	opts := models.SetupOptions{
		Container:        m.config.ContainerName,
		FromContainer:    m.config.FromContainer,
		CommandType:      m.config.CommandType,
		DockerDelay:      m.config.BuildDelay,
		BuildDelay:       m.config.BuildDelay,
		Mode:             models.MODE_TEST,
		MockMode:         true,
		RecordRequests:   requests,
		ConfigPath:       m.config.ConfigPath,
		PassThroughPorts: config.GetByPassPorts(m.config),
		AllowEgress:      m.config.Test.AllowEgress,

		DisableHandshakeHold: m.config.Record.DisableHandshakeHold,
	}
	if err := pkg.RetryAgentSetup(ctx, m.logger, func(c context.Context, _ int) error {
		return m.instrumentation.Setup(c, m.config.Command, opts)
	}); err != nil {
		if parent.Err() != nil {
			// The user's Ctrl+C. An errgroup-derived cancel is NOT that: the
			// group cancels whenever any goroutine in it fails, so testing
			// `ctx` here reported keploy's own internal failure as an
			// interrupt -- exit 0, and the previous receipt left standing as
			// proof of a run that had just failed.
			return nil
		}
		utils.LogError(m.logger, err, "failed setting up the environment")
		return err
	}

	// 2. Docker compose inverts this function's normal order: putting the
	//    proxy in mock-serving mode (step 3) needs a live agent, but under
	//    compose the agent is a service inside the compose project keploy
	//    generates, so it does not exist until the wrapped `docker compose up`
	//    runs. startComposeApp brings the project up and waits for the agent
	//    to answer; the app itself stays parked at the agent's healthcheck
	//    until step 7 releases it, so it cannot make a dependency call before
	//    the mocks are loaded. Its exit is collected at step 8 instead of
	//    being started there.
	//
	//    Without this the run dialled an agent that was never started and
	//    failed before serving a single mock.
	composeAppExit, err := m.startComposeApp(ctx, errGrp, "replay")
	if err != nil {
		if parent.Err() != nil {
			// The user's Ctrl+C. An errgroup-derived cancel is NOT that: the
			// group cancels whenever any goroutine in it fails, so testing
			// `ctx` here reported keploy's own internal failure as an
			// interrupt -- exit 0, and the previous receipt left standing as
			// proof of a run that had just failed.
			return nil
		}
		utils.LogError(m.logger, err, "failed to bring up the keploy-agent compose service")
		return err
	}

	// The run's mock.real (proven switch only), before the proxy serves:
	// those destinations are reached for real from the first connection.
	if err := m.pushRealDeps(ctx); err != nil {
		if parent.Err() != nil {
			return nil
		}
		utils.LogError(m.logger, err, "keploy could not hand its agent mock.real")
		return err
	}

	// 3. Put the proxy in mock-serving mode with the miss policy.
	if err := m.instrumentation.MockOutgoing(ctx, models.OutgoingOptions{
		// A session of its own: what an agent kept from an earlier one
		// recorded consumed is that session's.
		StartsTestSet:             true,
		Rules:                     m.config.BypassRules,
		MongoPassword:             m.config.Test.MongoPassword,
		SQLDelay:                  time.Duration(m.config.Test.Delay) * time.Second,
		Mocking:                   true,
		SwapIDs:                   true,
		OnMiss:                    policy,
		MysqlPorts:                m.config.MysqlPorts,
		DisableMysqlAutoDetect:    m.config.DisableMysqlAutoDetect,
		DisableMysqlEndpointDrift: m.config.DisableMysqlEndpointDrift,
		DisablePgTimeSliceCohort:  m.config.DisablePgTimeSliceCohort,
		PassThroughPorts:          m.config.Record.PassThroughPorts,
		PassThroughHosts:          m.config.Record.PassThroughHosts,
		DisableStatefulMocks:      m.statefulMocksDisabled(),
		DisableMockCorrelation:    m.config.Test.DisableMockCorrelation,
		Learn:                     m.learning(),
		LearnPool:                 m.learningPool(),
	}); err != nil {
		if parent.Err() != nil {
			// The user's Ctrl+C. An errgroup-derived cancel is NOT that: the
			// group cancels whenever any goroutine in it fails, so testing
			// `ctx` here reported keploy's own internal failure as an
			// interrupt -- exit 0, and the previous receipt left standing as
			// proof of a run that had just failed.
			return nil
		}
		utils.LogError(m.logger, err, "failed to enable mock serving")
		return err
	}

	// 4. Load the whole set and push it into the proxy.
	watchCtx, stopWatch := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWatch()
	// The watch's stream ends by the agent's marker when it can
	// (round_end.go): its token names it to the agent.
	watchRound := newRoundEnd(m.instrumentation)
	actual := m.watchIncoming(watchRound.on(watchCtx), requests)
	empty := map[string]bool{}
	filtered, unfiltered, failedPool, err := m.loadMocks(ctx, name, empty)
	if err != nil {
		utils.LogError(m.logger, err, "failed to load "+failedPool+" mocks", zap.String("mock-set", name))
		return err
	}
	// What the agent holds: the pools less what this keploy never sends it
	// (models.AgentBound: the connection failures, which it does not replay).
	// loaded counts exactly that, for the empty-set WARN, the "verified
	// nothing" exit, the receipt and the metered count.
	// Under the proven switch, every mock the set decoded, before what the
	// agent never holds is left out: what a mapping may name
	// (pushProvenTable).
	var decoded []*models.Mock
	if provenswitch.On() {
		decoded = append(append([]*models.Mock(nil), filtered...), unfiltered...)
	}
	filtered, unfiltered = models.AgentBound(filtered), models.AgentBound(unfiltered)
	loaded := len(filtered) + len(unfiltered)
	// The recording this run replays, for the receipt: read after Pull, so in
	// enterprise it is the registry's copy that was actually served.
	digest := MocksDigest(m.config.Path, name)
	if loaded == 0 {
		m.logger.Warn("the mock set is empty; the runner will hit a miss on every dependency call",
			zap.String("mock-set", name),
			zap.String("next_step", fmt.Sprintf("record it first with: keploy mock record -c %q --name %s", m.userCommand, name)))
	}
	if err := m.instrumentation.StoreMocks(ctx, filtered, unfiltered); err != nil {
		utils.LogError(m.logger, err, "failed to store mocks on the agent")
		return err
	}

	// 5. Hand the agent the per-test table so the runner's /agent/scope/begin
	//    calls can narrow the served pool per test (best-effort / optional;
	//    under the proven switch each test is judged by it, so required).
	if err := m.pushScopeTable(ctx, name, filtered, unfiltered, decoded); err != nil {
		return err
	}
	if provenswitch.On() {
		if err := m.pushTestBegins(ctx, name); err != nil {
			utils.LogError(m.logger, err, "keploy cannot run the tests on the clocks they were recorded on", zap.String("mock-set", name))
			return err
		}
	}
	// Under the proven switch the deny list is the only gate: a run list
	// (--run-only, or a wrapping build's ScopeGateSource) is not pushed.
	var gate requestedGate
	if !provenswitch.On() {
		gate = m.pushScopeGate(ctx, name, digest)
	}
	if err := m.pushScopeSkips(ctx, name, digest); err != nil {
		utils.LogError(m.logger, err, "refusing to replay", zap.String("mock-set", name))
		return err
	}

	// 6. Stage the whole pool as the initial serving window (BaseTime..now, no
	//    mapping) — same call RunTestSet makes before the first test. Under
	//    the proven switch the table push (step 5) already armed the pool
	//    slice of calls made before any claim; arming the whole pool would
	//    let any test's mock answer them.
	if !provenswitch.On() {
		if err := m.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:  models.BaseTime,
			BeforeTime: time.Now(),
		}); err != nil {
			utils.LogError(m.logger, err, "failed to arm the mock pool")
			return err
		}
	}

	// 6a. What the agent had already refused a place in its PID maps before
	//     the test command runs: the run's own refusals are what it grows by
	//     (PIDMapReader). Read here, with the compose app still parked.
	pidBase := m.pidMapBaseline(ctx, "replay")
	lossesBase := m.captureLossBaseline(ctx)

	// 7. Release the compose app now that the mock pool is armed. This is the
	//    post-arm half of step 2 — see releaseComposeApp.
	if err := m.releaseComposeApp(ctx); err != nil {
		if parent.Err() != nil {
			// The user's Ctrl+C. An errgroup-derived cancel is NOT that: the
			// group cancels whenever any goroutine in it fails, so testing
			// `ctx` here reported keploy's own internal failure as an
			// interrupt -- exit 0, and the previous receipt left standing as
			// proof of a run that had just failed.
			return nil
		}
		utils.LogError(m.logger, err, "failed to release the app behind the keploy-agent healthcheck")
		return err
	}

	// 7a. With --emit-mock-events, report each mock as it is first served so a
	//     client can show mocked egress live. Off by default; see MockCmd.
	if m.config.Mock.EmitMockEvents {
		errGrp.Go(func() error {
			m.emitServedMockEvents(ctx)
			return nil
		})
	}

	// 8. Block until the wrapped runner exits. Under compose it was already
	//    started at step 2, so wait on that same exit rather than starting it
	//    a second time.
	//    A plain receive is the same wait the native branch does: Run returns
	//    only once the app is fully down (its errgroup Wait is deferred), and
	//    the sender is the goroutine running exactly that Run.
	var appErr models.AppError
	if composeAppExit != nil {
		appErr = <-composeAppExit
	} else {
		appErr = m.instrumentation.Run(ctx, models.RunOptions{AppCommand: m.config.Command})
	}

	if parent.Err() != nil { // user Ctrl+C / a signal interrupted the replay
		// Stopped before it finished: not judged, 128+N (the deferred rule
		// above). Record is unaffected -- a stopped recording is a finish, and
		// it returns 0 from its own path, not here.
		return nil
	}
	// Either way the cause is returned as keploy's own failure (see
	// keployFailure below), and logged once, by the caller that receives it.
	if cause := m.composeAgentFailure(); cause != nil {
		// The agent died under the test command. Under compose that stops the
		// whole project, test command included, and the exit that arrived
		// above is compose's stop, not the suite's verdict: mirrored, it
		// failed a suite that never finished, in the suite's name.
		appErr = models.AppError{AppErrorType: models.ErrInternal, Err: cause}
	} else if ctx.Err() != nil && (appErr.AppErrorType == models.ErrCtxCanceled || appErr.AppErrorType == "") {
		// Not the user: something in the run's own errgroup failed while the
		// test command ran -- the agent died under it -- and the runner was
		// stopped with it. That is keploy not completing the run. Left as a
		// cancellation it mirrored nothing, and the replay exited 0 on a
		// suite that never finished.
		appErr = models.AppError{AppErrorType: models.ErrInternal, Err: context.Cause(ctx)}
	}
	ran = new(int)
	switch appErr.AppErrorType {
	case models.ErrAppStopped, models.ErrCtxCanceled, "":
	case models.ErrUnExpected, models.ErrCommandError:
		*ran = max(appErr.ExitCode, 1)
	default:
		// Keploy's own failure under the test command: it has no exit of
		// its own to report.
		*ran = -1
	}
	stoppedNow := false
	// requestsShort says why the requests this run compares are short of
	// what the agent captured, the user not having stopped it: keploy's own
	// failure, said at ERROR where it was found, and the run's (below).
	requestsShort := ""
	if actual != nil {
		// Unconfirmed (the stream closed on its own) is not said: under
		// compose this stream closes so on every run, and what it carries
		// feeds the run's report, not a recording.
		lost, _ := m.drainTrailingMocks(actual.done, time.Now(), false, func() (uint64, bool) { return uint64(actual.seen.Load()), false },
			func() uint64 { return uint64(actual.seen.Load()) }, replayDrainWords, watchRound)
		if !lost && watchRound.endsByMarkers() {
			// The agent said it holds nothing more from before the run
			// ended: the watch ends on its marker, every request the agent
			// had put on the stream read, not when the stream is ended here
			// (round_end.go). An agent this client started is there until
			// keploy stops it, so its stream closing before its marker
			// leaves the run short of requests; one compose or the user
			// stops is said at WARN, as before markers.
			// Not once the user stopped the run, by the time a stream is
			// judged: a stop that reached the agent too ends its stream (a
			// replay's agent does not hold its stop for the watch), and the
			// run ends unjudged, 128+N.
			ownAgent := !m.isDockerCompose() && startedAgent(m.instrumentation)
			strict := func() bool { return ownAgent && !utils.Interrupted() }
			lost = m.endRound(watchRound, strict, []roundStream{{kind: models.TestCaseStream, done: actual.done}}, replayDrainWords)
		}
		if lost {
			select {
			case <-utils.StopNow():
				stoppedNow = true
			default:
				// The drain gave up on an agent that made no progress, or
				// the agent left the watch before its marker: said at
				// ERROR, by the drain or endRound.
				requestsShort = "keploy could not read every request the app served from its agent"
			}
		}
	}
	stopWatch()
	// The watch has stopped: what the agent handed over is read, however
	// long that takes (waitConsumers), and only then is the run judged on
	// it. One the user gives up at a second interrupt ends unjudged, as a
	// drain given up so does (below).
	if actual != nil && !stoppedNow {
		seen := func() uint64 { return uint64(actual.seen.Load()) }
		if !m.waitConsumers(replayDrainWords, streamConsumer{what: "requests", done: actual.done, taken: seen, handled: seen}) {
			stoppedNow = true
		}
	}
	if stoppedNow {
		// The user's Ctrl+C, at the drain's offer, gave up the requests the
		// agent still held: what this run would judge is short of them, and
		// a verdict on part of the run could pass a suite that did not. It
		// ends as a replay the user stopped before it finished does (above):
		// 128+N, unjudged, the last run's receipt left standing.
		if code := utils.InterruptExitCode(); code != 0 {
			utils.SetExitCodeOnce(code)
		}
		return nil
	}

	// 9. Under --on-miss record, append any calls served live-from-upstream to
	//    the set so the next replay serves them from the mock (VCR new_episodes).
	var notAppended error
	if policy.RecordsOnMiss() {
		notAppended = m.persistCaptured(context.WithoutCancel(ctx), name)
	}

	// 10. Summarise what was served and missed.
	all, werr := m.replayWindows(ctx)
	if werr != nil {
		// Under the proven switch what each test did decides keploy's
		// verdict: a replay that cannot read it did not complete. When keploy
		// had already failed under the test command (its agent died), that is
		// the reason, and the read failed because of it.
		if appErr.AppErrorType == models.ErrInternal && appErr.Err != nil {
			return fmt.Errorf("keploy did not complete the run: %w", appErr.Err)
		}
		return fmt.Errorf("keploy could not read what each test did from its agent: %w", werr)
	}
	// What the run saw of mock.real beyond its tests (proven switch only),
	// once its last test ended: rule 4's reached destinations, and evidence
	// charged to no test, which the verdict needs as much as the windows.
	realRun, rerr := m.realRun(ctx)
	if rerr != nil {
		return fmt.Errorf("keploy could not read the run's mock.real destinations from its agent: %w", rerr)
	}
	if parent.Err() != nil {
		// The user's Ctrl+C came while keploy read what the run did, and
		// the read may be short of it (with the switch off, a failed read is
		// no windows): a verdict on part of a run could pass a suite that did
		// not. Not judged: 128+N (the deferred rule above).
		return nil
	}
	detail := replayDetail{
		set:      name,
		windows:  testWindows(all),
		starts:   appStarts(all),
		expected: m.expectedMocks(ctx, name),
		recorded: m.recordedCases(ctx, name),
		actual:   actual.list(),

		actualShort: requestsShort,
	}
	counts := m.reportOutcome(ctx, loaded, detail, func() bool { return parent.Err() != nil })
	missesKnown := counts.missed >= 0

	// 11. Exit code: mirror the runner; with --strict also fail on a miss
	//     that counts against a test.
	// The learn step's findings not kept means keploy did not complete the
	// round, whatever the test command did: that is the exit, and the
	// receipt's.
	if counts.learnErr != nil && appErr.AppErrorType != models.ErrInternal {
		utils.LogError(m.logger, counts.learnErr, "keploy did not complete this round of the record")
		appErr = models.AppError{AppErrorType: models.ErrInternal, Err: counts.learnErr}
	}
	m.propagateExit(appErr, "replay")
	// WHOSE failure this is, from what the runner reported -- not from the
	// global exit code. keploy's own internal failure (a panic in the app
	// runner) also lands in utils.ErrCode, and recording that as
	// FailedByRunner wrote "the test command failed" into the receipt about a
	// suite that may never have been reached.
	runnerExit, failedBy := 0, ""
	switch appErr.AppErrorType {
	case models.ErrAppStopped, models.ErrCtxCanceled, "":
		// Clean exit, or nothing to report.
	case models.ErrUnExpected, models.ErrCommandError:
		runnerExit, failedBy = utils.ErrCode, FailedByRunner
	default:
		// ErrInternal and anything else: keploy's side, not the suite's. The
		// test command has no exit of its own to report -- keploy failed
		// before it exited, or stopped it -- and a 0 here read as a suite
		// that passed.
		runnerExit, failedBy = -1, FailedByKeploy
	}
	// What the run compares is short of requests the agent captured: keploy
	// did not complete it, whatever the test command did (its exit is kept),
	// and a verdict on part of it is not one on the run.
	requestsOnly := requestsShort != "" && failedBy != FailedByKeploy
	if requestsOnly {
		failedBy = FailedByKeploy
	}
	// Calls captured on miss that --on-miss record did not add to the set
	// (refused, as for a set with two mock files, or not written): keploy did
	// not complete the run, whatever the test command did (its exit is kept,
	// and a passing one becomes 1), so a CI job that runs it to grow the set
	// does not go green while the set does not grow. A keploy failure already
	// decided keeps its name; this one is said at ERROR all the same.
	appendOnly := notAppended != nil && failedBy != FailedByKeploy
	if appendOnly {
		failedBy = FailedByKeploy
	}
	if notAppended != nil && utils.ErrCode == 0 {
		utils.ErrCode = utils.ExitKeployError
	}
	// A process the agent could not track -- the kernel refused it a place in
	// its PID map -- made calls the agent never saw: not mocked, not
	// counted as missed, answered by the real dependency if it was up. Nothing
	// else in this run can show that, and every other gate passes on it, so it
	// fails the run first and is what the receipt names, ahead even of a
	// failing runner, which it may be the cause of (the runner's own exit code
	// is kept, and stays in RunnerExitCode). Keploy's failure, not the
	// suite's: returned, like an unverifiable --strict run.
	var keployFailure error
	captureIncomplete, captureUnverified := "", ""
	pidCheck := m.pidMapRefusedDuring(ctx, pidBase, "replay")
	if refused := pidCheck.refused; pidCheck.known && refused.Any() {
		captureIncomplete = refused.CaptureIncomplete()
		if utils.ErrCode == 0 {
			utils.ErrCode = 1
		}
		if failedBy != FailedByKeploy {
			failedBy = FailedByCaptureIncomplete
		}
		keployFailure = errors.New(captureIncomplete)
		msg := "replay failed: the agent could not track every process the test command ran, so their calls were not mocked"
		if refused.Target == 0 {
			msg = "replay failed: the agent could not exclude every process that predates the test command, so their children's calls may have been mocked as the test command's"
		}
		m.logger.Error(msg, append(pidMapFields(refused), zap.String("mock-set", name), zap.String("reason", captureIncomplete))...)
	}
	// Under the proven switch, every other loss of capture during the run
	// fails it the same way: the test running at it already failed with it
	// (and each later one, but for a failed TLS handshake), and this covers
	// a loss outside every test too.
	lostAll, lostErr := m.captureLossesDuring(ctx, lossesBase)
	if parent.Err() != nil {
		// The user's Ctrl+C came while keploy read the agent's counts (what
		// it served and missed, its PID maps, its capture losses): a read it
		// cut short is no agent failure, and no gate below may fail the run
		// on it -- --strict's "never reported which calls were missed" did,
		// exit 1 -- nor pass a verdict on part of it. No read of the agent
		// comes after this one. Not judged: 128+N (the deferred rule above).
		return nil
	}
	// A call keploy cannot capture at all (decision 20) does not fail the
	// run: it leaves it unverified, below.
	lost, lostNotJudged := lostAll.Split()
	if t := lost.Text(); t != "" {
		if captureIncomplete == "" {
			captureIncomplete = t
		} else {
			captureIncomplete += "; " + t
		}
		if utils.ErrCode == 0 {
			utils.ErrCode = 1
		}
		if failedBy != FailedByKeploy {
			failedBy = FailedByCaptureIncomplete
		}
		keployFailure = errors.New(captureIncomplete)
		m.logger.Error("replay failed: keploy could not capture every call the test command made ("+provenswitch.Env+"=1)",
			zap.String("mock-set", name), zap.String("reason", t))
	}

	// --strict means "fail unless every recorded call was matched". An
	// unreadable miss list is not proof of that, so it fails too: a
	// verification flag that passes when it could not verify is worse than no
	// flag at all. Loud beats quietly green.
	//
	// And it is keploy's failure, not the suite's, so Replay returns it: a
	// returned error is how a caller -- the enterprise run result, and the
	// editor reading it -- tells keploy's own 1 from a test command's. Left as
	// a bare exit 1, an unverifiable run read as a failing suite.
	if m.config.Mock.Strict && !missesKnown && utils.ErrCode == 0 {
		utils.ErrCode = 1
		failedBy = FailedByStrict
		keployFailure = errors.New("--strict could not verify this replay: the agent could not report every call no recorded mock answered")
		m.logger.Error("replay failed under --strict: the agent could not report every call no recorded mock answered, so a clean run could not be proven",
			zap.String("why", counts.missesUnread),
			zap.String("next_step", "drop --strict to accept an unverified run, or remove the cause named in why and replay again"))
	}
	// Misses that WERE reported fail the run whatever else went unread.
	// Only a miss that counts against a test (models.UnmatchedCall.
	// CountsAgainstTest): not a DNS lookup's, which the proxy answers itself,
	// nor one on a connection whose client sent nothing, which made no call (a
	// MySQL client waiting at a port keploy could not tie to the recording, or
	// a dependency's TCP health check). Each is still told, with its next
	// step, above.
	if m.config.Mock.Strict && counts.missedData > 0 && utils.ErrCode == 0 {
		utils.ErrCode = 1
		failedBy = FailedByStrict
		m.logger.Error("replay failed under --strict: recorded dependency calls were missed",
			zap.Int("missed", counts.missedData),
			zap.String("next_step", "a dependency contract drifted; re-record the set (keploy mock record) or add the new calls with --on-miss record"))
	}

	// A replay that served NONE of the recorded calls verified nothing: the test
	// command never exercised the recording, so a green exit would be a false
	// pass (gap H2). This is the failure --strict cannot catch -- --strict is
	// about recorded calls that were MISSED, and a run that makes no matching
	// call at all misses nothing -- so it is enforced on its own, regardless of
	// --strict. It applies to the VERIFICATION policy only (--on-miss fail, the
	// default): --on-miss passthrough deliberately lets calls reach the real
	// service and never claims isolation, so a run that happened to match none
	// of the recording is that user's accepted outcome, not a false pass; and
	// --on-miss record consumes nothing while it extends the set. Also guarded
	// so it fires only when there WERE mocks to verify (loaded > 0; an empty set
	// is already warned loudly above, not failed here), the agent actually
	// reported the count (consumed is 0, not -1 "unknown"), and nothing else has
	// already failed the run (a crashed runner keeps its own, more useful
	// reason). Like a reported miss, it is the suite's contract, not keploy's own
	// failure, so it sets the exit code but is not returned as a keployFailure.
	// Under the proven switch the verdict's first rule (no test ran) takes
	// its place: a test recorded with no call serves nothing, and passes.
	if !provenswitch.On() && policy == models.MissFail && loaded > 0 && counts.consumed == 0 && utils.ErrCode == 0 {
		utils.ErrCode = 1
		failedBy = FailedByNothingVerified
		m.logger.Error("replay verified nothing: the test command served none of the recorded calls",
			zap.String("mock-set", name),
			zap.Int("loaded", loaded),
			zap.String("next_step", "check the test command actually makes the recorded dependency calls, and that the recording is not stale -- re-record it with keploy mock record if it is"))
	}

	// Last, because it is the weakest evidence: a count of untracked
	// processes that could not be read, from an agent that counts them (one
	// that never does is "unknown" and passes, as before), proves neither a
	// loss nor its absence. A reported miss or a run that verified nothing
	// claims the failure first and keeps its name. Only --strict fails on it;
	// without --strict it is said, and the receipt does not certify the run
	// as isolated (below).
	// Under the proven switch, a capture-loss count that could not be read
	// is the same verdict as a lost PID-map count, said as what it is.
	lossesUnread := false
	if pidCheck.unreadable == nil && lostErr != nil && captureIncomplete == "" {
		pidCheck.unreadable, lossesUnread = lostErr, true
	}
	if pidCheck.unreadable != nil {
		captureUnverified = models.CaptureUnverified(pidCheck.unreadable)
		unread, next := "tracked every process the test command ran", "check the agent logs for why its count could not be read, and for a WARN about its PID maps"
		if lossesUnread {
			captureUnverified = models.CaptureLossesUnverified(pidCheck.unreadable)
			unread, next = "captured every call the test command made", "check the agent logs for why its count could not be read"
		}
		if m.config.Mock.Strict && utils.ErrCode == 0 { // a failure already decided keeps its name
			utils.ErrCode = 1
			failedBy = FailedByStrict
			keployFailure = errors.New("--strict could not verify this replay: " + captureUnverified)
			m.logger.Error("replay failed under --strict: keploy could not read whether the agent "+unread+", so a clean run could not be proven",
				zap.String("mock-set", name), zap.Error(pidCheck.unreadable),
				zap.String("next_step", "drop --strict to accept an unverified run, or check the agent logs for why its count could not be read"))
		} else {
			if !m.config.Mock.Strict {
				next += "; --strict fails such a run"
			}
			m.logger.Warn("this replay is not verified: keploy could not read whether the agent "+unread+", so a call may have reached the real dependency unmocked",
				zap.String("mock-set", name), zap.Error(pidCheck.unreadable), zap.String("next_step", next))
		}
	}
	// The test command called what keploy cannot capture at all (decision
	// 20: a Unix socket; only counted under the proven switch, where the
	// tests it served are not judged): no loss of what keploy saw, so not a
	// failure, but nothing proves the run isolated either.
	if t := lostNotJudged.NotJudgedText(); t != "" && captureIncomplete == "" && captureUnverified == "" {
		captureUnverified = t
		m.logger.Warn("this replay is not verified: the test command called what keploy cannot capture, a TLS client abandoned its handshake, the kernel dropped connection attempts at keploy's proxy listener, or the Docker daemon did not answer keploy's read of a container a call reached ("+provenswitch.Env+"=1)",
			zap.String("mock-set", name), zap.String("reason", t),
			zap.String("next_step", lostNotJudged.NotJudgedRemedy()))
	}

	// 12. What the run proved -- decided once, so the log line below and the
	//     receipt cannot disagree -- then how much of the code it exercised,
	//     from the runner's own report, and the --min-coverage floor.
	bypass := bypassList(m.config.BypassRules)
	isolated, isolationNote := isolation(runnerExit, policy, counts, bypass)
	tests := countTests(counts.tests, gate.reason)
	if note, ungated := gateReport(gate, detail.windows); note != "" {
		m.logger.Warn("this replay was asked to run only some tests, and did not", zap.String("mock-set", name), zap.String("why", note),
			zap.String("next_step", `give the test runner a harness that reports each test to keploy (/agent/scope/begin with "canSkip":true) and skips a test whose answer says "action":"skip"`))
		if tests == nil {
			tests = &TestCounts{}
		}
		tests.GateNote, tests.Ungated = note, ungated
	}
	if isolated && tests != nil && tests.Gated > 0 {
		// Isolated holds for every call the tests that ran made; it says
		// nothing about the tests that did not run.
		isolationNote = fmt.Sprintf("isolated for the tests that ran: %d test(s) were gated and did not run", tests.Gated)
	}
	if captureIncomplete != "" {
		// Calls of an untracked process reached the real dependency.
		isolated, isolationNote = false, captureIncomplete
	} else if captureUnverified != "" && isolated {
		// Nothing shows they did, and nothing shows they did not.
		isolated, isolationNote = false, captureUnverified
	}
	var keployErr string
	if failedBy == FailedByKeploy && requestsOnly {
		isolated, isolationNote = false, "keploy did not complete the run: "+requestsShort
		keployFailure, keployErr = errors.New(isolationNote), requestsShort
	} else if failedBy == FailedByKeploy && appendOnly {
		isolated, isolationNote = false, "keploy did not complete the run: --on-miss record did not add every call it captured to the mock set"
		keployFailure, keployErr = fmt.Errorf("keploy did not complete the run: %w", notAppended), notAppended.Error()
	} else if failedBy == FailedByKeploy {
		isolated, isolationNote = false, "keploy did not complete the run: "+string(appErr.AppErrorType)
		keployFailure = errors.New(isolationNote)
		if appErr.Err != nil {
			keployErr = appErr.Err.Error()
			keployFailure = fmt.Errorf("keploy did not complete the run: %w", appErr.Err)
		}
	}
	cov, covNote := m.readCoverage(workDir, coverageBefore, isolated)
	if m.enforceMinCoverage(cov, covNote) {
		failedBy = FailedByMinCoverage
	}

	// 13. Say what the run proved, in the terminal as well as in the receipt.
	//     The verdict used to exist only in the receipt, and only a run that
	//     wrote a coverage report said anything about isolation at all.
	if isolated {
		m.logger.Info("this replay ran with every dependency answered from the recording",
			zap.String("mock-set", name), zap.Int("served", counts.consumed))
	} else {
		m.logger.Info("this replay did not prove the tests run with the dependencies off",
			zap.String("mock-set", name), zap.String("reason", isolationNote))
	}

	//     Leave the verdict where the editor, an agent and a status command
	//     can read it.
	finished = true
	writeReceipt(m.logger, m.config.Path, Receipt{
		Set:             name,
		At:              started.UTC().Truncate(time.Second),
		Command:         m.userCommand,
		OnMiss:          string(policy),
		Strict:          m.config.Mock.Strict,
		ExitCode:        utils.ErrCode,
		RunnerExitCode:  runnerExit,
		FailedBy:        failedBy,
		Error:           keployErr,
		Loaded:          loaded,
		Consumed:        counts.consumed,
		Missed:          counts.missed,
		MissedData:      provenCount(counts.missedData),
		Tests:           tests,
		Bypass:          bypass,
		Isolated:        isolated,
		IsolationNote:   isolationNote,
		MocksDigest:     digest,
		Coverage:        cov,
		CoverageNote:    covNote,
		MinCoverage:     m.config.Mock.MinCoverage,
		Version:         utils.Version,
		Refusals:        m.refusals,
		CannotCapture:   lostNotJudged,
		UnchargedMisses: counts.uncharged,
	})
	writeTestsReceipt(m.logger, m.config.Path, TestsReceipt{
		Set:           name,
		At:            started.UTC().Truncate(time.Second),
		MocksDigest:   digest,
		Tests:         counts.tests,
		Live:          m.live,
		Mocked:        m.mocked,
		Real:          realRun,
		CannotCapture: lostNotJudged,
		Learned:       counts.learnedPool,
	})
	return keployFailure
}

// readCoverage parses the coverage report this run's test command wrote, if
// it wrote one. The note says why there is none.
//
// The number is labelled by what the run proved. Only an isolated run
// exercised the code with every dependency off; any other run may have reached
// a real service, and calling its coverage "offline" would claim an isolation
// it never had.
func (m *mockService) readCoverage(dir string, before report.Before, isolated bool) (*report.Summary, string) {
	if dir == "" {
		return nil, "could not resolve the directory the test command ran in"
	}
	path, why := report.Find(dir, m.config.Mock.CoverageReport, m.userCommand, before)
	if path == "" {
		m.logger.Debug("no coverage report for this replay", zap.String("reason", why))
		return nil, why
	}
	s, err := report.ParseFile(path)
	if err != nil {
		m.logger.Warn("could not read the coverage report the test command wrote",
			zap.String("report", path), zap.Error(err))
		return nil, "the coverage report could not be read: " + err.Error()
	}
	if rel, err := filepath.Rel(dir, path); err == nil && !strings.HasPrefix(rel, "..") {
		s.Source = rel
	}
	label := "test coverage of this replay (not offline: isolation was not proven)"
	if isolated {
		label = "offline coverage: code your tests exercised with every dependency replayed"
	}
	m.logger.Info(label,
		zap.String("coverage", report.Floor1(s.Percent())+"%"),
		zap.String("covered", fmt.Sprintf("%d of %d %s", s.Covered, s.Total, s.Unit)),
		zap.String("report", s.Source))
	return &s, ""
}

// enforceMinCoverage fails a passing run that covered less than
// --min-coverage, and reports whether it did. A run already failed is left
// alone: it is red, and a second reason would only bury the first.
//
// A run that wrote no report FAILS the gate. A floor that passes whenever it
// cannot measure is a floor nobody has to clear.
func (m *mockService) enforceMinCoverage(cov *report.Summary, note string) bool {
	floor := m.config.Mock.MinCoverage
	if floor <= 0 || utils.ErrCode != 0 {
		return false
	}
	if cov == nil {
		next := "make the test command write one (go test -coverprofile=coverage.out, pytest --cov --cov-report=xml, jest --coverage, the JaCoCo report goal), or name it with --coverage-report"
		if utils.CmdType(m.config.CommandType) != utils.Native && m.config.CommandType != "" {
			// The runner is in a container: its report is written inside it.
			next = "the tests run in a container, so the report is written inside it: mount its directory into the working directory and name the report with --coverage-report"
		}
		utils.ErrCode = 1
		m.logger.Error("replay failed under --min-coverage: this run left no coverage report to check",
			zap.String("reason", note),
			zap.String("next_step", next))
		return true
	}
	if cov.Percent() < floor {
		utils.ErrCode = 1
		m.logger.Error("replay failed under --min-coverage: the tests cover less of the code than the floor",
			zap.String("coverage", report.Floor1(cov.Percent())+"%"),
			zap.String("covered", fmt.Sprintf("%d of %d %s", cov.Covered, cov.Total, cov.Unit)),
			zap.String("min-coverage", strconv.FormatFloat(floor, 'f', -1, 64)+"%"),
			zap.String("next_step", "add tests for the uncovered code and record their dependency calls (keploy mock record), or lower the floor"))
		return true
	}
	return false
}

// persistCaptured appends any calls the proxy captured on miss (served live from
// the real dependency under --on-miss record) to the set, then republishes it.
// It returns why calls it was handed were not all added to the set (each is
// also said at ERROR where it was found), and nil when every one was: the run
// then fails as keploy's, as keploy record fails a recording whose mocks were
// not written. It used to only log it, and a CI job that ran --on-miss record
// to grow the set went green while the set did not grow.
func (m *mockService) persistCaptured(ctx context.Context, name string) error {
	drainer, ok := m.instrumentation.(MissCapturer)
	if !ok {
		return nil
	}
	// An epilogue read: no deadline under the proven switch (epilogueRead),
	// should --on-miss record ever run under it (it is refused there today).
	var captured []*models.Mock
	err := epilogueRead(ctx, m.logger,
		"waiting for keploy's agent to hand over the calls it captured on miss",
		"press Ctrl+C again to stop now: they are then not added to the mock set",
		func(ctx context.Context) error {
			var err error
			captured, err = drainer.DrainCapturedMocks(ctx)
			return err
		})
	if err != nil {
		// What the agent captured on miss is not in keploy's hands, so not
		// in the set: a run that grows the set did not. A Ctrl+C that cut
		// the read short ends the run unjudged before its verdict (Replay).
		utils.LogError(m.logger, err, "the calls captured on miss could not be read from keploy's agent, so none was added to the mock set",
			zap.String("mock-set", name),
			zap.String("next_step", "check the agent logs for why, then run again with --on-miss record to capture them"))
		return fmt.Errorf("the calls captured on miss could not be read from keploy's agent, so none was added to the mock set %s: %w", name, err)
	}
	if len(captured) == 0 {
		return nil
	}
	// Seed the name counter to the set's highest existing mock-N so appended
	// mocks get fresh names (InsertMock always renames to mock-<counter+1>);
	// without this the replay-side counter starts at 0 and appended mocks reuse
	// mock-0, mock-1, … colliding with the recorded set (consumed-mock tracking
	// and mappings both key on name).
	//
	// A set that cannot be read cannot be numbered: numbering from the start
	// would give the appended mocks names already in the file, so nothing is
	// appended.
	highest, err := m.highestMockIndex(ctx, name)
	if err != nil {
		utils.LogError(m.logger, err, "the calls captured on miss were not added to the mock set: it could not be read to number them, and numbering from the start would reuse the names of mocks already in it",
			zap.String("mock-set", name), zap.Int("captured", len(captured)),
			zap.String("next_step", "fix the mock set so it loads (the error says why), then run again with --on-miss record to capture these calls"))
		return fmt.Errorf("the %d call(s) captured on miss were not added to the mock set %s: it could not be read to number them: %w", len(captured), name, err)
	}
	m.mockDB.SetCounterID(highest)
	appended, refused := 0, 0
	var refusedErr error
	for _, mk := range captured {
		if mk == nil {
			continue
		}
		if err := m.mockDB.InsertMock(ctx, mk, name); err != nil {
			utils.LogError(m.logger, err, "failed to add a call captured on miss to the mock set; it is not saved",
				zap.String("mock-set", name),
				zap.String("next_step", "run again with --on-miss record to capture it"))
			if refused++; refusedErr == nil {
				refusedErr = err
			}
			continue
		}
		appended++
	}
	var notAdded error
	if refused > 0 {
		notAdded = fmt.Errorf("%d of the %d call(s) captured on miss were not added to the mock set %s: %w", refused, refused+appended, name, refusedErr)
	}
	if appended == 0 {
		return notAdded
	}
	// The mock store may write in the background (the gob format): what it
	// was handed is in the set only once it is closed. It used to be left
	// open, and keploy exited with the captured calls still in the writer's
	// queue, after saying they were appended.
	if err := record.CloseMockStore(m.logger, m.mockDB, mockDrainNotice,
		"Still writing the calls captured on miss to the mock set",
		"press Ctrl+C again to stop now: the ones not written yet are then not added to it", nil); err != nil {
		utils.LogError(m.logger, err, "the calls captured on miss were not all added to the mock set",
			zap.String("mock-set", name), zap.Int("captured", appended),
			zap.String("next_step", "check the disk the mock set is on (space, permissions), then run again with --on-miss record to capture them"))
		return errors.Join(notAdded, fmt.Errorf("the %d call(s) captured on miss that the mock set %s was handed were not all written to it: %w", appended, name, err))
	}
	m.logger.Info("appended new dependency calls to the mock set (--on-miss record)",
		zap.Int("new", appended), zap.String("mock-set", name),
		zap.String("next_step", "review the added mocks and commit them; the next replay serves them without the real dependency"))
	if err := m.store.Push(ctx, name); err != nil {
		m.logger.Warn("failed to publish the refreshed mock set", zap.Error(err))
	}
	return notAdded
}

// statefulMocksDisabled is test.disableStatefulMocks, ignored under the proven
// switch (design v14 §10, a U18 row): a test's own mocks answer once
// each in recorded order, and the pool keeps its cursors. The flip deletes it.
func (m *mockService) statefulMocksDisabled() bool {
	if !m.config.Test.DisableStatefulMocks {
		return false
	}
	if provenswitch.On() {
		m.logger.Warn("disableStatefulMocks is ignored under KEPLOY_PROVEN: a test's own mocks answer once each, in recorded order",
			zap.String("next_step", "remove it from the configuration"))
		return false
	}
	return true
}

// loadMocks reads the mock set's per-test and session pools over the widest
// window, in one pass over the set's mock file when the store can
// (pkg.TestSetMocksReader), otherwise with one read per pool. failedPool names
// the pool whose read failed ("per-test" or "session").
func (m *mockService) loadMocks(ctx context.Context, name string, mapped map[string]bool) (filtered, unfiltered []*models.Mock, failedPool string, err error) {
	if reader, ok := m.mockDB.(pkg.TestSetMocksReader); ok {
		set, err := reader.GetTestSetMocks(ctx, name, models.BaseTime, time.Now(), mapped, mapped)
		if err != nil {
			// The pass that failed is the one the per-test pool was read in.
			return nil, nil, "per-test", err
		}
		return set.Filtered, set.Unfiltered, "", nil
	}
	filtered, err = m.mockDB.GetFilteredMocks(ctx, name, models.BaseTime, time.Now(), mapped, mapped)
	if err != nil {
		return nil, nil, "per-test", err
	}
	unfiltered, err = m.mockDB.GetUnFilteredMocks(ctx, name, models.BaseTime, time.Now(), mapped, mapped)
	if err != nil {
		return nil, nil, "session", err
	}
	return filtered, unfiltered, "", nil
}

// highestMockIndex returns the largest N across the set's existing "mock-N"
// names, or -1 when the set is empty / has no mock-N names. Seeding the counter
// to this value makes the next InsertMock name its mock "mock-<N+1>". It fails
// when the set cannot be read.
func (m *mockService) highestMockIndex(ctx context.Context, name string) (int64, error) {
	all := map[string]bool{}
	var filtered, unfiltered []*models.Mock
	var skipped map[string]models.Kind
	if reader, ok := m.mockDB.(pkg.TestSetMocksReader); ok {
		set, err := reader.GetTestSetMocks(ctx, name, models.BaseTime, time.Now(), all, all)
		if err != nil {
			return -1, err
		}
		filtered, unfiltered, skipped = set.Filtered, set.Unfiltered, set.Skipped
	} else {
		var err error
		if filtered, err = m.mockDB.GetFilteredMocks(ctx, name, models.BaseTime, time.Now(), all, all); err != nil {
			return -1, err
		}
		if unfiltered, err = m.mockDB.GetUnFilteredMocks(ctx, name, models.BaseTime, time.Now(), all, all); err != nil {
			return -1, err
		}
	}
	highest := int64(-1)
	consider := func(name string) {
		if n, ok := strings.CutPrefix(name, "mock-"); ok {
			if idx, err := strconv.ParseInt(n, 10, 64); err == nil && idx > highest {
				highest = idx
			}
		}
	}
	for _, mocks := range [][]*models.Mock{filtered, unfiltered} {
		for _, mk := range mocks {
			if mk != nil {
				consider(mk.Name)
			}
		}
	}
	// A document the decoders skipped (a kind this keploy cannot read, a
	// connection failure it cannot replay) is still in the file under its
	// name, which an appended mock must not take.
	for name := range skipped {
		consider(name)
	}
	return highest, nil
}

// pushScopeTable reads mappings.yaml for the set (if per-test mappings exist)
// and hands the agent the name→mock-names table so per-test scoping works.
// Under the proven switch it is pushProvenTable: loaded are the mocks the
// agent holds, and decoded every mock the set decoded.
func (m *mockService) pushScopeTable(ctx context.Context, name string, filtered, unfiltered, decoded []*models.Mock) error {
	if provenswitch.On() {
		return m.pushProvenTable(ctx, name, append(append([]*models.Mock(nil), filtered...), unfiltered...), decoded)
	}
	if m.mappingDB == nil {
		return nil
	}
	if m.pushSetTable(ctx, name) {
		return nil
	}
	pusher, ok := m.instrumentation.(ScopePusher)
	if !ok {
		return nil
	}
	mappings, meaningful, err := m.mappingDB.Get(ctx, name)
	if err != nil || len(mappings) == 0 || !meaningful {
		return nil
	}
	table := make(map[string][]string, len(mappings))
	for testName, entries := range mappings {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name)
		}
		table[testName] = names
	}
	if err := pusher.PushScopeTable(ctx, table); err != nil {
		m.logger.Debug("failed to push per-test scope table; per-test scoping disabled for this run", zap.Error(err))
		return nil
	}
	m.logger.Info("per-test scoping enabled", zap.Int("tests", len(table)), zap.String("mock-set", name))
	return nil
}

// pushProvenTable hands the agent, under the proven switch, what each test
// owns and each key's pool slice (design v14 §5.2): the one view the agent
// serves a replay from. A test owns exactly the mocks its mapping lists (the
// agent keeps the data-plane ones); a test the mapping omits, or a set with
// no mapping file or an all-empty one, owns none, so a dependency-free set
// stays green and every other call misses with its reason. Every mock no
// test lists is the pool's: under the key it was recorded with (mappings'
// startup:), else models.PoolKeyNone. A mapping that cannot be read, or that
// names a mock the recording does not have, refuses, and so does an agent
// that refuses the table: nothing is served from a whole pool instead.
func (m *mockService) pushProvenTable(ctx context.Context, name string, loaded, decoded []*models.Mock) error {
	refuse := func(why string) error {
		return fmt.Errorf("keploy cannot judge each test of mock set %q: %s; record it again with keploy mock record", name, why)
	}
	// unreadable is refuse for a mappings file keploy cannot read, which a
	// hand edit may have made so (a test entry with both mock lists): a
	// record would replace the edit, so correcting it comes first. err is
	// kept (models.BothMockListsError names the test).
	unreadable := func(err error) error {
		return fmt.Errorf("keploy cannot judge each test of mock set %q: its mappings cannot be read: %w; if you edited the set's mappings file, correct that edit (a record would replace it), otherwise record the set again with keploy mock record", name, err)
	}
	if m.mappingDB == nil {
		return refuse("this keploy reads no per-test mappings")
	}
	reader, ok := m.mappingDB.(MappingReader)
	if !ok {
		return refuse("this keploy cannot read the calls the set recorded outside its tests")
	}
	mp, err := reader.GetMapping(ctx, name)
	if err != nil {
		return unreadable(err)
	}
	if mp != nil && (len(mp.Boots) > 0 || len(mp.Suites) > 0) {
		return refuse("its tests start apps of their own (boots), which this release does not judge test by test")
	}
	mappings, _, err := m.mappingDB.Get(ctx, name)
	if err != nil {
		return unreadable(err)
	}
	pp, ok := m.instrumentation.(ProvenTablePusher)
	if !ok {
		return refuse("this keploy's agent takes no per-test table with pool slices")
	}
	table := make(map[string][]string, len(mappings))
	for testName, entries := range mappings {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name)
		}
		table[testName] = names
	}
	pool, unknown := poolSlices(mp, table, loaded, decoded)
	if unknown = append(unknown, unknownBesideTests(mp, decoded, unknown)...); len(unknown) > 0 {
		return m.refuseUnknownMocks(ctx, name, mp, unknown)
	}
	// Every test's recorded window, of every recording its mapping lists,
	// the ones the agent never holds included: they were made in it.
	windows := recordedTestWindows(mp, table, decoded)
	table = heldOnly(table, loaded)
	background := backgroundMarks(mappings, table)
	if !ownsTestMocks(table, loaded) {
		m.logger.Warn("under KEPLOY_PROVEN no test of this mock set owns a recorded call: it has no mappings.yaml, or its mapping lists no data-plane mock the agent holds, so every call a test makes to a dependency misses",
			zap.String("mock-set", name),
			zap.String("next_step", "record the set again with this keploy: keploy mock record writes mappings.yaml"))
	}
	if err := pp.PushProvenTable(ctx, table, pool, windows, background); err != nil {
		return refuse("its agent refused the per-test table: " + err.Error())
	}
	m.mockOwners = models.MockOwners(table)
	m.logger.Info("per-test scoping enabled", zap.Int("tests", len(table)), zap.Int("pool slices", len(pool)), zap.Int("recorded test windows", len(windows)), zap.Int("background recordings", len(background)), zap.String("mock-set", name))
	return nil
}

// unknownBesideTests is, sorted, each name mappings' worker:, shared: and
// runner: give that no decoded mock has and that seen does not list: a name
// none of them serves (they are the pool's whatever they say), but a hand edit
// that names a mock the recording does not have, which is refused like one in
// a test's list or startup: (poolSlices) rather than ignored.
func unknownBesideTests(mp *models.Mapping, decoded []*models.Mock, seen []string) []string {
	if mp == nil {
		return nil
	}
	have := make(map[string]bool, len(decoded)+len(seen))
	for _, mk := range decoded {
		if mk != nil && mk.Name != "" {
			have[mk.Name] = true
		}
	}
	for _, n := range seen {
		have[n] = true
	}
	var out []string
	for _, entries := range [][]models.MockEntry{mp.Worker, mp.Shared, mp.Runner} {
		for _, e := range entries {
			if e.Name != "" && !have[e.Name] {
				have[e.Name] = true
				out = append(out, e.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// refuseUnknownMocks is the refusal of a set whose mappings.yaml names mocks
// (unknown) its recording does not have: each with where the file names it
// (models.Mapping.MockPlaces), and what to do, for a hand edit and for a
// recording that lost them alike.
func (m *mockService) refuseUnknownMocks(ctx context.Context, name string, mp *models.Mapping, unknown []string) error {
	places := mp.MockPlaces()
	sort.Strings(unknown)
	dir := filepath.Base(m.config.Path)
	// The file the replay's read took the set's mocks from, and the named
	// documents of it the read skipped, with the read's own reasons: such a
	// name is not a wrong one, the edit to correct is in the mocks file. Nil
	// when the file could not be read again: then no name is called absent.
	var file string
	var unread map[string]models.SkippedMock
	if m.mockDB != nil {
		file, unread = m.mockDB.SkippedMocks(ctx, name)
	}
	if file == "" {
		file = MocksFile(m.config.Path, name)
	}
	mocks := "mocks.yaml"
	if file != "" {
		mocks = filepath.Base(file)
	}
	mappings := "mappings.yaml"
	for _, f := range []string{"mappings.yaml", "mappings.json"} {
		if info, err := os.Stat(filepath.Join(m.config.Path, name, f)); err == nil && info.Mode().IsRegular() {
			mappings = f
			break
		}
	}
	var absent, unchecked, skipped []string
	var cutShort, edited, newer, connFailure, yamlOnly bool
	for _, n := range unknown {
		where := ""
		if w := places[n]; len(w) > 0 {
			where = " (under " + strings.Join(w, ", ") + ")"
		}
		d, ok := unread[n]
		switch {
		case unread == nil:
			unchecked = append(unchecked, n+where)
		case !ok:
			absent = append(absent, n+where)
		case d.Why == models.MockSkipCutShort && d.Cause != "":
			// An NDJSON line: whole JSON, with a spec that does not decode.
			cutShort = true
			skipped = append(skipped, fmt.Sprintf("%s%s, the file's last line, which has no newline and whose spec does not decode (%s): keploy reads it as a write cut short", n, where, d.Cause))
		case d.Why == models.MockSkipCutShort:
			cutShort = true
			skipped = append(skipped, n+where+", the file's last document, whose last line has no newline: keploy reads it as a write cut short")
		case d.Why == models.MockSkipNoKind:
			edited = true
			skipped = append(skipped, n+where+", which has no kind")
		case d.Why == models.MockSkipConnFailure:
			connFailure = true
			skipped = append(skipped, n+where+", a connection failure this keploy cannot replay")
		case d.Why == models.MockSkipYAMLOnlyKind:
			yamlOnly = true
			skipped = append(skipped, fmt.Sprintf("%s%s, of kind %q, which keploy reads only from a YAML mock file", n, where, d.Kind))
		default:
			edited, newer = true, true
			skipped = append(skipped, fmt.Sprintf("%s%s, of kind %q, which this keploy cannot read", n, where, d.Kind))
		}
	}
	var why, fix []string
	if len(skipped) > 0 {
		why = append(why, fmt.Sprintf("%s/%s/%s has mocks %s/%s/%s names that this keploy skips: %s",
			dir, name, mocks, dir, name, mappings, strings.Join(skipped, "; ")))
		if cutShort {
			if filepath.Ext(mocks) == ".json" {
				fix = append(fix, fmt.Sprintf("If you saved %s by hand, give its last line the spec keploy wrote and end the line with a newline", mocks))
			} else {
				fix = append(fix, fmt.Sprintf("If you saved %s by hand, end it with a newline once its last mock is whole", mocks))
			}
		}
		if edited {
			fix = append(fix, fmt.Sprintf("If you edited %s, give each mock this keploy cannot read back the kind and spec keploy wrote", mocks))
		}
		if newer {
			fix = append(fix, "If a newer keploy or keploy enterprise recorded them, upgrade keploy or use keploy enterprise")
		}
		if connFailure {
			fix = append(fix, fmt.Sprintf("If a newer keploy recorded a connection failure, upgrade keploy to replay it; if you edited one in %s, give it back the fields keploy wrote", mocks))
		}
		if yamlOnly {
			fix = append(fix, fmt.Sprintf("Keploy reads a mock of such a kind only from a YAML mock file, and one with a \"-\" in its kind only with keploy enterprise: record the set again in YAML, not as %s", mocks))
		}
	}
	if len(absent) > 0 {
		why = append(why, fmt.Sprintf("%s/%s/%s names mocks %s/%s/%s does not have: %s",
			dir, name, mappings, dir, name, mocks, strings.Join(absent, ", ")))
		fix = append(fix, fmt.Sprintf("If you edited %s, correct those names", mappings))
	}
	if len(unchecked) > 0 {
		// The file was not read again, so nothing says whether it has them.
		why = append(why, fmt.Sprintf("%s/%s/%s names mocks the replay did not read from %s/%s/%s: %s",
			dir, name, mappings, dir, name, mocks, strings.Join(unchecked, ", ")))
		fix = append(fix, fmt.Sprintf("Check that %s has each of them, and that keploy can read it", mocks))
	}
	return fmt.Errorf("keploy cannot judge each test of mock set %q: %s. %s; otherwise record the set again with keploy mock record",
		name, strings.Join(why, "; "), strings.Join(fix, ". "))
}

// backgroundMarks is, of the mappings' test entries the agent is handed
// (table), those judging marked as the application's own background work
// (models.MockEntry.Background), each with why: the agent serves each in its
// test's window and never requires it. nil when none is marked.
func backgroundMarks(mappings map[string][]models.MockEntry, table map[string][]string) map[string]string {
	var marks map[string]string
	for test, entries := range mappings {
		held := make(map[string]bool, len(table[test]))
		for _, n := range table[test] {
			held[n] = true
		}
		for _, e := range entries {
			if e.Background == "" || !held[e.Name] {
				continue
			}
			if marks == nil {
				marks = map[string]string{}
			}
			marks[e.Name] = e.Background
		}
	}
	return marks
}

// ownsTestMocks reports whether a test of table owns a test mock: a
// data-plane mock of loaded (a protocol-phase mock is the pool's).
func ownsTestMocks(table map[string][]string, loaded []*models.Mock) bool {
	test := make(map[string]bool, len(loaded))
	for _, mk := range loaded {
		if mk != nil && mk.Name != "" {
			test[mk.Name] = !mk.IsProtocolPhase()
		}
	}
	for _, names := range table {
		for _, n := range names {
			if test[n] {
				return true
			}
		}
	}
	return false
}

// heldOnly is table less the names the agent does not hold: a mock the set
// decoded that keploy never hands the agent (models.AgentBound: a connection
// failure) is no test's, and the call it recorded misses. The agent would
// refuse the begin of a test that owns one.
func heldOnly(table map[string][]string, loaded []*models.Mock) map[string][]string {
	held := make(map[string]bool, len(loaded))
	for _, mk := range loaded {
		if mk != nil && mk.Name != "" {
			held[mk.Name] = true
		}
	}
	out := make(map[string][]string, len(table))
	for test, names := range table {
		kept := make([]string, 0, len(names))
		for _, n := range names {
			if held[n] {
				kept = append(kept, n)
			}
		}
		out[test] = kept
	}
	return out
}

// poolSlices is each key's pool slice of a recording (design v14 §5.2): every
// loaded mock no test's mapping lists, under the key its mappings' startup:
// entry gives it, else models.PoolKeyNone (an older recording's, or one made
// outside every claim). unknown lists, sorted, the names the table or the
// startup entries give that no decoded mock has (a mock the agent never holds,
// a connection failure, is decoded, and in no slice).
func poolSlices(mp *models.Mapping, table map[string][]string, loaded, decoded []*models.Mock) (pool map[string][]string, unknown []string) {
	have := make(map[string]bool, len(decoded))
	for _, mk := range decoded {
		if mk != nil && mk.Name != "" {
			have[mk.Name] = true
		}
	}
	held := make(map[string]bool, len(loaded))
	for _, mk := range loaded {
		if mk != nil && mk.Name != "" {
			held[mk.Name] = true
		}
	}
	missing := map[string]bool{}
	mapped := map[string]bool{}
	for _, names := range table {
		for _, n := range names {
			mapped[n] = true
			if !have[n] {
				missing[n] = true
			}
		}
	}
	pool = map[string][]string{}
	keyed := map[string]bool{}
	if mp != nil {
		for _, e := range mp.Startup {
			if e.Name == "" || mapped[e.Name] || keyed[e.Name] {
				continue
			}
			if !have[e.Name] {
				missing[e.Name] = true
				continue
			}
			if !held[e.Name] {
				continue
			}
			key := e.Key
			if key == "" {
				key = models.PoolKeyNone
			}
			keyed[e.Name] = true
			pool[key] = append(pool[key], e.Name)
		}
	}
	for _, mk := range loaded {
		if mk == nil || mk.Name == "" || mapped[mk.Name] || keyed[mk.Name] {
			continue
		}
		keyed[mk.Name] = true
		pool[models.PoolKeyNone] = append(pool[models.PoolKeyNone], mk.Name)
	}
	for n := range missing {
		unknown = append(unknown, n)
	}
	sort.Strings(unknown)
	return pool, unknown
}

// pushTestBegins hands the agent when each test of the recording began
// (mappings.yaml), so the agent moves the run's clock to a test's begin while
// it is open, and back to the run's start time after it (design v14 §3.4). A
// recording without per-test begins gives none, and its tests run at the
// start time. A keploy that cannot read the recording's mappings refuses: its
// tests would all run at the start time with no word.
func (m *mockService) pushTestBegins(ctx context.Context, name string) error {
	pusher, ok := m.instrumentation.(TestBeginsPusher)
	if !ok {
		return errors.New("this keploy's agent client cannot move the clock per test")
	}
	reader, ok := m.mappingDB.(MappingReader)
	if !ok {
		return errors.New("this keploy cannot read when the recording's tests began (its mappings)")
	}
	mapping, err := reader.GetMapping(ctx, name)
	if err != nil {
		return fmt.Errorf("read when the tests of %s began: %w", name, err)
	}
	begins := testBegins(mapping)
	real := testReal(mapping)
	if err := pusher.PushTestBegins(ctx, begins, real); err != nil {
		return err
	}
	m.realListed = real
	for _, id := range slices.Sorted(maps.Keys(real)) {
		// F3: the test runs on the real clock, so what its recording built
		// on the recorded time ages; the summary lists it UNFROZEN.
		m.logger.Info("a test that uses mock.real runs on the real clock, not at its recorded time: re-record it when its recording ages",
			zap.String("test", id), zap.Strings("real", real[id]))
	}
	m.logger.Info("sent the agent when each test began in its recording", zap.Int("tests", len(begins)), zap.String("mock-set", name))
	return nil
}

// testReal is, by test ID, the mock.real destinations each test of mapping
// lists: those tests run on the real clock (design v14 §3.4, F3).
func testReal(mapping *models.Mapping) map[string][]string {
	if mapping == nil {
		return nil
	}
	var out map[string][]string
	for _, tc := range mapping.TestCases {
		if len(tc.Real) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		out[tc.ID] = mergeSorted(out[tc.ID], tc.Real)
	}
	return out
}

// testBegins is when each test of mapping began, by test ID.
func testBegins(mapping *models.Mapping) map[string]time.Time {
	begins := map[string]time.Time{}
	if mapping == nil {
		return begins
	}
	for _, tc := range mapping.TestCases {
		if !tc.At.IsZero() {
			begins[tc.ID] = tc.At
		}
	}
	return begins
}

// replayWindows are the per-test windows of this replay. Under docker compose
// the agent was stopped with the runner, so they come from the account it left
// as it stopped (see reportOutcome); otherwise from the agent itself.
// With the proven switch on a failed read is an error; off, it is no windows.
func (m *mockService) replayWindows(ctx context.Context) ([]models.ScopeWindow, error) {
	if r, ok := m.instrumentation.(ComposeOutcomeReader); ok && m.isDockerCompose() {
		outcome, err := r.ComposeAgentOutcome()
		if err != nil {
			if provenswitch.On() {
				return nil, err
			}
			m.logger.Debug("no per-test windows: the keploy-agent container left no replay outcome", zap.Error(err))
			return nil, nil
		}
		return m.takeRunWindows(outcome.Windows), nil
	}
	if provenswitch.On() {
		return m.readWindows(ctx)
	}
	return m.agentWindows(ctx), nil
}

// ScopeGateRequest is what a ScopeGateSource is told about the replay it gates.
type ScopeGateRequest struct {
	// Set is the mock set being replayed.
	Set string
	// MocksDigest is the digest of the recording this replay serves (see
	// Receipt.MocksDigest), so a source can tell a list proven against this
	// recording from one proven against a recording since replaced.
	MocksDigest string
	// Path is the keploy folder the set lives in.
	Path string
}

// ScopeGateSource names the tests a replay should run, and why the rest
// should not; run == nil runs every test. A wrapping build (enterprise)
// installs one from init(), for instance to run only the tests an earlier
// replay of the same recording proved pass with every dependency mocked. OSS
// installs none, so every test runs.
type ScopeGateSource func(ctx context.Context, req ScopeGateRequest) (run []string, reason string)

var scopeGateSource ScopeGateSource

// RegisterScopeGateSource installs the gate source. A nil fn removes it.
func RegisterScopeGateSource(fn ScopeGateSource) {
	scopeGateSource = fn
}

// ScopeSkipSource is the proven model's gate (design v14 §6.1): the tests a
// replay skips, by ID, each with the reason its harness skips it with, and
// the tests each test needs to have begun before it
// (models.ScopeGateReq.Needs). It is a deny list: every test it does not
// name runs. A wrapping build
// (enterprise) installs one that reads the set's verdicts.yaml. An error
// refuses the replay: running a test the list quarantines, or skipping one
// it does not, would make the run something other than CI's.
type ScopeSkipSource func(ctx context.Context, req ScopeGateRequest) (skip map[string]string, needs map[string][]models.ScopeNeed, err error)

var scopeSkipSource ScopeSkipSource

// RegisterScopeSkipSource installs the skip source, read under the proven
// switch. A nil fn removes it.
func RegisterScopeSkipSource(fn ScopeSkipSource) {
	scopeSkipSource = fn
}

// ScopeSkipPusher is an optional Instrumentation extension: under the proven
// switch, Replay hands the agent the tests to skip with it.
type ScopeSkipPusher interface {
	PushScopeSkips(ctx context.Context, skip map[string]string, needs map[string][]models.ScopeNeed) error
}

// pushScopeSkips hands the agent the proven gate, under the proven switch and
// when a skip source is installed. Its failure refuses the replay.
func (m *mockService) pushScopeSkips(ctx context.Context, name, digest string) error {
	if !provenswitch.On() || scopeSkipSource == nil {
		return nil
	}
	skip, needs, err := scopeSkipSource(ctx, ScopeGateRequest{Set: name, MocksDigest: digest, Path: m.config.Path})
	if err != nil {
		return fmt.Errorf("could not read which tests this replay skips: %w", err)
	}
	pusher, ok := m.instrumentation.(ScopeSkipPusher)
	if !ok {
		if len(skip) == 0 && len(needs) == 0 {
			return nil
		}
		return errors.New("this keploy cannot tell its agent which tests to skip")
	}
	if skip == nil {
		skip = map[string]string{}
	}
	if err := pusher.PushScopeSkips(ctx, skip, needs); err != nil {
		return fmt.Errorf("could not tell the agent which tests to skip: %w", err)
	}
	if len(skip) > 0 {
		m.logger.Info("tests keploy skips this replay, each with its reason", zap.Int("tests", len(skip)), zap.String("mock-set", name))
	}
	if len(needs) > 0 {
		m.logger.Info("tests that use a value an earlier test sends: each runs only after that test, else its harness skips it naming both calls", zap.Int("tests", len(needs)), zap.String("mock-set", name))
	}
	return nil
}

// requestedGate is the gate a replay asked the agent for: the tests to run
// (nil: every test) and why the rest should not.
type requestedGate struct {
	run    []string
	reason string
}

// pushScopeGate tells the agent which tests should run this replay, and
// returns what it asked for. It pushes even when nothing is gated — run == nil
// clears the gate — so a long-lived agent never carries a gate into this
// replay.
func (m *mockService) pushScopeGate(ctx context.Context, name, digest string) requestedGate {
	pusher, ok := m.instrumentation.(ScopeGatePusher)
	if !ok {
		return requestedGate{}
	}
	var g requestedGate
	switch {
	case len(m.config.Mock.RunOnly) > 0:
		// The user's own choice wins over any policy a wrapping build installs.
		g = requestedGate{run: m.config.Mock.RunOnly, reason: "not in --run-only"}
		m.warnUnknownRunOnly(ctx, name)
	case scopeGateSource != nil:
		g.run, g.reason = scopeGateSource(ctx, ScopeGateRequest{Set: name, MocksDigest: digest})
	}
	err := pusher.PushScopeGate(ctx, g.run, g.reason)
	switch {
	case err == nil:
		if g.run != nil {
			// A request: only a harness that can skip a test honours it, and
			// gateReport says afterwards whether it did.
			m.logger.Info("asking the test runner's harness to run only some tests", zap.Int("tests", len(g.run)), zap.String("mock-set", name), zap.String("reason", g.reason))
		}
		return g
	case g.run == nil && errors.Is(err, models.ErrScopeGateUnsupported):
		// Nothing to gate, and nothing a gate-less agent could carry over.
	default:
		m.logger.Warn("could not tell the agent which tests to run; every test runs this replay",
			zap.String("mock-set", name), zap.Error(err),
			zap.String("next_step", "run the keploy-agent of this keploy version"))
	}
	return requestedGate{}
}

// warnUnknownRunOnly warns about a --run-only name that no recorded test of
// the set has: a typo there gates out every test it meant to keep.
func (m *mockService) warnUnknownRunOnly(ctx context.Context, name string) {
	if m.mappingDB == nil {
		return
	}
	mappings, meaningful, err := m.mappingDB.Get(ctx, name)
	if err != nil || !meaningful || len(mappings) == 0 {
		return
	}
	var unknown []string
	for _, want := range m.config.Mock.RunOnly {
		if !gateNamesAny(want, mappings) {
			unknown = append(unknown, want)
		}
	}
	if len(unknown) > 0 {
		m.logger.Warn("--run-only names tests this mock set's mappings do not have: a typo? (a test that recorded no dependency call, or was added since the recording, is not in them, and still runs)",
			zap.Strings("tests", unknown), zap.String("mock-set", name),
			zap.String("next_step", "compare with the names the test runner's harness reports, as in keploy/"+name+"/mappings.yaml"))
	}
}

// gateNamesAny reports whether name, as a gate entry, lets any recorded test
// run: the test itself or one of its subtests.
func gateNamesAny(name string, recorded map[string][]models.MockEntry) bool {
	for test := range recorded {
		if test == name || strings.HasPrefix(test, name+"/") {
			return true
		}
	}
	return false
}

// gateAllows reports whether a gate's run list lets the named test run: it, or
// a test it is a subtest of, is listed.
func gateAllows(run []string, name string) bool {
	for _, r := range run {
		if name == r || strings.HasPrefix(name, r+"/") {
			return true
		}
	}
	return false
}

// gateReport says how a requested gate played out, from the scopes the tests
// reported: "" when nothing was gated or it was honoured. A harness that is not
// there (no test reported a scope) or cannot skip (a test outside the run list
// ran — with a verdict or without) makes the run something other than the
// subset asked for — said here so it is never mistaken for that subset.
func gateReport(g requestedGate, windows []models.ScopeWindow) (note string, ungated int) {
	if g.run == nil {
		return "", 0
	}
	outside := map[string]bool{}
	for _, w := range windows {
		if w.Name == "" || w.Outcome == models.ScopeOutcomeGated {
			continue
		}
		if !gateAllows(g.run, w.Name) {
			outside[w.Name] = true
		}
	}
	switch {
	case len(windows) == 0:
		return "no test reported to keploy, so the request to run only some tests had no effect", 0
	case len(outside) > 0:
		return fmt.Sprintf("%d test(s) outside the run list ran: the test runner's harness cannot skip a test", len(outside)), len(outside)
	}
	return "", 0
}

func (m *mockService) pushSetTable(ctx context.Context, name string) bool {
	reader, ok := m.mappingDB.(MappingReader)
	if !ok {
		return false
	}
	pusher, ok := m.instrumentation.(SetPusher)
	if !ok {
		return false
	}
	mapping, err := reader.GetMapping(ctx, name)
	if err != nil || mapping == nil || (len(mapping.Boots) == 0 && len(mapping.Suites) == 0) {
		return false
	}
	root := gitTop()
	sets := setTable(mapping, root)
	if err := pusher.PushSetTable(ctx, root, sets); err != nil {
		m.logger.Warn("could not send the agent each set's mocks and recorded start; using the per-test mocks instead", zap.Error(err))
		return false
	}
	m.logger.Info("sent the agent each set's mocks and recorded start", zap.Int("sets", len(sets)), zap.String("mock-set", name))
	return true
}

func setTable(mapping *models.Mapping, root string) map[string]models.SetTable {
	sets := map[string]models.SetTable{}
	get := func(dir string) models.SetTable {
		set := setName(dir, root)
		st, ok := sets[set]
		if !ok {
			st = models.SetTable{Boots: map[string][]string{}, Tests: map[string][]models.Owned{}}
		}
		return st
	}
	for _, b := range mapping.Boots {
		st := get(b.Dir)
		names := make([]string, 0, len(b.Mocks))
		for _, e := range b.Mocks {
			names = append(names, e.Name)
		}
		if b.Key == "" {
			st.Runner = append(st.Runner, names...)
		} else {
			st.Boots[b.Key] = names
		}
		sets[setName(b.Dir, root)] = st
	}
	for _, tc := range mapping.TestCases {
		st := get(tc.Dir)
		owned := make([]models.Owned, 0, len(tc.Mocks))
		for _, e := range tc.Mocks {
			owned = append(owned, models.Owned{Name: e.Name, Start: e.Start})
		}
		st.Tests[tc.ID] = owned
		sets[setName(tc.Dir, root)] = st
	}
	for _, su := range mapping.Suites {
		k := setName(su.Dir, root)
		if st, ok := sets[k]; ok && !su.Start.IsZero() && (st.Start.IsZero() || su.Start.Before(st.Start)) {
			st.Start = su.Start
			sets[k] = st
		}
	}
	return sets
}

func ranSets(windows []models.ScopeWindow) []string {
	root := gitTop()
	seen := map[string]bool{}
	var sets []string
	for _, w := range windows {
		if w.Dir == "" || w.App || w.Suite {
			continue
		}
		if s := setName(w.Dir, root); !seen[s] {
			seen[s] = true
			sets = append(sets, s)
		}
	}
	return sets
}

func setName(dir, root string) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return filepath.ToSlash(dir)
	}
	if root != "" {
		dirs := []string{dir}
		if r, err := filepath.EvalSymlinks(dir); err == nil && r != dir {
			dirs = append(dirs, r)
		}
		for _, d := range dirs {
			if rel, err := filepath.Rel(root, d); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.ToSlash(dir)
}

func gitTop() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			if resolved, err := filepath.EvalSymlinks(top); err == nil {
				return resolved
			}
			return top
		}
	}
	wd, _ := os.Getwd()
	return wd
}

// ReplayOutcome is what a replay produced, for a wrapping build that meters or
// reports usage. Counts only; no payloads, no destinations.
type ReplayOutcome struct {
	SetName  string
	Loaded   int
	Consumed int
	Missed   int
	// Cases is each recorded request replayed, with the app's answer compared to the recording.
	Cases []CaseOutcome
	// Mocks is what each test used and missed, next to what it recorded.
	Mocks  []FlowMocks
	Starts map[string]int
	Sets   []string
}

// replayOutcomeReporter is installed by a wrapping build (enterprise) from
// init(). Mirrors the RegisterNativeCommandSupport extension point in
// cli/provider/hooks.go: OSS stays free of any billing/api-server dependency,
// and a build that has one opts in.
var replayOutcomeReporter func(context.Context, ReplayOutcome)

// RegisterReplayOutcomeReporter installs the reporter. A nil fn disables it.
func RegisterReplayOutcomeReporter(fn func(context.Context, ReplayOutcome)) {
	replayOutcomeReporter = fn
}

// replayCounts are the agent's end-of-run counts. -1 means that read failed:
// unknown, which is not the same as zero.
type replayCounts struct {
	consumed   int
	missed     int
	missedData int
	// missesUnread is why the read of the calls no mock answered was
	// incomplete (models.UnreadCause), when missed is -1.
	missesUnread string
	tests        []TestReceipt
	// learnedPool is what the learn step wrote onto mocks no test consumed.
	learnedPool []string
	// learnErr is why what the learn step learned could not be written: the
	// round did not complete.
	learnErr error
	// uncharged are, under the proven switch, the misses missedData counts
	// that no test was charged with (Receipt.UnchargedMisses); nil when the
	// misses were not read.
	uncharged []UnchargedMiss
}

// reportOutcome logs which mocks were consumed and which outgoing calls matched
// nothing. It returns the consumed and missed counts, each -1 when its read
// failed; whether the MISSES specifically were readable is the only half
// --strict turns on. stopped reports whether the user's interrupt stopped the
// replay: a read it cut short is not the agent's failure, and the replay ends
// unjudged on it (Replay).
//
// The two reads are tracked apart on purpose. Collapsing them into one "did the
// agent answer" flag makes a half-answer indistinguishable from no answer, and
// then reports a run whose misses were never read as a clean one.
func (m *mockService) reportOutcome(ctx context.Context, loaded int, detail replayDetail, stopped func() bool) replayCounts {
	var consumed []models.MockState
	var misses []models.UnmatchedCall
	var consumedErr, missesErr error
	// Under docker compose the runner exiting is what stops the whole project,
	// the agent service included, so by now there is no agent left to ask. It
	// says what it served and missed as it is stopped, and that account is
	// read here instead.
	composeOutcome, fromCompose := m.instrumentation.(ComposeOutcomeReader)
	fromCompose = fromCompose && m.isDockerCompose()
	if fromCompose {
		outcome, err := composeOutcome.ComposeAgentOutcome()
		consumed, misses, consumedErr, missesErr = outcome.Consumed, outcome.Missed, err, err
		if err == nil && outcome.MissedUnread != "" {
			// As the agent's own read would have said it: incomplete, with
			// what was read.
			missesErr = fmt.Errorf("%w: %s", models.ErrUnmatchedCallsUnread, outcome.MissedUnread)
		}
	} else {
		// Epilogue reads: no deadline under the proven switch, where they are
		// what keploy's verdict rests on (epilogueRead). Each read's error is
		// its own; the user's interrupt that gives them up is the run's
		// (stopped).
		_ = epilogueRead(ctx, m.logger,
			"waiting for keploy's agent to report what it served and missed",
			replayDrainWords.stopNow,
			func(outcomeCtx context.Context) error {
				consumed, consumedErr = m.instrumentation.GetConsumedMocks(outcomeCtx)
				misses, missesErr = m.instrumentation.GetMockErrors(outcomeCtx)
				if reader, ok := m.instrumentation.(idPairReader); ok {
					if pairs, err := reader.GetIDPairs(outcomeCtx); err == nil {
						m.logger.Debug("ids this run made in place of recorded ones", zap.Any("pairs", pairs))
						m.ids = ids.New(pairs)
					} else {
						m.logger.Debug("failed to read the ids this run made", zap.Error(err))
					}
				}
				return nil
			})
	}
	if consumedErr == nil && m.config.Mock.EmitMockEvents {
		// Flush the tail. The poll loop stops when the runner exits, so
		// anything served in the last poll interval would be counted in the
		// summary below and never announced - leaving a client showing fewer
		// served mocks than keploy's own total, which is the exact
		// inconsistency these events exist to remove. This costs no extra
		// round trip: the read has already happened.
		tail := make(map[string]models.MockState, len(consumed))
		for _, state := range consumed {
			tail[state.Name] = state
		}
		m.announceServed(tail)
	}
	if consumedErr != nil {
		m.logger.Debug("failed to read consumed mocks", zap.Error(consumedErr))
	}
	if missesErr != nil {
		m.logger.Debug("failed to read the misses the agent recorded", zap.Error(missesErr))
	}

	// The outcome is read after the runner exits, from an agent that may have
	// died with it, or -- under compose -- from an account the agent never got
	// to leave. Reporting a failed read as "0 consumed, 0 missed" would read as
	// a clean replay when in truth nothing is known. Each count says "unknown"
	// only for the read that actually failed; the other still carries its real
	// value.
	summary := []zap.Field{zap.Int("loaded", loaded)}
	if consumedErr != nil {
		summary = append(summary, zap.String("consumed", "unknown"))
	} else {
		summary = append(summary, zap.Int("consumed", len(consumed)))
	}
	if missesErr != nil {
		summary = append(summary, zap.String("missed", "unknown"))
	} else {
		summary = append(summary, zap.Int("missed", len(misses)))
	}
	if consumedErr == nil && missesErr == nil {
		m.logger.Info("mock replay summary", summary...)
	} else if stopped() {
		// The user's Ctrl+C cut the reads short, not the agent: nothing here
		// is the agent's to explain, and the run ends unjudged (Replay).
		m.logger.Debug("mock replay summary (incomplete: the user stopped the replay while keploy read it)", summary...)
	} else {
		next := "check the agent logs for why it stopped before the run ended"
		if fromCompose && consumedErr == nil {
			// The agent left its account, but its read of the calls no mock
			// answered was incomplete: that is what to look into.
			summary = append(summary, zap.NamedError("reason", missesErr))
			next = "the keploy-agent container could not read every call no mock answered as compose stopped it; check its logs above for why"
		} else if fromCompose {
			// Compose stopping the agent is the normal end of the run, not
			// the thing to look into: the agent writes its account as it
			// stops, and what needs explaining is why this one did not.
			summary = append(summary, zap.NamedError("reason", consumedErr))
			next = "the keploy-agent container writes what it served and missed as compose stops it; check its logs above for why this one did not, and that its image is the one this keploy version runs"
		}
		m.logger.Warn("mock replay summary (incomplete: the agent did not report the whole outcome)",
			append(summary, zap.String("next_step", next))...)
	}

	for _, miss := range misses {
		logMiss(m.logger, miss)
	}

	// What each test used and missed, and its verdict: for the receipt, and
	// for the reporter below.
	flows := attributeMocks(detail.windows, detail.expected, consumed, misses)
	// What the learn step learned goes onto the recording, before the record
	// that runs this round commits it. A round whose learning cannot be read
	// or kept did not complete: its record would judge recordings that lack
	// what this round served.
	var learned map[string][]string
	var learnedPool []string
	var learnErr error
	if m.learning() {
		learned, learnedPool, learnErr = m.keepLearned(ctx, detail.set, consumed, consumedErr, flows, detail.expected)
	}

	// Metering LAST: it may do network I/O, and the miss warnings above are what
	// the user actually needs to see first. Never for --local — that is the free
	// offline loop and is deliberately unmetered and untracked.
	//
	// And never on a partial read. Reporting it anyway would meter the run as
	// zero mocks consumed — the same false-clean the summary above exists to
	// stop, only silent and permanent. A run left uncounted is recoverable; a
	// run counted wrong is not.
	if replayOutcomeReporter != nil && !m.config.Mock.Local {
		if consumedErr != nil || missesErr != nil {
			m.logger.Info("not metering this replay: the agent did not report the whole outcome, and a run counted as zero is worse than one left uncounted")
		} else if detail.actualShort != "" {
			m.logger.Info("not metering this replay: " + detail.actualShort + ", and a request it lost would be reported as one the app did not make")
		} else {
			replayOutcomeReporter(ctx, ReplayOutcome{
				SetName:  m.setName(),
				Loaded:   loaded,
				Consumed: len(consumed),
				Missed:   len(misses),
				Cases:    pairCases(detail.windows, detail.recorded, detail.actual, m.compareCase),
				Mocks:    flows,
				Starts:   startsByTest(detail.windows, detail.starts),
				Sets:     ranSets(detail.windows),
			})
		}
	}
	counts := replayCounts{consumed: len(consumed), missed: len(misses)}
	for _, miss := range misses {
		if miss.CountsAgainstTest() {
			counts.missedData++
		}
	}
	if consumedErr != nil {
		counts.consumed = -1
	}
	if missesErr != nil {
		counts.missed, counts.missedData = -1, -1
		counts.missesUnread = models.UnreadCause(missesErr)
	} else if provenswitch.On() {
		counts.uncharged = unchargedMisses(misses)
	}
	counts.tests = withRealListed(withReal(withEvidence(testReceipts(flows, consumedErr == nil, missesErr == nil), detail.windows), detail.windows), m.realListed)
	// Under the proven switch a miss whose closest recording another test
	// owns names that test, as keploy test's replay does (markRecordedIn):
	// judging lists it as the root of the test's entry.
	if named := withRecordedIn(counts.tests, m.mockOwners); len(named) > 0 {
		m.logger.Warn("tests made a call that another test recorded: likely, at record time that test made it on a connection it opened, whose handshake was recorded there (KEPLOY_PROVEN=1); each is named under evidence.misses.recorded_in in "+TestsReceiptFile,
			zap.String("mock-set", m.setName()),
			zap.Strings("calls", named[:min(len(named), 20)]),
			zap.String("next_step", "record the set again with keploy mock record so each test opens the connections it uses, or keep the application's connection reuse as it was when recording"))
	}
	if provenswitch.On() {
		// When each test began serves the proven flow's reading of a cut
		// round (the last test begun): without the switch the receipt stays
		// as it was.
		counts.tests = withBegan(counts.tests, detail.windows)
	}
	for i := range counts.tests {
		counts.tests[i].Learned = learned[counts.tests[i].Name]
	}
	counts.learnedPool = learnedPool
	counts.learnErr = learnErr
	return counts
}

// logMiss warns of a call no recorded mock answered, with the next step its
// matcher gave it (a MySQL endpoint that moved names the port to list, a value
// drift the fields), and, where it gave none, to record the call. A
// connection whose client sent nothing made no call: it is said as such, and
// as what it was if its client waited for its server to speak first (a TCP
// health check closes the same way).
func logMiss(logger *zap.Logger, miss models.UnmatchedCall) {
	next := miss.NextSteps
	if next == "" {
		next = "record this call with --on-miss record, or re-record the set"
	}
	msg := "no recorded mock matched an outgoing call"
	if miss.MatchPhase == models.MatchPhaseNoRequest {
		msg = "a connection ended with nothing sent: if its client was waiting for its server to speak first, no recorded mock answered it"
	}
	fields := []zap.Field{
		zap.String("protocol", miss.Protocol),
		zap.String("call", miss.ActualSummary),
		zap.String("destination", miss.Destination),
	}
	if miss.Reason != "" {
		// Why it missed, in judging's words (record's full round): the
		// next step then is the learn step's own reason.
		fields = append(fields, zap.String("reason", miss.Reason))
	}
	logger.Warn(msg, append(fields, zap.String("next_step", next))...)
}

// provenCount is n under the proven switch, and nothing with it off, so a
// receipt written with the switch off is exactly what it was before it.
func provenCount(n int) *int {
	if !provenswitch.On() {
		return nil
	}
	return &n
}

// mergeCaptureLosses is a and b, one entry per kind: the one with the larger
// count (the counts are since the run began, so the later attempt's), but for
// a test's own kind (models.CaptureLossKind.PerTest: a TLS handshake that
// failed or was abandoned), which each attempt was charged its own of, so
// the two added.
func mergeCaptureLosses(a, b models.CaptureLosses) models.CaptureLosses {
	out := append(models.CaptureLosses(nil), a...)
	for _, l := range b {
		found := false
		for i := range out {
			if out[i].Kind == l.Kind {
				found = true
				switch {
				case l.Kind.PerTest():
					out[i].Count += l.Count
					if l.What != "" {
						out[i].What = l.What
					}
				case l.Count > out[i].Count:
					out[i] = l
				}
			}
		}
		if !found {
			out = append(out, l)
		}
	}
	return out
}

// withEvidence puts on each test's line the evidence the proven wire charged
// to it at its end (models.ScopeWindow.Evidence). A test that ran more than
// once keeps the evidence of every attempt: one failed attempt fails it.
func withEvidence(tests []TestReceipt, windows []models.ScopeWindow) []TestReceipt {
	var byName map[string]*models.ScopeEvidence
	for _, w := range windows {
		// Evidence with only a reason (a test whose process exited, or
		// whose evidence could not be read) is keploy's verdict too.
		if w.Evidence == nil || (w.Evidence.Empty() && w.Evidence.Reason == "") {
			continue
		}
		if byName == nil {
			byName = map[string]*models.ScopeEvidence{}
		}
		ev := byName[w.Name]
		if ev == nil {
			ev = &models.ScopeEvidence{Reason: w.Evidence.Reason}
			byName[w.Name] = ev
		}
		ev.Misses = append(ev.Misses, w.Evidence.Misses...)
		ev.Unconsumed = append(ev.Unconsumed, w.Evidence.Unconsumed...)
		ev.Incomplete = append(ev.Incomplete, w.Evidence.Incomplete...)
		// One attempt keploy could not read leaves the test unread, with
		// that attempt's reason.
		if w.Evidence.Unread && !ev.Unread {
			ev.Unread, ev.Reason = true, w.Evidence.Reason
		}
		// Counts since the run began: each attempt carries the run's, so
		// keep one per kind, the latest (largest); a test's own kinds,
		// each attempt's, added.
		ev.CaptureIncomplete = mergeCaptureLosses(ev.CaptureIncomplete, w.Evidence.CaptureIncomplete)
		// What keploy cannot capture at all leaves the test not judged,
		// never failed: a line that dropped it would read as keploy's
		// failure with no call to name.
		ev.CannotCapture = mergeCaptureLosses(ev.CannotCapture, w.Evidence.CannotCapture)
	}
	for i := range tests {
		if ev := byName[tests[i].Name]; ev != nil {
			tests[i].Evidence = ev
		}
	}
	return tests
}

// withRecordedIn names, on each miss of tests' evidence that is a recording
// its test does not own (NotOwned, ClosestMock), the other test that owns it
// (owners: models.MockOwners of the table the replay pushed), as keploy
// test's replay does (models.UnmatchedCall.NameRecordedIn). The evidence is
// withEvidence's own copy, never the windows'. It returns
// "test: call (closest mock, recorded in test)" for each it named. With the
// switch off owners is nil and no miss is NotOwned: nothing changes.
func withRecordedIn(tests []TestReceipt, owners map[string][]string) []string {
	var named []string
	for i := range tests {
		ev := tests[i].Evidence
		if ev == nil {
			continue
		}
		for j := range ev.Misses {
			c := &ev.Misses[j]
			if c.NameRecordedIn(tests[i].Name, owners) {
				named = append(named, fmt.Sprintf("%s: %s (%s, recorded in %s)", tests[i].Name, c.ActualSummary, c.ClosestMock, c.RecordedIn))
			}
		}
	}
	return named
}

// withBegan puts on each test's line when its latest run began: its latest
// window's start, a test still open when the run ended included (the agent
// closes it as failed, from its begin). A cut round names its last test begun
// by it. windows are the run's test windows (testWindows): an app a test
// started is not among them.
func withBegan(tests []TestReceipt, windows []models.ScopeWindow) []TestReceipt {
	began := make(map[string]time.Time, len(windows))
	for _, w := range windows {
		if w.Name == "" {
			continue
		}
		if prev, seen := began[w.Name]; !seen || w.Start.After(prev) {
			began[w.Name] = w.Start
		}
	}
	for i := range tests {
		if b, ok := began[tests[i].Name]; ok && !b.IsZero() {
			tests[i].Began = b.UTC()
		}
	}
	return tests
}

// testReceipts is each test's line in the receipt: its verdict and what it
// used and missed. A count the agent did not report in full is -1.
func testReceipts(flows []FlowMocks, consumedKnown, missesKnown bool) []TestReceipt {
	if len(flows) == 0 {
		return nil
	}
	out := make([]TestReceipt, 0, len(flows))
	for _, f := range flows {
		t := TestReceipt{Name: f.Flow, Outcome: f.Outcome, Consumed: len(f.Consumed), Missed: len(f.Missed)}
		if !consumedKnown {
			t.Consumed = -1
		}
		if !missesKnown {
			t.Missed = -1
		}
		out = append(out, t)
	}
	return out
}

// servedMockPoller is an optional extension of Instrumentation, asserted rather
// than added to the interface so existing implementations (and test fakes) keep
// compiling without it — the same discipline ConsumedStateReader uses.
type idPairReader interface {
	GetIDPairs(ctx context.Context) (map[string]string, error)
}

type servedMockPoller interface {
	GetServedMocks(ctx context.Context) (map[string]models.MockState, error)
}

// servedMockPollInterval is a UI cadence, not a measurement cadence. The state
// it reads is cumulative, so a slow poll delays a line but can never lose one.
const servedMockPollInterval = 500 * time.Millisecond

// emitServedMockEvents logs one line per mock the moment it is first seen to
// have been served, so a client driving keploy can show which dependency calls
// were answered from the set while the run is still in flight.
//
// It polls the agent's never-drained served map rather than consuming events:
//   - polling GetConsumedMocks instead would drain the state reportOutcome and
//     --strict are computed from, silently shrinking both;
//   - because the map is cumulative, a client that starts late or misses a tick
//     still converges on the truth instead of losing an event forever.
//
// Failures are logged at debug and the loop continues: the agent is not up for
// the whole window, and a progress feed must never be the thing that fails a run.
func (m *mockService) emitServedMockEvents(ctx context.Context) {
	poller, ok := m.instrumentation.(servedMockPoller)
	if !ok {
		// Warn, not Debug: the user asked for these events and is otherwise
		// told nothing about why none arrive.
		m.logger.Warn("--emit-mock-events was requested but this build cannot report served mocks",
			zap.String("next_step", "no events will be emitted; the rest of the replay is unaffected"))
		return
	}

	ticker := time.NewTicker(servedMockPollInterval)
	defer ticker.Stop()

	warnedOnce := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Bounded per poll: a hung (as opposed to dead) agent would
			// otherwise block on a client with no timeout, and because a
			// Ticker coalesces, the feed would stay dead for the whole run.
			pollCtx, cancel := context.WithTimeout(ctx, 2*servedMockPollInterval)
			served, err := poller.GetServedMocks(pollCtx)
			cancel()
			if err != nil {
				if !warnedOnce {
					warnedOnce = true
					// Once at Warn, then quiet: the likeliest cause is an agent
					// older than this binary, which has no /mock/served route.
					m.logger.Warn("cannot read served mocks; --emit-mock-events will emit nothing",
						zap.Error(err),
						zap.String("next_step", "check the keploy agent is the same version as this binary"))
					continue
				}
				m.logger.Debug("failed to read served mocks", zap.Error(err))
				continue
			}
			m.announceServed(served)
		}
	}
}

// announceServed logs one line per mock not already reported, and is safe to
// call from both the poll loop and the end-of-run flush.
func (m *mockService) announceServed(served map[string]models.MockState) {
	m.servedAnnouncedMu.Lock()
	defer m.servedAnnouncedMu.Unlock()
	if m.servedAnnounced == nil {
		m.servedAnnounced = make(map[string]struct{})
	}
	for name, state := range served {
		if _, seen := m.servedAnnounced[name]; seen {
			continue
		}
		m.servedAnnounced[name] = struct{}{}
		m.logger.Info("mock served",
			zap.String("name", name),
			zap.String("kind", string(state.Kind)),
			zap.String("type", state.Type),
			zap.String("lifetime", state.Lifetime.String()),
			// Named for what it is: the timestamp the call had when it was
			// RECORDED, not when it was served. A client rendering a live feed
			// would otherwise read it as "when this happened".
			zap.String("recordedReqTimestamp", state.ReqTimestampMock))
	}
}
