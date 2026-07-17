package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"facette.io/natsort"
	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	grpcMatcher "go.keploy.io/server/v3/pkg/matcher/grpc"
	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/coverage"
	"go.keploy.io/server/v3/pkg/platform/coverage/golang"
	"go.keploy.io/server/v3/pkg/platform/coverage/java"
	"go.keploy.io/server/v3/pkg/platform/coverage/javascript"
	"go.keploy.io/server/v3/pkg/platform/coverage/python"
	"go.keploy.io/server/v3/pkg/platform/telemetry"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const applicationFailedToRunLogMessage = "application failed to run; check the application logs for details or verify the app command is correct"

// startupMockCutoff returns the startup-mock exemption boundary for a test set:
// any mock recorded before this time is a startup mock that UpdateMocks must
// keep even when unconsumed. It is the request timestamp of the
// (models.StartupMockTestCaseWindow+1)-th test case (ordered by request time),
// so the startup window covers app boot through the recording of the Nth test
// case — the replay-side twin of the record-side IsStartup tagging keyed off
// SyncMockManager.resolvedTestCount.
//
// When the set has <= models.StartupMockTestCaseWindow test cases, the entire
// recording is startup; keepAll (the replay start time, pruneBefore) is
// returned so every recorded mock is retained — mocks recorded before replay
// start are kept by this exemption and any written after it are kept by
// UpdateMocks' post-replay-write rule. Returns the zero time when no test case
// carries a usable timestamp, which disables the exemption (matching the prior
// behaviour for timestamp-less sets).
func startupMockCutoff(testCases []*models.TestCase, keepAll time.Time) time.Time {
	tcTimes := make([]time.Time, 0, len(testCases))
	for _, tc := range testCases {
		var candidate time.Time

		// Prefer high-precision request timestamps when available.
		if !tc.HTTPReq.Timestamp.IsZero() {
			candidate = tc.HTTPReq.Timestamp
		} else if !tc.GrpcReq.Timestamp.IsZero() {
			candidate = tc.GrpcReq.Timestamp
		} else if tc.Created > 0 {
			// Fallback to the coarser Created timestamp.
			candidate = time.Unix(tc.Created, 0)
		}

		if !candidate.IsZero() {
			tcTimes = append(tcTimes, candidate)
		}
	}
	sort.Slice(tcTimes, func(i, j int) bool { return tcTimes[i].Before(tcTimes[j]) })

	if len(tcTimes) > models.StartupMockTestCaseWindow {
		return tcTimes[models.StartupMockTestCaseWindow]
	}
	if len(tcTimes) > 0 {
		return keepAll
	}
	return time.Time{}
}

func shouldAbortTestRun(status models.TestSetStatus, cmdType utils.CmdType) bool {
	switch status {
	case models.TestSetStatusAppHalted, models.TestSetStatusFaultUserApp:
		return cmdType != utils.DockerCompose
	case models.TestSetStatusInternalErr:
		return true
	default:
		return false
	}
}

func mapAppErrorToTestSetStatus(appErr models.AppError) (models.TestSetStatus, bool) {
	switch appErr.AppErrorType {
	case "":
		return models.TestSetStatusAppHalted, true
	case models.ErrCtxCanceled:
		return "", false
	case models.ErrCommandError:
		return models.TestSetStatusFaultUserApp, true
	case models.ErrUnExpected, models.ErrAppStopped, models.ErrTestBinStopped:
		return models.TestSetStatusAppHalted, true
	case models.ErrInternal:
		return models.TestSetStatusInternalErr, true
	default:
		return models.TestSetStatusAppHalted, true
	}
}

func resolveTestSetStatus(cmdType utils.CmdType, current, derived models.TestSetStatus, err error) (models.TestSetStatus, bool) {
	switch current {
	case models.TestSetStatusAppHalted, models.TestSetStatusFaultUserApp, models.TestSetStatusInternalErr, models.TestSetStatusUserAbort:
		return current, true
	}
	switch derived {
	case models.TestSetStatusAppHalted, models.TestSetStatusFaultUserApp, models.TestSetStatusInternalErr, models.TestSetStatusUserAbort:
		return derived, true
	}
	if cmdType == utils.DockerCompose && isDockerComposeReplayShutdown(err) {
		return models.TestSetStatusAppHalted, true
	}
	return "", false
}

func isDockerComposeReplayShutdown(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "connection reset by peer") ||
		strings.Contains(errMsg, "broken pipe") ||
		strings.Contains(errMsg, "server closed") ||
		containsStandalonePhrase(errMsg, "unexpected eof") ||
		containsStandalonePhrase(errMsg, "eof") ||
		strings.Contains(errMsg, "found no test results")
}

func containsStandalonePhrase(msg, phrase string) bool {
	start := 0
	for {
		idx := strings.Index(msg[start:], phrase)
		if idx == -1 {
			return false
		}
		idx += start

		beforeIdx := idx - 1
		afterIdx := idx + len(phrase)
		beforeOK := beforeIdx < 0 || !isWordChar(msg[beforeIdx])
		afterOK := afterIdx >= len(msg) || !isWordChar(msg[afterIdx])
		if beforeOK && afterOK {
			return true
		}

		start = idx + 1
	}
}

func isWordChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_'
}

func describeTestSetFailure(status models.TestSetStatus, testCaseResults []models.TestResult) string {
	switch status {
	case models.TestSetStatusAppHalted, models.TestSetStatusFaultUserApp:
		if len(testCaseResults) == 0 {
			return "application startup failed - check application logs in the app_logs field for details and next steps"
		}
		return "application stopped during replay - check application logs in the app_logs field for details and next steps"
	case models.TestSetStatusInternalErr:
		return "replay failed with an internal error - please report this issue if it persists"
	default:
		return ""
	}
}

func shouldIncludeAppLogs(status models.TestSetStatus) bool {
	switch status {
	case models.TestSetStatusAppHalted, models.TestSetStatusFaultUserApp, models.TestSetStatusInternalErr:
		return true
	default:
		return false
	}
}

type Replayer struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	mappingDB       MappingDB
	reportDB        ReportDB
	testSetConf     TestSetConfig
	telemetry       Telemetry
	instrumentation Instrumentation
	config          *config.Config
	instrument      bool
	isLastTestSet   bool
	isLastTestCase  bool
	// isFirstTestSet mirrors isLastTestSet — true while the test-set
	// about to run is the first one of the current replay run. Read
	// by the waitForAppReady gate so --delay applies only to the first
	// test-set when --keep-app-alive is set (subsequent test-sets
	// inherit a warm app and would otherwise pay --delay seconds of
	// dead time per boundary).
	isFirstTestSet     bool
	runDomainSet       *telemetry.DomainSet // collects host domains across a test run for telemetry
	testRunTestSets    []string             // all test set IDs for the current run (used by RunTestSet)
	testRunID          string               // current test run ID (used by RunTestSet)
	afterTestRunCalled bool                 // guards duplicate AfterTestRun calls
	hookImpl           TestHooks

	completeTestReport    map[string]TestReportVerdict
	firstRun              bool
	completeTestReportMu  sync.RWMutex
	totalTests            int
	totalTestPassed       int
	totalTestFailed       int
	totalTestObsolete     int
	totalTestIgnored      int
	consumedMockNames     map[string]struct{} // distinct mock names consumed across all test sets in the current run; size emitted as Mocks-Consumed on the TestRun event
	totalTestTimeTaken    time.Duration
	failedTCsBySetID      map[string][]string
	mockMismatchFailures  *TestFailureStore
	fallbackDeprecateOnce sync.Once
}

// GetMockMismatchFailures returns a copy of the accumulated mock mismatch failures.
func (r *Replayer) GetMockMismatchFailures() []TestFailure {
	return r.mockMismatchFailures.GetFailures()
}

// GetTestRunID returns the current test run ID.
func (r *Replayer) GetTestRunID() string {
	return r.testRunID
}

func NewReplayer(logger *zap.Logger, testDB TestDB, mockDB MockDB, reportDB ReportDB, mappingDB MappingDB, testSetConf TestSetConfig, telemetry Telemetry, instrumentation Instrumentation, storage Storage, config *config.Config) Service {
	defaultHook := NewHooks(logger, config, instrumentation)

	instrument := config.Command != ""
	return &Replayer{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		mappingDB:       mappingDB,
		reportDB:        reportDB,
		testSetConf:     testSetConf,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          config,
		instrument:      instrument,
		hookImpl:        defaultHook,

		completeTestReport:   make(map[string]TestReportVerdict),
		failedTCsBySetID:     make(map[string][]string),
		mockMismatchFailures: NewTestFailureStore(),
	}
}

func (r *Replayer) SetTestHooks(testHooks TestHooks) {
	if testHooks == nil {
		return
	}
	r.hookImpl = testHooks
}

func (r *Replayer) GetTestHooks() TestHooks {
	return r.hookImpl
}

func (r *Replayer) Start(ctx context.Context) error {

	r.logger.Debug("Starting Keploy replay... Please wait.")

	// parentCtx is the context as passed into Start — canceled only by a
	// real user interrupt (SIGINT via utils.NewCtx). The errgroup-derived
	// ctx below additionally cancels on ANY goroutine error, so it must NOT
	// be used to detect user-abort: doing so would suppress TestRunAborted
	// for exactly the internal graceful-abort paths this telemetry targets.
	parentCtx := ctx

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, g))

	setupErrGrp, _ := errgroup.WithContext(ctx)
	setupCtx := context.WithoutCancel(ctx)
	setupCtx, setupCtxCancel := context.WithCancel(setupCtx)
	setupCtx = context.WithValue(setupCtx, models.ErrGroupKey, setupErrGrp)

	var hookCancel context.CancelFunc
	var stopReason = "replay completed successfully"
	// summaryEmitted flips true once the normal TestRun summary fires. If the
	// run stops gracefully before that (setup/instrument failure), the defer
	// emits a TestRunAborted with the categorized reason instead.
	var summaryEmitted bool

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-parentCtx.Done():
			break
		default:
			r.logger.Info("stopping Keploy", zap.String("reason", stopReason))
			// Keploy-initiated (not user Ctrl+C, which lands in the
			// parentCtx.Done case above) graceful stop before any run summary
			// was emitted — record why so the replay funnel can see where test
			// runs die on setup. Graceful only; hard crashes are covered by
			// Sentry. parentCtx (not the errgroup ctx) is checked so internal
			// goroutine errors don't masquerade as user interrupts.
			if !summaryEmitted {
				r.telemetry.TestRunAborted(stopReason)
				if s, ok := r.telemetry.(interface{ Shutdown() }); ok {
					s.Shutdown()
				}
			}
		}

		// Notify the agent that we are shutting down gracefully. It covers early exits before RunTestSet runs
		// and shutdown paths where the per‑test‑set defer doesn’t execute (or never starts).
		// Bound it: context.Background() with no deadline meant an up-but-unresponsive
		// agent (common while it is still booting under CPU contention) could block this
		// POST — and therefore the whole teardown, which is the path SIGINT takes — forever.
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := r.instrumentation.NotifyGracefulShutdown(notifyCtx); err != nil {
			r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
		}
		notifyCancel()

		if hookCancel != nil {
			hookCancel()
		}
		cancel()
		// Bounded drain: a setup/run goroutine that wedges under contention and does
		// not observe cancel() must not be able to block this teardown forever, or
		// SIGINT (which runs through this same defer) is swallowed and the process
		// hangs until an external SIGKILL. See utils.DrainErrGroup.
		if err := utils.DrainErrGroup(r.logger, "replay", g, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop replaying")
		}

		setupCtxCancel()
		if err := utils.DrainErrGroup(r.logger, "replay-setup", setupErrGrp, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "failed to stop replaying")
		}
	}()

	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get all test set ids: %v", err)
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	r.logger.Info("Test Sets to be Replayed", zap.Strings("testSets", testSetIDs))

	if len(testSetIDs) == 0 {
		recordCmd := models.HighlightGrayString("keploy record")
		errMsg := fmt.Sprintf("No test sets found in the keploy folder. Please record testcases using %s command", recordCmd)
		utils.LogError(r.logger, err, errMsg)
		// Ran `keploy test` before any tests were recorded — a distinct
		// funnel signal, so surface it via the categorized stop reason
		// rather than letting the default "completed" mask it.
		stopReason = "no test sets found"
		return fmt.Errorf("%s", errMsg)
	}

	r.completeTestReportMu.Lock()
	r.completeTestReport = make(map[string]TestReportVerdict)
	r.totalTests, r.totalTestPassed, r.totalTestFailed, r.totalTestObsolete, r.totalTestIgnored = 0, 0, 0, 0, 0
	r.consumedMockNames = make(map[string]struct{})
	r.totalTestTimeTaken = 0
	r.completeTestReportMu.Unlock()
	r.failedTCsBySetID = make(map[string][]string)
	r.mockMismatchFailures = NewTestFailureStore()

	testRunID, err := r.GetNextTestRunID(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get next test run id: %v", err)
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	var language models.Language
	var executable string
	// only find language to calculate coverage if instrument is true
	if r.instrument {
		language, executable = utils.DetectLanguage(r.logger, r.config.Command)
		// if language is not provided and language detected is known
		// then set the language to detected language
		if r.config.Test.Language == "" {
			if language == models.Unknown {
				r.logger.Debug("failed to detect language, skipping coverage calculation. please use --language to manually set the language")
				r.config.Test.SkipCoverage = true
			} else {
				r.logger.Debug(fmt.Sprintf("%s language detected. please use --language to manually set the language if needed", language))
			}
			r.config.Test.Language = language
		} else if language != r.config.Test.Language && language != models.Unknown {
			utils.LogError(r.logger, nil, "language detected is different from the language provided")
			r.config.Test.SkipCoverage = true
		}
	}

	r.logger.Debug("language detected", zap.String("language", r.config.Test.Language.String()), zap.String("executable", executable))

	var cov coverage.Service
	switch r.config.Test.Language {
	case models.Go:
		cov = golang.New(ctx, r.logger, r.reportDB, r.config.Command, r.config.Test.CoverageReportPath, r.config.CommandType)
	case models.Python:
		// if the executable is not starting with "python" or "python3" then skipCoverage
		if !strings.HasPrefix(executable, "python") && !strings.HasPrefix(executable, "python3") {
			r.logger.Debug("python command not python or python3, skipping coverage calculation")
			r.config.Test.SkipCoverage = true
		}
		cov = python.New(ctx, r.logger, r.reportDB, r.config.Command, executable)
	case models.Javascript:
		cov = javascript.New(ctx, r.logger, r.reportDB, r.config.Command)
	case models.Java:
		cov = java.New(ctx, r.logger, r.reportDB, r.config.Command, r.config.Test.JacocoAgentPath, executable)
	default:
		r.config.Test.SkipCoverage = true
	}
	if !r.config.Test.SkipCoverage {
		if utils.CmdType(r.config.CommandType) == utils.Native {
			r.config.Command, err = cov.PreProcess(r.config.Test.DisableLineCoverage)

			if err != nil {
				r.config.Test.SkipCoverage = true
			}
		}
		err = os.Setenv("CLEAN", "true") // related to javascript coverage calculation
		if err != nil {
			r.config.Test.SkipCoverage = true
			r.logger.Debug("failed to set CLEAN env variable, skipping coverage calculation", zap.Error(err))
		}
	}

	// Instrument will load the hooks and start the proxy
	inst, err := r.Instrument(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to instrument: %v", err)
		utils.LogError(r.logger, err, stopReason)
		if ctx.Err() == context.Canceled {
			return err
		}
		return fmt.Errorf("%s", stopReason)
	}

	hookCancel = inst.HookCancel

	// Hoist cmdType up to before the test-set loop so the one-shot
	// user-app spawn below can read it. The original assignment that
	// lived just above the for-range over testSets is removed (kept
	// in a single canonical location here).
	cmdType := utils.CmdType(r.config.CommandType)

	// --keep-app-alive: start the user app ONCE on the outer errgroup
	// so its lifecycle is tied to the whole replay run rather than a
	// single test-set. The per-test-set RunApplication spawn (and its
	// sibling NotifyGracefulShutdown notify) below is gated off via
	// the existing `serveTest` parameter on RunTestSet — we pass
	// `effectiveKeepAlive` as that parameter at the call site (line
	// ~480). Keeps long-lived TCP connections (asyncpg pool, HikariCP,
	// etc.) warm across the test-set boundary — the precondition the
	// cross-test-set session/startup-tier staleness bugs in
	// keploy/integrations#203 need to surface.
	//
	// Supported command types: docker-compose, docker-run,
	// docker-start, native. All of them ultimately call
	// r.instrumentation.Run(ctx, opts) — the only difference is what
	// child command runs — so the same one-shot spawn pattern works
	// for every cmdType. cmdType == Empty (no -c) means there is no
	// user application to manage; the same gate ALSO suppresses
	// passing serveTest=true to RunTestSet (which would otherwise skip
	// the per-test-set RunApplication path and leave the app
	// unstarted), and the per-test-set waitForAppReady skip below
	// stays armed only when the one-shot spawn actually fired.
	// Single computed predicate so all three gates move together.
	//
	// Docker-compose replay reuses the stack across test-sets BY DEFAULT (when
	// mocking is on): otherwise keploy tears down + recreates the whole
	// agent+app+network+volumes for every test-set, multiplying docker-daemon
	// create/destroy churn N-fold and re-paying a per-test-set agent-readiness
	// window — under loaded CI that churn is what blows past the readiness
	// timeout. The agent is reused safely: each test-set's mocks are re-scoped
	// in-band per set (MockOutgoing→ResetForReplaySession, StoreMocks,
	// SendMockFilterParamsToAgent), the same contract the runner already relies
	// on. Gated on Mocking so a replay against a real (unmocked) dependency —
	// where surviving app state across the boundary could change expected
	// before-state — still restarts per test-set. An explicit --keep-app-alive
	// keeps working unchanged (OR can only widen).
	composeReuse := cmdType == utils.DockerCompose && r.config.Test.Mocking
	effectiveKeepAlive := (r.config.Test.KeepAppAlive || composeReuse) && r.instrument && cmdType != utils.Empty
	if effectiveKeepAlive {
		g.Go(func() error {
			defer utils.Recover(r.logger)
			appErr := r.RunApplication(ctx, models.RunOptions{
				AppCommand: r.config.Command,
			})
			// Two outcomes that are NOT failures from the replay's
			// point of view:
			//   1. Zero-value AppError: RunApplication returned cleanly.
			//   2. ErrCtxCanceled: ctx was cancelled (normal end-of-run
			//      teardown — the outer Start() defers a graceful
			//      shutdown notify; the app exits as a consequence).
			// Either case → return nil; errgroup keeps the run alive.
			if appErr == (models.AppError{}) || appErr.AppErrorType == models.ErrCtxCanceled {
				return nil
			}
			// Anything else is an app startup / crash failure. Propagate
			// it back through the errgroup so g.Wait() at the bottom of
			// Start() surfaces the cause as the run's exit reason and
			// any sibling goroutine listening on the errgroup-derived
			// context unblocks. Logging the failure here too because
			// errgroup only ever returns the FIRST error — if the test
			// loop trips on a parallel error first, the actionable
			// next_step that explains WHY tests started failing en
			// masse would otherwise be hidden.
			utils.LogError(r.logger, fmt.Errorf("user application failed under --keep-app-alive: %v", appErr),
				applicationFailedToRunLogMessage,
				zap.String("kind", string(appErr.AppErrorType)),
				zap.Any("err", appErr),
				zap.String("next_step",
					"the user application started by --keep-app-alive exited or failed to start during the replay run. "+
						"Check the application's own logs (the -c command's stdout/stderr is captured by keploy) for the root cause. "+
						"Common causes: image build failure, port already in use, missing env var, dependency container crash-looped. "+
						"If the app is expected to manage its own lifecycle across test-sets, drop --keep-app-alive and let keploy "+
						"restart it per test-set instead."))
			return appErr
		})
	}

	var testSetResult bool
	testRunResult := true
	abortTestRun := false
	r.afterTestRunCalled = false
	var flakyTestSets []string
	var testSets []string
	runDomainSet := telemetry.NewDomainSet()
	r.runDomainSet = runDomainSet
	for _, testSetID := range testSetIDs {
		if _, ok := r.config.Test.SelectedTests[testSetID]; !ok && len(r.config.Test.SelectedTests) != 0 {
			continue
		}
		testSets = append(testSets, testSetID)
	}
	if len(testSets) == 0 {
		testSets = testSetIDs
	}

	// Sort the testsets.
	natsort.Sort(testSets)
	// cmdType is hoisted above the one-shot RunApplication block; the
	// original assignment that lived here is removed to keep a single
	// canonical declaration.
	r.testRunTestSets = testSets
	r.testRunID = testRunID
	r.firstRun = true
	for i, testSet := range testSets {
		var backupCreated bool
		testSetResult = false

		err := r.hookImpl.BeforeTestSetRun(ctx, testSet)
		if err != nil {
			stopReason = fmt.Sprintf("failed to run before test hook: %v", err)
			utils.LogError(r.logger, err, stopReason)
			if ctx.Err() == context.Canceled {
				return err
			}
			return fmt.Errorf("%s", stopReason)
		}

		if !r.config.Test.SkipCoverage {
			err = os.Setenv("TESTSETID", testSet) // related to java coverage calculation
			if err != nil {
				r.config.Test.SkipCoverage = true
				r.logger.Debug("failed to set TESTSETID env variable, skipping coverage calculation", zap.Error(err))
			}
		}

		// check if its the last testset running -

		if i == len(testSets)-1 {
			r.isLastTestSet = true
		}
		// Mirror of isLastTestSet but for the first-testset boundary.
		// Set BEFORE the RunTestSet call so the waitForAppReady call
		// sites in RunTestSet observe the right value. Reset only at
		// the top of this iteration (so a flaky-retry attempt within
		// the SAME iteration still observes the same value).
		r.isFirstTestSet = (i == 0)

		r.completeTestReportMu.RLock()
		initTotal := r.totalTests
		initPassed := r.totalTestPassed
		initFailed := r.totalTestFailed
		initObsolete := r.totalTestObsolete
		initIgnored := r.totalTestIgnored
		initTimeTaken := r.totalTestTimeTaken
		// Snapshot a copy of the run-level distinct mock set so per-attempt
		// additions can be reverted on retry just like the int totals above.
		initConsumedMockNames := make(map[string]struct{}, len(r.consumedMockNames))
		for k := range r.consumedMockNames {
			initConsumedMockNames[k] = struct{}{}
		}
		r.completeTestReportMu.RUnlock()

		var initialFailedTCs map[string]bool
		flaky := false // only be changed during replay with --must-pass flag set
		for attempt := 1; attempt <= int(r.config.Test.MaxFlakyChecks); attempt++ {

			// clearing testcase from map is required for 2 reasons:
			// 1st: in next attempt, we need to append results in a fresh array,
			// rather than appending in the old array which would contain outdated tc results.
			// 2nd: in must-pass mode, we delete the failed testcases from the map
			// if the array has some failed testcases, which has already been removed, then not cleaning
			// the array would mean deleting the already deleted failed testcases again (error).
			r.reportDB.ClearTestCaseResults(ctx, testRunID, testSet)

			// overwrite with values before testset run, so after all reruns we don't get a cummulative value
			// gathered from rerunning, instead only metrics from the last rerun would get added to the variables.
			r.completeTestReportMu.Lock()
			r.totalTests = initTotal
			r.totalTestPassed = initPassed
			r.totalTestFailed = initFailed
			r.totalTestObsolete = initObsolete
			r.totalTestIgnored = initIgnored
			r.totalTestTimeTaken = initTimeTaken
			// Re-clone from the snapshot so the next RunTestSet attempt
			// mutates a fresh per-attempt copy without polluting the
			// preserved snapshot for any further retry.
			r.consumedMockNames = make(map[string]struct{}, len(initConsumedMockNames))
			for k := range initConsumedMockNames {
				r.consumedMockNames[k] = struct{}{}
			}
			r.completeTestReportMu.Unlock()

			r.logger.Info("running", zap.String("test-set", models.HighlightString(testSet)), zap.Int("attempt", attempt))
			// serveTest=true reuses the pre-existing gating inside
			// RunTestSet (skips per-test-set RunApplication spawn and
			// NotifyGracefulShutdown). Pass `effectiveKeepAlive` —
			// not the raw config bool — so a `--keep-app-alive` set
			// with `--cmd-type=""` (or with instrument disabled) does
			// NOT skip per-test-set RunApplication while the one-shot
			// spawn was also suppressed; that combination would leave
			// the app entirely unstarted. With this gate the
			// historical per-test-set restart behaviour is preserved
			// whenever the one-shot spawn didn't fire.
			testSetStatus, err := r.RunTestSet(ctx, testSet, testRunID, effectiveKeepAlive)
			if err != nil {
				if ctx.Err() == context.Canceled {
					return err
				}
				// DockerCompose: when the user-app container exits,
				// compose's --abort-on-container-exit cancels our inner
				// runTestSetCtx. RunTestSet then returns one of three
				// statuses depending on which goroutine wins the race —
				// AppHalted, FaultUserApp, or UserAbort (the last one
				// happens at line ~964 when the ctx-Done select case
				// fires before the appErr-status mapping). The outer
				// ctx is intact (we already early-returned above if it
				// was), so all three are recoverable: the next test set
				// builds a fresh compose. Other errors still abort.
				if cmdType == utils.DockerCompose &&
					(testSetStatus == models.TestSetStatusAppHalted ||
						testSetStatus == models.TestSetStatusFaultUserApp ||
						testSetStatus == models.TestSetStatusUserAbort) {
					utils.LogError(r.logger, err,
						"test set failed because the app exited; continuing to next test set",
						zap.String("test-set", testSet),
						zap.String("status", string(testSetStatus)))
					testSetResult = false
					testRunResult = false
					break
				}
				stopReason = fmt.Sprintf("failed to run test set: %v", err)
				utils.LogError(r.logger, err, stopReason)
				return fmt.Errorf("%s", stopReason)
			}
			switch testSetStatus {
			case models.TestSetStatusAppHalted:
				testSetResult = false
				abortTestRun = shouldAbortTestRun(testSetStatus, cmdType)
			case models.TestSetStatusInternalErr:
				testSetResult = false
				abortTestRun = shouldAbortTestRun(testSetStatus, cmdType)
			case models.TestSetStatusFaultUserApp:
				testSetResult = false
				abortTestRun = shouldAbortTestRun(testSetStatus, cmdType)
			case models.TestSetStatusUserAbort:
				return nil
			case models.TestSetStatusFailed:
				testSetResult = false
			case models.TestSetStatusPassed:
				testSetResult = true
			case models.TestSetStatusIgnored:
				testSetResult = false
			case models.TestSetStatusNoTestsToRun:
				testSetResult = false
			}

			if testSetStatus != models.TestSetStatusIgnored {
				testRunResult = testRunResult && testSetResult
				if abortTestRun {
					break
				}
			}

			tcResults, err := r.reportDB.GetTestCaseResults(ctx, testRunID, testSet)
			if err != nil {
				if testSetStatus != models.TestSetStatusNoTestsToRun {
					utils.LogError(r.logger, err, "failed to get testcase results")
				}
				break
			}
			failedTcIDs := getFailedTCs(tcResults)
			r.failedTCsBySetID[testSet] = failedTcIDs

			// checking for flakiness when --must-pass flag is not set
			// else if --must-pass is set, delete the failed testcases and rerun
			if !r.config.Test.MustPass {
				// populate the map only once at first iteration for flakiness test
				if attempt == 1 {
					initialFailedTCs = make(map[string]bool)
					for _, id := range failedTcIDs {
						initialFailedTCs[id] = true
					}
					continue
				}
				// checking if there is no mismatch in failed testcases across max retries
				// check both length and value
				if len(failedTcIDs) != len(initialFailedTCs) {
					utils.LogError(r.logger, nil, "the testset is flaky, rerun the testset with --must-pass flag to remove flaky testcases", zap.String("testSet", testSet))
					// don't run more attempts if the testset is flaky
					flakyTestSets = append(flakyTestSets, testSet)
					break
				}
				for _, id := range failedTcIDs {
					if _, ok := initialFailedTCs[id]; !ok {
						flaky = true
						utils.LogError(r.logger, nil, "the testset is flaky, rerun the testset with --must-pass flag to remove flaky testcases", zap.String("testSet", testSet))
						break
					}
				}
				if flaky {
					// don't run more attempts if the testset is flaky
					flakyTestSets = append(flakyTestSets, testSet)
					break
				}
				continue
			}

			// this would be executed only when --must-pass flag is set
			// we would be removing failed testcases
			if r.config.Test.MaxFailAttempts == 0 {
				utils.LogError(r.logger, nil, "no. of testset failure occurred during rerun reached maximum limit, testset still failing, increase count of maxFailureAttempts", zap.String("testSet", testSet))
				break
			}
			if len(failedTcIDs) == 0 {
				// if no testcase failed in this attempt move to next attempt
				continue
			}

			if !backupCreated {
				if err := r.createBackup(testSet); err != nil {
					utils.LogError(r.logger, err, "failed to create backup, proceeding with test case deletion", zap.String("testSet", testSet))
				}
				backupCreated = true
			}

			r.logger.Info("deleting failing testcases", zap.String("testSet", testSet), zap.Strings("testCaseIDs", failedTcIDs))

			if err := r.testDB.DeleteTests(ctx, testSet, failedTcIDs); err != nil {
				utils.LogError(r.logger, err, "failed to delete failing testcases", zap.String("testSet", testSet), zap.Strings("testCaseIDs", failedTcIDs))
				break
			}
			// after deleting rerun it maxFlakyChecks times to be sure that no further testcase fails
			// and if it does then delete those failing testcases and rerun it again maxFlakyChecks times
			r.config.Test.MaxFailAttempts--
			attempt = 0
		}

		if abortTestRun {
			break
		}

		err = r.hookImpl.AfterTestSetRun(ctx, testSet, testSetResult)
		if err != nil {
			utils.LogError(r.logger, err, "failed to execute after test set run hook", zap.String("testSet", testSet))
		}

		if i == 0 && !r.config.Test.SkipCoverage {
			err = os.Setenv("CLEAN", "false") // related to javascript coverage calculation
			if err != nil {
				r.config.Test.SkipCoverage = true
				r.logger.Debug("failed to set CLEAN env variable, skipping coverage calculation.", zap.Error(err))
			}
			err = os.Setenv("APPEND", "--append") // related to python coverage calculation
			if err != nil {
				r.config.Test.SkipCoverage = true
				r.logger.Debug("failed to set APPEND env variable, skipping coverage calculation.", zap.Error(err))
			}
		}
	}
	if !r.config.Test.SkipCoverage && r.config.Test.Language == models.Java {
		err = java.MergeAndGenerateJacocoReport(ctx, r.logger)
		if err != nil {
			r.config.Test.SkipCoverage = true
		}
	}

	if len(flakyTestSets) > 0 {
		r.logger.Info("flaky testsets detected, please rerun the specific testsets with --must-pass flag to remove flaky testcases", zap.Strings("testSets", flakyTestSets))
	}

	testRunStatus := "fail"
	if testRunResult {
		testRunStatus = "pass"
	}

	r.completeTestReportMu.RLock()
	passed := r.totalTestPassed
	failed := r.totalTestFailed
	mocksConsumed := len(r.consumedMockNames)
	r.completeTestReportMu.RUnlock()
	r.telemetry.TestRun(passed, failed, len(testSets), mocksConsumed, testRunStatus, map[string]interface{}{
		"host-domains": runDomainSet.ToSlice(),
	})
	// The run reached its summary; the teardown defer must not also emit a
	// TestRunAborted for this invocation.
	summaryEmitted = true
	// Shutdown is optional: the static Telemetry interface does not require it,
	// but the concrete implementation exposes it for graceful drain of in-flight events.
	if s, ok := r.telemetry.(interface{ Shutdown() }); ok {
		s.Shutdown()
	}

	if !abortTestRun {
		r.printSummary(ctx, testRunResult)

		// Print the mismatch table whenever there ARE mock mismatches — not
		// only when the run as a whole failed. A green run with mock misses
		// (e.g. tests demoted to OBSOLETE, or a protocol whose misses can't
		// fail a test) is exactly the case the user must not stay blind to.
		//
		// Exception on green runs: DNS misses are answered with a synthetic
		// response by design (the app keeps working), so on a fully passing
		// run they are routine, not actionable — without this filter every
		// healthy run with app-startup DNS chatter would print the table.
		mismatchRows := r.mockMismatchFailures.GetFailures()
		if testRunResult {
			actionable := make([]TestFailure, 0, len(mismatchRows))
			for _, f := range mismatchRows {
				if f.FailureReason == models.ErrMockNotFound && f.MismatchReport != nil && f.MismatchReport.Protocol == "DNS" {
					continue
				}
				actionable = append(actionable, f)
			}
			mismatchRows = actionable
		}
		if len(mismatchRows) > 0 && !r.config.DisableMapping {
			failuresByTestSet := make(map[string]bool)
			for _, failure := range mismatchRows {
				failuresByTestSet[failure.TestSetID] = true
			}

			var testSetIDs []string
			for testSetID := range failuresByTestSet {
				testSetIDs = append(testSetIDs, testSetID)
			}
			testSets := strings.Join(testSetIDs, ", ")
			if testRunResult {
				r.logger.Warn("Tests passed, but some outgoing calls did not match the recorded mocks.",
					zap.String("test_sets", testSets),
					zap.String("next_step", "Review the mismatch summary below. Add drifting dynamic fields as noise (test.globalNoise), or re-record the test-set with 'keploy record' if the request structure changed."))
			} else {
				r.logger.Info("Some testsets failed due to mock differences.",
					zap.String("test_sets", testSets),
					zap.String("next_step", "Add drifting dynamic fields as noise (test.globalNoise); if the request structure changed, re-record the test-set with 'keploy record', or refresh mappings with --update-test-mapping."))
			}

			r.mockMismatchFailures.PrintFailuresTable()
		}
		coverageData := models.TestCoverage{}
		var err error
		if !r.config.Test.SkipCoverage {
			r.logger.Info("calculating coverage for the test run and inserting it into the report")
			coverageData, err = cov.GetCoverage()
			if err == nil {
				r.logger.Sugar().Infoln(models.HighlightPassingString("Total Coverage Percentage: ", coverageData.TotalCov))
				err = cov.AppendCoverage(&coverageData, testRunID)
				if err != nil {
					utils.LogError(r.logger, err, "failed to update report with the coverage data")
				}

			} else {
				r.logger.Debug("failed to calculate coverage for the test run", zap.Error(err))
			}
		}

		//executing afterTestRun hook, executed after running all the test-sets
		if !r.afterTestRunCalled {
			err = r.hookImpl.AfterTestRun(ctx, testRunID, testSets, coverageData)
			if err != nil {
				utils.LogError(r.logger, err, "failed to execute after test run hook")
			}
		}
	}

	// return non-zero error code so that pipeline processes
	// know that there is a failure in tests
	if !testRunResult {
		utils.ErrCode = 1
	}
	return nil
}

func (r *Replayer) Instrument(ctx context.Context) (*InstrumentState, error) {
	if !r.instrument {
		r.logger.Info("Keploy will not mock the outgoing calls when base path is provided", zap.Any("base path", r.config.Test.BasePath))
		return &InstrumentState{}, nil
	}
	passPortsUint := config.GetByPassPorts(r.config)
	passPortsUint32 := make([]uint32, len(passPortsUint)) // slice type of uint32
	for i, port := range passPortsUint {
		passPortsUint32[i] = uint32(port)
	}

	err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, CommandType: r.config.CommandType, DockerDelay: r.config.BuildDelay, Mode: models.MODE_TEST, BuildDelay: r.config.BuildDelay, EnableTesting: true, GlobalPassthrough: r.config.Record.GlobalPassthrough, ChannelBindingShim: r.config.Record.ChannelBindingShim, ConfigPath: r.config.ConfigPath, PassThroughPorts: passPortsUint, InMemoryCompose: r.config.InMemoryCompose})
	if err != nil {
		stopReason := "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return &InstrumentState{}, fmt.Errorf("%s", stopReason)
	}
	return &InstrumentState{}, nil
}

func (r *Replayer) GetNextTestRunID(ctx context.Context) (string, error) {
	testRunIDs, err := r.reportDB.GetAllTestRunIDs(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		return "", fmt.Errorf("failed to get all test run ids: %w", err)
	}
	return pkg.NextID(testRunIDs, models.TestRunTemplateName), nil
}

func (r *Replayer) GetAllTestSetIDs(ctx context.Context) ([]string, error) {
	return r.testDB.GetAllTestSetIDs(ctx)
}

func (r *Replayer) GetTestCases(ctx context.Context, testID string) ([]*models.TestCase, error) {
	return r.testDB.GetTestCases(ctx, testID)
}

func (r *Replayer) RunTestSet(ctx context.Context, testSetID string, testRunID string, serveTest bool) (models.TestSetStatus, error) {

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	runTestSetErrGrp, runTestSetCtx := errgroup.WithContext(ctx)
	runTestSetCtx = context.WithValue(runTestSetCtx, models.ErrGroupKey, runTestSetErrGrp)
	runTestSetCtx, runTestSetCtxCancel := context.WithCancel(runTestSetCtx)

	startTime := time.Now()
	pruneBefore := startTime.UTC()

	exitLoopChan := make(chan bool, 2)
	defer func() {
		// Notify the agent before cancelling the app context so proxy logs shutdown errors as debug.
		if r.instrument && !serveTest {
			notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := r.instrumentation.NotifyGracefulShutdown(notifyCtx); err != nil {
				r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
			}
			notifyCancel()
		}
		runTestSetCtxCancel()
		// Bounded drain so a wedged per-test-set goroutine can't hang teardown/SIGINT.
		if err := utils.DrainErrGroup(r.logger, "replay-testset", runTestSetErrGrp, 30*time.Second); err != nil {
			utils.LogError(r.logger, err, "error in testLoopErrGrp")
		}
		close(exitLoopChan)
	}()

	testCases, err := r.testDB.GetTestCases(runTestSetCtx, testSetID)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to get test cases: %w", err)
	}

	// Extract host domains from test cases for telemetry (HTTP and gRPC only)
	if r.runDomainSet != nil {
		for _, tc := range testCases {
			r.runDomainSet.AddAll(telemetry.ExtractDomainsFromTestCase(tc))
		}
	}

	if len(testCases) == 0 {
		r.logger.Debug("no valid test cases found to run for test set", zap.String("test-set", testSetID))

		testReport := &models.TestReport{
			Version:   models.GetVersion(),
			TestSet:   testSetID,
			Status:    string(models.TestSetStatusNoTestsToRun),
			Total:     0,
			Ignored:   0,
			TimeTaken: time.Since(startTime).String(),
			CmdUsed:   r.config.Test.CmdUsed,
		}
		err = r.reportDB.InsertReport(runTestSetCtx, testRunID, testSetID, testReport)
		if err != nil {
			utils.LogError(r.logger, err, "failed to insert report")
			return models.TestSetStatusFailed, err
		}
		return models.TestSetStatusNoTestsToRun, nil
	}

	if _, ok := r.config.Test.IgnoredTests[testSetID]; ok && len(r.config.Test.IgnoredTests[testSetID]) == 0 {
		timeTaken := time.Since(startTime)
		testReport := &models.TestReport{
			Version:   models.GetVersion(),
			TestSet:   testSetID,
			Status:    string(models.TestSetStatusIgnored),
			Total:     len(testCases),
			Ignored:   len(testCases),
			TimeTaken: timeTaken.String(),
			CmdUsed:   r.config.Test.CmdUsed,
		}

		err = r.reportDB.InsertReport(runTestSetCtx, testRunID, testSetID, testReport)
		if err != nil {
			utils.LogError(r.logger, err, "failed to insert report")
			return models.TestSetStatusFailed, err
		}

		verdict := TestReportVerdict{
			total:     testReport.Total,
			failed:    0,
			passed:    0,
			obsolete:  0,
			ignored:   testReport.Ignored,
			status:    true,
			duration:  timeTaken,
			timeTaken: timeTaken.String(),
		}

		r.completeTestReportMu.Lock()
		r.completeTestReport[testSetID] = verdict
		r.totalTests += testReport.Total
		r.totalTestIgnored += testReport.Ignored
		r.totalTestTimeTaken += timeTaken
		r.completeTestReportMu.Unlock()

		return models.TestSetStatusIgnored, nil
	}

	var conf *models.TestSet
	if r.testSetConf != nil {
		conf, err = r.testSetConf.Read(runTestSetCtx, testSetID)
		if err != nil {
			if strings.Contains(err.Error(), "no such file or directory") || strings.Contains(err.Error(), "The system cannot find the file specified") {
				r.logger.Info("test-set config file not found, continuing execution...", zap.String("test-set", testSetID))
			} else {
				return models.TestSetStatusFailed, fmt.Errorf("failed to read test set config: %w", err)
			}
		}
	}
	if conf == nil {
		conf = &models.TestSet{}
	}

	if conf.PreScript != "" {
		r.logger.Info("Running Pre-script", zap.String("script", conf.PreScript), zap.String("test-set", testSetID))
		err := r.executeScript(runTestSetCtx, conf.PreScript)
		if err != nil {
			return models.TestSetStatusFaultScript, fmt.Errorf("failed to execute pre-script: %w", err)
		}
	}

	var appErrChan = make(chan models.AppError, 1)
	var appErr models.AppError
	var success int
	var failure int
	var obsolete int
	var ignored int
	var totalConsumedMocks = map[string]models.MockState{}
	var passingTotalConsumedMocks = map[string]models.MockState{}

	testSetStatus := models.TestSetStatusPassed
	testSetStatusByErrChan := models.TestSetStatusRunning
	var testSetStatusByErrChanMu sync.RWMutex
	var lastAppErr models.AppError
	setErrStatus := func(status models.TestSetStatus) {
		testSetStatusByErrChanMu.Lock()
		testSetStatusByErrChan = status
		testSetStatusByErrChanMu.Unlock()
	}
	setLastAppErr := func(appErr models.AppError) {
		testSetStatusByErrChanMu.Lock()
		lastAppErr = appErr
		testSetStatusByErrChanMu.Unlock()
	}
	getErrStatus := func() models.TestSetStatus {
		testSetStatusByErrChanMu.RLock()
		defer testSetStatusByErrChanMu.RUnlock()
		return testSetStatusByErrChan
	}
	getLastAppErr := func() models.AppError {
		testSetStatusByErrChanMu.RLock()
		defer testSetStatusByErrChanMu.RUnlock()
		return lastAppErr
	}

	cmdType := utils.CmdType(r.config.CommandType)
	// Check if mappings are present and decide filtering strategy
	var expectedTestMockMappings map[string][]models.MockEntry
	var useMappingBased bool
	var isMappingEnabled bool
	isMappingEnabled = !r.config.DisableMapping
	selectedTests := matcherUtils.ArrayToMap(r.config.Test.SelectedTests[testSetID])
	// Map mock name to Kind for DNS filtering (mappings may have empty Kind)
	mockKindByName := make(map[string]models.Kind)
	// reusableMockNames marks mocks whose recorder-derived tier is reusable
	// across tests (session / connection / config) or app-startup traffic —
	// i.e. NOT per-test. They belong in the mapping (so the pool is complete)
	// but are excluded from the per-test consumed-vs-expected assertion: only
	// per-test mocks are deterministically attributed to a single test, so
	// comparing reusable/startup mocks would falsely demote tests to OBSOLETE
	// (the same reason DNS mocks are excluded). MockEntry carries no tier, so
	// we derive it from the loaded mocks (which do, via TestModeInfo.Lifetime).
	reusableMockNames := make(map[string]bool)
	addKinds := func(mocks []*models.Mock) {
		for _, m := range mocks {
			mockKindByName[m.Name] = m.Kind
			if isReusableTierMock(m) {
				reusableMockNames[m.Name] = true
			}
		}
	}

	if r.instrument && cmdType == utils.DockerCompose {
		if !serveTest {
			runTestSetErrGrp.Go(func() error {
				defer utils.Recover(r.logger)
				appErr = r.RunApplication(runTestSetCtx, models.RunOptions{
					AppCommand: conf.AppCommand,
				})
				if appErr != (models.AppError{}) {
					setLastAppErr(appErr)
				}
				if mappedStatus, ok := mapAppErrorToTestSetStatus(appErr); ok {
					setErrStatus(mappedStatus)
				}
				if appErr.AppErrorType == models.ErrCtxCanceled || appErr == (models.AppError{}) {
					return nil
				}
				appErrChan <- appErr
				return nil
			})
		}

		// Checking for errors in the mocking and application
		runTestSetErrGrp.Go(func() error {
			defer utils.Recover(r.logger)
			select {
			case err := <-appErrChan:
				setLastAppErr(err)
				if mappedStatus, ok := mapAppErrorToTestSetStatus(err); ok {
					setErrStatus(mappedStatus)
				} else if err.AppErrorType == models.ErrCtxCanceled {
					return nil
				}
				utils.LogError(r.logger, err, applicationFailedToRunLogMessage)
			case <-runTestSetCtx.Done():
				setErrStatus(models.TestSetStatusUserAbort)
			}
			exitLoopChan <- true
			runTestSetCtxCancel()
			return nil
		})

		// When the stack is reused across test-sets (serveTest), the agent is
		// started once and stays healthy — only the first test-set needs to wait
		// for readiness. Re-paying a 120s window per test-set is dead time where a
		// transient daemon stall can land. Mirrors the waitForAppReady gating.
		if !serveTest || r.isFirstTestSet {
			// Aligned with the agent's own healthcheck budget; a fixed 120s wait
			// gave up while the agent container was still starting under CI daemon
			// contention. See pkg.AgentReadyTimeout (KEPLOY_AGENT_READY_TIMEOUT).
			agentCtx, cancel := context.WithTimeout(runTestSetCtx, pkg.AgentReadyTimeout())
			defer cancel()

			agentReadyCh := make(chan bool, 1)
			go pkg.AgentHealthTicker(agentCtx, r.logger, string(r.config.Agent.AgentURI), agentReadyCh, 1*time.Second)

			select {
			case <-runTestSetCtx.Done():
				// Parent context cancelled (user pressed Ctrl+C)
				return models.TestSetStatusUserAbort, runTestSetCtx.Err()
			case <-agentCtx.Done():
				// Tear down the compose stack we already created before returning,
				// so a retry's `compose up` doesn't hit "container name already in
				// use" — the readiness timeout otherwise leaves the dependency
				// containers + project network behind. Fresh ctx since
				// runTestSetCtx is being torn down; composeDown bounds itself.
				if downErr := r.instrumentation.ComposeDownOnSetupFailure(context.Background()); downErr != nil {
					r.logger.Debug("composeDown after agent-ready timeout failed", zap.Error(downErr))
				}
				return models.TestSetStatusFailed, fmt.Errorf("keploy-agent did not become ready in time")
			case <-agentReadyCh:
			}
		}

		// In case of Docker Compose : since for every test set the agent and application are restarted, hence each test set can be considered as an indicidual test run
		// We also need the firstRun for knowing the first test set run in the whole test mode for purpose like cleanup
		err := r.hookImpl.BeforeTestSetCompose(ctx, testRunID, testSetID, r.firstRun)
		if err != nil {
			stopReason := fmt.Sprintf("failed to run BeforeTestSetCompose hook: %v", err)
			utils.LogError(r.logger, err, stopReason)
		}
		r.firstRun = false
		// Prepare header + body noise configuration for mock matching
		mockNoiseConfig := PrepareMockNoiseConfig(r.config.Test.GlobalNoise.Global, r.config.Test.GlobalNoise.Testsets, testSetID)

		if r.config.Test.FallBackOnMiss {
			r.fallbackDeprecateOnce.Do(func() {
				r.logger.Info("fallBackOnMiss flag is deprecated and ignored. Replay is now always deterministic. Remove this flag from your config.")
			})
		}

		err = r.instrumentation.MockOutgoing(runTestSetCtx, models.OutgoingOptions{
			Rules:                  r.config.BypassRules,
			MongoPassword:          r.config.Test.MongoPassword,
			SQLDelay:               time.Duration(r.config.Test.Delay) * time.Second,
			Mocking:                r.config.Test.Mocking,
			Backdate:               testCases[0].HTTPReq.Timestamp,
			NoiseConfig:            mockNoiseConfig,
			DisableAutoHeaderNoise: r.config.Test.DisableAutoHeaderNoise,
			SchemaNoiseDetection:   r.config.Test.SchemaNoiseDetection,
			SchemaNoiseStrict:      r.config.Test.SchemaNoiseStrict,
			MysqlPorts:             r.config.MysqlPorts,
		})
		if err != nil {
			if ctx.Err() != context.Canceled {
				utils.LogError(r.logger, err, "failed to mock outgoing")
			}
			return models.TestSetStatusFailed, err
		}

		useMappingBased, expectedTestMockMappings = r.determineMockingStrategy(ctx, testSetID, isMappingEnabled)
		mocksThatHaveMappings := make(map[string]bool)

		mocksWeNeed := make(map[string]bool)

		if isMappingEnabled && len(expectedTestMockMappings) > 0 {
			// Populate the Registry
			for _, mocks := range expectedTestMockMappings {
				for _, m := range mocks {
					mocksThatHaveMappings[m.Name] = true
				}
			}

			if len(selectedTests) > 0 {
				for testID := range selectedTests {
					if mocks, ok := expectedTestMockMappings[testID]; ok {
						for _, m := range mocks {
							mocksWeNeed[m.Name] = true
						}
					}
				}
			} else {
				// If running all tests, we need all mapped mocks
				mocksWeNeed = mocksThatHaveMappings
			}
		}
		// Get all mocks for mapping-based filtering
		filteredMocks, unfilteredMocks, err := r.GetMocks(ctx, testSetID, models.BaseTime, time.Now(), mocksThatHaveMappings, mocksWeNeed)
		if err != nil {
			return models.TestSetStatusFailed, err
		}

		addKinds(filteredMocks)
		addKinds(unfilteredMocks)

		// Extract host domains from mocks for telemetry (HTTP and gRPC only)
		if r.runDomainSet != nil {
			for _, m := range filteredMocks {
				r.runDomainSet.AddAll(telemetry.ExtractDomainsFromMock(m))
			}
			for _, m := range unfilteredMocks {
				r.runDomainSet.AddAll(telemetry.ExtractDomainsFromMock(m))
			}
		}

		if filteredMocks == nil && unfilteredMocks == nil {
			r.logger.Debug("no mocks found for test set", zap.String("testSetID", testSetID))
		}

		if mutator, ok := r.hookImpl.(MockMutator); ok {
			if err := mutator.AfterGetMocks(ctx, filteredMocks, unfilteredMocks); err != nil {
				return models.TestSetStatusFailed, err
			}
		}

		err = r.instrumentation.StoreMocks(ctx, filteredMocks, unfilteredMocks)
		if err != nil {
			utils.LogError(r.logger, err, "failed to store mocks on agent")
			return models.TestSetStatusFailed, err
		}

		if !isMappingEnabled {
			r.logger.Debug("Mapping-based mock filtering strategy is disabled, using timestamp-based mock filtering strategy")
		}

		// Send initial filtering parameters to set up mocks for test set
		err = r.SendMockFilterParamsToAgent(ctx, []string{}, models.BaseTime, time.Now(), totalConsumedMocks, useMappingBased)
		if err != nil {
			return models.TestSetStatusFailed, err
		}

		err = r.instrumentation.MakeAgentReadyForDockerCompose(ctx)
		if err != nil {
			utils.LogError(r.logger, err, "Failed to make the request to make agent ready for the docker compose")
		}

		// Wait for the user application to become ready before firing the first test.
		// Prefers polling Test.HealthURL when set, otherwise falls back to the fixed --delay sleep.
		//
		// --keep-app-alive: the app was spawned ONCE in Start() and
		// has already passed its readiness check during the first
		// test-set; subsequent test-sets inherit the warm app, so
		// paying --delay seconds (or another HealthURL poll round)
		// per boundary is dead time. Skip when we're past the first
		// test-set. The first test-set still runs the wait so the
		// initial app startup can finish (build/boot/connect) before
		// the first request fires.
		//
		// Gate keyed on `serveTest` (the parameter Start() computes
		// as `effectiveKeepAlive`) rather than the raw config bool,
		// so the skip ONLY arms when the one-shot spawn actually
		// fired. If --keep-app-alive was set on a cmdType where the
		// one-shot spawn was suppressed (cmdType == Empty / instrument
		// off), serveTest will be false here and the readiness wait
		// runs every test-set, matching the historical lifecycle.
		if serveTest && !r.isFirstTestSet {
			r.logger.Debug("--keep-app-alive: skipping waitForAppReady on post-first test-set; app already warm")
		} else if !waitForAppReady(runTestSetCtx, r.logger, r.config) {
			return models.TestSetStatusUserAbort, context.Canceled
		}
	}

	if cmdType != utils.DockerCompose {

		useMappingBased, expectedTestMockMappings = r.determineMockingStrategy(ctx, testSetID, isMappingEnabled)
		mocksThatHaveMappings := make(map[string]bool)

		mocksWeNeed := make(map[string]bool)

		if isMappingEnabled && len(expectedTestMockMappings) > 0 {
			// Populate the Registry
			for _, mocks := range expectedTestMockMappings {
				for _, m := range mocks {
					mocksThatHaveMappings[m.Name] = true
				}
			}

			if len(selectedTests) > 0 {
				for testID := range selectedTests {
					if mocks, ok := expectedTestMockMappings[testID]; ok {
						for _, m := range mocks {
							mocksWeNeed[m.Name] = true
						}
					}
				}
			} else {
				// If running all tests, we need all mapped mocks
				mocksWeNeed = mocksThatHaveMappings
			}
		}
		// Get all mocks for mapping-based filtering
		filteredMocks, unfilteredMocks, err := r.GetMocks(ctx, testSetID, models.BaseTime, time.Now(), mocksThatHaveMappings, mocksWeNeed)
		if err != nil {
			return models.TestSetStatusFailed, err
		}
		addKinds(filteredMocks)
		addKinds(unfilteredMocks)
		// Extract host domains from mocks for telemetry (HTTP and gRPC only)
		if r.runDomainSet != nil {
			for _, m := range filteredMocks {
				r.runDomainSet.AddAll(telemetry.ExtractDomainsFromMock(m))
			}
			for _, m := range unfilteredMocks {
				r.runDomainSet.AddAll(telemetry.ExtractDomainsFromMock(m))
			}
		}
		if mutator, ok := r.hookImpl.(MockMutator); ok {
			if err := mutator.AfterGetMocks(ctx, filteredMocks, unfilteredMocks); err != nil {
				return models.TestSetStatusFailed, err
			}
		}
		err = r.instrumentation.StoreMocks(ctx, filteredMocks, unfilteredMocks)
		if err != nil {
			utils.LogError(r.logger, err, "failed to store mocks on agent")
			return models.TestSetStatusFailed, err
		}
		if r.firstRun {
			err = r.hookImpl.BeforeTestRun(ctx, testRunID)
			if err != nil {
				stopReason := fmt.Sprintf("failed to run before test run hook: %v", err)
				utils.LogError(r.logger, err, stopReason)
			}
			r.firstRun = false
		}
		isMappingEnabled := !r.config.DisableMapping

		if !isMappingEnabled {
			r.logger.Debug("Mapping-based mock filtering strategy is disabled, using timestamp-based mock filtering strategy")
		}

		pkg.InitSortCounter(int64(max(len(filteredMocks), len(unfilteredMocks))))

		// Prepare header + body noise configuration for mock matching
		mockNoiseConfig := PrepareMockNoiseConfig(r.config.Test.GlobalNoise.Global, r.config.Test.GlobalNoise.Testsets, testSetID)

		if r.config.Test.FallBackOnMiss {
			r.fallbackDeprecateOnce.Do(func() {
				r.logger.Info("fallBackOnMiss flag is deprecated and ignored. Replay is now always deterministic. Remove this flag from your config.")
			})
		}

		err = r.instrumentation.MockOutgoing(runTestSetCtx, models.OutgoingOptions{
			Rules:                  r.config.BypassRules,
			MongoPassword:          r.config.Test.MongoPassword,
			SQLDelay:               time.Duration(r.config.Test.Delay) * time.Second,
			Mocking:                r.config.Test.Mocking,
			Backdate:               testCases[0].HTTPReq.Timestamp,
			NoiseConfig:            mockNoiseConfig,
			DisableAutoHeaderNoise: r.config.Test.DisableAutoHeaderNoise,
			SchemaNoiseDetection:   r.config.Test.SchemaNoiseDetection,
			SchemaNoiseStrict:      r.config.Test.SchemaNoiseStrict,
			MysqlPorts:             r.config.MysqlPorts,
		})
		if err != nil {
			if ctx.Err() != context.Canceled {
				utils.LogError(r.logger, err, "failed to mock outgoing")
			}
			return models.TestSetStatusFailed, err
		}

		// Send initial filtering parameters to set up mocks for test set
		err = r.SendMockFilterParamsToAgent(ctx, []string{}, models.BaseTime, time.Now(), totalConsumedMocks, useMappingBased)
		if err != nil {
			return models.TestSetStatusFailed, err
		}

		if r.instrument {
			if !serveTest {
				runTestSetErrGrp.Go(func() error {
					defer utils.Recover(r.logger)
					appErr = r.RunApplication(runTestSetCtx, models.RunOptions{
						AppCommand: conf.AppCommand,
					})
					if appErr != (models.AppError{}) {
						setLastAppErr(appErr)
					}
					if mappedStatus, ok := mapAppErrorToTestSetStatus(appErr); ok {
						setErrStatus(mappedStatus)
					}
					if appErr.AppErrorType == models.ErrCtxCanceled || appErr == (models.AppError{}) {
						return nil
					}
					appErrChan <- appErr
					return nil
				})
			}

			// Checking for errors in the mocking and application
			runTestSetErrGrp.Go(func() error {
				defer utils.Recover(r.logger)
				select {
				case err := <-appErrChan:
					setLastAppErr(err)
					if mappedStatus, ok := mapAppErrorToTestSetStatus(err); ok {
						setErrStatus(mappedStatus)
					} else if err.AppErrorType == models.ErrCtxCanceled {
						return nil
					}
					utils.LogError(r.logger, err, applicationFailedToRunLogMessage)
				case <-runTestSetCtx.Done():
					setErrStatus(models.TestSetStatusUserAbort)
				}
				exitLoopChan <- true
				runTestSetCtxCancel()
				return nil
			})

			// Wait for the user application to become ready before firing the first test.
			// Prefers polling Test.HealthURL when set, otherwise falls back to the fixed --delay sleep.
			// See the parallel call in the docker-compose branch above
			// for the --keep-app-alive rationale. On the non-compose
			// paths (native, docker-run, docker-start) the one-shot
			// spawn in Start() also fires when KeepAppAlive is set
			// AND cmdType is non-empty AND instrument is enabled.
			// Gate keyed on `serveTest` (the Start()-computed
			// effectiveKeepAlive predicate) — identical to the compose
			// branch above — so the readiness skip arms only when the
			// one-shot spawn actually fired.
			if serveTest && !r.isFirstTestSet {
				r.logger.Debug("--keep-app-alive: skipping waitForAppReady on post-first test-set; app already warm")
			} else if !waitForAppReady(runTestSetCtx, r.logger, r.config) {
				return models.TestSetStatusUserAbort, context.Canceled
			}

		}
	}

	if err := r.hookImpl.BeforeTestSetReplay(runTestSetCtx, testSetID); err != nil {
		utils.LogError(r.logger, err, "BeforeTestSetReplay hook failed; inspect your custom hook implementation or disable it for this test set if this failure is expected",
			zap.String("testSetID", testSetID),
		)
	}

	ignoredTests := matcherUtils.ArrayToMap(r.config.Test.IgnoredTests[testSetID])

	testCasesCount := len(testCases)

	if len(selectedTests) != 0 {
		testCasesCount = len(selectedTests)
	}

	// Inserting the initial report for the test set
	testReport := &models.TestReport{
		Version:   models.GetVersion(),
		Total:     testCasesCount,
		Status:    string(models.TestStatusRunning),
		TimeTaken: time.Since(startTime).String(),
		CmdUsed:   r.config.Test.CmdUsed,
	}

	err = r.reportDB.InsertReport(runTestSetCtx, testRunID, testSetID, testReport)
	if err != nil {
		utils.LogError(r.logger, err, "failed to insert report")
		return models.TestSetStatusFailed, err
	}

	// var to exit the loop
	var exitLoop bool
	// var to store the error in the loop
	var loopErr error
	utils.TemplatizedValues = conf.Template
	utils.SecretValues = conf.Secret

	// Add secret files to .gitignore if they exist
	if len(utils.SecretValues) > 0 {
		err = utils.AddToGitIgnore(r.logger, r.config.Path, "/*/secret.yaml")
		if err != nil {
			r.logger.Debug("Failed to add secret files to .gitignore", zap.Error(err))
		}
	}

	actualTestMockMappings := &models.Mapping{
		Version:   string(models.GetVersion()),
		Kind:      models.MappingKind,
		TestSetID: testSetID,
	}
	var consumedMocks []models.MockState
	consumedMocks, err = r.hookImpl.GetConsumedMocks(runTestSetCtx) // Getting mocks consumed during initial setup
	if err != nil {
		if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), err); ok {
			testSetStatus = resolvedStatus
			exitLoop = true
		} else {
			utils.LogError(r.logger, err, "failed to get consumed filtered mocks")
		}
	}
	r.logger.Debug("consumed mocks during initial setup",
		zap.String("testSetID", testSetID),
		zap.Int("count", len(consumedMocks)),
		zap.Any("mocks", consumedMocks))
	for _, m := range consumedMocks {
		totalConsumedMocks[m.Name] = m
		passingTotalConsumedMocks[m.Name] = m
	}

	// Snapshot the post-setup consumed-mock baseline. These are the
	// reusable/session mocks (driver handshake, auth, connection pool
	// warm-up) consumed during application bootstrap, before any test
	// fires. Each mock-consistency retry cycle re-runs the passing tests
	// as a fresh forward pass, so its serving pool must START from this
	// baseline — see the rewind at the top of the replay loop below.
	baselineConsumedMocks := make(map[string]models.MockState, len(totalConsumedMocks))
	for k, v := range totalConsumedMocks {
		baselineConsumedMocks[k] = v
	}
	// Persistent union of every MockState consumed across ALL retry cycles.
	// totalConsumedMocks is rewound to baselineConsumedMocks at the start of
	// each retry cycle, so after the loop it no longer holds the full set of
	// consumed mocks. Two post-loop readers need that full union: the
	// run-level Mocks-Consumed telemetry (r.consumedMockNames) and the
	// schema-noise-detection persistence (PersistMockNoise, which reads the
	// learned ReqBodyNoise off each MockState value — including mocks a later
	// test failed on). Fold consumed mocks in here as cycles complete; last
	// write wins, matching the pre-rewind behavior where a re-consumed mock's
	// final-cycle MockState overwrote earlier ones.
	allConsumedAcrossCycles := make(map[string]models.MockState, len(totalConsumedMocks))
	for k, v := range totalConsumedMocks {
		allConsumedAcrossCycles[k] = v
	}
	// Build a lookup of mock name -> summary and protocol from the mock registry (once per test set).
	type mockInfo struct {
		summary  string
		protocol string
	}
	mockLookup := map[string]mockInfo{}
	if r.mockDB != nil {
		if allMocks, err := r.mockDB.GetUnFilteredMocks(runTestSetCtx, testSetID, models.BaseTime, time.Now(), nil, nil); err == nil {
			for _, mock := range allMocks {
				mockLookup[mock.Name] = mockInfo{
					summary:  models.MockSummaryFromSpec(mock),
					protocol: string(mock.Kind),
				}
			}
		}
	}

	// Separate replay into regular and streaming buckets. Regular tests can use the
	// iterative replay path from main, while streaming tests are replayed sequentially
	// afterwards so long-lived connections do not block the normal flow.
	type streamingTest struct {
		testCase      *models.TestCase
		expectedMocks []string
	}

	var activeTestCases []*models.TestCase
	var streamingTests []streamingTest
	for _, testCase := range testCases {
		if _, ok := selectedTests[testCase.Name]; !ok && len(selectedTests) != 0 {
			continue
		}

		if _, ok := ignoredTests[testCase.Name]; ok {
			testCaseResult := &models.TestResult{
				Kind:         models.HTTP,
				Name:         testSetID,
				Status:       models.TestStatusIgnored,
				TestCaseID:   testCase.Name,
				TestCasePath: filepath.Join(r.config.Path, testSetID),
				MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
			}
			loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
			if loopErr != nil {
				utils.LogError(r.logger, loopErr, "failed to insert test case result")
				break
			}
			ignored++
			continue
		}

		if testCase != nil && testCase.Kind == models.HTTP && pkg.IsHTTPStreamingTestCase(testCase) {
			tcCopy := *testCase
			expectedMockNames := make([]string, len(expectedTestMockMappings[testCase.Name]))
			for i, m := range expectedTestMockMappings[testCase.Name] {
				expectedMockNames[i] = m.Name
			}
			streamingTests = append(streamingTests, streamingTest{
				testCase:      &tcCopy,
				expectedMocks: expectedMockNames,
			})
			r.logger.Debug("deferring streaming test case",
				zap.String("testcase", testCase.Name),
				zap.String("testset", testSetID))
			continue
		}

		activeTestCases = append(activeTestCases, testCase)
	}

	testsToRun := activeTestCases
	finalTestCaseResults := make(map[string]*models.TestResult)
	itr := 1
	if r.config.RetryPassing {
		itr = 5
	}
	for replay := 0; replay < itr; replay++ {
		// Mock-consistency retry cycles (replay > 0) re-run the tests that
		// passed the previous cycle as a fresh forward pass. Rewind the
		// consumed-mock filter to the post-setup baseline so each re-run
		// test's PER-TEST single-use mocks — consumed via DeleteFilteredMock
		// and flagged Usage=Deleted during the previous cycle — are served
		// again. Without this rewind those mocks stay in totalConsumedMocks
		// as Deleted; the agent's filterOutDeleted then strips them from the
		// retry pool and every test that drives a per-test mock (MongoDB
		// find/update, SQL query, non-idempotent HTTP) fails the retry with
		// match_phase=no_mocks even though it passed the first pass. The
		// window filter alone can't save them: the mock IS in the window,
		// it's the Deleted flag that removes it. Mocks consumed in the prior
		// cycle are folded into allConsumedAcrossCycles first so the post-loop
		// readers (Mocks-Consumed telemetry and PersistMockNoise) stay complete.
		if replay > 0 {
			for k, v := range totalConsumedMocks {
				allConsumedAcrossCycles[k] = v
			}
			totalConsumedMocks = make(map[string]models.MockState, len(baselineConsumedMocks))
			for k, v := range baselineConsumedMocks {
				totalConsumedMocks[k] = v
			}
		}
		var nextTestsToRun []*models.TestCase
		var currentFailures int
		var currentObsolete int
		var currentSuccess int
		currentPassingMocks := make(map[string]models.MockState)

		for idx, testCase := range testsToRun {

			// check if its the last test case running
			if idx == len(testsToRun)-1 && r.isLastTestSet && len(streamingTests) == 0 {
				r.isLastTestCase = true
				testCase.IsLast = true
			}

			if _, ok := selectedTests[testCase.Name]; !ok && len(selectedTests) != 0 {
				continue
			}

			if _, ok := ignoredTests[testCase.Name]; ok {
				testCaseResult := &models.TestResult{
					Kind:         models.HTTP,
					Name:         testSetID,
					Status:       models.TestStatusIgnored,
					TestCaseID:   testCase.Name,
					TestCasePath: filepath.Join(r.config.Path, testSetID),
					MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
				}
				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, "failed to insert test case result")
					break
				}
				ignored++
				continue
			}

			// Stop early before the hook and URL mutations if an exit signal is
			// already pending — avoids mutating test cases that will never run.
			select {
			case <-exitLoopChan:
				testSetStatus = getErrStatus()
				exitLoop = true
			default:
			}
			if exitLoop {
				break
			}

			// Run pre-test mutation hook once per test case (not on retries) to
			// avoid compounding side effects from in-place mutations.
			if replay == 0 {
				if mutator, ok := r.hookImpl.(TestCaseMutator); ok {
					if err := mutator.BeforeTestCaseRun(runTestSetCtx, testCase, testSetID); err != nil {
						utils.LogError(r.logger, err, "BeforeTestCaseRun hook failed; replay continues with test case in current state",
							zap.String("testcase", testCase.Name),
							zap.String("next_step", "check the BeforeTestCaseRun implementation and any external dependencies it uses (e.g. KMS, auth, network)"))
					}
				}
			}

			// replace the request URL's BasePath/origin if provided — gated on
			// replay==0 to prevent path.Join from doubling the prefix on retries.
			if r.config.Test.BasePath != "" && replay == 0 {
				newURL, err := ReplaceBaseURL(r.config.Test.BasePath, testCase.HTTPReq.URL)
				if err != nil {
					r.logger.Error("failed to replace the request basePath",
						zap.String("testcase", testCase.Name),
						zap.String("basePath", r.config.Test.BasePath),
						zap.String("next_step", "verify --basePath / test.basePath value — expected format is an absolute URL like http://host:port or a path prefix starting with /; ensure the recorded URL is compatible with this base"),
						zap.Error(err))
				} else {
					testCase.HTTPReq.URL = newURL
				}
				r.logger.Debug("test case request origin", zap.String("testcase", testCase.Name), zap.String("TestCaseURL", testCase.HTTPReq.URL), zap.String("basePath", r.config.Test.BasePath))
			}

			var testStatus models.TestStatus
			var testResult *models.Result
			var testPass bool
			var loopErr error

			var reqTime, respTime time.Time
			switch testCase.Kind {
			case models.HTTP:
				reqTime = testCase.HTTPReq.Timestamp
				respTime = testCase.HTTPResp.Timestamp
			case models.GRPC_EXPORT:
				reqTime = testCase.GrpcReq.Timestamp
				respTime = testCase.GrpcResp.Timestamp
			}

			expectedNames := make([]string, len(expectedTestMockMappings[testCase.Name]))
			for i, m := range expectedTestMockMappings[testCase.Name] {
				expectedNames[i] = m.Name
			}
			err = r.SendMockFilterParamsToAgent(runTestSetCtx, expectedNames, reqTime, respTime, totalConsumedMocks, useMappingBased)
			if err != nil {
				if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), err); ok {
					testSetStatus = resolvedStatus
				} else {
					utils.LogError(r.logger, err, "failed to update mock parameters on agent")
				}
				break
			}

			// Host and Port replacements are now handled inside SimulateHTTP/SimulateGRPC via config parameters.
			// This ensures that replaceWith configuration takes precedence over global host/port overrides.

			started := time.Now().UTC()

			r.beginTestErrorCapture(runTestSetCtx)

			resp, loopErr := r.hookImpl.SimulateRequest(runTestSetCtx, testCase, testSetID)

			// A "connection reset by peer" / unexpected EOF while exchanging
			// the request is, under loaded CI replaying a DOCKER app, dominated
			// by docker's userland proxy (docker-proxy) resetting a freshly
			// accepted host-side connection during connection setup — the app
			// never processed the request, so zero mocks were consumed. That
			// case is a transient transport failure, not a real regression, and
			// keploy would otherwise record a false status_code=0.
			//
			// We re-send ONLY when it is provably safe: the agent reports that
			// THIS request consumed no new mocks. MockManager.GetConsumedMocks
			// is per-call and DRAINING (it returns only what was consumed since
			// the last drain, which the normal per-test drain already emptied),
			// so a non-empty result means this request burned a single-use mock
			// — the gate then refuses to retry, because re-running would
			// re-exercise non-idempotent logic against exhausted mocks and
			// fabricate a verdict. This mirrors the ECONNREFUSED re-send
			// (pkg.SimulateHTTP), just establishing the "nothing irreversible
			// happened" invariant via mock consumption instead of error type.
			//
			// On that refusal path retryResetOnce hands back the mocks its gate
			// DRAINED for this failed request; we fold them into
			// totalConsumedMocks so the next SendMockFilterParamsToAgent's
			// filterOutDeleted still drops those exhausted mocks (the drain only
			// affects keploy-side reporting, never the agent's own serving
			// state — see retryResetOnce).
			if loopErr != nil && pkg.IsTransportConnReset(loopErr) {
				retryResp, retried, drainedConsumed := r.retryResetOnce(runTestSetCtx, testCase, testSetID, loopErr)
				if retried {
					resp, loopErr = retryResp, nil
				} else {
					for _, m := range drainedConsumed {
						totalConsumedMocks[m.Name] = m
					}
				}
			}

			if loopErr != nil {
				utils.LogError(r.logger, loopErr, "failed to simulate request")
				currentFailures++
				testSetStatus = models.TestSetStatusFailed
				testCaseResult := r.CreateFailedTestResult(testCase, testSetID, started, loopErr.Error())
				// Finalize the capture window even on this early exit, so a miss
				// during this (failed) test attaches here and isn't carried to
				// the next test or lost.
				r.attachMockErrors(runTestSetCtx, testSetID, testCase.Name, testCaseResult)
				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, "failed to insert test case result for simulation error")
					break
				}
				continue
			}

			// instrumentConsumedFetchErr captures whether THIS test's per-test
			// consumed-mock fetch in instrument mode succeeded. Read later by
			// the per-test perTestConsumedKnown signal so a failed instrument
			// fetch is treated the same as a failed non-instrument fetch
			// (skip MockMismatches / MatchedCalls rather than emit stale data).
			var instrumentConsumedFetchErr error
			if r.instrument {
				consumedMocks, err = r.hookImpl.GetConsumedMocks(runTestSetCtx)
				instrumentConsumedFetchErr = err
				if err != nil {
					if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), err); ok {
						testSetStatus = resolvedStatus
						exitLoop = true
					} else {
						utils.LogError(r.logger, err, "failed to get consumed filtered mocks")
					}
					// On fetch failure, consumedMocks may carry stale/partial
					// data from the previous test. Clear it so downstream uses
					// in this iteration (logging, mockNames, upsertActualTest-
					// MockMapping) and the next iteration's filter params
					// don't attribute the previous test's data to this one.
					consumedMocks = nil
				}
				r.logger.Debug("consumed mocks after test case simulation",
					zap.String("testSetID", testSetID),
					zap.String("testCaseID", testCase.Name),
					zap.Int("count", len(consumedMocks)),
					zap.Any("mocks", consumedMocks),
					zap.Bool("fetchOk", instrumentConsumedFetchErr == nil))
				// totalConsumedMocks aggregation is implicit-gated: on fetch
				// failure consumedMocks was nil'd above, so this loop is a
				// no-op and stale data can't leak into the next iteration's
				// filter params (SendMockFilterParamsToAgent).
				for _, m := range consumedMocks {
					totalConsumedMocks[m.Name] = m
				}
			}

			r.logger.Debug("test case kind", zap.String("kind", string(testCase.Kind)), zap.String("testcase", testCase.Name), zap.String("testset", testSetID))

			mockNames := make([]string, 0, len(consumedMocks))
			for _, m := range consumedMocks {
				mockNames = append(mockNames, m.Name)
			}

			expectedMocks, hasExpectedMocks := expectedTestMockMappings[testCase.Name]

			// Compute non-DNS expected and consumed name slices once; reused for subset check and mismatch reporting.
			// Only PER-TEST mocks participate in the consumed-vs-expected
			// assertion. DNS (non-deterministic resolution order) and
			// reusable/startup-tier mocks (session / connection / config,
			// recorded once at app boot and shared across tests) stay in the
			// mapping but are excluded here: they are not deterministically
			// attributed to a single test's window, so including them would
			// falsely demote tests to OBSOLETE.
			filteredExpectedNames := make([]string, 0, len(expectedMocks))
			for _, m := range expectedMocks {
				isDNS := strings.EqualFold(m.Kind, string(models.DNS))
				if !isDNS {
					if kind, ok := mockKindByName[m.Name]; ok && kind == models.DNS {
						isDNS = true
					}
				}
				if isDNS || reusableMockNames[m.Name] {
					continue
				}
				filteredExpectedNames = append(filteredExpectedNames, m.Name)
			}

			filteredMockNames := make([]string, 0, len(consumedMocks))
			for _, m := range consumedMocks {
				if m.Kind == models.DNS || isReusableTierState(m) {
					continue
				}
				filteredMockNames = append(filteredMockNames, m.Name)
			}

			mockSetMismatch := false
			if r.instrument && useMappingBased && isMappingEnabled && hasExpectedMocks && instrumentConsumedFetchErr == nil {
				// Compare only per-test mocks (DNS + reusable/startup tiers
				// excluded above) since those are the only mocks
				// deterministically tied to a single test. Also gate on a
				// successful per-test GetConsumedMocks — when the fetch failed,
				// consumedMocks was cleared to nil above, which would
				// deterministically compute mockSetMismatch=true (empty subset
				// of any non-empty expected set) and falsely mark the test
				// OBSOLETE due to an unrelated transport error rather than a
				// real mock-pool divergence.
				mockSetMismatch = !isMockSubset(filteredMockNames, filteredExpectedNames)
			}

			emitFailureLogs := !mockSetMismatch

			switch testCase.Kind {
			case models.HTTP:
				httpResp, ok := resp.(*models.HTTPResp)
				if !ok {
					r.logger.Error("invalid response type for HTTP test case")
					currentFailures++
					testSetStatus = models.TestSetStatusFailed
					testCaseResult := r.CreateFailedTestResult(testCase, testSetID, started, "invalid response type for HTTP test case")
					r.attachMockErrors(runTestSetCtx, testSetID, testCase.Name, testCaseResult)
					loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
					if loopErr != nil {
						utils.LogError(r.logger, loopErr, fmt.Sprintf("failed to insert test case result for type assertion error in %s test case", testCase.Kind))
						break
					}
					continue
				}
				testPass, testResult = r.compareHTTPRespForReplay(testCase, httpResp, testSetID, emitFailureLogs)

			case models.GRPC_EXPORT:
				grpcResp, ok := resp.(*models.GrpcResp)
				if !ok {
					r.logger.Error("invalid response type for gRPC test case")
					currentFailures++
					testSetStatus = models.TestSetStatusFailed
					testCaseResult := r.CreateFailedTestResult(testCase, testSetID, started, "invalid response type for gRPC test case")
					r.attachMockErrors(runTestSetCtx, testSetID, testCase.Name, testCaseResult)
					loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
					if loopErr != nil {
						utils.LogError(r.logger, loopErr, "failed to insert test case result for type assertion error")
						break
					}
					continue
				}

				respCopy := *grpcResp

				if r.config.Test.ProtoFile != "" || r.config.Test.ProtoDir != "" || len(r.config.Test.ProtoInclude) > 0 {
					// get the :path header from the request
					method, ok := testCase.GrpcReq.Headers.PseudoHeaders[":path"]
					if !ok {
						utils.LogError(r.logger, nil, "failed to get :path header from the request, cannot convert grpc response to json")
						goto compareResp
					}

					pc := models.ProtoConfig{
						ProtoFile:    r.config.Test.ProtoFile,
						ProtoDir:     r.config.Test.ProtoDir,
						ProtoInclude: r.config.Test.ProtoInclude,
						RequestURI:   method,
					}

					// get the proto message descriptor
					md, files, err := utils.GetProtoMessageDescriptor(context.Background(), r.logger, pc)
					if err != nil {
						utils.LogError(r.logger, err, "failed to get proto message descriptor, cannot convert grpc response to json")
						goto compareResp
					}

					// convert both actual and expected using the same path (protoscope-text -> wire -> json)
					actJSON, actOK := utils.ProtoTextToJSON(md, files, respCopy.Body.DecodedData, r.logger)
					testJSON, testOK := utils.ProtoTextToJSON(md, files, testCase.GrpcResp.Body.DecodedData, r.logger)

					if actOK && testOK {
						respCopy.Body.DecodedData = string(actJSON)
						testCase.GrpcResp.Body.DecodedData = string(testJSON)
					}
				}

			compareResp:
				testPass, testResult = r.CompareGRPCResp(testCase, &respCopy, testSetID, emitFailureLogs)
			}

			tcReqTime, tcRespTime := recordReqResTimestamps(testCase)
			upsertActualTestMockMapping(actualTestMockMappings, testCase.Name, consumedMocks, tcReqTime, tcRespTime)

			// log the consumed mocks during the test run of the test case for test set
			r.logger.Debug("consumed mocks for test case",
				zap.String("testSetID", testSetID),
				zap.String("testCaseID", testCase.Name),
				zap.Strings("mockNames", mockNames),
				zap.Any("mocks", consumedMocks))

			// strictMockReject: under SchemaNoiseStrict, an expected mock that
			// went unconsumed means strict req-body matching REJECTED it — a
			// non-noise request field drifted. The app's response can still
			// match (e.g. a deterministic dependency), so the response check
			// alone won't catch it; treat it as a real test failure.
			strictMockReject := false
			if mockSetMismatch {
				switch {
				case testPass && r.config.Test.SchemaNoiseStrict:
					r.logger.Error("strict schema-noise: expected mock was rejected (non-noise request-body drift); failing testcase even though the response matched",
						zap.String("testcase", testCase.Name),
						zap.String("testset", testSetID),
						zap.Strings("expectedMocks", filteredExpectedNames),
						zap.Strings("actualMocks", filteredMockNames))
					testPass = false
					strictMockReject = true
					r.mockMismatchFailures.AddFailure(testSetID, testCase.Name, filteredExpectedNames, filteredMockNames)
				case testPass:
					r.logger.Debug("mock mapping mismatch ignored because testcase passed",
						zap.String("testcase", testCase.Name),
						zap.String("testset", testSetID),
						zap.Strings("expectedMocks", filteredExpectedNames),
						zap.Strings("actualMocks", filteredMockNames))
				default:
					r.logger.Error("mock mapping mismatch detected; marking testcase as obsolete. Re-record the test case or run with --update-test-mapping to regenerate mappings",
						zap.String("testcase", testCase.Name),
						zap.String("testset", testSetID),
						zap.Strings("expectedMocks", filteredExpectedNames),
						zap.Strings("actualMocks", filteredMockNames))
					r.mockMismatchFailures.AddFailure(testSetID, testCase.Name, filteredExpectedNames, filteredMockNames)
				}
			}

			if !testPass {
				r.logger.Info("result", zap.String("testcase id", models.HighlightFailingString(testCase.Name)), zap.String("testset id", models.HighlightFailingString(testSetID)), zap.String("passed", models.HighlightFailingString(testPass)))
			} else {
				r.logger.Info("result", zap.String("testcase id", models.HighlightPassingString(testCase.Name)), zap.String("testset id", models.HighlightPassingString(testSetID)), zap.String("passed", models.HighlightPassingString(testPass)))
			}
			if testPass {
				testStatus = models.TestStatusPassed
				currentSuccess++
				nextTestsToRun = append(nextTestsToRun, testCase)
				for _, m := range consumedMocks {
					passingTotalConsumedMocks[m.Name] = m
				}
			} else if mockSetMismatch && !strictMockReject && !r.config.Test.StrictFailure {
				testStatus = models.TestStatusObsolete
				currentObsolete++
			} else {
				testStatus = models.TestStatusFailed
				currentFailures++
				testSetStatus = models.TestSetStatusFailed
			}

			if testResult != nil {
				var testCaseResult *models.TestResult

				switch testCase.Kind {
				case models.HTTP:
					httpResp := resp.(*models.HTTPResp)

					testCaseResult = &models.TestResult{
						Kind:       models.HTTP,
						Name:       testSetID,
						Status:     testStatus,
						Started:    started.Unix(),
						Completed:  time.Now().UTC().Unix(),
						TestCaseID: testCase.Name,
						Req: models.HTTPReq{
							Method:     testCase.HTTPReq.Method,
							ProtoMajor: testCase.HTTPReq.ProtoMajor,
							ProtoMinor: testCase.HTTPReq.ProtoMinor,
							URL:        testCase.HTTPReq.URL,
							URLParams:  testCase.HTTPReq.URLParams,
							Header:     testCase.HTTPReq.Header,
							Body:       testCase.HTTPReq.Body,
							Binary:     testCase.HTTPReq.Binary,
							Form:       testCase.HTTPReq.Form,
							Timestamp:  testCase.HTTPReq.Timestamp,
						},
						Res:          *httpResp,
						TestCasePath: filepath.Join(r.config.Path, testSetID),
						MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
						Noise:        testCase.Noise,
						Result:       *testResult,
						TimeTaken:    time.Since(started).String(),
					}
				case models.GRPC_EXPORT:
					grpcResp := resp.(*models.GrpcResp)

					testCaseResult = &models.TestResult{
						Kind:         models.GRPC_EXPORT,
						Name:         testSetID,
						Status:       testStatus,
						Started:      started.Unix(),
						Completed:    time.Now().UTC().Unix(),
						TestCaseID:   testCase.Name,
						GrpcReq:      testCase.GrpcReq,
						GrpcRes:      *grpcResp,
						TestCasePath: filepath.Join(r.config.Path, testSetID),
						MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
						Noise:        testCase.Noise,
						Result:       *testResult,
						TimeTaken:    time.Since(started).String(),
					}
				}

				if testCaseResult != nil {
					if testStatus == models.TestStatusFailed && testResult.FailureInfo.Risk != models.None {
						testCaseResult.FailureInfo.Risk = testResult.FailureInfo.Risk
						testCaseResult.FailureInfo.Category = testResult.FailureInfo.Category
						testCaseResult.FailureInfo.Assessment = testResult.FailureInfo.Assessment
					}
					// Per-test consumed-mock fetch + failure-diagnostic population.
					// Two distinct outputs are computed from the per-test
					// consumed-mock list below:
					//   (a) MatchedCalls / UnmatchedCalls on FailureInfo —
					//       populated only on failed/obsolete tests
					//       (UnmatchedCalls is independent of the consumed
					//       set; MatchedCalls gates on perTestConsumedKnown).
					//   (b) TestResult.MockMismatches — populated on EVERY
					//       test (pass or fail) when consumed data is known.
					//
					// In non-instrument mode (k8s-proxy / remote agent) the
					// loop-scoped `consumedMocks` is only refreshed inside the
					// `if r.instrument` branch after simulation — for non-
					// instrument it stays at whatever the initial setup set,
					// which is stale for every test except the first. Fetch
					// the per-test consumed-mock set into a PER-TEST LOCAL
					// (perTestConsumed) rather than overwriting consumedMocks,
					// so the value doesn't leak into the next iteration's
					// upsertActualTestMockMapping / mockNames computations.
					// One fetch services both outputs (a) and (b).
					//
					// On fetch error we set perTestConsumedKnown=false and
					// skip both MatchedCalls population AND MockMismatches
					// for this test (rather than falling back to the stale
					// loop-scoped consumedMocks, which would emit misleading
					// data attributed to the wrong test case).
					// perTestConsumedKnown tracks whether consumed-mock data for
					// THIS test is reliable. Instrument mode: only when the
					// per-test fetch upstream (in the `if r.instrument` branch
					// after simulation) succeeded — a failed instrument fetch
					// leaves consumedMocks at stale/partial state, same as
					// non-instrument's stale loop-scope problem. Non-instrument
					// mode: only when this block's per-test fetch succeeds.
					// MatchedCalls / MockMismatches gate on this signal so
					// stale data isn't attributed to the wrong test case.
					perTestConsumed := consumedMocks
					perTestConsumedKnown := r.instrument && instrumentConsumedFetchErr == nil
					if !r.instrument {
						if fetched, fetchErr := r.hookImpl.GetConsumedMocks(runTestSetCtx); fetchErr == nil {
							perTestConsumed = fetched
							perTestConsumedKnown = true
						} else {
							// Mirror the instrument-mode skip: leaves
							// perTestConsumedKnown false so downstream skips
							// MatchedCalls AND MockMismatches (any consumed-
							// mock-derived field) rather than attribute stale
							// data. UnmatchedCalls is unaffected — independent
							// sources (mockMismatchFailures channel + GetMock-
							// Errors) still populate for failed/obsolete tests.
							r.logger.Debug("non-instrument GetConsumedMocks failed; skipping all consumed-mock-derived fields (MatchedCalls + MockMismatches) for this test",
								zap.String("testSetID", testSetID),
								zap.String("testCaseID", testCase.Name),
								zap.Error(fetchErr))
						}
					}
					if testStatus == models.TestStatusFailed || testStatus == models.TestStatusObsolete {
						// MatchedCalls is built from perTestConsumed and is
						// only meaningful when that data is known for THIS
						// test. Skip if not — stale data would attribute the
						// previous test's matched mocks to this one.
						if perTestConsumedKnown {
							for _, m := range perTestConsumed {
								if m.Kind != models.DNS {
									info := mockLookup[m.Name]
									protocol := info.protocol
									if protocol == "" {
										protocol = string(m.Kind)
									}
									testCaseResult.FailureInfo.MatchedCalls = append(testCaseResult.FailureInfo.MatchedCalls, models.MatchedCall{
										MockName: m.Name,
										Protocol: protocol,
										Summary:  info.summary,
									})
								}
							}
						}
					}
					// UnmatchedCalls is finalized for EVERY test, not just
					// failed/obsolete ones: (1) a miss during an otherwise-passing
					// test must still surface; (2) the per-test capture window
					// opened by beginTestErrorCapture must be drained-and-closed
					// each iteration so a miss can't carry over to the next test.
					// attachMockErrors (GetMockErrors -> result + summary store) is
					// the single source of unmatched outgoing calls across all
					// transports — native and k8s alike reach the agent over HTTP,
					// so the agent's error channel is consumed only inside the
					// agent's own drain goroutine, never by the replayer.
					r.attachMockErrors(runTestSetCtx, testSetID, testCase.Name, testCaseResult)
					// Build the {expected, actual} mock set for THIS test case.
					// See buildExpectedMockInfos / buildActualMockInfos at the
					// bottom of this file for DNS-filter + perTestConsumed-known
					// semantics. Both extracted as helpers for unit testability.
					expectedMockInfos := buildExpectedMockInfos(expectedMocks, mockKindByName)
					actualMockInfos := buildActualMockInfos(perTestConsumed, perTestConsumedKnown)

					// TestResult.MockMismatches: populated for tests going
					// through this (non-streaming) replay path — regardless of
					// pass/fail or obsolescence. Mirrors the sandbox integration
					// runner's behaviour so report UIs can render expected vs
					// actual mocks for passing tests too, not just the
					// obsolete-mismatch path. The deferred streaming-test path
					// (Phase 2 stream replay) does NOT populate this field
					// today — consumers should treat absence as "data not
					// available for this run mode", not "no mocks". Skip when
					// both sides are empty so the json/yaml field stays absent
					// via omitempty. Also skip when perTestConsumed is unknown
					// (non-instrument GetConsumedMocks failed) so we don't
					// emit ActualMocks built from stale loop-scoped data.
					if perTestConsumedKnown && (len(expectedMockInfos) > 0 || len(actualMockInfos) > 0) {
						testCaseResult.MockMismatches = &models.MockMismatchInfo{
							ExpectedMocks: expectedMockInfos,
							ActualMocks:   actualMockInfos,
						}
					}

					// Existing FailureInfo.MockMismatch semantics preserved:
					// only set when the test became OBSOLETE due to a real
					// mock-pool divergence. Downstream code that branches on
					// "is this an obsolete-mismatch?" keeps working.
					if mockSetMismatch && testStatus == models.TestStatusObsolete {
						testCaseResult.FailureInfo.MockMismatch = &models.MockMismatchInfo{
							ExpectedMocks: expectedMockInfos,
							ActualMocks:   actualMockInfos,
						}
					}
					finalTestCaseResults[testCase.Name] = testCaseResult
				} else {
					utils.LogError(r.logger, nil, "test case result is nil")
					// Finalize the capture window and persist a failed result so a
					// miss during this test attaches here instead of leaking into a
					// later test (or vanishing) when we bail out on this internal error.
					failedResult := r.CreateFailedTestResult(testCase, testSetID, started, "internal error: test case result is nil")
					r.attachMockErrors(runTestSetCtx, testSetID, testCase.Name, failedResult)
					if insErr := r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, failedResult); insErr != nil {
						utils.LogError(r.logger, insErr, "failed to insert failed test case result for nil test case result")
					}
					// Outcome was already counted at lines ~1850-1864; don't double-count here.
					break
				}
			} else {
				utils.LogError(r.logger, nil, "test result is nil")
				// Matcher returned no result (e.g. a comparison path returning
				// (false, nil)). Same as above: finalize the window + persist a
				// failed result so the miss surfaces and can't carry forward.
				failedResult := r.CreateFailedTestResult(testCase, testSetID, started, "internal error: comparison returned no result")
				r.attachMockErrors(runTestSetCtx, testSetID, testCase.Name, failedResult)
				if insErr := r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, failedResult); insErr != nil {
					utils.LogError(r.logger, insErr, "failed to insert failed test case result for nil comparison result")
				}
				// Outcome was already counted at lines ~1850-1864; don't double-count here.
				break
			}

			// We need to sleep for a second to avoid mismatching of mocks during keploy testing via test-bench
			if r.config.EnableTesting {
				r.logger.Debug("sleeping for a second to avoid mismatching of mocks during keploy testing via test-bench")
				time.Sleep(time.Second)
			}
		}
		failure += currentFailures
		success = currentSuccess
		obsolete += currentObsolete
		if currentFailures == 0 || replay == 2 {
			success = currentSuccess
			for k, v := range currentPassingMocks {
				passingTotalConsumedMocks[k] = v
			}
			break
		}

		// Otherwise, set up the passing tests to run again in the next replay cycle
		testsToRun = nextTestsToRun
		r.logger.Info("Retrying passing test cases to validate mock consistency", zap.Int("replay", replay+1), zap.Int("remaining_tests", len(testsToRun)))
	}
	for _, tcResult := range finalTestCaseResults {
		insertStart := time.Now()
		err := r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, tcResult)
		if time.Since(insertStart) > 50*time.Millisecond {
			r.logger.Debug("Slow InsertTestCaseResult", zap.Duration("duration", time.Since(insertStart)))
		}
		if err != nil {
			utils.LogError(r.logger, err, "failed to insert final test case result")
			testSetStatus = models.TestSetStatusInternalErr
		}
	}

	if loopErr != nil {
		runTestSetCtxCancel()
	}

	// ====== Phase 2: Execute deferred streaming tests sequentially ======
	// Only run Phase 2 if Phase 1 completed without fatal errors and there are deferred tests.
	if loopErr == nil && !exitLoop && len(streamingTests) > 0 {
		r.logger.Info("Now executing streaming tests",
			zap.String("testset", testSetID),
			zap.Int("count", len(streamingTests)))

		for i, deferred := range streamingTests {
			tc := deferred.testCase

			// Set isLastTestCase on the last deferred streaming test: it is the true last
			// test to execute in the entire run, so finalization must trigger here, not in Phase 1.
			if i == len(streamingTests)-1 && r.isLastTestSet {
				r.isLastTestCase = true
				tc.IsLast = true
			}

			// Check for exit signals before each streaming test
			select {
			case <-exitLoopChan:
				testSetStatus = testSetStatusByErrChan
				exitLoop = true
			default:
			}
			if exitLoop {
				break
			}

			// Run pre-test mutation before computing the mock window so that
			// any timestamp fields decrypted by the mutator feed into the filter.
			// Streaming Phase 2 has no retry loop so the replay==0 guard used
			// in Phase 1 is not needed here — each tc is executed exactly once.
			if mutator, ok := r.hookImpl.(TestCaseMutator); ok {
				if err := mutator.BeforeTestCaseRun(runTestSetCtx, tc, testSetID); err != nil {
					utils.LogError(r.logger, err, "BeforeTestCaseRun hook failed; replay continues with test case in current state",
						zap.String("testcase", tc.Name),
						zap.String("next_step", "check the BeforeTestCaseRun implementation and any external dependencies it uses (e.g. KMS, auth, network)"))
				}
			}

			// Mock Window: Calculate the effective mock filter window for streaming
			// using the request timestamp to the response timestamp plus a timeout buffer.
			streamReqTime, streamRespTime := effectiveStreamMockWindow(tc, r.config.Test.APITimeout)
			err = r.SendMockFilterParamsToAgent(runTestSetCtx, deferred.expectedMocks, streamReqTime, streamRespTime, totalConsumedMocks, useMappingBased)
			if err != nil {
				utils.LogError(r.logger, err, "failed to update mock parameters for streaming test")
				loopErr = err
				break
			}

			// Open the per-test capture window before simulation. Unlike the
			// non-streaming path we must NOT finalize it right after
			// SimulateRequest (which returns at response headers) — outgoing mock
			// calls keep happening while CompareHTTPStream consumes the stream
			// body below. Every exit path therefore calls attachMockErrors only
			// AFTER stream consumption has finished.
			r.beginTestErrorCapture(runTestSetCtx)

			// Execute: SimulateRequest returns once response headers arrive;
			// for streaming cases the body reader is drained later by
			// CompareHTTPStream below, so in-stream mock consumption can
			// continue after this call returns.
			started := time.Now().UTC()
			resp, simErr := r.hookImpl.SimulateRequest(runTestSetCtx, tc, testSetID)

			// Mirror the non-streaming reset-resend: a docker userland-proxy reset on
			// a freshly-accepted host-port conn under CI load never reached the app
			// (zero mocks consumed), so re-send rather than synthesize a false got=0
			// failure. retryResetOnce returns a fresh streaming response for a
			// streaming tc; on the unsafe refusal path we fold its drained mocks into
			// totalConsumedMocks identically to the non-streaming loop.
			if simErr != nil && pkg.IsTransportConnReset(simErr) {
				retryResp, retried, drainedConsumed := r.retryResetOnce(runTestSetCtx, tc, testSetID, simErr)
				if retried {
					resp, simErr = retryResp, nil
				} else {
					for _, m := range drainedConsumed {
						totalConsumedMocks[m.Name] = m
					}
				}
			}

			if simErr != nil {
				utils.LogError(r.logger, simErr, "failed to simulate streaming request")
				failure++
				testSetStatus = models.TestSetStatusFailed
				testCaseResult := r.CreateFailedTestResult(tc, testSetID, started, simErr.Error())
				r.attachMockErrors(runTestSetCtx, testSetID, tc.Name, testCaseResult)
				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, "failed to insert streaming test case result for simulation error")
					break
				}
				continue
			}

			httpResp, ok := resp.(*models.HTTPResp)
			var hadStreamingMismatch bool              // Track if streaming comparison failed
			var streamMismatch *pkg.StreamMismatchInfo // Detailed mismatch info for body result

			// Pre-stream drain + mismatch snapshot: needed to decide whether
			// CompareHTTPStream below should emit failure logs. The mismatch
			// is re-evaluated after the post-stream drain so extra mocks
			// consumed while reading the stream body are counted too.
			mockNames := make([]string, 0)
			if r.instrument {
				consumedMocks, err = r.hookImpl.GetConsumedMocks(runTestSetCtx)
				if err != nil {
					utils.LogError(r.logger, err, "failed to get consumed filtered mocks for streaming test")
				} else {
					for _, m := range consumedMocks {
						totalConsumedMocks[m.Name] = m
						mockNames = append(mockNames, m.Name)
					}
				}
			}

			expectedMocks := deferred.expectedMocks
			mockSetMismatch := false
			hasExpectedMocks := len(expectedMocks) > 0
			if r.instrument && useMappingBased && isMappingEnabled && hasExpectedMocks {
				mockSetMismatch = !isMockSubsetWithConfig(consumedMocks, expectedMocks)
			}
			emitFailureLogs := !mockSetMismatch

			if !ok {
				// Handle streaming response type
				streamResp, streamOk := resp.(*pkg.StreamingHTTPResponse)
				if streamOk {
					noiseConfig := r.config.Test.GlobalNoise.Global
					if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
						noiseConfig = LeftJoinNoise(r.config.Test.GlobalNoise.Global, tsNoise)
					}
					streamBodyNoise := map[string][]string{}
					if bodyNoise, ok := noiseConfig["body"]; ok {
						streamBodyNoise = cloneNoiseMap(bodyNoise)
					}
					jsonNoiseKeys := pkg.CollectStreamingGlobalNoiseKeys(streamBodyNoise, tc.Noise)

					streamMatched, capturedBody, streamMismatchInfo, streamErr := pkg.CompareHTTPStream(tc.HTTPResp, streamResp.Reader, streamResp.StreamConfig, jsonNoiseKeys, r.logger)
					if closeErr := streamResp.Reader.Close(); closeErr != nil {
						r.logger.Debug("failed to close streaming response reader", zap.Error(closeErr))
					}

					if streamErr != nil {
						r.logger.Error("failed to read streaming response", zap.Error(streamErr))
						failure++
						testSetStatus = models.TestSetStatusFailed
						testCaseResult := r.CreateFailedTestResult(tc, testSetID, started, streamErr.Error())
						r.attachMockErrors(runTestSetCtx, testSetID, tc.Name, testCaseResult)
						loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
						if loopErr != nil {
							utils.LogError(r.logger, loopErr, "failed to save streaming test result")
							break
						}
						continue
					}

					if !streamMatched {
						r.logger.Error("streaming response mismatch detected", zap.String("testcase", tc.Name))
						hadStreamingMismatch = true
						streamMismatch = streamMismatchInfo
						// Suppress HTTP matcher logs since we detected streaming mismatch
						emitFailureLogs = false
						// Create HTTPResp with captured body for proper diff display
						httpResp = &models.HTTPResp{
							StatusCode:    streamResp.StatusCode,
							StatusMessage: streamResp.StatusMessage,
							Body:          capturedBody,
							Header:        streamResp.Header,
						}
						// Will be compared by CompareHTTPResp below
					} else {
						bodyForMatcher := capturedBody
						if streamMatched {
							bodyForMatcher = tc.HTTPResp.Body
						}

						httpResp = &models.HTTPResp{
							StatusCode:    streamResp.StatusCode,
							StatusMessage: streamResp.StatusMessage,
							Body:          bodyForMatcher,
							Header:        streamResp.Header,
						}
					}
				} else {
					errMsg := fmt.Sprintf("invalid response type for streaming HTTP test case, got %T", resp)
					r.logger.Error(errMsg)
					failure++
					testSetStatus = models.TestSetStatusFailed
					testCaseResult := r.CreateFailedTestResult(tc, testSetID, started, errMsg)
					r.attachMockErrors(runTestSetCtx, testSetID, tc.Name, testCaseResult)
					loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
					if loopErr != nil {
						utils.LogError(r.logger, loopErr, "failed to insert streaming test case result for type assertion error")
						break
					}
					continue
				}
			}

			// Drain mocks consumed during stream body transmission and union
			// with the pre-stream mocks captured earlier. GetConsumedMocks
			// drains on read, so if any mocks land between the pre-stream
			// drain and here (e.g., one backend call per SSE frame, consumed
			// while CompareHTTPStream was still reading frames above), they
			// would otherwise be dropped from mappings.yaml.
			if r.instrument {
				additionalMocks, drainErr := r.hookImpl.GetConsumedMocks(runTestSetCtx)
				if drainErr != nil {
					utils.LogError(r.logger, drainErr, "failed to get consumed filtered mocks for streaming test")
				}
				for _, m := range additionalMocks {
					totalConsumedMocks[m.Name] = m
					mockNames = append(mockNames, m.Name)
				}
				consumedMocks = append(consumedMocks, additionalMocks...)
			}

			// Re-evaluate mismatch now that consumedMocks covers the full
			// test case (pre-stream + in-stream). If CompareHTTPStream
			// already forced emitFailureLogs=false on a streaming body
			// mismatch, keep it suppressed — don't un-suppress just because
			// the mock set still looks like a subset.
			if r.instrument && useMappingBased && isMappingEnabled && hasExpectedMocks {
				mockSetMismatch = !isMockSubsetWithConfig(consumedMocks, expectedMocks)
				if !hadStreamingMismatch {
					emitFailureLogs = !mockSetMismatch
				}
			}

			testPass, testResult := r.CompareHTTPResp(tc, httpResp, testSetID, emitFailureLogs)
			if testResult == nil {
				// Matcher returned no result (e.g. an internal compare path returning
				// (false, nil)). Handle it HERE, before the hadStreamingMismatch block
				// below dereferences testResult.BodyResult. Finalize the window + persist
				// a failed result so the miss surfaces and can't carry forward.
				utils.LogError(r.logger, nil, "streaming test result is nil")
				failedResult := r.CreateFailedTestResult(tc, testSetID, started, "internal error: streaming comparison returned no result")
				r.attachMockErrors(runTestSetCtx, testSetID, tc.Name, failedResult)
				failure++
				testSetStatus = models.TestSetStatusFailed
				if insErr := r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, failedResult); insErr != nil {
					utils.LogError(r.logger, insErr, "failed to insert failed streaming test case result for nil result")
				}
				continue
			}
			// Override testPass if streaming comparison failed
			// (HTTP matcher skips body comparison for non-JSON bodies by default)
			if hadStreamingMismatch {
				testPass = false
				// Add body result showing the mismatched frame diff
				if streamMismatch != nil {
					expectedFrameDisplay := fmt.Sprintf("Frame %d: %s", streamMismatch.FrameIndex, streamMismatch.ExpectedFrame)
					actualFrameDisplay := fmt.Sprintf("Frame %d: %s", streamMismatch.FrameIndex, streamMismatch.ActualFrame)
					if streamMismatch.Reason != "" {
						actualFrameDisplay = fmt.Sprintf("Frame %d (%s): %s", streamMismatch.FrameIndex, streamMismatch.Reason, streamMismatch.ActualFrame)
					}
					testResult.BodyResult = append(testResult.BodyResult, models.BodyResult{
						Normal:   false,
						Type:     models.Plain,
						Expected: expectedFrameDisplay,
						Actual:   actualFrameDisplay,
					})
					// Display the streaming frame diff in same format as HTTP matcher
					logDiffs := matcherUtils.NewDiffsPrinter(tc.Name)
					logDiffs.PushBodyDiff(expectedFrameDisplay, actualFrameDisplay, nil)
					_ = logDiffs.Render()
				}
				// Log failure message in same format as HTTP matcher
				r.logger.Info("result", zap.String("testcase id", models.HighlightFailingString(tc.Name)), zap.String("testset id", models.HighlightFailingString(testSetID)), zap.String("passed", models.HighlightFailingString(false)))
				// Print user-facing failure message with red color on test case ID only (matching HTTP matcher style)
				fmt.Printf("\nTestrun failed for testcase with id: \"%s%s%s\"\n\n--------------------------------------------------------------------\n\n",
					"\033[91m", tc.Name, "\033[0m")
			}

			tcReqTimeStream, tcRespTimeStream := recordReqResTimestamps(tc)
			upsertActualTestMockMapping(actualTestMockMappings, tc.Name, consumedMocks, tcReqTimeStream, tcRespTimeStream)

			// Log consumed mocks for streaming test
			r.logger.Debug("consumed mocks for streaming test case",
				zap.String("testSetID", testSetID),
				zap.String("testCaseID", tc.Name),
				zap.Strings("mockNames", mockNames))

			if mockSetMismatch {
				if testPass {
					r.logger.Debug("mock mapping mismatch ignored because streaming testcase passed",
						zap.String("testcase", tc.Name),
						zap.String("testset", testSetID),
						zap.Strings("expectedMocks", expectedMocks),
						zap.Strings("actualMocks", mockNames))
				} else {
					r.logger.Error("mock mapping mismatch detected for streaming testcase; marking as obsolete",
						zap.String("testcase", tc.Name),
						zap.String("testset", testSetID),
						zap.Strings("expectedMocks", expectedMocks),
						zap.Strings("actualMocks", mockNames))
					r.mockMismatchFailures.AddFailure(testSetID, tc.Name, expectedMocks, mockNames)
				}
			}

			if !hadStreamingMismatch {
				if !testPass {
					r.logger.Info("result", zap.String("testcase id", models.HighlightFailingString(tc.Name)), zap.String("testset id", models.HighlightFailingString(testSetID)), zap.String("passed", models.HighlightFailingString(testPass)))
				} else {
					r.logger.Info("result", zap.String("testcase id", models.HighlightPassingString(tc.Name)), zap.String("testset id", models.HighlightPassingString(testSetID)), zap.String("passed", models.HighlightPassingString(testPass)))
				}
			}

			//  Record: Update counters and persist result.
			var testStatus models.TestStatus
			if testPass {
				testStatus = models.TestStatusPassed
				success++
				for _, m := range consumedMocks {
					passingTotalConsumedMocks[m.Name] = m
				}
			} else if mockSetMismatch && !r.config.Test.StrictFailure {
				testStatus = models.TestStatusObsolete
				obsolete++
			} else {
				testStatus = models.TestStatusFailed
				failure++
				testSetStatus = models.TestSetStatusFailed
			}

			if testResult != nil {
				testCaseResult := &models.TestResult{
					Kind:       models.HTTP,
					Name:       testSetID,
					Status:     testStatus,
					Started:    started.Unix(),
					Completed:  time.Now().UTC().Unix(),
					TestCaseID: tc.Name,
					Req: models.HTTPReq{
						Method:     tc.HTTPReq.Method,
						ProtoMajor: tc.HTTPReq.ProtoMajor,
						ProtoMinor: tc.HTTPReq.ProtoMinor,
						URL:        tc.HTTPReq.URL,
						URLParams:  tc.HTTPReq.URLParams,
						Header:     tc.HTTPReq.Header,
						Body:       tc.HTTPReq.Body,
						Binary:     tc.HTTPReq.Binary,
						Form:       tc.HTTPReq.Form,
						Timestamp:  tc.HTTPReq.Timestamp,
					},
					Res:          *httpResp,
					TestCasePath: filepath.Join(r.config.Path, testSetID),
					MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
					Noise:        tc.Noise,
					Result:       *testResult,
					TimeTaken:    time.Since(started).String(),
				}

				if testStatus == models.TestStatusFailed && testResult.FailureInfo.Risk != models.None {
					testCaseResult.FailureInfo.Risk = testResult.FailureInfo.Risk
					testCaseResult.FailureInfo.Category = testResult.FailureInfo.Category
				}

				// Finalize the capture window now - AFTER CompareHTTPStream and the
				// post-stream consumed-mock drain above, so in-stream mock misses
				// are included - and attach them to this result.
				r.attachMockErrors(runTestSetCtx, testSetID, tc.Name, testCaseResult)

				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, "failed to save streaming test result")
					break
				}
			} else {
				// Unreachable: a nil testResult is finalized right after CompareHTTPResp
				// above (persists a failed result and continues). Defensive only.
				utils.LogError(r.logger, nil, "streaming test result is nil")
				break
			}
		}
	}

	// Fold every mock consumed since the last rewind into the cross-cycle
	// union — the final main-loop cycle's mocks and the streaming Phase 2
	// mocks (Phase 2 appends to totalConsumedMocks and never rewinds).
	// Earlier retry cycles were already folded at each rewind above. This
	// must run after Phase 2's last write so the post-loop readers
	// (PersistMockNoise and the Mocks-Consumed telemetry) see the complete
	// union of everything consumed across all cycles and phases.
	for k, v := range totalConsumedMocks {
		allConsumedAcrossCycles[k] = v
	}

	timeTaken := time.Since(startTime)

	testCaseResults, err := r.reportDB.GetTestCaseResults(runTestSetCtx, testRunID, testSetID)
	if err != nil {
		if runTestSetCtx.Err() != context.Canceled {
			if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), err); ok {
				testSetStatus = resolvedStatus
			} else {
				utils.LogError(r.logger, err, "failed to get test case results")
				testSetStatus = models.TestSetStatusInternalErr
			}
		}
	}

	err = r.hookImpl.BeforeTestResult(ctx, testRunID, testSetID, testCaseResults)
	if err != nil {
		stopReason := fmt.Sprintf("failed to run before test result hook: %v", err)
		utils.LogError(r.logger, err, stopReason)
	}
	if conf.PostScript != "" {
		//Execute the Post-script after each test-set if provided
		r.logger.Info("Running Post-script", zap.String("script", conf.PostScript), zap.String("test-set", testSetID))
		err = r.executeScript(runTestSetCtx, conf.PostScript)
		if err != nil {
			return models.TestSetStatusFaultScript, fmt.Errorf("failed to execute post-script: %w", err)
		}
	}

	riskHigh, riskMed, riskLow := 0, 0, 0
	for _, tr := range testCaseResults {
		if tr.Status == models.TestStatusFailed && tr.Result.FailureInfo.Risk != models.None {
			switch tr.Result.FailureInfo.Risk {
			case models.High:
				riskHigh++
			case models.Medium:
				riskMed++
			case models.Low:
				riskLow++
			}
		}
	}

	// Checking errors for final iteration
	// Checking for errors in the loop
	if loopErr != nil && !errors.Is(loopErr, context.Canceled) {
		if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), loopErr); ok {
			testSetStatus = resolvedStatus
		} else {
			testSetStatus = models.TestSetStatusInternalErr
		}
	} else if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), nil); ok {
		testSetStatus = resolvedStatus
	}

	appFailure := getLastAppErr()
	appLogs := appFailure.AppLogs
	if !shouldIncludeAppLogs(testSetStatus) {
		appLogs = ""
	} else if appLogs == "" && testSetStatus == models.TestSetStatusAppHalted {
		logCtx, cancel := context.WithTimeout(context.WithoutCancel(runTestSetCtx), 10*time.Second)
		defer cancel()
		appLogs = r.instrumentation.GetRecentAppLogs(logCtx)
	}
	testReport = &models.TestReport{
		Version:       models.GetVersion(),
		TestSet:       testSetID,
		Status:        string(testSetStatus),
		Total:         testCasesCount,
		Success:       success,
		Failure:       failure,
		Obsolete:      obsolete,
		Ignored:       ignored,
		Tests:         testCaseResults,
		TimeTaken:     timeTaken.String(),
		FailureReason: describeTestSetFailure(testSetStatus, testCaseResults),
		AppLogs:       appLogs,
		HighRisk:      riskHigh,
		MediumRisk:    riskMed,
		LowRisk:       riskLow,
		CmdUsed:       r.config.Test.CmdUsed,
	}

	// final report should have reason for sudden stop of the test run so this should get canceled
	reportCtx := context.WithoutCancel(runTestSetCtx)
	err = r.reportDB.InsertReport(reportCtx, testRunID, testSetID, testReport)
	if err != nil {
		utils.LogError(r.logger, err, "failed to insert report")
		return models.TestSetStatusInternalErr, fmt.Errorf("failed to insert report")
	}

	err = utils.AddToGitIgnore(r.logger, r.config.Path, "/reports/")
	if err != nil {
		utils.LogError(r.logger, err, "failed to create .gitignore file")
	}

	// Preserve mocks and mappings for tests that were expected but not executed in this run
	if len(expectedTestMockMappings) > 0 {
		for expectedTestCaseID, expectedMocks := range expectedTestMockMappings {
			testExecuted := false
			for _, tc := range activeTestCases {
				if tc.Name == expectedTestCaseID {
					testExecuted = true
					break
				}
			}

			if !testExecuted {
				// Preserve its expected mocks so they aren't deleted by UpdateMocks
				for _, m := range expectedMocks {
					if _, exists := passingTotalConsumedMocks[m.Name]; !exists {
						passingTotalConsumedMocks[m.Name] = models.MockState{
							Name: m.Name,
							Kind: models.Kind(m.Kind),
						}
					}
				}
			}
		}
	}

	// Remove the mocks no test case consumed (when no base path is provided).
	//
	// Pruning keeps what the tests consumed and deletes the rest, so it is only
	// sound when a mock going unconsumed actually means the app didn't need it. A
	// run whose app could not reach its dependencies fails every test and consumes
	// nothing — pruning that would delete the recording's mocks because of an
	// infrastructure fault. shouldPrune holds them; see shouldSkipPruning for the
	// full set of reasons.
	pruneEnabled := r.config.Test.RemoveUnusedMocks && r.instrument
	prune := shouldPrune(r.config.Test.RemoveUnusedMocks, r.instrument, success, failure, obsolete,
		r.config.Test.PreserveFailedMocks, testCaseResults)
	if pruneEnabled && !prune {
		r.logger.Warn("skipping mock pruning: this run's consumed-mock set is not trustworthy enough to delete against, so recorded mocks are preserved",
			zap.String("testSetID", testSetID),
			zap.Int("passed", success),
			zap.Int("failed", failure),
			zap.Bool("appUnreachable", anyAppConnectionError(testCaseResults)))
	}
	if prune {
		noisyTestCases := r.hookImpl.GetNoisyTestCaseNames(testSetID)
		if len(noisyTestCases) > 0 {
			added := retainNoisyTestCaseMocks(noisyTestCases, actualTestMockMappings, passingTotalConsumedMocks)
			if added > 0 {
				r.logger.Debug("preserved mocks used by noisy testcases from pruning",
					zap.String("testSetID", testSetID),
					zap.Int("noisyTestCaseCount", len(noisyTestCases)),
					zap.Int("additionalMocksKept", added))
			}
		}

		// Compute the startup-mock cutoff so UpdateMocks can exempt startup/init
		// mocks from deletion. "Startup mocks" are every mock recorded from app
		// boot up to and including the StartupMockTestCaseWindow-th test case;
		// mocks recorded before the cutoff survive pruning while still keeping
		// their per-test mappings for replay matching. With <= window test cases
		// the whole recording is startup, so pruneBefore is used as the cutoff to
		// retain everything (see startupMockCutoff).
		startupCutoffTime := startupMockCutoff(testCases, pruneBefore)

		err = r.mockDB.UpdateMocks(runTestSetCtx, testSetID, passingTotalConsumedMocks, pruneBefore, startupCutoffTime)
		if err != nil {
			utils.LogError(r.logger, err, "failed to delete unused mocks")
		}
	} else if r.config.Test.SchemaNoiseDetection && r.instrument {
		// --schema-noise-detection without --remove-unused-mocks: the learned
		// req_body_noise used to ride only inside UpdateMocks (the pruning
		// path), so detection alone learned noise and threw it away at exit.
		// Persist it through the prune-free path instead. We persist noise
		// from ALL consumed mocks (not just passing tests): the mock matched,
		// so the request-side drift it learned is valid even when the test
		// later failed on its response.
		type mockNoisePersister interface {
			PersistMockNoise(ctx context.Context, testSetID string, mockStates map[string]models.MockState) error
		}
		if p, ok := r.mockDB.(mockNoisePersister); ok {
			if err := p.PersistMockNoise(runTestSetCtx, testSetID, allConsumedAcrossCycles); err != nil {
				utils.LogError(r.logger, err, "failed to persist learned request-body noise onto mocks")
			}
		} else {
			r.logger.Debug("mockDB implementation does not support prune-free noise persistence; learned req_body_noise not persisted")
		}
	}

	// Test-mode mapping write semantics:
	//
	//   UpdateTestMapping=true  → always write/merge mappings.yaml
	//                             (operator-driven update).
	//   UpdateTestMapping=false → create-if-not-present: write only
	//                             when mappings.yaml is absent for
	//                             this test-set AND we have at least
	//                             one populated test→mocks entry.
	//                             Once a file exists, we leave it
	//                             alone — repeat test runs don't
	//                             churn it.
	//
	// Rationale: mappings.yaml is the artifact k8s-proxy autoreplay
	// (and other final-candidate analyses) consults to find which
	// mocks each test consumed. Operators who never set the flag
	// still need a usable file the first time test mode runs; gating
	// strictly on UpdateTestMapping leaves them without one. The
	// create-if-not-present default closes that gap without changing
	// behaviour for existing users who already have a mappings.yaml
	// they don't want overwritten — they can keep flag=false and the
	// existing file is preserved. To force a refresh, flag=true.
	//
	// Independence from DisableMapping: the two flags express
	// orthogonal concerns. DisableMapping picks the replay-time mock
	// FILTERING strategy (timestamp-vs-index); the mapping WRITE is
	// a separate side-effect that records consumption. We don't gate
	// the write on the filter strategy.
	//
	// The non-emptiness guard on the create-if-not-present branch
	// matters because actualTestMockMappings is populated by
	// upsertActualTestMockMapping calls that depend on consumedMocks,
	// which is fetched from r.hookImpl.GetConsumedMocks INSIDE
	// `if r.instrument` blocks (lines 1453, 1884). In non-instrument
	// modes (e.g. k8s-proxy autoreplay) consumedMocks stays empty,
	// so without this guard the first run would create an empty
	// mappings.yaml and every subsequent run would skip the create
	// branch (file exists), leaving autoreplay permanently without
	// the mappings the feature relies on. UpdateTestMapping=true
	// still writes an empty file when explicitly requested — that
	// matches the operator intent of "force a refresh".
	shouldWriteMappings := r.config.Test.UpdateTestMapping
	if !shouldWriteMappings && r.mappingDB != nil && len(actualTestMockMappings.TestCases) > 0 {
		exists, existsErr := r.mappingDB.Exists(ctx, testSetID)
		if existsErr != nil {
			r.logger.Debug("Skipping create-if-not-present mappings.yaml write — file-existence check failed; treating as 'exists' to avoid clobbering",
				zap.String("testSetID", testSetID),
				zap.Error(existsErr))
		} else if !exists {
			shouldWriteMappings = true
		}
	}
	if shouldWriteMappings {
		if err := r.StoreMappings(ctx, actualTestMockMappings); err != nil {
			r.logger.Error("Error saving test-mock mappings to YAML file", zap.Error(err))
		} else {
			r.logger.Info("Successfully saved test-mock mappings",
				zap.String("testSetID", testSetID),
				zap.Int("numTests", len(actualTestMockMappings.TestCases)))
		}
	}

	// TODO Need to decide on whether to use global variable or not
	verdict := TestReportVerdict{
		total:     testReport.Total,
		failed:    testReport.Failure,
		passed:    testReport.Success,
		obsolete:  testReport.Obsolete,
		ignored:   testReport.Ignored,
		status:    testSetStatus == models.TestSetStatusPassed,
		duration:  timeTaken,
		timeTaken: timeTaken.String(),
	}

	r.completeTestReportMu.Lock()
	r.completeTestReport[testSetID] = verdict
	r.totalTests += testReport.Total
	r.totalTestPassed += testReport.Success
	r.totalTestFailed += testReport.Failure
	r.totalTestObsolete += testReport.Obsolete
	r.totalTestIgnored += testReport.Ignored
	r.totalTestTimeTaken += timeTaken
	r.completeTestReportMu.Unlock()

	timeTakenStr := timeWithUnits(timeTaken)

	if testSetStatus == models.TestSetStatusFailed || testSetStatus == models.TestSetStatusPassed {
		if r.config.JSONOutput {
			if err := utils.NewJSONWriter(true).Write(testReport); err != nil {
				utils.LogError(r.logger, err, "failed to print json testrun summary")
			}
		} else {

			if !r.config.DisableANSI {
				if testSetStatus == models.TestSetStatusFailed {
					pp.SetColorScheme(models.GetFailingColorScheme())
				} else {
					pp.SetColorScheme(models.GetPassingColorScheme())
				}

				summaryFormat := "\n <=========================================> \n" +
					"  TESTRUN SUMMARY. For test-set: %s\n" +
					"\tTotal tests:        %s\n" +
					"\tTotal test passed:  %s\n" +
					"\tTotal test failed:  %s\n"

				args := []interface{}{testReport.TestSet, testReport.Total, testReport.Success, testReport.Failure}
				if testReport.Obsolete > 0 {
					summaryFormat += "\tTotal test obsolete: %s\n"
					args = append(args, testReport.Obsolete)
				}

				if testReport.Ignored > 0 {
					summaryFormat += "\tTotal test ignored: %s\n"
					args = append(args, testReport.Ignored)
				}

				summaryFormat += "\tTime Taken:         %s\n <=========================================> \n\n"
				args = append(args, timeTakenStr)

				if _, err := pp.Printf(summaryFormat, args...); err != nil {
					utils.LogError(r.logger, err, "failed to print testrun summary")
				}

			} else {
				var sb strings.Builder
				sb.WriteString("\n <=========================================> \n")
				sb.WriteString(fmt.Sprintf("  TESTRUN SUMMARY. For test-set: %s\n", testReport.TestSet))
				sb.WriteString(fmt.Sprintf("\tTotal tests:        %d\n", testReport.Total))
				sb.WriteString(fmt.Sprintf("\tTotal test passed:  %d\n", testReport.Success))
				sb.WriteString(fmt.Sprintf("\tTotal test failed:  %d\n", testReport.Failure))
				if testReport.Obsolete > 0 {
					sb.WriteString(fmt.Sprintf("\tTotal test obsolete: %d\n", testReport.Obsolete))
				}

				if testReport.Ignored > 0 {
					sb.WriteString(fmt.Sprintf("\tTotal test ignored: %d\n", testReport.Ignored))
				}

				sb.WriteString(fmt.Sprintf("\tTime Taken:         %s\n", timeTakenStr))
				sb.WriteString(" <=========================================> \n\n")

				fmt.Print(sb.String())
			}
		}
	}

	r.telemetry.TestSetRun(testReport.Success, testReport.Failure, testSetID, string(testSetStatus))

	if r.config.Test.UpdateTemplate || r.config.Test.BasePath != "" {
		utils.RemoveDoubleQuotes(utils.TemplatizedValues) // Write the templatized values to the yaml.
		if len(utils.TemplatizedValues) > 0 {
			err = r.testSetConf.Write(ctx, testSetID, &models.TestSet{
				PreScript:  conf.PreScript,
				PostScript: conf.PostScript,
				Template:   utils.TemplatizedValues,
			})
			if err != nil {
				utils.LogError(r.logger, err, "failed to write the templatized values to the yaml")
			}
		}
	}

	// In Docker Compose mode, RunTestSet's defer block stops the agent container.
	// We must call AfterTestRun HERE (before defer fires) while the agent is alive.
	if r.isLastTestSet && r.instrument && cmdType == utils.DockerCompose {
		r.afterTestRunCalled = true
		if hookErr := r.hookImpl.AfterTestRun(ctx, r.testRunID, r.testRunTestSets, models.TestCoverage{}); hookErr != nil {
			utils.LogError(r.logger, hookErr, "failed to execute after test run hook")
		}
	}

	// Merge mock NAMES (not the count) into the run-level distinct set so
	// duplicates across test sets aren't double-counted. allConsumedAcrossCycles
	// is the cross-cycle union (folded at each rewind and once after the loop),
	// so names consumed only in an earlier retry cycle aren't lost from the
	// Mocks-Consumed telemetry.
	r.completeTestReportMu.Lock()
	for name := range allConsumedAcrossCycles {
		r.consumedMockNames[name] = struct{}{}
	}
	r.completeTestReportMu.Unlock()

	return testSetStatus, nil
}

func (r *Replayer) GetMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) (filtered, unfiltered []*models.Mock, err error) {
	filtered, err = r.mockDB.GetFilteredMocks(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		utils.LogError(r.logger, err, "failed to get filtered mocks")
		return nil, nil, err
	}
	unfiltered, err = r.mockDB.GetUnFilteredMocks(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed)
	if err != nil {
		utils.LogError(r.logger, err, "failed to get unfiltered mocks")
		return nil, nil, err
	}
	return filtered, unfiltered, err
}

// SendMockFilterParamsToAgent sends filtering parameters to agent instead of sending filtered mocks
func (r *Replayer) SendMockFilterParamsToAgent(ctx context.Context, expectedMockMapping []string, afterTime, beforeTime time.Time, totalConsumedMocks map[string]models.MockState, useMappingBased bool) error {
	if !r.instrument {
		r.logger.Debug("Keploy will not filter and set mocks when base path is provided", zap.String("base path", r.config.Test.BasePath))
		return nil
	}

	// Build filter parameters. Default to strict=true when r.config is
	// nil (unit tests, embedders) — matches the shipped default in
	// config.Test.StrictMockWindow. The env override applies at the
	// agent so KEPLOY_STRICT_MOCK_WINDOW=0 still opts out without
	// touching code.
	strictMockWindow := true
	if r.config != nil {
		strictMockWindow = r.config.Test.StrictMockWindow
	}
	params := models.MockFilterParams{
		AfterTime:          afterTime,
		BeforeTime:         beforeTime,
		MockMapping:        expectedMockMapping,
		UseMappingBased:    useMappingBased,
		TotalConsumedMocks: totalConsumedMocks,
		StrictMockWindow:   strictMockWindow,
	}

	// Send parameters to agent for filtering and mock updates
	err := r.instrumentation.UpdateMockParams(ctx, params)
	if err != nil {
		utils.LogError(r.logger, err, "failed to update mock parameters on agent")
		return err
	}

	r.logger.Debug("Successfully sent mock filter parameters to agent",
		zap.Bool("useMappingBased", useMappingBased),
		zap.Int("mockMappingCount", len(expectedMockMapping)))

	return nil
}

func (r *Replayer) GetTestSetStatus(ctx context.Context, testRunID string, testSetID string) (models.TestSetStatus, error) {
	testReport, err := r.reportDB.GetReport(ctx, testRunID, testSetID)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to get report: %w", err)
	}
	status, err := models.StringToTestSetStatus(testReport.Status)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to convert string to test set status: %w", err)
	}
	return status, nil
}

func (r *Replayer) CompareHTTPResp(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string, emitFailureLogs bool) (bool, *models.Result) {
	noiseConfig := r.httpNoiseConfig(testSetID)
	originalBodySize := originalHTTPRespBodySize(tc, actualResponse)

	if r.config.Test.SchemaMatch {
		pass, result := httpMatcher.MatchSchema(tc, actualResponse, r.logger)
		normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)
		return pass, result
	}

	pass, result := httpMatcher.Match(tc, actualResponse, noiseConfig, r.config.Test.IgnoreOrdering, r.config.Test.CompareAll, r.logger, emitFailureLogs)
	normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)

	return pass, result
}

func (r *Replayer) compareHTTPRespForReplay(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string, emitFailureLogs bool) (bool, *models.Result) {
	noiseConfig := r.httpNoiseConfig(testSetID)
	originalBodySize := originalHTTPRespBodySize(tc, actualResponse)

	if r.config.Test.SchemaMatch {
		pass, result := httpMatcher.MatchSchema(tc, actualResponse, r.logger)
		normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)
		return pass, result
	}

	if emitFailureLogs {
		pass, result := httpMatcher.Match(tc, cloneHTTPResp(actualResponse), noiseConfig, r.config.Test.IgnoreOrdering, r.config.Test.CompareAll, r.logger, false)
		if !pass && r.autoPassHTTPResponseSchemaAddition(tc, actualResponse, testSetID, noiseConfig, result) {
			normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)
			return true, result
		}
		if pass {
			normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)
			return pass, result
		}
	}

	pass, result := httpMatcher.Match(tc, actualResponse, noiseConfig, r.config.Test.IgnoreOrdering, r.config.Test.CompareAll, r.logger, emitFailureLogs)
	normalizeHTTPRespForReport(tc, actualResponse, originalBodySize)
	if !pass && r.autoPassHTTPResponseSchemaAddition(tc, actualResponse, testSetID, noiseConfig, result) {
		return true, result
	}

	return pass, result
}

func (r *Replayer) httpNoiseConfig(testSetID string) map[string]map[string][]string {
	noiseConfig := deepCopyNoiseConfig(r.config.Test.GlobalNoise.Global)
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
		noiseConfig = LeftJoinNoise(noiseConfig, tsNoise)
	}

	return noiseConfig
}

func deepCopyNoiseConfig(src map[string]map[string][]string) map[string]map[string][]string {
	if src == nil {
		return nil
	}

	dst := make(map[string]map[string][]string, len(src))
	for key, inner := range src {
		if inner == nil {
			dst[key] = nil
			continue
		}

		copiedInner := make(map[string][]string, len(inner))
		for innerKey, values := range inner {
			if values == nil {
				copiedInner[innerKey] = nil
				continue
			}

			copiedInner[innerKey] = append([]string(nil), values...)
		}

		dst[key] = copiedInner
	}

	return dst
}

func cloneHTTPResp(resp *models.HTTPResp) *models.HTTPResp {
	if resp == nil {
		return nil
	}

	clone := *resp
	if resp.Header != nil {
		clone.Header = make(map[string]string, len(resp.Header))
		for key, value := range resp.Header {
			clone.Header[key] = value
		}
	}

	return &clone
}

func originalHTTPRespBodySize(tc *models.TestCase, actualResponse *models.HTTPResp) int64 {
	if tc == nil || actualResponse == nil || !tc.HTTPResp.BodySkipped {
		return 0
	}

	return int64(len(actualResponse.Body))
}

func normalizeHTTPRespForReport(tc *models.TestCase, actualResponse *models.HTTPResp, originalBodySize int64) {
	if tc == nil || actualResponse == nil || !tc.HTTPResp.BodySkipped {
		return
	}

	actualResponse.BodySize = originalBodySize
	actualResponse.BodySkipped = true
	actualResponse.Body = ""
}

func (r *Replayer) autoPassHTTPResponseSchemaAddition(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string, noiseConfig map[string]map[string][]string, result *models.Result) bool {
	if !qualifiesForHTTPResponseSchemaAdditionPass(result) {
		return false
	}

	assessment, assessmentErr := matcherUtils.ComputeFailureAssessmentJSON(
		tc.HTTPResp.Body,
		actualResponse.Body,
		bodyNoiseForTestCase(tc.Noise, noiseConfig),
		r.config.Test.IgnoreOrdering,
	)

	fields := []zap.Field{
		zap.String("protocol", "http"),
		zap.String("testcase", tc.Name),
		zap.String("testset", testSetID),
	}
	if assessment != nil {
		if len(assessment.AddedFields) > 0 {
			fields = append(fields, zap.Strings("added_fields", assessment.AddedFields))
		}
		if len(assessment.Reasons) > 0 {
			fields = append(fields, zap.Strings("assessment_reasons", assessment.Reasons))
		}
	}
	if assessmentErr != nil {
		fields = append(fields, zap.NamedError("assessment_error", assessmentErr))
	}

	r.logger.Info(
		"Additive response schema change detected. Passing testcase by default because only new response fields were added. Verify downstream consumers are not affected by the schema expansion.",
		fields...,
	)

	normalizePassingHTTPResult(result)
	return true
}

func qualifiesForHTTPResponseSchemaAdditionPass(result *models.Result) bool {
	if result == nil {
		return false
	}

	return (result.FailureInfo.Risk == models.Low &&
		hasOnlyFailureCategories(result.FailureInfo.Category, models.SchemaAdded)) ||
		hasOnlySchemaAdditionAndContentLengthDiff(result)
}

func hasOnlyFailureCategories(categories []models.FailureCategory, allowed ...models.FailureCategory) bool {
	if len(categories) == 0 {
		return false
	}

	allowedSet := make(map[models.FailureCategory]struct{}, len(allowed))
	for _, category := range allowed {
		allowedSet[category] = struct{}{}
	}

	for _, category := range categories {
		if _, ok := allowedSet[category]; !ok {
			return false
		}
	}

	return true
}

func hasOnlySchemaAdditionAndContentLengthDiff(result *models.Result) bool {
	if !hasOnlyFailureCategories(result.FailureInfo.Category, models.SchemaAdded, models.HeaderChanged) {
		return false
	}

	hasSchemaAdded := false
	hasHeaderChanged := false
	for _, category := range result.FailureInfo.Category {
		hasSchemaAdded = hasSchemaAdded || category == models.SchemaAdded
		hasHeaderChanged = hasHeaderChanged || category == models.HeaderChanged
	}

	return hasSchemaAdded && hasHeaderChanged && hasOnlyContentLengthDiff(result.HeadersResult)
}

func hasOnlyContentLengthDiff(headers []models.HeaderResult) bool {
	foundDiff := false

	for _, header := range headers {
		if header.Normal {
			continue
		}

		foundDiff = true
		key := strings.ToLower(header.Expected.Key)
		if key == "" {
			key = strings.ToLower(header.Actual.Key)
		}
		if key != "content-length" {
			return false
		}
		if len(header.Expected.Value) == 0 || len(header.Actual.Value) == 0 {
			return false
		}
	}

	return foundDiff
}

func normalizePassingHTTPResult(result *models.Result) {
	if result == nil {
		return
	}

	result.FailureInfo = models.FailureInfo{}
	result.StatusCode.Normal = true
	result.BodySizeResult.Normal = true

	for i := range result.HeadersResult {
		result.HeadersResult[i].Normal = true
	}
	for i := range result.BodyResult {
		result.BodyResult[i].Normal = true
	}
	for i := range result.TrailerResult {
		result.TrailerResult[i].Normal = true
	}
}

func bodyNoiseForTestCase(testCaseNoise map[string][]string, noiseConfig map[string]map[string][]string) map[string][]string {
	bodyNoise := cloneNoiseMap(noiseConfig["body"])

	for field, regexArr := range testCaseNoise {
		parts := strings.Split(field, ".")
		if len(parts) <= 1 || parts[0] != "body" {
			continue
		}

		bodyNoise[strings.ToLower(strings.Join(parts[1:], "."))] = append([]string(nil), regexArr...)
	}

	return bodyNoise
}

func cloneNoiseMap(input map[string][]string) map[string][]string {
	if len(input) == 0 {
		return map[string][]string{}
	}

	out := make(map[string][]string, len(input))
	for key, values := range input {
		out[key] = append([]string(nil), values...)
	}

	return out
}

func (r *Replayer) CompareGRPCResp(tc *models.TestCase, actualResp *models.GrpcResp, testSetID string, emitFailureLogs bool) (bool, *models.Result) {
	noiseConfig := r.config.Test.GlobalNoise.Global
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
		noiseConfig = LeftJoinNoise(r.config.Test.GlobalNoise.Global, tsNoise)
	}

	return grpcMatcher.Match(tc, actualResp, noiseConfig, r.config.Test.IgnoreOrdering, r.logger, emitFailureLogs)
}

func (r *Replayer) printSummary(_ context.Context, _ bool) {
	if r.config.JSONOutput {
		return
	}

	r.completeTestReportMu.RLock()
	totalTestsSnapshot := r.totalTests
	totalTestPassedSnapshot := r.totalTestPassed
	totalTestFailedSnapshot := r.totalTestFailed
	totalTestObsoleteSnapshot := r.totalTestObsolete
	totalTestIgnoredSnapshot := r.totalTestIgnored
	totalTestTimeTakenSnapshot := r.totalTestTimeTaken
	reportSnapshot := make(map[string]TestReportVerdict, len(r.completeTestReport))
	for key, val := range r.completeTestReport {
		reportSnapshot[key] = val
	}
	r.completeTestReportMu.RUnlock()

	if totalTestsSnapshot > 0 {
		testSuiteNames := make([]string, 0, len(reportSnapshot))
		for testSuiteName := range reportSnapshot {
			testSuiteNames = append(testSuiteNames, testSuiteName)
		}
		sort.SliceStable(testSuiteNames, func(i, j int) bool {
			testSuitePartsI := strings.Split(testSuiteNames[i], "-")
			testSuitePartsJ := strings.Split(testSuiteNames[j], "-")
			if len(testSuitePartsI) < 3 || len(testSuitePartsJ) < 3 {
				return testSuiteNames[i] < testSuiteNames[j]
			}
			testSuiteIDNumberI, err1 := strconv.Atoi(testSuitePartsI[2])
			testSuiteIDNumberJ, err2 := strconv.Atoi(testSuitePartsJ[2])
			if err1 != nil || err2 != nil {
				return false
			}
			return testSuiteIDNumberI < testSuiteIDNumberJ
		})

		totalTestTimeTakenStr := timeWithUnits(totalTestTimeTakenSnapshot)

		if !r.config.DisableANSI {
			summaryFormat := "\n <=========================================> \n  COMPLETE TESTRUN SUMMARY. \n" +
				"\tTotal tests: %s\n" +
				"\tTotal test passed: %s\n" +
				"\tTotal test failed: %s\n"
			summaryArgs := []interface{}{totalTestsSnapshot, totalTestPassedSnapshot, totalTestFailedSnapshot}
			if totalTestObsoleteSnapshot > 0 {
				summaryFormat += "\tTotal test obsolete: %s\n"
				summaryArgs = append(summaryArgs, totalTestObsoleteSnapshot)
			}
			if totalTestIgnoredSnapshot > 0 {
				summaryFormat += "\tTotal test ignored: %s\n"
				summaryArgs = append(summaryArgs, totalTestIgnoredSnapshot)
			}
			summaryFormat += "\tTotal time taken: %s\n"
			summaryArgs = append(summaryArgs, totalTestTimeTakenStr)

			if _, err := pp.Printf(summaryFormat, summaryArgs...); err != nil {
				utils.LogError(r.logger, err, "failed to print test run summary")
				return
			}

			header := "\n\tTest Suite Name\t\tTotal Test\tPassed\t\tFailed"
			if totalTestObsoleteSnapshot > 0 {
				header += "\t\tObsolete"
			}
			if totalTestIgnoredSnapshot > 0 {
				header += "\t\tIgnored"
			}
			header += "\t\tTime Taken"
			if totalTestFailedSnapshot > 0 {
				header += "\tFailed Testcases"
			}
			header += "\t\n"

			_, err := pp.Printf(header)
			if err != nil {
				utils.LogError(r.logger, err, "failed to print test suite summary header")
				return
			}

			for _, testSuiteName := range testSuiteNames {
				report := reportSnapshot[testSuiteName]
				if report.status {
					pp.SetColorScheme(models.GetPassingColorScheme())
				} else {
					pp.SetColorScheme(models.GetFailingColorScheme())
				}

				testSetTimeTakenStr := timeWithUnits(report.duration)

				var format strings.Builder
				args := []interface{}{}

				format.WriteString("\n\t%s\t\t%s\t\t%s\t\t%s")
				args = append(args, testSuiteName, report.total, report.passed, report.failed)

				if totalTestObsoleteSnapshot > 0 {
					format.WriteString("\t\t%s")
					args = append(args, report.obsolete)
				}

				if totalTestIgnoredSnapshot > 0 && !r.config.Test.MustPass {
					format.WriteString("\t\t%s")
					args = append(args, report.ignored)
				}

				format.WriteString("\t\t%s") // Time Taken
				args = append(args, testSetTimeTakenStr)

				if totalTestFailedSnapshot > 0 && !r.config.Test.MustPass {
					failedCasesStr := "-"
					if failedCases, ok := r.failedTCsBySetID[testSuiteName]; ok && len(failedCases) > 0 {
						failedCasesStr = strings.Join(failedCases, ", ")
					}
					format.WriteString("\t%s")
					args = append(args, failedCasesStr)
				}

				if _, err := pp.Printf(format.String(), args...); err != nil {
					utils.LogError(r.logger, err, "failed to print test suite details")
					return
				}
			}
			if _, err := pp.Printf("\n<=========================================> \n\n"); err != nil {
				utils.LogError(r.logger, err, "failed to print separator")
				return
			}

		} else {
			fmt.Printf("\n <=========================================> \n  COMPLETE TESTRUN SUMMARY. \n\tTotal tests: %d\n"+"\tTotal test passed: %d\n"+"\tTotal test failed: %d\n", totalTestsSnapshot, totalTestPassedSnapshot, totalTestFailedSnapshot)
			if totalTestObsoleteSnapshot > 0 {
				fmt.Printf("\tTotal test obsolete: %d\n", totalTestObsoleteSnapshot)
			}
			if totalTestIgnoredSnapshot > 0 {
				fmt.Printf("\tTotal test ignored: %d\n", totalTestIgnoredSnapshot)
			}
			fmt.Printf("\tTotal time taken: %s\n", totalTestTimeTakenStr)

			header := "\n\tTest Suite Name\t\tTotal Test\tPassed\t\tFailed"
			if totalTestObsoleteSnapshot > 0 {
				header += "\t\tObsolete"
			}
			if totalTestIgnoredSnapshot > 0 {
				header += "\t\tIgnored"
			}
			header += "\t\tTime Taken"
			if totalTestFailedSnapshot > 0 {
				header += "\tFailed Testcases"
			}
			header += "\t\n"

			fmt.Print(header)

			for _, testSuiteName := range testSuiteNames {
				report := reportSnapshot[testSuiteName]
				testSetTimeTakenStr := timeWithUnits(report.duration)

				var format strings.Builder
				args := []interface{}{}

				format.WriteString("\n\t%s\t\t%d\t\t%d\t\t%d")
				args = append(args, testSuiteName, report.total, report.passed, report.failed)

				if totalTestObsoleteSnapshot > 0 {
					format.WriteString("\t\t%d")
					args = append(args, report.obsolete)
				}

				if totalTestIgnoredSnapshot > 0 && !r.config.Test.MustPass {
					format.WriteString("\t\t%d")
					args = append(args, report.ignored)
				}

				format.WriteString("\t\t%s") // Time Taken
				args = append(args, testSetTimeTakenStr)

				if totalTestFailedSnapshot > 0 && !r.config.Test.MustPass {
					failedCasesStr := "-"
					if failedCases, ok := r.failedTCsBySetID[testSuiteName]; ok && len(failedCases) > 0 {
						failedCasesStr = strings.Join(failedCases, ", ")
					}
					format.WriteString("\t\t%s")
					args = append(args, failedCasesStr)
				}
				fmt.Printf(format.String(), args...)
			}

			fmt.Print("\n<=========================================> \n\n")
		}
	}
}
func (r *Replayer) RunApplication(ctx context.Context, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, opts)
}

func (r *Replayer) GetTestSetConf(ctx context.Context, testSet string) (*models.TestSet, error) {
	return r.testSetConf.Read(ctx, testSet)
}

// UpdateTestSetTemplate writes the updated template values to the test-set's config.
// It preserves existing pre/post scripts, secret and metadata fields.
func (r *Replayer) UpdateTestSetTemplate(ctx context.Context, testSetID string, template map[string]interface{}) error {
	if len(template) == 0 { // nothing to persist
		return nil
	}
	existing, err := r.testSetConf.Read(ctx, testSetID)
	if err != nil {
		// If file missing we still attempt to write minimal config.
		r.logger.Debug("failed reading existing test-set config while updating template; will create new", zap.String("testSetID", testSetID), zap.Error(err))
	}
	ts := &models.TestSet{}
	if existing != nil {
		ts.PreScript = existing.PreScript
		ts.PostScript = existing.PostScript
		ts.Secret = existing.Secret
		ts.Metadata = existing.Metadata
	} else {
		ts.Metadata = map[string]interface{}{}
	}
	ts.Template = template
	if err := r.testSetConf.Write(ctx, testSetID, ts); err != nil {
		utils.LogError(r.logger, err, "failed to write updated template map", zap.String("testSetID", testSetID))
		return err
	}
	return nil
}

func (r *Replayer) DenoiseTestCases(ctx context.Context, testSetID string, noiseParams []*models.NoiseParams) ([]*models.NoiseParams, error) {

	testCases, err := r.testDB.GetTestCases(ctx, testSetID)
	if err != nil {
		return nil, fmt.Errorf("failed to get test cases: %w", err)
	}

	for _, v := range testCases {
		for _, noiseParam := range noiseParams {
			if v.Name == noiseParam.TestCaseID {
				// append the noise map
				if noiseParam.Ops == string(models.OpsAdd) {
					v.Noise = mergeMaps(v.Noise, noiseParam.Assertion)
				} else {
					// remove from the original noise map
					v.Noise = removeFromMap(v.Noise, noiseParam.Assertion)
				}
				err = r.testDB.UpdateTestCase(ctx, v, testSetID, true)
				if err != nil {
					return nil, fmt.Errorf("failed to update test case: %w", err)
				}
				noiseParam.AfterNoise = v.Noise
			}
		}
	}

	return noiseParams, nil
}

func (r *Replayer) executeScript(ctx context.Context, script string) error {

	if script == "" {
		return nil
	}

	// Define the function to cancel the command
	cmdCancel := func(cmd *exec.Cmd) func() error {
		return func() error {
			return utils.InterruptProcessTree(r.logger, cmd.Process.Pid, syscall.SIGINT)
		}
	}

	cmdErr := utils.ExecuteCommand(ctx, r.logger, script, cmdCancel, 25*time.Second, nil)
	if cmdErr.Err != nil {
		return fmt.Errorf("failed to execute script: %w", cmdErr.Err)
	}
	return nil
}

func (r *Replayer) DeleteTestSet(ctx context.Context, testSetID string) error {
	return r.testDB.DeleteTestSet(ctx, testSetID)
}

func (r *Replayer) DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error {
	return r.testDB.DeleteTests(ctx, testSetID, testCaseIDs)
}

// CreateFailedTestResult creates a test result for failed test cases
// isAppConnectionErrorMsg reports whether a simulate-request error string is a
// transport/connection-level failure (the app produced no response) rather than
// a content diff. CreateFailedTestResult only receives the error message, so this
// matches the stable net/syscall error texts (same string-classification
// approach as isDockerComposeReplayShutdown above).
func isAppConnectionErrorMsg(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "connection refused") ||
		strings.Contains(m, "connection reset by peer") ||
		strings.Contains(m, "broken pipe") ||
		strings.Contains(m, "no such host") ||
		strings.Contains(m, ": eof")
}

// appendCategoryUnique appends c only if it is not already present.
func appendCategoryUnique(cats []models.FailureCategory, c models.FailureCategory) []models.FailureCategory {
	for _, x := range cats {
		if x == c {
			return cats
		}
	}
	return append(cats, c)
}

// anyAppConnectionError reports whether any test in the run failed because the
// application produced no response at all (connection refused/reset/EOF). Such a
// test says nothing about which mocks are needed — its request never reached the
// app — so it must not be read as evidence that mocks are unused.
//
// Reads the TOP-LEVEL TestResult.FailureInfo, not Result.FailureInfo, for the
// simple reason that CreateFailedTestResult only sets the category there. Both
// survive the report store (it holds the structs in memory; ReportResults reads
// Result.FailureInfo.Risk off the very same values), so this is about where the
// category is written, not about what survives.
func anyAppConnectionError(results []models.TestResult) bool {
	for _, tr := range results {
		for _, c := range tr.FailureInfo.Category {
			if c == models.AppConnectionError {
				return true
			}
		}
	}
	return false
}

// pruneInputUntrustworthy reports whether a run's consumed-mock set is too weak
// to justify deleting anything.
//
// Pruning keeps what the tests consumed and deletes the rest, which is only sound
// when a test not consuming a mock actually means "the app didn't need it". Two
// cases break that:
//
//   - success == 0: no test passed, so nothing vouches for any mock. The keep-set
//     collapses to what the startup/never-executed paths contributed, and every
//     mock a real request would have used is deleted.
//   - any AppConnectionError: those requests never reached the app, so they
//     consumed no mocks for want of a connection, not for want of need.
//
// Both mean "no information", and pruning reads that as "delete" — which is the
// bug. Note this is NOT the same as "the run produced no signal at all": a run
// can have passing tests AND one connection error, and it lands here because that
// one test's mocks would be wrongly deleted. Callers must not describe it as a
// zero-passing run.
func pruneInputUntrustworthy(success int, results []models.TestResult) bool {
	return success == 0 || anyAppConnectionError(results)
}

// shouldSkipPruning decides whether a completed run is allowed to delete recorded
// mocks. Pruning is destructive and its input (the keep-set) is only trustworthy
// when the run actually produced signal, so there are two independent reasons to
// refuse:
//
//   - the keep-set is untrustworthy (pruneInputUntrustworthy): no test passed, or
//     some request never reached the app. Mocks went unconsumed for lack of
//     evidence, not because they are unused. This reason is deliberately
//     INDEPENDENT of preserveFailedMocks: callers turn that flag off (k8s-proxy
//     auto-replay sets it false), so it cannot serve as the safety net here. A
//     crashed container must never be able to destroy mocks.
//   - preserveFailedMocks (opt-in): a caller that wants every recorded mock kept
//     available for inspection whenever anything failed or went obsolete.
//
// shouldPrune is the COMPLETE gate on the destructive prune: it must be enabled
// (RemoveUnusedMocks), keploy must be instrumenting the run, and the run's
// consumed-mock set must be trustworthy enough to delete against.
//
// It exists as one function because the data-loss bug lives in this conjunction,
// not in shouldSkipPruning alone — a correct predicate that isn't wired into the
// decision deletes mocks just the same. Keeping the whole condition here means a
// unit test can pin the wiring instead of only the predicate.
func shouldPrune(removeUnusedMocks, instrument bool, success, failure, obsolete int, preserveFailedMocks bool, results []models.TestResult) bool {
	if !removeUnusedMocks || !instrument {
		return false
	}
	return !shouldSkipPruning(success, failure, obsolete, preserveFailedMocks, results)
}

func shouldSkipPruning(success, failure, obsolete int, preserveFailedMocks bool, results []models.TestResult) bool {
	if pruneInputUntrustworthy(success, results) {
		return true
	}
	return preserveFailedMocks && (failure > 0 || obsolete > 0)
}

func (r *Replayer) CreateFailedTestResult(testCase *models.TestCase, testSetID string, started time.Time, errorMessage string) *models.TestResult {
	testCaseResult := &models.TestResult{
		Kind:         testCase.Kind,
		Name:         testSetID,
		Status:       models.TestStatusFailed,
		Started:      started.Unix(),
		Completed:    time.Now().UTC().Unix(),
		TestCaseID:   testCase.Name,
		TestCasePath: filepath.Join(r.config.Path, testSetID),
		MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
		Noise:        testCase.Noise,
		TimeTaken:    time.Since(started).String(),
	}

	var result *models.Result

	switch testCase.Kind {
	case models.HTTP:
		actualResponse := &models.HTTPResp{
			StatusCode: 0,
			Header:     make(map[string]string),
			Body:       errorMessage,
		}

		_, result = r.CompareHTTPResp(testCase, actualResponse, testSetID, true)

		testCaseResult.Req = models.HTTPReq{
			Method:     testCase.HTTPReq.Method,
			ProtoMajor: testCase.HTTPReq.ProtoMajor,
			ProtoMinor: testCase.HTTPReq.ProtoMinor,
			URL:        testCase.HTTPReq.URL,
			URLParams:  testCase.HTTPReq.URLParams,
			Header:     testCase.HTTPReq.Header,
			Body:       testCase.HTTPReq.Body,
			Binary:     testCase.HTTPReq.Binary,
			Form:       testCase.HTTPReq.Form,
			Timestamp:  testCase.HTTPReq.Timestamp,
		}
		testCaseResult.Res = *actualResponse

	case models.GRPC_EXPORT:
		actualResponse := &models.GrpcResp{
			Headers: models.GrpcHeaders{
				PseudoHeaders:   make(map[string]string),
				OrdinaryHeaders: make(map[string]string),
			},
			Body: models.GrpcLengthPrefixedMessage{
				DecodedData: errorMessage,
			},
			Trailers: models.GrpcHeaders{
				PseudoHeaders:   make(map[string]string),
				OrdinaryHeaders: make(map[string]string),
			},
		}

		respCopy := *actualResponse

		if r.config.Test.ProtoFile != "" || r.config.Test.ProtoDir != "" || len(r.config.Test.ProtoInclude) > 0 {
			// get the :path header from the request
			method, ok := testCase.GrpcReq.Headers.PseudoHeaders[":path"]
			if !ok {
				utils.LogError(r.logger, nil, "failed to get :path header from the request, cannot convert grpc response to json")
				goto compareResp
			}

			pc := models.ProtoConfig{
				ProtoFile:    r.config.Test.ProtoFile,
				ProtoDir:     r.config.Test.ProtoDir,
				ProtoInclude: r.config.Test.ProtoInclude,
				RequestURI:   method,
			}

			// get the proto message descriptor
			md, files, err := utils.GetProtoMessageDescriptor(context.Background(), r.logger, pc)
			if err != nil {
				utils.LogError(r.logger, err, "failed to get proto message descriptor, cannot convert grpc response to json")
				goto compareResp
			}

			// convert both actual and expected using the same path (protoscope-text -> wire -> json)
			actJSON, actOK := utils.ProtoTextToJSON(md, files, respCopy.Body.DecodedData, r.logger)
			testJSON, testOK := utils.ProtoTextToJSON(md, files, testCase.GrpcResp.Body.DecodedData, r.logger)

			if actOK && testOK {
				respCopy.Body.DecodedData = string(actJSON)
				testCase.GrpcResp.Body.DecodedData = string(testJSON)
			}
		}

	compareResp:
		_, result = r.CompareGRPCResp(testCase, &respCopy, testSetID, true)

		testCaseResult.GrpcReq = testCase.GrpcReq
		testCaseResult.GrpcRes = *actualResponse
	}

	if result != nil {
		testCaseResult.Result = *result
	}

	if result != nil && result.FailureInfo.Risk != models.None {
		testCaseResult.FailureInfo.Risk = result.FailureInfo.Risk
		testCaseResult.FailureInfo.Category = result.FailureInfo.Category
	}

	// Attribute a connection-level failure distinctly: the status_code=0 recorded
	// above is the synthetic value we use when the app produced NO response. If the
	// cause is a transport error (refused/reset/EOF/host unreachable) it is an
	// app-unreachable/availability failure, NOT a content regression — label it so
	// operators and downstream (k8s-proxy reads TestResult.FailureInfo) triage it
	// as infra rather than a STATUS_CODE_CHANGED regression. Raw StatusCode stays 0.
	if isAppConnectionErrorMsg(errorMessage) {
		testCaseResult.FailureInfo.Category = appendCategoryUnique(testCaseResult.FailureInfo.Category, models.AppConnectionError)
	}

	return testCaseResult
}

func (r *Replayer) replaceHostInTestCase(testCase *models.TestCase, newHost, logContext string) error {
	var err error
	switch testCase.Kind {
	case models.HTTP:
		testCase.HTTPReq.URL, err = utils.ReplaceHost(testCase.HTTPReq.URL, newHost)
		if err != nil {
			utils.LogError(r.logger, err, fmt.Sprintf("failed to replace host to %s", logContext))
			return err
		}
		r.logger.Debug("", zap.String(fmt.Sprintf("replaced %s", logContext), testCase.HTTPReq.URL))

	case models.GRPC_EXPORT:
		testCase.GrpcReq.Headers.PseudoHeaders[":authority"], err = utils.ReplaceGrpcHost(testCase.GrpcReq.Headers.PseudoHeaders[":authority"], newHost)
		if err != nil {
			utils.LogError(r.logger, err, fmt.Sprintf("failed to replace host to %s", logContext))
			return err
		}
		r.logger.Debug("", zap.String(fmt.Sprintf("replaced %s", logContext), testCase.GrpcReq.Headers.PseudoHeaders[":authority"]))
	}
	return nil
}

func (r *Replayer) replacePortInTestCase(testCase *models.TestCase, newPort string) error {
	var err error
	switch testCase.Kind {
	case models.HTTP:
		testCase.HTTPReq.URL, err = utils.ReplacePort(testCase.HTTPReq.URL, newPort)
	case models.GRPC_EXPORT:
		testCase.GrpcReq.Headers.PseudoHeaders[":authority"], err = utils.ReplaceGrpcPort(testCase.GrpcReq.Headers.PseudoHeaders[":authority"], newPort)
	}
	if err != nil {
		utils.LogError(r.logger, err, "failed to replace port")
		return err
	}
	return nil
}

func (r *Replayer) GetSelectedTestSets(ctx context.Context) ([]string, error) {
	// get all the testset ids
	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		utils.LogError(r.logger, err, "failed to get all test set ids")
		return nil, fmt.Errorf("failed to get test set ids")
	}

	var testSets []string
	for _, testSetID := range testSetIDs {
		if _, ok := r.config.Test.SelectedTests[testSetID]; !ok && len(r.config.Test.SelectedTests) != 0 {
			continue
		}
		testSets = append(testSets, testSetID)
	}
	if len(testSets) == 0 {
		testSets = testSetIDs
	}

	// Sort the testsets.
	natsort.Sort(testSets)
	return testSets, nil
}

func (r *Replayer) StoreMappings(ctx context.Context, mapping *models.Mapping) error {
	// Save test-mock mappings to YAML file
	err := r.mappingDB.Insert(ctx, mapping)
	return err
}

// createBackup creates a timestamped backup of a test set directory before modification.
func (r *Replayer) createBackup(testSetID string) error {
	srcPath := filepath.Join(r.config.Path, testSetID)
	timestamp := time.Now().Format("20060102T150405")
	backupDestPath := filepath.Join(srcPath, ".backup", timestamp)

	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return fmt.Errorf("source directory for backup does not exist: %s", srcPath)
	}

	if err := os.MkdirAll(backupDestPath, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	err := r.copyDirContents(srcPath, backupDestPath)
	if err != nil {
		// Clean up the failed backup attempt
		_ = os.RemoveAll(backupDestPath)
		return fmt.Errorf("failed to copy contents for backup: %w", err)
	}

	r.logger.Info("Successfully created a backup of the test set before modification.", zap.String("testSet", testSetID), zap.String("location", backupDestPath))
	return nil
}

// copyDirContents recursively copies contents from src to dst, excluding the .backup directory.
func (r *Replayer) copyDirContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		// CRITICAL: Exclude the backup directory itself to prevent recursion.
		if entry.Name() == ".backup" {
			continue
		}

		fileInfo, err := os.Stat(srcPath)
		if err != nil {
			return err
		}

		if fileInfo.IsDir() {
			if err := os.MkdirAll(dstPath, fileInfo.Mode()); err != nil {
				return err
			}
			if err := r.copyDirContents(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			// It's a file, copy it.
			srcFile, err := os.Open(srcPath)
			if err != nil {
				return err
			}
			defer srcFile.Close()

			dstFile, err := os.Create(dstPath)
			if err != nil {
				return err
			}
			defer dstFile.Close()

			if _, err := io.Copy(dstFile, srcFile); err != nil {
				return err
			}

			if err := os.Chmod(dstPath, fileInfo.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

// maxResetResends bounds how many times a transport-reset test request is
// re-sent. The reset is docker's userland-proxy resetting freshly-accepted
// host-port conns in bursts under load; under heavy CI contention that burst
// outlasts a couple of back-to-back attempts, so re-send work-slow — more
// attempts, spread by resetResendBackoff, to ride the burst out. Each attempt is
// gated on no-mock-consumed + port readiness, and a non-reset failure (genuinely
// broken app) still stops immediately, so the extra attempts only ever add
// latency on the reset path.
const maxResetResends = 6

// resetResendBackoff grows the pause between reset re-sends (attempt*backoff) so
// the bounded attempts spread across the docker-proxy reset window instead of
// hammering it back-to-back.
const resetResendBackoff = 300 * time.Millisecond

// resetResendReadyTimeout caps the per-attempt app-port readiness re-poll done
// before re-sending. It only ever adds latency on the (rare) reset path.
const resetResendReadyTimeout = 5 * time.Second

// retryResetOnce re-sends a test request that failed with a transport-level
// connection reset / unexpected EOF (origErr), but ONLY when doing so is
// provably safe. It returns (resp, true, nil) when a re-send produced a real
// response, and (nil, false, drained) when the request was not retried or every
// re-send also failed (the caller then treats origErr as the genuine failure).
//
// drained carries the mocks the safety gate observed-and-DRAINED for the
// just-failed request on the UNSAFE refusal path (see resetResendUnsafe and the
// drain-accounting note below); it is nil on every other path. The caller must
// fold it back into totalConsumedMocks so the agent's next-iteration filter
// (filterOutDeleted) still removes those exhausted single-use mocks.
//
// Safety invariant (this is the whole point): a reset is ambiguous about
// whether the app already consumed a single-use mock. MockManager.GetConsumed-
// Mocks is per-call and DRAINING — it returns only the mocks consumed SINCE the
// last drain, then clears its list. In the normal flow the per-test drain at
// RunTestSet (the `GetConsumedMocks` after a successful request) already emptied
// that list, so when we get here for a failed request the gate sees EXACTLY this
// request's consumption. We therefore re-send only while that per-call count is
// ZERO — i.e. THIS request consumed nothing irreversible. The moment it is >0 we
// stop and let the original error stand, so a mid-stream reset that already
// burned a mock is never re-run against exhausted mocks (which would fabricate a
// verdict). Under the dominant docker-proxy reset the app never saw the request,
// so the count stays at 0 and the re-send recovers the real response instead of
// a false status_code=0.
//
// Drain side-effect accounting: calling GetConsumedMocks in the gate DRAINS the
// agent's consumed-reporting list. This does NOT resurrect any exhausted mock —
// the agent serves from its mock TREES (DeleteFilteredMock removes a single-use
// mock from them on consume); the drained list is a separate keploy-side
// REPORTING channel. So a later test can never be re-served a drained-but-
// consumed mock by the agent's own state. The only risk is keploy-side: those
// drained mocks would be missing from totalConsumedMocks, so the next
// SendMockFilterParamsToAgent's filterOutDeleted would fail to filter them and
// SetMocksWithWindow could re-insert them into the serving pool. We close that
// by returning the drained mocks (UNSAFE path) for the caller to fold into
// totalConsumedMocks. On the SAFE path (count==0) the drain is a no-op, and the
// successful re-send's consumption is still accounted by the subsequent per-test
// GetConsumedMocks in RunTestSet (loopErr becomes nil, so that block runs).
func (r *Replayer) retryResetOnce(ctx context.Context, testCase *models.TestCase, testSetID string, origErr error) (interface{}, bool, []models.MockState) {
	for attempt := 1; attempt <= maxResetResends; attempt++ {
		if ctx.Err() != nil {
			return nil, false, nil
		}

		// Refuse to retry if THIS request consumed a new mock (per-call count
		// > 0 since the last drain) — that means it was (at least partially)
		// processed and a re-send would not be idempotent. Hand the drained
		// mocks back so the caller can keep totalConsumedMocks accurate.
		if unsafe, drained := r.resetResendUnsafe(ctx); unsafe {
			r.logger.Debug("transport reset is not safe to re-send (a mock was consumed); leaving the original error",
				zap.String("testSetID", testSetID),
				zap.String("testCaseID", testCase.Name))
			return nil, false, drained
		}

		// Re-gate on the app actually serving again (HTTP-level, on the same
		// endpoint the request dials) so we re-send only once the app/proxy is
		// ready, not blindly. Best effort: if we can't determine a probeable
		// target we proceed — the bounded attempts still protect us.
		r.waitForResetResendReady(ctx, testCase, testSetID)

		r.logger.Warn("test request reset by app/proxy before any mock was consumed; re-sending — the request was never processed, so this is not a retry of an assertion",
			zap.Int("attempt", attempt),
			zap.Int("maxRetries", maxResetResends),
			zap.String("testSetID", testSetID),
			zap.String("testCaseID", testCase.Name),
			zap.Error(origErr))

		// Re-open the per-test capture window so a mock miss on the re-send
		// attributes to THIS test (the previous window was for the failed try).
		r.beginTestErrorCapture(ctx)

		resp, err := r.hookImpl.SimulateRequest(ctx, testCase, testSetID)
		if err == nil {
			return resp, true, nil
		}
		if !pkg.IsTransportConnReset(err) {
			// A different failure on the re-send (e.g. the app really is down):
			// stop and let the caller report the original transport reset.
			return nil, false, nil
		}
		origErr = err
		// Back off (growing) before the next re-send so the bounded attempts
		// spread across the docker-proxy reset burst rather than hammering it.
		select {
		case <-ctx.Done():
			return nil, false, nil
		case <-time.After(time.Duration(attempt) * resetResendBackoff):
		}
	}
	return nil, false, nil
}

// resetResendUnsafe reports whether re-sending is unsafe because the failed
// request consumed at least one mock, and returns whatever it drained from the
// agent's consumed-reporting list so the caller can keep totalConsumedMocks
// accurate (see the drain side-effect note on retryResetOnce).
//
// GetConsumedMocks is per-call and DRAINING: it returns only the mocks consumed
// since the last drain (which, in the normal flow, is empty by the time we get
// here for a failed request). So the per-call result IS exactly this request's
// consumption, and the gate is unsafe iff that count is > 0. On a fetch error it
// returns unsafe=true with no drained mocks (fail safe): if we cannot prove
// nothing was consumed, we must not retry.
func (r *Replayer) resetResendUnsafe(ctx context.Context) (bool, []models.MockState) {
	consumed, err := r.hookImpl.GetConsumedMocks(ctx)
	if err != nil {
		r.logger.Debug("could not fetch consumed mocks to validate reset re-send; not retrying", zap.Error(err))
		return true, nil
	}
	if len(consumed) > 0 {
		return true, consumed
	}
	return false, nil
}

// waitForResetResendReady best-effort gates a reset re-send on the app actually
// serving again, bounded by resetResendReadyTimeout.
//
// It prefers an HTTP-level probe against the SAME endpoint the test request is
// dialed to — resolved via resolveProbeTarget, which mirrors the simulation's
// ResolveTestTarget (ConfigHost/--host, replaceWith, port precedence) rather than
// the raw recorded URL. This makes the gate address-family-correct (an IPv6
// "localhost" dial is gated on the IPv6 path, not a mismatched IPv4 127.0.0.1)
// and, unlike a bare TCP-accept gate, it does not declare readiness while
// docker's userland-proxy is still accepting-then-resetting connections
// mid-response. For a non-HTTP or unresolvable target it falls back to the docker
// published host-port TCP gate (no-op for native apps / unmapped publishes). On
// timeout it returns and lets the bounded re-send attempts run.
func (r *Replayer) waitForResetResendReady(ctx context.Context, testCase *models.TestCase, testSetID string) {
	wctx, cancel := context.WithTimeout(ctx, resetResendReadyTimeout)
	defer cancel()

	if scheme, host, port, ok := resolveProbeTarget(r.config.Test, testCase, testSetID, r.logger); ok {
		if err := waitForHTTPServing(wctx, scheme, host, port); err != nil {
			r.logger.Debug("app not serving HTTP before reset re-send; re-sending anyway",
				zap.String("host", host), zap.String("port", port), zap.Error(err))
		}
		return
	}

	host, port, ok := dockerPublishedHostPort(r.config.Command)
	if !ok {
		return
	}
	if err := pkg.WaitForPort(wctx, host, port, resetResendReadyTimeout); err != nil {
		r.logger.Debug("app host port not ready before reset re-send; re-sending anyway",
			zap.String("host", host), zap.String("port", port), zap.Error(err))
	}
}

// beginTestErrorCapture opens a per-test mock-error capture window on the agent
// (via an optional capability — older agents / non-agent instrumentations skip
// it and fall back to the legacy global queue) so a mock miss during this test
// attributes to THIS test instead of whichever test drains GetMockErrors next.
// Called right before SimulateRequest on BOTH the normal and streaming paths;
// the matching attachMockErrors/GetMockErrors closes the window (the streaming
// path closes it only after the stream body is fully consumed).
// Best-effort: a failure only degrades to the old behaviour.
func (r *Replayer) beginTestErrorCapture(ctx context.Context) {
	if b, ok := r.instrumentation.(interface {
		BeginTestErrorCapture(context.Context) error
	}); ok {
		if err := b.BeginTestErrorCapture(ctx); err != nil {
			r.logger.Debug("failed to begin test error capture", zap.Error(err))
		}
	}
}

// attachMockErrors drains the per-test mock-error capture window (closing it)
// and records any unmatched outgoing calls onto the result and the end-of-run
// summary store. It must run on EVERY test-iteration exit — including the early
// simulation-error / invalid-response returns — so the window is finalized for
// THIS test and a miss is never carried forward to the next test or lost.
func (r *Replayer) attachMockErrors(ctx context.Context, testSetID, testCaseName string, result *models.TestResult) {
	mockErrors, err := r.instrumentation.GetMockErrors(ctx)
	if err != nil {
		// Don't swallow silently. This test's misses can't be attached, but the
		// agent-side window is reset by the next BeginTestErrorCapture (which
		// discards a never-closed window), so the failure can't bleed into the
		// next test. Log it so a persistent transport problem is visible rather
		// than reports vanishing without a trace.
		r.logger.Debug("failed to fetch mock errors for test; skipping unmatched-call attachment",
			zap.String("testSetID", testSetID),
			zap.String("testCaseID", testCaseName),
			zap.Error(err))
		return
	}
	for _, me := range mockErrors {
		if result != nil {
			result.FailureInfo.UnmatchedCalls = append(result.FailureInfo.UnmatchedCalls, me)
		}
		r.mockMismatchFailures.AddUnmatchedCallForTest(testSetID, testCaseName, me)
	}
}

func (r *Replayer) determineMockingStrategy(ctx context.Context, testSetID string, isMappingEnabled bool) (bool, map[string][]models.MockEntry) {
	// Default to timestamp-based strategy with empty mappings.
	defaultMappings := make(map[string][]models.MockEntry)

	if r.mappingDB == nil {
		r.logger.Debug("No mapping database available, using timestamp-based mock filtering strategy")
		return false, defaultMappings
	}

	if !isMappingEnabled {
		// The calling function already logs this, so we don't log it again here.
		return false, defaultMappings
	}

	// Try to get mappings from the database.
	expectedTestMockMappings, hasMeaningfulMappings, err := r.mappingDB.Get(ctx, testSetID)
	if err != nil {
		r.logger.Debug("Failed to get mappings, falling back to timestamp-based filtering",
			zap.String("testSetID", testSetID),
			zap.Error(err))
		return false, defaultMappings
	}

	if hasMeaningfulMappings {
		// Meaningful mappings were found, so use the mapping-based strategy.
		r.logger.Debug("Using mapping-based mock filtering strategy",
			zap.String("testSetID", testSetID),
			zap.Int("totalMappings", len(expectedTestMockMappings)))
		return true, expectedTestMockMappings
	}

	// No meaningful mappings were found, so fall back to the timestamp-based strategy.
	r.logger.Debug("No meaningful mappings found, using timestamp-based mock filtering strategy (legacy approach)",
		zap.String("testSetID", testSetID))
	return false, defaultMappings
}

// isMockSubset checks if all expected mocks are present in the actual mocks list
// isReusableTierMock reports whether a loaded mock is a reusable, non-per-test
// tier — session / connection / config — recorded once at app startup and
// shared across every test rather than consumed by exactly one. Such mocks
// belong in the per-test mapping (so the replay mock pool is complete) but
// must be excluded from the per-test consumed-vs-expected assertion: they are
// not deterministically attributed to a single test's window, so comparing
// them would falsely demote tests to OBSOLETE. Only per-test mocks are
// compared. Tier is read from TestModeInfo.Lifetime (derived at ingest) with
// Spec.Metadata["type"] as a fallback for mocks whose lifetime wasn't derived.
func isReusableTierMock(m *models.Mock) bool {
	switch m.TestModeInfo.Lifetime {
	case models.LifetimeSession, models.LifetimeConnection:
		return true
	}
	if m.Spec.Metadata != nil {
		switch m.Spec.Metadata["type"] {
		case "config", "connection":
			return true
		}
	}
	return false
}

// isReusableTierState is the MockState (consumed-mock) equivalent of
// isReusableTierMock. GetConsumedMocks carries the recorder-derived Lifetime
// and metadata type, so session/connection/config mocks are carved out of the
// consumed side of the assertion the same way they are on the expected side.
func isReusableTierState(s models.MockState) bool {
	switch s.Lifetime {
	case models.LifetimeSession, models.LifetimeConnection:
		return true
	}
	switch s.Type {
	case "config", "connection":
		return true
	}
	return false
}

func isMockSubset(actual []string, expected []string) bool {
	actualMap := make(map[string]bool)
	for _, mock := range actual {
		actualMap[mock] = true
	}

	for _, mock := range expected {
		if !actualMap[mock] {
			return false
		}
	}
	return true
}

// buildExpectedMockInfos converts the per-test expected mock list (from the
// recorded mappings) into the MockMismatchInfo.ExpectedMocks shape. DNS
// entries are filtered out both at the entry level (m.Kind == "DNS") and via
// the mockKindByName lookup, because DNS resolution order is non-deterministic
// and including it produces noisy spurious mismatches downstream. Resolves an
// empty Kind by looking up mockKindByName so the consumer always gets a kind
// label when one is available.
//
// Extracted from RunTestSet for unit-testability.
func buildExpectedMockInfos(expectedMocks []models.MockEntry, mockKindByName map[string]models.Kind) []models.MockMismatchMock {
	out := make([]models.MockMismatchMock, 0, len(expectedMocks))
	for _, m := range expectedMocks {
		isDNS := strings.EqualFold(m.Kind, string(models.DNS))
		if !isDNS {
			if kind, ok := mockKindByName[m.Name]; ok && kind == models.DNS {
				isDNS = true
			}
		}
		if isDNS {
			continue
		}
		resolvedKind := m.Kind
		if resolvedKind == "" {
			if kind, ok := mockKindByName[m.Name]; ok {
				resolvedKind = string(kind)
			}
		}
		out = append(out, models.MockMismatchMock{Name: m.Name, Kind: resolvedKind})
	}
	return out
}

// buildActualMockInfos converts the per-test consumed mock list (from the
// instrumentation/agent) into the MockMismatchInfo.ActualMocks shape.
// `known` gates the build: when false (e.g. GetConsumedMocks failed for THIS
// test in non-instrument mode) we return an empty slice rather than walking
// stale data attributed to the wrong test case. DNS entries are filtered.
//
// Extracted from RunTestSet for unit-testability.
func buildActualMockInfos(consumed []models.MockState, known bool) []models.MockMismatchMock {
	out := make([]models.MockMismatchMock, 0, len(consumed))
	if !known {
		return out
	}
	for _, m := range consumed {
		if m.Kind == models.DNS {
			continue
		}
		out = append(out, models.MockMismatchMock{Name: m.Name, Kind: string(m.Kind)})
	}
	return out
}
