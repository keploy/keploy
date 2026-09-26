package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/coverage/report"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// Replay runs the wrapped test command with the configured named mock set
// served in place of the real dependencies. It needs no incoming test cases —
// the runner drives the requests, Keploy answers the outgoing calls from the
// set. On a miss it applies the configured policy (fail / passthrough /
// record). It propagates the runner's exit code, and with --strict also exits
// non-zero when any recorded mock was missed. It returns an error for a
// failure of keploy's own -- one that stopped the run, or left --strict unable
// to verify it -- which a failing test command never is.
func (m *mockService) Replay(ctx context.Context) (err error) {
	name := m.setName()
	started := time.Now()
	parent := ctx
	// A replay that could not start leaves a receipt saying so. Without it
	// the PREVIOUS receipt -- green, isolated -- stayed in place while keploy
	// exited 1, and every reader reported a proof this run had just failed to
	// repeat. A user interrupt is not a failure and leaves the last receipt,
	// which still truthfully describes the last completed run.
	finished := false
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
			IsolationNote:  note,
			MocksDigest:    MocksDigest(m.config.Path, name),
			MinCoverage:    m.config.Mock.MinCoverage,
			Version:        utils.Version,
		})
	}()
	// Coverage is read from a report the runner writes during THIS run. What
	// the report files looked like beforehand is what tells that report from
	// one an earlier run left behind.
	workDir, _ := os.Getwd()
	coverageBefore := report.Snapshot(workDir, m.config.Mock.CoverageReport, m.userCommand)

	// 0. Materialise the set locally (registry download in enterprise; no-op on files).
	if err := m.store.Pull(ctx, name); err != nil {
		utils.LogError(m.logger, err, "failed to fetch the mock set", zap.String("mock-set", name))
		return err
	}

	policy := models.MissPolicy(m.config.Mock.OnMiss)
	if !policy.Valid() {
		return fmt.Errorf("invalid --on-miss value %q: allowed values are fail, passthrough, record", m.config.Mock.OnMiss)
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
		if err := utils.DrainErrGroup(m.logger, "mock-replay", errGrp, 30*time.Second); err != nil {
			utils.LogError(m.logger, err, "failed to drain mock-replay goroutines")
		}
	}()

	// 1. Instrument in mock (test) mode — no ingress port relocation.
	if err := m.instrumentation.Setup(ctx, m.config.Command, models.SetupOptions{
		Container:     m.config.ContainerName,
		FromContainer: m.config.FromContainer,
		CommandType:   m.config.CommandType,
		DockerDelay:   m.config.BuildDelay,
		BuildDelay:    m.config.BuildDelay,
		Mode:          models.MODE_TEST,
		MockMode:      true,
		ConfigPath:    m.config.ConfigPath,
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

	// 3. Put the proxy in mock-serving mode with the miss policy.
	if err := m.instrumentation.MockOutgoing(ctx, models.OutgoingOptions{
		Rules:                     m.config.BypassRules,
		MongoPassword:             m.config.Test.MongoPassword,
		SQLDelay:                  time.Duration(m.config.Test.Delay) * time.Second,
		Mocking:                   true,
		OnMiss:                    policy,
		MysqlPorts:                m.config.MysqlPorts,
		DisableMysqlAutoDetect:    m.config.DisableMysqlAutoDetect,
		DisableMysqlEndpointDrift: m.config.DisableMysqlEndpointDrift,
		PassThroughPorts:          m.config.Record.PassThroughPorts,
		PassThroughHosts:          m.config.Record.PassThroughHosts,
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
	empty := map[string]bool{}
	filtered, err := m.mockDB.GetFilteredMocks(ctx, name, models.BaseTime, time.Now(), empty, empty)
	if err != nil {
		utils.LogError(m.logger, err, "failed to load per-test mocks", zap.String("mock-set", name))
		return err
	}
	unfiltered, err := m.mockDB.GetUnFilteredMocks(ctx, name, models.BaseTime, time.Now(), empty, empty)
	if err != nil {
		utils.LogError(m.logger, err, "failed to load session mocks", zap.String("mock-set", name))
		return err
	}
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
	//    calls can narrow the served pool per test (best-effort / optional).
	m.pushScopeTable(ctx, name)

	// 6. Stage the whole pool as the initial serving window (BaseTime..now, no
	//    mapping) — same call RunTestSet makes before the first test.
	if err := m.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime:  models.BaseTime,
		BeforeTime: time.Now(),
	}); err != nil {
		utils.LogError(m.logger, err, "failed to arm the mock pool")
		return err
	}

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

	if parent.Err() != nil { // user Ctrl+C
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

	// 9. Under --on-miss record, append any calls served live-from-upstream to
	//    the set so the next replay serves them from the mock (VCR new_episodes).
	if policy.RecordsOnMiss() {
		m.persistCaptured(context.WithoutCancel(ctx), name)
	}

	// 10. Summarise what was served and missed.
	counts := m.reportOutcome(ctx, loaded)
	missed, missesKnown := counts.missed, counts.missed >= 0

	// 11. Exit code: mirror the runner; with --strict also fail on any miss.
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
	// --strict means "fail unless every recorded call was matched". An
	// unreadable miss list is not proof of that, so it fails too: a
	// verification flag that passes when it could not verify is worse than no
	// flag at all. Loud beats quietly green.
	//
	// And it is keploy's failure, not the suite's, so Replay returns it: a
	// returned error is how a caller -- the enterprise run result, and the
	// editor reading it -- tells keploy's own 1 from a test command's. Left as
	// a bare exit 1, an unverifiable run read as a failing suite.
	var keployFailure error
	if m.config.Mock.Strict && !missesKnown && utils.ErrCode == 0 {
		utils.ErrCode = 1
		failedBy = FailedByStrict
		keployFailure = errors.New("--strict could not verify this replay: the agent never reported which recorded calls were missed")
		m.logger.Error("replay failed under --strict: the agent never reported which calls were missed, so a clean run could not be proven",
			zap.String("next_step", "drop --strict to accept an unverified run, or check the agent logs for why it stopped before the run ended"))
	}
	// Misses that WERE reported fail the run whatever else went unread.
	if m.config.Mock.Strict && missed > 0 && utils.ErrCode == 0 {
		utils.ErrCode = 1
		failedBy = FailedByStrict
		m.logger.Error("replay failed under --strict: recorded dependency calls were missed",
			zap.Int("missed", missed),
			zap.String("next_step", "a dependency contract drifted; re-record the set (keploy mock record) or add the new calls with --on-miss record"))
	}

	// 12. What the run proved -- decided once, so the log line below and the
	//     receipt cannot disagree -- then how much of the code it exercised,
	//     from the runner's own report, and the --min-coverage floor.
	bypass := bypassList(m.config.BypassRules)
	isolated, isolationNote := isolation(runnerExit, policy, counts, bypass)
	var keployErr string
	if failedBy == FailedByKeploy {
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
		Set:            name,
		At:             started.UTC().Truncate(time.Second),
		Command:        m.userCommand,
		OnMiss:         string(policy),
		Strict:         m.config.Mock.Strict,
		ExitCode:       utils.ErrCode,
		RunnerExitCode: runnerExit,
		FailedBy:       failedBy,
		Error:          keployErr,
		Loaded:         loaded,
		Consumed:       counts.consumed,
		Missed:         counts.missed,
		Bypass:         bypass,
		Isolated:       isolated,
		IsolationNote:  isolationNote,
		MocksDigest:    digest,
		Coverage:       cov,
		CoverageNote:   covNote,
		MinCoverage:    m.config.Mock.MinCoverage,
		Version:        utils.Version,
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
func (m *mockService) persistCaptured(ctx context.Context, name string) {
	drainer, ok := m.instrumentation.(MissCapturer)
	if !ok {
		return
	}
	drainCtx, cancel := context.WithTimeout(ctx, agentEpilogueTimeout)
	defer cancel()
	captured, err := drainer.DrainCapturedMocks(drainCtx)
	if err != nil {
		m.logger.Debug("failed to drain captured-on-miss mocks", zap.Error(err))
		return
	}
	if len(captured) == 0 {
		return
	}
	// Seed the name counter to the set's highest existing mock-N so appended
	// mocks get fresh names (InsertMock always renames to mock-<counter+1>);
	// without this the replay-side counter starts at 0 and appended mocks reuse
	// mock-0, mock-1, … colliding with the recorded set (consumed-mock tracking
	// and mappings both key on name).
	m.mockDB.SetCounterID(m.highestMockIndex(ctx, name))
	appended := 0
	for _, mk := range captured {
		if mk == nil {
			continue
		}
		if err := m.mockDB.InsertMock(ctx, mk, name); err != nil {
			m.logger.Debug("failed to append a captured-on-miss mock", zap.Error(err))
			continue
		}
		appended++
	}
	if appended > 0 {
		m.logger.Info("appended new dependency calls to the mock set (--on-miss record)",
			zap.Int("new", appended), zap.String("mock-set", name),
			zap.String("next_step", "review the added mocks and commit them; the next replay serves them without the real dependency"))
		if err := m.store.Push(ctx, name); err != nil {
			m.logger.Warn("failed to publish the refreshed mock set", zap.Error(err))
		}
	}
}

// highestMockIndex returns the largest N across the set's existing "mock-N"
// names, or -1 when the set is empty / has no mock-N names. Seeding the counter
// to this value makes the next InsertMock name its mock "mock-<N+1>".
func (m *mockService) highestMockIndex(ctx context.Context, name string) int64 {
	all := map[string]bool{}
	filtered, _ := m.mockDB.GetFilteredMocks(ctx, name, models.BaseTime, time.Now(), all, all)
	unfiltered, _ := m.mockDB.GetUnFilteredMocks(ctx, name, models.BaseTime, time.Now(), all, all)
	highest := int64(-1)
	consider := func(mocks []*models.Mock) {
		for _, mk := range mocks {
			if mk == nil {
				continue
			}
			if n, ok := strings.CutPrefix(mk.Name, "mock-"); ok {
				if idx, err := strconv.ParseInt(n, 10, 64); err == nil && idx > highest {
					highest = idx
				}
			}
		}
	}
	consider(filtered)
	consider(unfiltered)
	return highest
}

// pushScopeTable reads mappings.yaml for the set (if per-test mappings exist)
// and hands the agent the name→mock-names table so per-test scoping works.
func (m *mockService) pushScopeTable(ctx context.Context, name string) {
	if m.mappingDB == nil {
		return
	}
	pusher, ok := m.instrumentation.(ScopePusher)
	if !ok {
		return
	}
	mappings, meaningful, err := m.mappingDB.Get(ctx, name)
	if err != nil || !meaningful || len(mappings) == 0 {
		return
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
		return
	}
	m.logger.Info("per-test scoping enabled", zap.Int("tests", len(table)), zap.String("mock-set", name))
}

// ReplayOutcome is what a replay produced, for a wrapping build that meters or
// reports usage. Counts only; no payloads, no destinations.
type ReplayOutcome struct {
	SetName  string
	Loaded   int
	Consumed int
	Missed   int
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
	consumed int
	missed   int
}

// reportOutcome logs which mocks were consumed and which outgoing calls matched
// nothing. It returns the consumed and missed counts, each -1 when its read
// failed; whether the MISSES specifically were readable is the only half
// --strict turns on.
//
// The two reads are tracked apart on purpose. Collapsing them into one "did the
// agent answer" flag makes a half-answer indistinguishable from no answer, and
// then reports a run whose misses were never read as a clean one.
func (m *mockService) reportOutcome(ctx context.Context, loaded int) replayCounts {
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
	} else {
		outcomeCtx, cancel := context.WithTimeout(ctx, agentEpilogueTimeout)
		defer cancel()
		consumed, consumedErr = m.instrumentation.GetConsumedMocks(outcomeCtx)
		misses, missesErr = m.instrumentation.GetMockErrors(outcomeCtx)
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
		m.logger.Debug("failed to read mock misses", zap.Error(missesErr))
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
	} else {
		next := "check the agent logs for why it stopped before the run ended"
		if fromCompose {
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
		m.logger.Warn("no recorded mock matched an outgoing call",
			zap.String("protocol", miss.Protocol),
			zap.String("call", miss.ActualSummary),
			zap.String("destination", miss.Destination),
			zap.String("next_step", "record this call with --on-miss record, or re-record the set"))
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
		} else {
			replayOutcomeReporter(ctx, ReplayOutcome{
				SetName:  m.setName(),
				Loaded:   loaded,
				Consumed: len(consumed),
				Missed:   len(misses),
			})
		}
	}
	counts := replayCounts{consumed: len(consumed), missed: len(misses)}
	if consumedErr != nil {
		counts.consumed = -1
	}
	if missesErr != nil {
		counts.missed = -1
	}
	return counts
}

// servedMockPoller is an optional extension of Instrumentation, asserted rather
// than added to the interface so existing implementations (and test fakes) keep
// compiling without it — the same discipline ConsumedStateReader uses.
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
