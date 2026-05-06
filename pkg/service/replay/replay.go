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

const UNKNOWN_TEST = "UNKNOWN_TEST"
const applicationFailedToRunLogMessage = "application failed to run; check the application logs for details or verify the app command is correct"

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
	logger             *zap.Logger
	testDB             TestDB
	mockDB             MockDB
	mappingDB          MappingDB
	reportDB           ReportDB
	testSetConf        TestSetConfig
	telemetry          Telemetry
	instrumentation    Instrumentation
	config             *config.Config
	instrument         bool
	isLastTestSet      bool
	isLastTestCase     bool
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

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, g))

	setupErrGrp, _ := errgroup.WithContext(ctx)
	setupCtx := context.WithoutCancel(ctx)
	setupCtx, setupCtxCancel := context.WithCancel(setupCtx)
	setupCtx = context.WithValue(setupCtx, models.ErrGroupKey, setupErrGrp)

	var hookCancel context.CancelFunc
	var stopReason = "replay completed successfully"

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-ctx.Done():
			break
		default:
			r.logger.Info("stopping Keploy", zap.String("reason", stopReason))
		}

		// Notify the agent that we are shutting down gracefully. It covers early exits before RunTestSet runs
		// and shutdown paths where the per‑test‑set defer doesn’t execute (or never starts)
		if err := r.instrumentation.NotifyGracefulShutdown(context.Background()); err != nil {
			r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
		}

		if hookCancel != nil {
			hookCancel()
		}
		cancel()
		err := g.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop replaying")
		}

		setupCtxCancel()
		err = setupErrGrp.Wait()
		if err != nil {
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
	cmdType := utils.CmdType(r.config.CommandType)
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
			testSetStatus, err := r.RunTestSet(ctx, testSet, testRunID, false)
			if err != nil {
				if ctx.Err() == context.Canceled {
					return err
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
	// Shutdown is optional: the static Telemetry interface does not require it,
	// but the concrete implementation exposes it for graceful drain of in-flight events.
	if s, ok := r.telemetry.(interface{ Shutdown() }); ok {
		s.Shutdown()
	}

	if !abortTestRun {
		r.printSummary(ctx, testRunResult)

		if !testRunResult && len(r.mockMismatchFailures.GetFailures()) > 0 && !r.config.DisableMapping {
			failuresByTestSet := make(map[string]bool)
			for _, failure := range r.mockMismatchFailures.GetFailures() {
				failuresByTestSet[failure.TestSetID] = true
			}

			var testSetIDs []string
			for testSetID := range failuresByTestSet {
				testSetIDs = append(testSetIDs, testSetID)
			}
			testSets := strings.Join(testSetIDs, ", ")
			r.logger.Info("Some testsets failed due to mock differences. Please kindly rerecord these testsets to update the mocks.", zap.String("command", fmt.Sprintf("keploy rerecord -c '%s' -t %s", r.config.Command, testSets)))

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

	err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, CommandType: r.config.CommandType, DockerDelay: r.config.BuildDelay, Mode: models.MODE_TEST, BuildDelay: r.config.BuildDelay, EnableTesting: true, GlobalPassthrough: r.config.Record.GlobalPassthrough, ConfigPath: r.config.ConfigPath, PassThroughPorts: passPortsUint, InMemoryCompose: r.config.InMemoryCompose})
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
			if err := r.instrumentation.NotifyGracefulShutdown(context.Background()); err != nil {
				r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
			}
		}
		runTestSetCtxCancel()
		err := runTestSetErrGrp.Wait()
		if err != nil {
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
	addKinds := func(mocks []*models.Mock) {
		for _, m := range mocks {
			mockKindByName[m.Name] = m.Kind
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

		agentCtx, cancel := context.WithTimeout(runTestSetCtx, 120*time.Second)
		defer cancel()

		agentReadyCh := make(chan bool, 1)
		go pkg.AgentHealthTicker(agentCtx, r.logger, string(r.config.Agent.AgentURI), agentReadyCh, 1*time.Second)

		select {
		case <-runTestSetCtx.Done():
			// Parent context cancelled (user pressed Ctrl+C)
			return models.TestSetStatusUserAbort, runTestSetCtx.Err()
		case <-agentCtx.Done():
			return models.TestSetStatusFailed, fmt.Errorf("keploy-agent did not become ready in time")
		case <-agentReadyCh:
		}

		// In case of Docker Compose : since for every test set the agent and application are restarted, hence each test set can be considered as an indicidual test run
		// We also need the firstRun for knowing the first test set run in the whole test mode for purpose like cleanup
		err := r.hookImpl.BeforeTestSetCompose(ctx, testRunID, r.firstRun)
		if err != nil {
			stopReason := fmt.Sprintf("failed to run BeforeTestSetCompose hook: %v", err)
			utils.LogError(r.logger, err, stopReason)
		}
		r.firstRun = false
		// Prepare header noise configuration for mock matching
		headerNoiseConfig := PrepareHeaderNoiseConfig(r.config.Test.GlobalNoise.Global, r.config.Test.GlobalNoise.Testsets, testSetID)

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
			NoiseConfig:            headerNoiseConfig,
			DisableAutoHeaderNoise: r.config.Test.DisableAutoHeaderNoise,
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
		if !waitForAppReady(runTestSetCtx, r.logger, r.config) {
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

		// Prepare header noise configuration for mock matching
		headerNoiseConfig := PrepareHeaderNoiseConfig(r.config.Test.GlobalNoise.Global, r.config.Test.GlobalNoise.Testsets, testSetID)

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
			NoiseConfig:            headerNoiseConfig,
			DisableAutoHeaderNoise: r.config.Test.DisableAutoHeaderNoise,
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
			if !waitForAppReady(runTestSetCtx, r.logger, r.config) {
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

			// replace the request URL's BasePath/origin if provided
			if r.config.Test.BasePath != "" {
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

			// Checking for errors in the mocking and application
			select {
			case <-exitLoopChan:
				testSetStatus = getErrStatus()
				exitLoop = true
			default:
			}

			if exitLoop {
				break
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

			testCaseProxyErrCtx, testCaseProxyErrCancel := context.WithCancel(runTestSetCtx)
			go r.monitorProxyErrors(testCaseProxyErrCtx, testSetID, testCase.Name)

			resp, loopErr := r.hookImpl.SimulateRequest(runTestSetCtx, testCase, testSetID)

			// Stop monitoring for this specific test case
			testCaseProxyErrCancel()

			if loopErr != nil {
				utils.LogError(r.logger, loopErr, "failed to simulate request")
				currentFailures++
				testSetStatus = models.TestSetStatusFailed
				testCaseResult := r.CreateFailedTestResult(testCase, testSetID, started, loopErr.Error())
				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, "failed to insert test case result for simulation error")
					break
				}
				continue
			}

			if r.instrument {
				consumedMocks, err = r.hookImpl.GetConsumedMocks(runTestSetCtx)
				if err != nil {
					if resolvedStatus, ok := resolveTestSetStatus(cmdType, testSetStatus, getErrStatus(), err); ok {
						testSetStatus = resolvedStatus
						exitLoop = true
					} else {
						utils.LogError(r.logger, err, "failed to get consumed filtered mocks")
					}
				}
				r.logger.Debug("consumed mocks after test case simulation",
					zap.String("testSetID", testSetID),
					zap.String("testCaseID", testCase.Name),
					zap.Int("count", len(consumedMocks)),
					zap.Any("mocks", consumedMocks))
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
			filteredExpectedNames := make([]string, 0, len(expectedMocks))
			for _, m := range expectedMocks {
				isDNS := strings.EqualFold(m.Kind, string(models.DNS))
				if !isDNS {
					if kind, ok := mockKindByName[m.Name]; ok && kind == models.DNS {
						isDNS = true
					}
				}
				if !isDNS {
					filteredExpectedNames = append(filteredExpectedNames, m.Name)
				}
			}

			filteredMockNames := make([]string, 0, len(consumedMocks))
			for _, m := range consumedMocks {
				if m.Kind != models.DNS {
					filteredMockNames = append(filteredMockNames, m.Name)
				}
			}

			mockSetMismatch := false
			if r.instrument && useMappingBased && isMappingEnabled && hasExpectedMocks {
				// Filter out DNS mocks from comparison since DNS resolution order is non-deterministic
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

			if mockSetMismatch {
				if testPass {
					r.logger.Debug("mock mapping mismatch ignored because testcase passed",
						zap.String("testcase", testCase.Name),
						zap.String("testset", testSetID),
						zap.Strings("expectedMocks", filteredExpectedNames),
						zap.Strings("actualMocks", filteredMockNames))
				} else {
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
			} else if mockSetMismatch {
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
					// Populate matched/unmatched calls for failed/obsolete test cases
					if testStatus == models.TestStatusFailed || testStatus == models.TestStatusObsolete {
						// In non-instrument mode (k8s-proxy), fetch consumed mocks for this test case via HTTP API
						matchedMocks := consumedMocks
						if !r.instrument {
							if fetched, err := r.hookImpl.GetConsumedMocks(runTestSetCtx); err == nil {
								matchedMocks = fetched
							}
						}
						for _, m := range matchedMocks {
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
						// Populate unmatched calls from mock errors (channel-based for in-process)
						for _, f := range r.mockMismatchFailures.GetFailuresForTestCase(testSetID, testCase.Name) {
							if f.MismatchReport != nil {
								testCaseResult.FailureInfo.UnmatchedCalls = append(testCaseResult.FailureInfo.UnmatchedCalls, models.UnmatchedCall{
									Protocol:      f.MismatchReport.Protocol,
									ActualSummary: f.MismatchReport.ActualSummary,
									ClosestMock:   f.MismatchReport.ClosestMock,
									Diff:          f.MismatchReport.Diff,
									NextSteps:     f.MismatchReport.NextSteps,
								})
							}
						}
						// Fetch mock errors via HTTP API (for remote agent / k8s-proxy mode only)
						if !r.instrument {
							if mockErrors, err := r.instrumentation.GetMockErrors(runTestSetCtx); err == nil {
								for _, me := range mockErrors {
									testCaseResult.FailureInfo.UnmatchedCalls = append(testCaseResult.FailureInfo.UnmatchedCalls, me)
								}
							}
						}
					}
					if mockSetMismatch && testStatus == models.TestStatusObsolete {
						expectedMockInfos := make([]models.MockMismatchMock, 0, len(expectedMocks))
						for _, m := range expectedMocks {
							isDNS := strings.EqualFold(m.Kind, string(models.DNS))
							if !isDNS {
								if kind, ok := mockKindByName[m.Name]; ok && kind == models.DNS {
									isDNS = true
								}
							}
							if !isDNS {
								resolvedKind := m.Kind
								if resolvedKind == "" {
									if kind, ok := mockKindByName[m.Name]; ok {
										resolvedKind = string(kind)
									}
								}
								expectedMockInfos = append(expectedMockInfos, models.MockMismatchMock{Name: m.Name, Kind: resolvedKind})
							}
						}
						actualMockInfos := make([]models.MockMismatchMock, 0, len(consumedMocks))
						for _, m := range consumedMocks {
							if m.Kind != models.DNS {
								actualMockInfos = append(actualMockInfos, models.MockMismatchMock{Name: m.Name, Kind: string(m.Kind)})
							}
						}
						testCaseResult.FailureInfo.MockMismatch = &models.MockMismatchInfo{
							ExpectedMocks: expectedMockInfos,
							ActualMocks:   actualMockInfos,
						}
					}
					finalTestCaseResults[testCase.Name] = testCaseResult
				} else {
					utils.LogError(r.logger, nil, "test case result is nil")
					break
				}
			} else {
				utils.LogError(r.logger, nil, "test result is nil")
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

			// Mock Window: Calculate the effective mock filter window for streaming
			// using the request timestamp to the response timestamp plus a timeout buffer.
			streamReqTime, streamRespTime := effectiveStreamMockWindow(tc, r.config.Test.APITimeout)
			err = r.SendMockFilterParamsToAgent(runTestSetCtx, deferred.expectedMocks, streamReqTime, streamRespTime, totalConsumedMocks, useMappingBased)
			if err != nil {
				utils.LogError(r.logger, err, "failed to update mock parameters for streaming test")
				loopErr = err
				break
			}

			// Proxy Monitor: Start a per-test proxy error monitor.
			streamProxyErrCtx, streamProxyErrCancel := context.WithCancel(runTestSetCtx)
			go r.monitorProxyErrors(streamProxyErrCtx, testSetID, tc.Name)

			// Execute: SimulateRequest returns once response headers arrive;
			// for streaming cases the body reader is drained later by
			// CompareHTTPStream below, so in-stream mock consumption can
			// continue after this call returns.
			started := time.Now().UTC()
			resp, simErr := r.hookImpl.SimulateRequest(runTestSetCtx, tc, testSetID)

			// Cleanup: Cancel the proxy error monitor immediately after simulation.
			streamProxyErrCancel()

			if simErr != nil {
				utils.LogError(r.logger, simErr, "failed to simulate streaming request")
				failure++
				testSetStatus = models.TestSetStatusFailed
				testCaseResult := r.CreateFailedTestResult(tc, testSetID, started, simErr.Error())
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
			} else if mockSetMismatch {
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

				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, "failed to save streaming test result")
					break
				}
			} else {
				utils.LogError(r.logger, nil, "streaming test result is nil")
				break
			}
		}
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

	// remove the unused mocks by the test cases of a testset (if the base path is not provided )
	// When PreserveFailedMocks is enabled (k8s-proxy autoreplay), skip pruning if any test
	// failed or was marked obsolete so all recorded mocks remain available for UI inspection.
	skipPruning := r.config.Test.PreserveFailedMocks && (failure > 0 || obsolete > 0)
	if r.config.Test.RemoveUnusedMocks && r.instrument && !skipPruning {
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

		// Find the earliest test-case timestamp so UpdateMocks can exempt
		// startup/init mocks (recorded before any test case) from deletion.
		var firstTestCaseTime time.Time
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

			if !candidate.IsZero() && (firstTestCaseTime.IsZero() || candidate.Before(firstTestCaseTime)) {
				firstTestCaseTime = candidate
			}
		}

		err = r.mockDB.UpdateMocks(runTestSetCtx, testSetID, passingTotalConsumedMocks, pruneBefore, firstTestCaseTime)
		if err != nil {
			utils.LogError(r.logger, err, "failed to delete unused mocks")
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

	// Merge mock NAMES (not the count) into the run-level distinct
	// set so duplicates across test sets aren't double-counted.
	r.completeTestReportMu.Lock()
	for name := range totalConsumedMocks {
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

// monitorProxyErrors monitors the proxy error channel and logs errors
func (r *Replayer) monitorProxyErrors(ctx context.Context, testSetID string, testCaseID string) {
	defer utils.Recover(r.logger)

	errorChannel := r.instrumentation.GetErrorChannel()
	if errorChannel == nil {
		r.logger.Debug("Proxy error channel is nil, skipping error monitoring")
		return
	}

	r.logger.Debug("Starting proxy error monitoring",
		zap.String("testSetID", testSetID),
		zap.String("testCaseID", testCaseID))

	for {
		select {
		case <-ctx.Done():
			r.logger.Debug("Stopping proxy error monitoring",
				zap.String("testSetID", testSetID),
				zap.String("testCaseID", testCaseID))
			return
		case proxyErr, ok := <-errorChannel:
			if !ok {
				r.logger.Debug("Proxy error channel closed",
					zap.String("testSetID", testSetID),
					zap.String("testCaseID", testCaseID))
				return
			}

			// Determine effective test case ID
			effectiveTestCaseID := testCaseID
			if effectiveTestCaseID == "" {
				effectiveTestCaseID = UNKNOWN_TEST
			}

			if parserErr, ok := proxyErr.(models.ParserError); ok {
				// Handle typed ParserError
				switch parserErr.ParserErrorType {
				case models.ErrMockNotFound:
					r.mockMismatchFailures.AddProxyErrorForTest(testSetID, effectiveTestCaseID, parserErr)
				}
			}

		}
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
