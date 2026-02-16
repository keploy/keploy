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
	"go.keploy.io/server/v3/pkg/service"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

var (
	completeTestReport   = make(map[string]TestReportVerdict)
	firstRun             bool
	completeTestReportMu sync.RWMutex
	totalTests           int
	totalTestPassed      int
	totalTestFailed      int
	totalTestObsolete    int
	totalTestIgnored     int
	totalTestTimeTaken   time.Duration
)
var failedTCsBySetID = make(map[string][]string)
var mockMismatchFailures = NewTestFailureStore()

const UNKNOWN_TEST = "UNKNOWN_TEST"

var HookImpl TestHooks

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
	auth            service.Auth
	mock            *mock
	instrument      bool
	isLastTestSet   bool
	isLastTestCase  bool
	runDomainSet    *telemetry.DomainSet // collects host domains across a test run for telemetry
}

func NewReplayer(logger *zap.Logger, testDB TestDB, mockDB MockDB, reportDB ReportDB, mappingDB MappingDB, testSetConf TestSetConfig, telemetry Telemetry, instrumentation Instrumentation, auth service.Auth, storage Storage, config *config.Config) Service {

	// TODO: add some comment.
	mock := &mock{
		cfg:        config,
		storage:    storage,
		logger:     logger,
		tsConfigDB: testSetConf,
	}

	// set the request emulator for simulating test case requests, if not set
	if HookImpl == nil {
		SetTestHooks(NewHooks(logger, config, testSetConf, storage, auth, instrumentation, mock))
	}

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
		auth:            auth,
		mock:            mock,
	}
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

	completeTestReportMu.Lock()
	completeTestReport = make(map[string]TestReportVerdict)
	totalTests, totalTestPassed, totalTestFailed, totalTestObsolete, totalTestIgnored = 0, 0, 0, 0, 0
	totalTestTimeTaken = 0
	completeTestReportMu.Unlock()
	failedTCsBySetID = make(map[string][]string)
	mockMismatchFailures = NewTestFailureStore()

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
				r.logger.Warn("failed to detect language, skipping coverage caluclation. please use --language to manually set the language")
				r.config.Test.SkipCoverage = true
			} else {
				r.logger.Warn(fmt.Sprintf("%s language detected. please use --language to manually set the language if needed", language))
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
			r.logger.Warn("python command not python or python3, skipping coverage calculation")
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
			r.logger.Warn("failed to set CLEAN env variable, skipping coverage caluclation", zap.Error(err))
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
	firstRun = true
	for i, testSet := range testSets {
		var backupCreated bool
		testSetResult = false

		err := HookImpl.BeforeTestSetRun(ctx, testSet)
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
				r.logger.Warn("failed to set TESTSETID env variable, skipping coverage caluclation", zap.Error(err))
			}
		}

		// check if its the last testset running -

		if i == len(testSets)-1 {
			r.isLastTestSet = true
		}

		completeTestReportMu.RLock()
		initTotal := totalTests
		initPassed := totalTestPassed
		initFailed := totalTestFailed
		initObsolete := totalTestObsolete
		initIgnored := totalTestIgnored
		initTimeTaken := totalTestTimeTaken
		completeTestReportMu.RUnlock()

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
			completeTestReportMu.Lock()
			totalTests = initTotal
			totalTestPassed = initPassed
			totalTestFailed = initFailed
			totalTestObsolete = initObsolete
			totalTestIgnored = initIgnored
			totalTestTimeTaken = initTimeTaken
			completeTestReportMu.Unlock()

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
				abortTestRun = true
			case models.TestSetStatusInternalErr:
				testSetResult = false
				abortTestRun = true
			case models.TestSetStatusFaultUserApp:
				testSetResult = false
				abortTestRun = true
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
			failedTCsBySetID[testSet] = failedTcIDs

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

		err = HookImpl.AfterTestSetRun(ctx, testSet, testSetResult)
		if err != nil {
			utils.LogError(r.logger, err, "failed to execute after test set run hook", zap.String("testSet", testSet))
		}

		if i == 0 && !r.config.Test.SkipCoverage {
			err = os.Setenv("CLEAN", "false") // related to javascript coverage calculation
			if err != nil {
				r.config.Test.SkipCoverage = true
				r.logger.Warn("failed to set CLEAN env variable, skipping coverage caluclation.", zap.Error(err))
			}
			err = os.Setenv("APPEND", "--append") // related to python coverage calculation
			if err != nil {
				r.config.Test.SkipCoverage = true
				r.logger.Warn("failed to set APPEND env variable, skipping coverage caluclation.", zap.Error(err))
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
		r.logger.Warn("flaky testsets detected, please rerun the specific testsets with --must-pass flag to remove flaky testcases", zap.Strings("testSets", flakyTestSets))
	}

	testRunStatus := "fail"
	if testRunResult {
		testRunStatus = "pass"
	}

	if testRunResult && r.config.Test.DisableMockUpload {
		r.logger.Warn("To enable storing mocks in cloud, please use --disableMockUpload=false flag or test:disableMockUpload:false in config file")
	}

	completeTestReportMu.RLock()
	passed := totalTestPassed
	failed := totalTestFailed
	completeTestReportMu.RUnlock()
	r.telemetry.TestRun(passed, failed, len(testSets), testRunStatus, map[string]interface{}{
		"host-domains": runDomainSet.ToSlice(),
	})
	// Shutdown is optional: the static Telemetry interface does not require it,
	// but the concrete implementation exposes it for graceful drain of in-flight events.
	if s, ok := r.telemetry.(interface{ Shutdown() }); ok {
		s.Shutdown()
	}

	if !abortTestRun {
		r.printSummary(ctx, testRunResult)

		if !testRunResult && len(mockMismatchFailures.GetFailures()) > 0 && !r.config.DisableMapping {
			failuresByTestSet := make(map[string]bool)
			for _, failure := range mockMismatchFailures.GetFailures() {
				failuresByTestSet[failure.TestSetID] = true
			}

			var testSetIDs []string
			for testSetID := range failuresByTestSet {
				testSetIDs = append(testSetIDs, testSetID)
			}
			testSets := strings.Join(testSetIDs, ", ")
			r.logger.Warn("Some testsets failed due to mock differences. Please kindly rerecord these testsets to update the mocks.", zap.String("command", fmt.Sprintf("keploy rerecord -c '%s' -t %s", r.config.Command, testSets)))

			if r.config.Debug {
				mockMismatchFailures.PrintFailuresTable()
			}
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
				r.logger.Warn("failed to calculate coverage for the test run", zap.Error(err))
			}
		}

		//executing afterTestRun hook, executed after running all the test-sets
		err = HookImpl.AfterTestRun(ctx, testRunID, testSets, coverageData)
		if err != nil {
			utils.LogError(r.logger, err, "failed to execute after test run hook")
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

	err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, CommandType: r.config.CommandType, DockerDelay: r.config.BuildDelay, Mode: models.MODE_TEST, BuildDelay: r.config.BuildDelay, EnableTesting: true, GlobalPassthrough: r.config.Record.GlobalPassthrough, ConfigPath: r.config.ConfigPath, PassThroughPorts: passPortsUint})
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
		r.logger.Warn("no valid test cases found to run for test set", zap.String("test-set", testSetID))

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

		completeTestReportMu.Lock()
		completeTestReport[testSetID] = verdict
		totalTests += testReport.Total
		totalTestIgnored += testReport.Ignored
		totalTestTimeTaken += timeTaken
		completeTestReportMu.Unlock()

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

	testSetStatus := models.TestSetStatusPassed
	testSetStatusByErrChan := models.TestSetStatusRunning

	cmdType := utils.CmdType(r.config.CommandType)
	// Check if mappings are present and decide filtering strategy
	var expectedTestMockMappings map[string][]string
	var useMappingBased bool
	var isMappingEnabled bool
	isMappingEnabled = !r.config.DisableMapping
	selectedTests := matcherUtils.ArrayToMap(r.config.Test.SelectedTests[testSetID])

	if r.instrument && cmdType == utils.DockerCompose {
		if !serveTest {
			runTestSetErrGrp.Go(func() error {
				defer utils.Recover(r.logger)
				appErr = r.RunApplication(runTestSetCtx, models.RunOptions{
					AppCommand: conf.AppCommand,
				})
				if (appErr.AppErrorType == models.ErrCtxCanceled || appErr == models.AppError{}) {
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
				switch err.AppErrorType {
				case models.ErrCommandError:
					testSetStatusByErrChan = models.TestSetStatusFaultUserApp
				case models.ErrUnExpected:
					testSetStatusByErrChan = models.TestSetStatusAppHalted
				case models.ErrAppStopped:
					testSetStatusByErrChan = models.TestSetStatusAppHalted
				case models.ErrCtxCanceled:
					return nil
				case models.ErrInternal:
					testSetStatusByErrChan = models.TestSetStatusInternalErr
				default:
					testSetStatusByErrChan = models.TestSetStatusAppHalted
				}
				utils.LogError(r.logger, err, "application failed to run")
			case <-runTestSetCtx.Done():
				testSetStatusByErrChan = models.TestSetStatusUserAbort
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
		err := HookImpl.BeforeTestSetCompose(ctx, testRunID, firstRun)
		if err != nil {
			stopReason := fmt.Sprintf("failed to run BeforeTestSetCompose hook: %v", err)
			utils.LogError(r.logger, err, stopReason)
		}
		firstRun = false
		// Prepare header noise configuration for mock matching
		headerNoiseConfig := PrepareHeaderNoiseConfig(r.config.Test.GlobalNoise.Global, r.config.Test.GlobalNoise.Testsets, testSetID)

		err = r.instrumentation.MockOutgoing(runTestSetCtx, models.OutgoingOptions{
			Rules:          r.config.BypassRules,
			MongoPassword:  r.config.Test.MongoPassword,
			SQLDelay:       time.Duration(r.config.Test.Delay),
			FallBackOnMiss: r.config.Test.FallBackOnMiss,
			Mocking:        r.config.Test.Mocking,
			Backdate:       testCases[0].HTTPReq.Timestamp,
			NoiseConfig:    headerNoiseConfig,
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
					mocksThatHaveMappings[m] = true
				}
			}

			if len(selectedTests) > 0 {
				for testID := range selectedTests {
					if mocks, ok := expectedTestMockMappings[testID]; ok {
						for _, m := range mocks {
							mocksWeNeed[m] = true
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
			r.logger.Warn("no mocks found for test set", zap.String("testSetID", testSetID))
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

		// Delay for user application to run
		select {
		case <-time.After(time.Duration(r.config.Test.Delay) * time.Second):
		case <-runTestSetCtx.Done():
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
					mocksThatHaveMappings[m] = true
				}
			}

			if len(selectedTests) > 0 {
				for testID := range selectedTests {
					if mocks, ok := expectedTestMockMappings[testID]; ok {
						for _, m := range mocks {
							mocksWeNeed[m] = true
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
		if firstRun {
			err = HookImpl.BeforeTestRun(ctx, testRunID)
			if err != nil {
				stopReason := fmt.Sprintf("failed to run before test run hook: %v", err)
				utils.LogError(r.logger, err, stopReason)
			}
			firstRun = false
		}
		isMappingEnabled := !r.config.DisableMapping

		if !isMappingEnabled {
			r.logger.Debug("Mapping-based mock filtering strategy is disabled, using timestamp-based mock filtering strategy")
		}

		pkg.InitSortCounter(int64(max(len(filteredMocks), len(unfilteredMocks))))

		// Prepare header noise configuration for mock matching
		headerNoiseConfig := PrepareHeaderNoiseConfig(r.config.Test.GlobalNoise.Global, r.config.Test.GlobalNoise.Testsets, testSetID)

		err = r.instrumentation.MockOutgoing(runTestSetCtx, models.OutgoingOptions{
			Rules:          r.config.BypassRules,
			MongoPassword:  r.config.Test.MongoPassword,
			SQLDelay:       time.Duration(r.config.Test.Delay),
			FallBackOnMiss: r.config.Test.FallBackOnMiss,
			Mocking:        r.config.Test.Mocking,
			Backdate:       testCases[0].HTTPReq.Timestamp,
			NoiseConfig:    headerNoiseConfig,
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
					if appErr.AppErrorType == models.ErrCtxCanceled {
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
					switch err.AppErrorType {
					case models.ErrCommandError:
						testSetStatusByErrChan = models.TestSetStatusFaultUserApp
					case models.ErrUnExpected:
						testSetStatusByErrChan = models.TestSetStatusAppHalted
					case models.ErrAppStopped:
						testSetStatusByErrChan = models.TestSetStatusAppHalted
					case models.ErrCtxCanceled:
						return nil
					case models.ErrInternal:
						testSetStatusByErrChan = models.TestSetStatusInternalErr
					default:
						testSetStatusByErrChan = models.TestSetStatusAppHalted
					}
					utils.LogError(r.logger, err, "application failed to run")
				case <-runTestSetCtx.Done():
					testSetStatusByErrChan = models.TestSetStatusUserAbort
				}
				exitLoopChan <- true
				runTestSetCtxCancel()
				return nil
			})

			// Delay for user application to run
			select {
			case <-time.After(time.Duration(r.config.Test.Delay) * time.Second):
			case <-runTestSetCtx.Done():
				return models.TestSetStatusUserAbort, context.Canceled
			}

		}
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
			r.logger.Warn("Failed to add secret files to .gitignore", zap.Error(err))
		}
	}

	actualTestMockMappings := &models.Mapping{
		Version:   string(models.GetVersion()),
		Kind:      models.MappingKind,
		TestSetID: testSetID,
	}
	var consumedMocks []models.MockState
	consumedMocks, err = HookImpl.GetConsumedMocks(runTestSetCtx) // Getting mocks consumed during initial setup
	if err != nil {
		utils.LogError(r.logger, err, "failed to get consumed filtered mocks")
	}
	for _, m := range consumedMocks {
		totalConsumedMocks[m.Name] = m
	}

	for idx, testCase := range testCases {

		// check if its the last test case running
		if idx == len(testCases)-1 && r.isLastTestSet {
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
				utils.LogError(r.logger, err, "failed to insert test case result")
				break
			}
			ignored++
			continue
		}

		// replace the request URL's BasePath/origin if provided
		if r.config.Test.BasePath != "" {
			newURL, err := ReplaceBaseURL(r.config.Test.BasePath, testCase.HTTPReq.URL)
			if err != nil {
				r.logger.Warn("failed to replace the request basePath", zap.String("testcase", testCase.Name), zap.String("basePath", r.config.Test.BasePath), zap.Error(err))
			} else {
				testCase.HTTPReq.URL = newURL
			}
			r.logger.Debug("test case request origin", zap.String("testcase", testCase.Name), zap.String("TestCaseURL", testCase.HTTPReq.URL), zap.String("basePath", r.config.Test.BasePath))
		}

		// Checking for errors in the mocking and application
		select {
		case <-exitLoopChan:
			testSetStatus = testSetStatusByErrChan
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

		err = r.SendMockFilterParamsToAgent(runTestSetCtx, expectedTestMockMappings[testCase.Name], reqTime, respTime, totalConsumedMocks, useMappingBased)
		if err != nil {
			utils.LogError(r.logger, err, "failed to update mock parameters on agent")
			break
		}

		// Handle host replacement - use user-provided host or default to localhost
		// This is necessary because the agent architecture doesn't intercept the test runner's
		// network requests (unlike the eBPF approach in v2), so we need to explicitly
		// replace the recorded hostname with a reachable address.
		hostToUse := r.config.Test.Host
		if hostToUse == "" {
			hostToUse = "localhost"
		}
		err = r.replaceHostInTestCase(testCase, hostToUse, "target host")
		if err != nil {
			break
		}

		// Handle user-provided http port replacement
		if r.config.Test.Port != 0 && testCase.Kind == models.HTTP {
			err = r.replacePortInTestCase(testCase, strconv.Itoa(int(r.config.Test.Port)))
			if err != nil {
				break
			}
		}

		// Handle user-provided grpc port replacement
		if r.config.Test.GRPCPort != 0 && testCase.Kind == models.GRPC_EXPORT {
			err = r.replacePortInTestCase(testCase, strconv.Itoa(int(r.config.Test.GRPCPort)))
			if err != nil {
				break
			}
		}

		started := time.Now().UTC()

		testCaseProxyErrCtx, testCaseProxyErrCancel := context.WithCancel(runTestSetCtx)
		go r.monitorProxyErrors(testCaseProxyErrCtx, testSetID, testCase.Name)

		resp, loopErr := HookImpl.SimulateRequest(runTestSetCtx, testCase, testSetID)

		// Stop monitoring for this specific test case
		testCaseProxyErrCancel()

		if loopErr != nil {
			utils.LogError(r.logger, loopErr, "failed to simulate request")
			failure++
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
			consumedMocks, err = HookImpl.GetConsumedMocks(runTestSetCtx)
			if err != nil {
				utils.LogError(r.logger, err, "failed to get consumed filtered mocks")
			}
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
		mockSetMismatch := false
		if r.instrument && useMappingBased && isMappingEnabled && hasExpectedMocks {
			mockSetMismatch = !isMockSubset(mockNames, expectedMocks)
		}

		emitFailureLogs := !mockSetMismatch

		switch testCase.Kind {
		case models.HTTP:
			httpResp, ok := resp.(*models.HTTPResp)
			if !ok {
				r.logger.Error("invalid response type for HTTP test case")
				failure++
				testSetStatus = models.TestSetStatusFailed
				testCaseResult := r.CreateFailedTestResult(testCase, testSetID, started, "invalid response type for HTTP test case")
				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, fmt.Sprintf("failed to insert test case result for type assertion error in %s test case", testCase.Kind))
					break
				}
				continue
			}
			testPass, testResult = r.CompareHTTPResp(testCase, httpResp, testSetID, emitFailureLogs)

		case models.GRPC_EXPORT:
			grpcResp, ok := resp.(*models.GrpcResp)
			if !ok {
				r.logger.Error("invalid response type for gRPC test case")
				failure++
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

		if len(mockNames) > 0 {
			found := false
			for i, t := range actualTestMockMappings.Tests {
				if t.ID == testCase.Name {
					actualTestMockMappings.Tests[i].Mocks = models.FromSlice(append(actualTestMockMappings.Tests[i].Mocks.ToSlice(), mockNames...))
					found = true
					break
				}
			}
			if !found {
				actualTestMockMappings.Tests = append(actualTestMockMappings.Tests, models.Test{
					ID:    testCase.Name,
					Mocks: models.FromSlice(mockNames),
				})
			}
		}

		// log the consumed mocks during the test run of the test case for test set
		r.logger.Debug("Consumed Mocks", zap.Any("mocks", consumedMocks))

		if mockSetMismatch {
			if testPass {
				r.logger.Debug("mock mapping mismatch ignored because testcase passed",
					zap.String("testcase", testCase.Name),
					zap.String("testset", testSetID),
					zap.Strings("expectedMocks", expectedMocks),
					zap.Strings("actualMocks", mockNames))
			} else {
				r.logger.Error("mock mapping mismatch detected; marking testcase as obsolete",
					zap.String("testcase", testCase.Name),
					zap.String("testset", testSetID),
					zap.Strings("expectedMocks", expectedMocks),
					zap.Strings("actualMocks", mockNames))
				mockMismatchFailures.AddFailure(testSetID, testCase.Name, expectedMocks, mockNames)
			}
		}

		if !testPass {
			r.logger.Info("result", zap.String("testcase id", models.HighlightFailingString(testCase.Name)), zap.String("testset id", models.HighlightFailingString(testSetID)), zap.String("passed", models.HighlightFailingString(testPass)))
		} else {
			r.logger.Info("result", zap.String("testcase id", models.HighlightPassingString(testCase.Name)), zap.String("testset id", models.HighlightPassingString(testSetID)), zap.String("passed", models.HighlightPassingString(testPass)))
		}
		if testPass {
			testStatus = models.TestStatusPassed
			success++
		} else if mockSetMismatch {
			testStatus = models.TestStatusObsolete
			obsolete++
		} else {
			testStatus = models.TestStatusFailed
			failure++
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
				}
				insertStart := time.Now()
				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if time.Since(insertStart) > 50*time.Millisecond {
					r.logger.Warn("Slow InsertTestCaseResult", zap.Duration("duration", time.Since(insertStart)))
				}
				if loopErr != nil {
					utils.LogError(r.logger, loopErr, "failed to insert test case result")
					break
				}
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

	timeTaken := time.Since(startTime)

	testCaseResults, err := r.reportDB.GetTestCaseResults(runTestSetCtx, testRunID, testSetID)
	if err != nil {
		if runTestSetCtx.Err() != context.Canceled {
			utils.LogError(r.logger, err, "failed to get test case results")
			testSetStatus = models.TestSetStatusInternalErr
		}
	}

	err = HookImpl.BeforeTestResult(ctx, testRunID, testSetID, testCaseResults)
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
		testSetStatus = models.TestSetStatusInternalErr
	} else {
		// Checking for errors in the mocking and application
		select {
		case <-exitLoopChan:
			testSetStatus = testSetStatusByErrChan
		default:
		}
	}

	testReport = &models.TestReport{
		Version:    models.GetVersion(),
		TestSet:    testSetID,
		Status:     string(testSetStatus),
		Total:      testCasesCount,
		Success:    success,
		Failure:    failure,
		Obsolete:   obsolete,
		Ignored:    ignored,
		Tests:      testCaseResults,
		TimeTaken:  timeTaken.String(),
		HighRisk:   riskHigh,
		MediumRisk: riskMed,
		LowRisk:    riskLow,
		CmdUsed:    r.config.Test.CmdUsed,
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

	// remove the unused mocks by the test cases of a testset (if the base path is not provided )
	if r.config.Test.RemoveUnusedMocks && testSetStatus == models.TestSetStatusPassed && obsolete == 0 && r.instrument {
		r.logger.Debug("consumed mocks from the completed testset", zap.String("for test-set", testSetID), zap.Any("consumed mocks", totalConsumedMocks))
		// delete the unused mocks from the data store
		r.logger.Info("deleting unused mocks from the data store", zap.String("for test-set", testSetID))
		err = r.mockDB.UpdateMocks(runTestSetCtx, testSetID, totalConsumedMocks)
		if err != nil {
			utils.LogError(r.logger, err, "failed to delete unused mocks")
		}
	}

	if testSetStatus == models.TestSetStatusPassed && obsolete == 0 && r.instrument && isMappingEnabled {
		if err := r.StoreMappings(ctx, actualTestMockMappings); err != nil {
			r.logger.Error("Error saving test-mock mappings to YAML file", zap.Error(err))
		} else {
			r.logger.Info("Successfully saved test-mock mappings",
				zap.String("testSetID", testSetID),
				zap.Int("numTests", len(actualTestMockMappings.Tests)))
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

	completeTestReportMu.Lock()
	completeTestReport[testSetID] = verdict
	totalTests += testReport.Total
	totalTestPassed += testReport.Success
	totalTestFailed += testReport.Failure
	totalTestObsolete += testReport.Obsolete
	totalTestIgnored += testReport.Ignored
	totalTestTimeTaken += timeTaken
	completeTestReportMu.Unlock()

	timeTakenStr := timeWithUnits(timeTaken)

	if testSetStatus == models.TestSetStatusFailed || testSetStatus == models.TestSetStatusPassed {

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

	// Build filter parameters
	params := models.MockFilterParams{
		AfterTime:          afterTime,
		BeforeTime:         beforeTime,
		MockMapping:        expectedMockMapping,
		UseMappingBased:    useMappingBased,
		TotalConsumedMocks: totalConsumedMocks,
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
	noiseConfig := r.config.Test.GlobalNoise.Global
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
		noiseConfig = LeftJoinNoise(r.config.Test.GlobalNoise.Global, tsNoise)
	}

	if r.config.Test.SchemaMatch {
		return httpMatcher.MatchSchema(tc, actualResponse, r.logger)
	}

	return httpMatcher.Match(tc, actualResponse, noiseConfig, r.config.Test.IgnoreOrdering, r.config.Test.CompareAll, r.logger, emitFailureLogs)
}

func (r *Replayer) CompareGRPCResp(tc *models.TestCase, actualResp *models.GrpcResp, testSetID string, emitFailureLogs bool) (bool, *models.Result) {
	noiseConfig := r.config.Test.GlobalNoise.Global
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
		noiseConfig = LeftJoinNoise(r.config.Test.GlobalNoise.Global, tsNoise)
	}

	return grpcMatcher.Match(tc, actualResp, noiseConfig, r.config.Test.IgnoreOrdering, r.logger, emitFailureLogs)
}

func (r *Replayer) printSummary(_ context.Context, _ bool) {
	completeTestReportMu.RLock()
	totalTestsSnapshot := totalTests
	totalTestPassedSnapshot := totalTestPassed
	totalTestFailedSnapshot := totalTestFailed
	totalTestObsoleteSnapshot := totalTestObsolete
	totalTestIgnoredSnapshot := totalTestIgnored
	totalTestTimeTakenSnapshot := totalTestTimeTaken
	reportSnapshot := make(map[string]TestReportVerdict, len(completeTestReport))
	for key, val := range completeTestReport {
		reportSnapshot[key] = val
	}
	completeTestReportMu.RUnlock()

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
					if failedCases, ok := failedTCsBySetID[testSuiteName]; ok && len(failedCases) > 0 {
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
					if failedCases, ok := failedTCsBySetID[testSuiteName]; ok && len(failedCases) > 0 {
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
// It preserves existing pre/post scripts, secret, mock registry and metadata fields.
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
		ts.MockRegistry = existing.MockRegistry
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

	cmdErr := utils.ExecuteCommand(ctx, r.logger, script, cmdCancel, 25*time.Second)
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

func SetTestHooks(testHooks TestHooks) {
	HookImpl = testHooks
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
		return nil, fmt.Errorf("mocks downloading failed to due to error in getting test set ids")
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

func (r *Replayer) authenticateUser(ctx context.Context) error {
	//authenticate the user
	token, err := r.auth.GetToken(ctx)
	if err != nil {
		r.logger.Error("Failed to Authenticate user", zap.Error(err))
		r.logger.Warn("Looks like you haven't logged in, skipping mock upload/download")
		r.logger.Warn("Please login using `keploy login` to perform mock upload/download action")
		return fmt.Errorf("mocks downloading failed to due to authentication error")
	}

	r.mock.setToken(token)
	return nil
}
func (r *Replayer) DownloadMocks(ctx context.Context) error {
	// Authenticate the user for mock registry
	err := r.authenticateUser(ctx)
	if err != nil {
		return err
	}

	if len(r.config.MockDownload.RegistryIDs) > 0 {
		for _, registryID := range r.config.MockDownload.RegistryIDs {
			// Use the registry ID to download mocks directly
			r.logger.Info("Downloading mocks using registry ID",
				zap.String("registryID", registryID),
				zap.String("app", r.config.AppName))

			err = r.mock.downloadByRegistryID(ctx, registryID, r.config.AppName)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				utils.LogError(r.logger, err, "failed to download mocks using registry ID", zap.String("registryID", registryID))
				continue
			}

			r.logger.Info("Successfully downloaded mocks using registry ID",
				zap.String("registryID", registryID),
				zap.String("outputFile", fmt.Sprintf("%s.mocks.yaml", registryID)))
		}
		return nil
	}

	testSets, err := r.GetSelectedTestSets(ctx)
	if err != nil {
		utils.LogError(r.logger, err, "failed to get selected test sets")
		return fmt.Errorf("mocks downloading failed to due to error in getting selected test sets")
	}

	for _, testSetID := range testSets {
		r.logger.Info("Downloading mocks for the testset", zap.String("testset", testSetID))

		err := r.mock.download(ctx, testSetID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			utils.LogError(r.logger, err, "failed to download mocks", zap.String("testset", testSetID))
			continue
		}

	}
	return nil
}

func (r *Replayer) UploadMocks(ctx context.Context, testSets []string) error {
	// Authenticate the user for mock registry
	err := r.authenticateUser(ctx)
	if err != nil {
		return err
	}

	if len(testSets) == 0 {
		testSets, err = r.GetSelectedTestSets(ctx)
		if err != nil {
			utils.LogError(r.logger, err, "failed to get selected test sets")
			return fmt.Errorf("mocks uploading failed to due to error in getting selected test sets")
		}
	}

	for _, testSetID := range testSets {
		r.logger.Info("Uploading mocks for the testset", zap.String("testset", testSetID))

		err := r.mock.upload(ctx, testSetID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			utils.LogError(r.logger, err, "failed to upload mocks", zap.String("testset", testSetID))
			continue
		}

	}

	return nil
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
					mockMismatchFailures.AddProxyErrorForTest(testSetID, effectiveTestCaseID, parserErr)
				}
			}

		}
	}
}

func (r *Replayer) determineMockingStrategy(ctx context.Context, testSetID string, isMappingEnabled bool) (bool, map[string][]string) {
	// Default to timestamp-based strategy with empty mappings.
	defaultMappings := make(map[string][]string)

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
		r.logger.Warn("Failed to get mappings, falling back to timestamp-based filtering",
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
