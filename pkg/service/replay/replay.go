package replay

import (
	// "bytes"

	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"time"

	"facette.io/natsort"
	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	matcherUtils "go.keploy.io/server/v2/pkg/matcher"
	grpcMatcher "go.keploy.io/server/v2/pkg/matcher/grpc"
	httpMatcher "go.keploy.io/server/v2/pkg/matcher/http"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/coverage"
	"go.keploy.io/server/v2/pkg/platform/coverage/golang"
	"go.keploy.io/server/v2/pkg/platform/coverage/java"
	"go.keploy.io/server/v2/pkg/platform/coverage/javascript"
	"go.keploy.io/server/v2/pkg/platform/coverage/python"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

var completeTestReport = make(map[string]TestReportVerdict)
var totalTests int
var totalTestPassed int
var totalTestFailed int
var totalTestIgnored int
var totalTestTimeTaken time.Duration
var HookImpl TestHooks

type Replayer struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	reportDB        ReportDB
	testSetConf     TestSetConfig
	telemetry       Telemetry
	instrumentation Instrumentation
	config          *config.Config
	instrument      bool
	isLastTestSet   bool
	isLastTestCase  bool
}

func NewReplayer(logger *zap.Logger, testDB TestDB, mockDB MockDB, reportDB ReportDB, testSetConf TestSetConfig, telemetry Telemetry, instrumentation Instrumentation, auth service.Auth, storage Storage, config *config.Config) Service {
	// set the request emulator for simulating test case requests, if not set
	if HookImpl == nil {
		SetTestHooks(NewHooks(logger, config, testSetConf, storage, auth, instrumentation))
	}
	instrument := config.Command != ""
	return &Replayer{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		reportDB:        reportDB,
		testSetConf:     testSetConf,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          config,
		instrument:      instrument,
	}
}

func (r *Replayer) Start(ctx context.Context) error {

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	g, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)

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
		if hookCancel != nil {
			hookCancel()
		}
		err := g.Wait()
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

	if len(testSetIDs) == 0 {
		recordCmd := models.HighlightGrayString("keploy record")
		errMsg := fmt.Sprintf("No test sets found in the keploy folder. Please record testcases using %s command", recordCmd)
		utils.LogError(r.logger, err, errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	testRunID, err := r.GetNextTestRunID(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get next test run id: %v", err)
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s", stopReason)
	}

	var language config.Language
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

	var cov coverage.Service
	switch r.config.Test.Language {
	case models.Go:
		cov = golang.New(ctx, r.logger, r.reportDB, r.config.Command, r.config.Test.CoverageReportPath, r.config.CommandType)
	case models.Python:
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
	for i, testSet := range testSets {
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

		var (
			initTotal, initPassed, initFailed, initIgnored int
			initTimeTaken                                  time.Duration
		)

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
			// gathered from reruning, instead only metrics from the last rerun would get added to the varaibles.
			totalTests = initTotal
			totalTestPassed = initPassed
			totalTestFailed = initFailed
			totalTestIgnored = initIgnored
			totalTestTimeTaken = initTimeTaken

			r.logger.Info("running", zap.String("test-set", models.HighlightString(testSet)), zap.Int("attempt", attempt))
			testSetStatus, err := r.RunTestSet(ctx, testSet, testRunID, inst.AppID, false)
			if err != nil {
				stopReason = fmt.Sprintf("failed to run test set: %v", err)
				utils.LogError(r.logger, err, stopReason)
				if ctx.Err() == context.Canceled {
					return err
				}
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

			tcResults, _ := r.reportDB.GetTestCaseResults(ctx, testRunID, testSet)
			failedTcIDs := getFailedTCs(tcResults)

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
				utils.LogError(r.logger, nil, "no. of testset failure occured during rerun reached maximum limit, testset still failing, increase count of maxFailureAttempts", zap.String("testSet", testSet))
				break
			}
			if len(failedTcIDs) == 0 {
				// if no testcase failed in this attempt move to next attempt
				continue
			}

			r.logger.Info("deleting failing testcases", zap.String("testSet", testSet), zap.Any("testCaseIDs", failedTcIDs))

			if err := r.testDB.DeleteTests(ctx, testSet, failedTcIDs); err != nil {
				utils.LogError(r.logger, err, "failed to delete failing testcases", zap.String("testSet", testSet), zap.Any("testCaseIDs", failedTcIDs))
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
			utils.LogError(r.logger, err, "failed to execute after test set run hook", zap.Any("testSet", testSet))
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
		r.logger.Warn("flaky testsets detected, please rerun the specific testsets with --must-pass flag to remove flaky testcases", zap.Any("testSets", flakyTestSets))
	}

	testRunStatus := "fail"
	if testRunResult {
		testRunStatus = "pass"
	}

	if testRunResult && r.config.Test.DisableMockUpload {
		r.logger.Warn("To enable storing mocks in cloud, please use --disableMockUpload=false flag or test:disableMockUpload:false in config file")
	}

	r.telemetry.TestRun(totalTestPassed, totalTestFailed, len(testSets), testRunStatus)

	if !abortTestRun {
		r.printSummary(ctx, testRunResult)
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
				utils.LogError(r.logger, err, "failed to calculate coverage for the test run")
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
	appID, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, DockerNetwork: r.config.NetworkName, DockerDelay: r.config.BuildDelay})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return &InstrumentState{}, err
		}
		return &InstrumentState{}, fmt.Errorf("failed to setup instrumentation: %w", err)
	}
	r.config.AppID = appID

	var cancel context.CancelFunc
	// starting the hooks and proxy
	select {
	case <-ctx.Done():
		return &InstrumentState{}, context.Canceled
	default:
		hookCtx := context.WithoutCancel(ctx)
		hookCtx, cancel = context.WithCancel(hookCtx)
		err = r.instrumentation.Hook(hookCtx, appID, models.HookOptions{Mode: models.MODE_TEST, EnableTesting: r.config.EnableTesting, Rules: r.config.BypassRules})
		if err != nil {
			cancel()
			if errors.Is(err, context.Canceled) {
				return &InstrumentState{}, err
			}
			return &InstrumentState{}, fmt.Errorf("failed to start the hooks and proxy: %w", err)
		}
	}
	return &InstrumentState{AppID: appID, HookCancel: cancel}, nil
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

func (r *Replayer) RunTestSet(ctx context.Context, testSetID string, testRunID string, appID uint64, serveTest bool) (models.TestSetStatus, error) {

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	runTestSetErrGrp, runTestSetCtx := errgroup.WithContext(ctx)
	runTestSetCtx = context.WithValue(runTestSetCtx, models.ErrGroupKey, runTestSetErrGrp)
	runTestSetCtx, runTestSetCtxCancel := context.WithCancel(runTestSetCtx)

	startTime := time.Now()

	exitLoopChan := make(chan bool, 2)
	defer func() {
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

	if len(testCases) == 0 {
		r.logger.Warn("no valid test cases found to run for test set", zap.String("test-set", testSetID))

		testReport := &models.TestReport{
			Version: models.GetVersion(),
			TestSet: testSetID,
			Status:  string(models.TestSetStatusNoTestsToRun),
			Total:   0,
			Ignored: 0,
		}
		err = r.reportDB.InsertReport(runTestSetCtx, testRunID, testSetID, testReport)
		if err != nil {
			utils.LogError(r.logger, err, "failed to insert report")
			return models.TestSetStatusFailed, err
		}
		return models.TestSetStatusNoTestsToRun, nil
	}

	if _, ok := r.config.Test.IgnoredTests[testSetID]; ok && len(r.config.Test.IgnoredTests[testSetID]) == 0 {
		testReport := &models.TestReport{
			Version: models.GetVersion(),
			TestSet: testSetID,
			Status:  string(models.TestSetStatusIgnored),
			Total:   len(testCases),
			Ignored: len(testCases),
		}

		err = r.reportDB.InsertReport(runTestSetCtx, testRunID, testSetID, testReport)
		if err != nil {
			utils.LogError(r.logger, err, "failed to insert report")
			return models.TestSetStatusFailed, err
		}

		verdict := TestReportVerdict{
			total:    testReport.Total,
			failed:   0,
			passed:   0,
			ignored:  testReport.Ignored,
			status:   true,
			duration: time.Duration(0),
		}

		completeTestReport[testSetID] = verdict
		totalTests += testReport.Total
		totalTestIgnored += testReport.Ignored

		return models.TestSetStatusIgnored, nil
	}

	var conf *models.TestSet
	conf, err = r.testSetConf.Read(runTestSetCtx, testSetID)
	if err != nil {
		if strings.Contains(err.Error(), "no such file or directory") || strings.Contains(err.Error(), "The system cannot find the file specified") {
			r.logger.Info("config file not found, continuing execution...", zap.String("test-set", testSetID))
		} else {
			return models.TestSetStatusFailed, fmt.Errorf("failed to read test set config: %w", err)
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
	var ignored int
	var totalConsumedMocks = map[string]models.MockState{}

	testSetStatus := models.TestSetStatusPassed
	testSetStatusByErrChan := models.TestSetStatusRunning

	cmdType := utils.CmdType(r.config.CommandType)
	var userIP string

	filteredMocks, unfilteredMocks, err := r.GetMocks(ctx, testSetID, models.BaseTime, time.Now())
	if err != nil {
		return models.TestSetStatusFailed, err
	}

	pkg.InitSortCounter(int64(max(len(filteredMocks), len(unfilteredMocks))))

	err = r.instrumentation.MockOutgoing(runTestSetCtx, appID, models.OutgoingOptions{
		Rules:          r.config.BypassRules,
		MongoPassword:  r.config.Test.MongoPassword,
		SQLDelay:       time.Duration(r.config.Test.Delay),
		FallBackOnMiss: r.config.Test.FallBackOnMiss,
		Mocking:        r.config.Test.Mocking,
		Backdate:       testCases[0].HTTPReq.Timestamp,
	})
	if err != nil {
		utils.LogError(r.logger, err, "failed to mock outgoing")
		return models.TestSetStatusFailed, err
	}

	// filtering is redundant, but we need to set the mocks
	err = r.FilterAndSetMocks(ctx, appID, filteredMocks, unfilteredMocks, models.BaseTime, time.Now(), totalConsumedMocks)
	if err != nil {
		return models.TestSetStatusFailed, err
	}

	if r.instrument {
		if !serveTest {
			runTestSetErrGrp.Go(func() error {
				defer utils.Recover(r.logger)
				appErr = r.RunApplication(runTestSetCtx, appID, models.RunOptions{})
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

		if utils.IsDockerCmd(cmdType) {
			userIP, err = r.instrumentation.GetContainerIP(ctx, appID)
			if err != nil {
				return models.TestSetStatusFailed, err
			}
		}
	}

	selectedTests := matcherUtils.ArrayToMap(r.config.Test.SelectedTests[testSetID])
	ignoredTests := matcherUtils.ArrayToMap(r.config.Test.IgnoredTests[testSetID])

	testCasesCount := len(testCases)

	if len(selectedTests) != 0 {
		testCasesCount = len(selectedTests)
	}

	// Inserting the initial report for the test set
	testReport := &models.TestReport{
		Version: models.GetVersion(),
		Total:   testCasesCount,
		Status:  string(models.TestStatusRunning),
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

		err = r.FilterAndSetMocks(runTestSetCtx, appID, filteredMocks, unfilteredMocks, testCase.HTTPReq.Timestamp, testCase.HTTPResp.Timestamp, totalConsumedMocks)
		if err != nil {
			utils.LogError(r.logger, err, "failed to filter and set mocks")
			break
		}

		// Handle Docker environment IP replacement
		if utils.IsDockerCmd(cmdType) {
			err = r.replaceHostInTestCase(testCase, userIP, "docker container's IP")
			if err != nil {
				break
			}
		}

		// Handle user-provided host replacement
		if r.config.Test.Host != "" {
			err = r.replaceHostInTestCase(testCase, r.config.Test.Host, "host provided by the user")
			if err != nil {
				break
			}
		}

		// Handle user-provided port replacement
		if r.config.Test.Port != 0 {
			err = r.replacePortInTestCase(testCase, strconv.Itoa(int(r.config.Test.Port)))
			if err != nil {
				break
			}
		}

		started := time.Now().UTC()
		resp, loopErr := HookImpl.SimulateRequest(runTestSetCtx, appID, testCase, testSetID)
		if loopErr != nil {
			utils.LogError(r.logger, loopErr, "failed to simulate request")
			failure++
			continue
		}

		var consumedMocks []models.MockState
		if r.instrument {
			consumedMocks, err = HookImpl.GetConsumedMocks(runTestSetCtx, appID)
			if err != nil {
				utils.LogError(r.logger, err, "failed to get consumed filtered mocks")
			}
			for _, m := range consumedMocks {
				totalConsumedMocks[m.Name] = m
			}
		}

		r.logger.Debug("test case kind", zap.Any("kind", testCase.Kind))

		switch testCase.Kind {
		case models.HTTP:
			httpResp, ok := resp.(*models.HTTPResp)
			if !ok {
				r.logger.Error("invalid response type for HTTP test case")
				failure++
				continue
			}
			testPass, testResult = r.compareHTTPResp(testCase, httpResp, testSetID)

		case models.GRPC_EXPORT:
			grpcResp, ok := resp.(*models.GrpcResp)
			if !ok {
				r.logger.Error("invalid response type for gRPC test case")
				failure++
				continue
			}
			testPass, testResult = r.compareGRPCResp(testCase, grpcResp, testSetID)
		}

		if !testPass {
			// log the consumed mocks during the test run of the test case for test set
			r.logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(testCase.Name)), zap.Any("testset id", models.HighlightFailingString(testSetID)), zap.Any("passed", models.HighlightFailingString(testPass)))
			r.logger.Debug("Consumed Mocks", zap.Any("mocks", consumedMocks))
		} else {
			r.logger.Info("result", zap.Any("testcase id", models.HighlightPassingString(testCase.Name)), zap.Any("testset id", models.HighlightPassingString(testSetID)), zap.Any("passed", models.HighlightPassingString(testPass)))
		}
		if testPass {
			testStatus = models.TestStatusPassed
			success++
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
				}
			}

			if testCaseResult != nil {
				loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
				if loopErr != nil {
					utils.LogError(r.logger, err, "failed to insert test case result")
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

	if conf.PostScript != "" {
		//Execute the Post-script after each test-set if provided
		r.logger.Info("Running Post-script", zap.String("script", conf.PostScript), zap.String("test-set", testSetID))
		err = r.executeScript(runTestSetCtx, conf.PostScript)
		if err != nil {
			return models.TestSetStatusFaultScript, fmt.Errorf("failed to execute post-script: %w", err)
		}
	}

	timeTaken := time.Since((startTime))

	testCaseResults, err := r.reportDB.GetTestCaseResults(runTestSetCtx, testRunID, testSetID)
	if err != nil {
		if runTestSetCtx.Err() != context.Canceled {
			utils.LogError(r.logger, err, "failed to get test case results")
			testSetStatus = models.TestSetStatusInternalErr
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
		Version: models.GetVersion(),
		TestSet: testSetID,
		Status:  string(testSetStatus),
		Total:   testCasesCount,
		Success: success,
		Failure: failure,
		Ignored: ignored,
		Tests:   testCaseResults,
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
	if r.config.Test.RemoveUnusedMocks && testSetStatus == models.TestSetStatusPassed && r.instrument {
		r.logger.Debug("consumed mocks from the completed testset", zap.Any("for test-set", testSetID), zap.Any("consumed mocks", totalConsumedMocks))
		// delete the unused mocks from the data store
		r.logger.Info("deleting unused mocks from the data store", zap.Any("for test-set", testSetID))
		err = r.mockDB.UpdateMocks(runTestSetCtx, testSetID, totalConsumedMocks)
		if err != nil {
			utils.LogError(r.logger, err, "failed to delete unused mocks")
		}
	}

	// TODO Need to decide on whether to use global variable or not
	verdict := TestReportVerdict{
		total:    testReport.Total,
		failed:   testReport.Failure,
		passed:   testReport.Success,
		ignored:  testReport.Ignored,
		status:   testSetStatus == models.TestSetStatusPassed,
		duration: timeTaken,
	}

	completeTestReport[testSetID] = verdict
	totalTests += testReport.Total
	totalTestPassed += testReport.Success
	totalTestFailed += testReport.Failure
	totalTestIgnored += testReport.Ignored
	totalTestTimeTaken += timeTaken

	timeTakenStr := timeWithUnits(timeTaken)

	if testSetStatus == models.TestSetStatusFailed || testSetStatus == models.TestSetStatusPassed {
		if testSetStatus == models.TestSetStatusFailed {
			pp.SetColorScheme(models.GetFailingColorScheme())
		} else {
			pp.SetColorScheme(models.GetPassingColorScheme())
		}
		if testReport.Ignored > 0 {
			if _, err := pp.Printf("\n <=========================================> \n  TESTRUN SUMMARY. For test-set: %s\n"+"\tTotal tests: %s\n"+"\tTotal test passed: %s\n"+"\tTotal test failed: %s\n"+"\tTotal test ignored: %s\n"+"\tTime Taken: %s\n <=========================================> \n\n", testReport.TestSet, testReport.Total, testReport.Success, testReport.Failure, testReport.Ignored, timeTakenStr); err != nil {
				utils.LogError(r.logger, err, "failed to print testrun summary")
			}
		} else {
			if _, err := pp.Printf("\n <=========================================> \n  TESTRUN SUMMARY. For test-set: %s\n"+"\tTotal tests: %s\n"+"\tTotal test passed: %s\n"+"\tTotal test failed: %s\n"+"\tTime Taken: %s\n <=========================================> \n\n", testReport.TestSet, testReport.Total, testReport.Success, testReport.Failure, timeTakenStr); err != nil {
				utils.LogError(r.logger, err, "failed to print testrun summary")
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

	return testSetStatus, nil
}

func (r *Replayer) GetMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) (filtered, unfiltered []*models.Mock, err error) {
	filtered, err = r.mockDB.GetFilteredMocks(ctx, testSetID, afterTime, beforeTime)
	if err != nil {
		utils.LogError(r.logger, err, "failed to get filtered mocks")
		return nil, nil, err
	}
	unfiltered, err = r.mockDB.GetUnFilteredMocks(ctx, testSetID, afterTime, beforeTime)
	if err != nil {
		utils.LogError(r.logger, err, "failed to get unfiltered mocks")
		return nil, nil, err
	}
	return filtered, unfiltered, err
}

func (r *Replayer) FilterAndSetMocks(ctx context.Context, appID uint64, filtered, unfiltered []*models.Mock, afterTime, beforeTime time.Time, totalConsumedMocks map[string]models.MockState) error {
	if !r.instrument {
		r.logger.Debug("Keploy will not filter and set mocks when base path is provided", zap.Any("base path", r.config.Test.BasePath))
		return nil
	}

	filtered = pkg.FilterTcsMocks(ctx, r.logger, filtered, afterTime, beforeTime)
	unfiltered = pkg.FilterConfigMocks(ctx, r.logger, unfiltered, afterTime, beforeTime)

	filterOutDeleted := func(in []*models.Mock) []*models.Mock {
		out := make([]*models.Mock, 0, len(in))
		for _, m := range in {
			// treat empty/missing names as never consumed
			if m == nil || m.Name == "" {
				out = append(out, m)
				continue
			}
			// we are picking mocks that are not consumed till now (not present in map),
			// and, mocks that are updated.
			if k, ok := totalConsumedMocks[m.Name]; !ok || k.Usage != models.Deleted {
				if ok {
					m.TestModeInfo.IsFiltered = k.IsFiltered
					m.TestModeInfo.SortOrder = k.SortOrder
				}
				out = append(out, m)
			}
		}
		return out
	}

	filtered = filterOutDeleted(filtered)
	unfiltered = filterOutDeleted(unfiltered)

	err := r.instrumentation.SetMocks(ctx, appID, filtered, unfiltered)
	if err != nil {
		utils.LogError(r.logger, err, "failed to set mocks")
		return err
	}

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

func (r *Replayer) compareHTTPResp(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string) (bool, *models.Result) {
	noiseConfig := r.config.Test.GlobalNoise.Global
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
		noiseConfig = LeftJoinNoise(r.config.Test.GlobalNoise.Global, tsNoise)
	}
	return httpMatcher.Match(tc, actualResponse, noiseConfig, r.config.Test.IgnoreOrdering, r.logger)
}

func (r *Replayer) compareGRPCResp(tc *models.TestCase, actualResp *models.GrpcResp, testSetID string) (bool, *models.Result) {
	noiseConfig := r.config.Test.GlobalNoise.Global
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
		noiseConfig = LeftJoinNoise(r.config.Test.GlobalNoise.Global, tsNoise)
	}

	return grpcMatcher.Match(tc, actualResp, noiseConfig, r.logger)

}

func (r *Replayer) printSummary(_ context.Context, _ bool) {
	if totalTests > 0 {
		testSuiteNames := make([]string, 0, len(completeTestReport))
		for testSuiteName := range completeTestReport {
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

		totalTestTimeTakenStr := timeWithUnits(totalTestTimeTaken)

		if totalTestIgnored > 0 {
			if _, err := pp.Printf("\n <=========================================> \n  COMPLETE TESTRUN SUMMARY. \n\tTotal tests: %s\n"+"\tTotal test passed: %s\n"+"\tTotal test failed: %s\n"+"\tTotal test ignored: %s\n"+"\tTotal time taken: %s\n", totalTests, totalTestPassed, totalTestFailed, totalTestIgnored, totalTestTimeTakenStr); err != nil {
				utils.LogError(r.logger, err, "failed to print test run summary")
				return
			}
			if _, err := pp.Printf("\n\tTest Suite Name\t\tTotal Test\tPassed\t\tFailed\t\tIgnored\t\tTime Taken\t\n"); err != nil {
				utils.LogError(r.logger, err, "failed to print test suite summary")
				return
			}
		} else {
			if _, err := pp.Printf("\n <=========================================> \n  COMPLETE TESTRUN SUMMARY. \n\tTotal tests: %s\n"+"\tTotal test passed: %s\n"+"\tTotal test failed: %s\n"+"\tTotal time taken: %s\n", totalTests, totalTestPassed, totalTestFailed, totalTestTimeTakenStr); err != nil {
				utils.LogError(r.logger, err, "failed to print test run summary")
				return
			}
			if _, err := pp.Printf("\n\tTest Suite Name\t\tTotal Test\tPassed\t\tFailed\t\tTime Taken\t\n"); err != nil {
				utils.LogError(r.logger, err, "failed to print test suite summary")
				return
			}
		}
		for _, testSuiteName := range testSuiteNames {
			if completeTestReport[testSuiteName].status {
				pp.SetColorScheme(models.GetPassingColorScheme())
			} else {
				pp.SetColorScheme(models.GetFailingColorScheme())
			}

			testSetTimeTakenStr := timeWithUnits(completeTestReport[testSuiteName].duration)

			if totalTestIgnored > 0 {
				if _, err := pp.Printf("\n\t%s\t\t%s\t\t%s\t\t%s\t\t%s\t\t%s", testSuiteName, completeTestReport[testSuiteName].total, completeTestReport[testSuiteName].passed, completeTestReport[testSuiteName].failed, completeTestReport[testSuiteName].ignored, testSetTimeTakenStr); err != nil {
					utils.LogError(r.logger, err, "failed to print test suite details")
					return
				}
			} else {
				if _, err := pp.Printf("\n\t%s\t\t%s\t\t%s\t\t%s\t\t%s", testSuiteName, completeTestReport[testSuiteName].total, completeTestReport[testSuiteName].passed, completeTestReport[testSuiteName].failed, testSetTimeTakenStr); err != nil {
					utils.LogError(r.logger, err, "failed to print test suite details")
					return
				}
			}
		}
		if _, err := pp.Printf("\n<=========================================> \n\n"); err != nil {
			utils.LogError(r.logger, err, "failed to print separator")
			return
		}
	}
}

func (r *Replayer) RunApplication(ctx context.Context, appID uint64, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, appID, opts)
}

func (r *Replayer) GetTestSetConf(ctx context.Context, testSet string) (*models.TestSet, error) {
	return r.testSetConf.Read(ctx, testSet)
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

func (r *Replayer) Normalize(ctx context.Context) error {

	var testRun string
	if r.config.Normalize.TestRun == "" {
		testRunIDs, err := r.reportDB.GetAllTestRunIDs(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("failed to get all test run ids: %w", err)
		}
		testRun = pkg.LastID(testRunIDs, models.TestRunTemplateName)
	}

	if len(r.config.Normalize.SelectedTests) == 0 {
		testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("failed to get all test set ids: %w", err)
		}
		for _, testSetID := range testSetIDs {
			r.config.Normalize.SelectedTests = append(r.config.Normalize.SelectedTests, config.SelectedTests{TestSet: testSetID})
		}
	}

	for _, testSet := range r.config.Normalize.SelectedTests {
		testSetID := testSet.TestSet
		testCases := testSet.Tests
		err := r.NormalizeTestCases(ctx, testRun, testSetID, testCases, nil)
		if err != nil {
			return err
		}
	}
	r.logger.Info("Normalized test cases successfully. Please run keploy tests to verify the changes.")
	return nil
}

func (r *Replayer) NormalizeTestCases(ctx context.Context, testRun string, testSetID string, selectedTestCaseIDs []string, testCaseResults []models.TestResult) error {

	if len(testCaseResults) == 0 {
		testReport, err := r.reportDB.GetReport(ctx, testRun, testSetID)
		if err != nil {
			return fmt.Errorf("failed to get test report: %w", err)
		}
		testCaseResults = testReport.Tests
	}

	testCaseResultMap := make(map[string]models.TestResult)
	testCases, err := r.testDB.GetTestCases(ctx, testSetID)
	if err != nil {
		return fmt.Errorf("failed to get test cases: %w", err)
	}
	selectedTestCases := make([]*models.TestCase, 0, len(selectedTestCaseIDs))

	if len(selectedTestCaseIDs) == 0 {
		selectedTestCases = testCases
	} else {
		for _, testCase := range testCases {
			if _, ok := matcherUtils.ArrayToMap(selectedTestCaseIDs)[testCase.Name]; ok {
				selectedTestCases = append(selectedTestCases, testCase)
			}
		}
	}

	for _, testCaseResult := range testCaseResults {
		testCaseResultMap[testCaseResult.TestCaseID] = testCaseResult
	}

	for _, testCase := range selectedTestCases {
		if _, ok := testCaseResultMap[testCase.Name]; !ok {
			r.logger.Info("test case not found in the test report", zap.String("test-case-id", testCase.Name), zap.String("test-set-id", testSetID))
			continue
		}
		if testCaseResultMap[testCase.Name].Status == models.TestStatusPassed {
			continue
		}
		testCase.HTTPResp = testCaseResultMap[testCase.Name].Res
		err = r.testDB.UpdateTestCase(ctx, testCase, testSetID, true)
		if err != nil {
			return fmt.Errorf("failed to update test case: %w", err)
		}
	}
	return nil
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

func (r *Replayer) replaceHostInTestCase(testCase *models.TestCase, newHost, logContext string) error {
	var err error
	switch testCase.Kind {
	case models.HTTP:
		testCase.HTTPReq.URL, err = utils.ReplaceHost(testCase.HTTPReq.URL, newHost)
		if err != nil {
			utils.LogError(r.logger, err, fmt.Sprintf("failed to replace host to %s", logContext))
			return err
		}
		r.logger.Debug("", zap.Any(fmt.Sprintf("replaced %s", logContext), testCase.HTTPReq.URL))

	case models.GRPC_EXPORT:
		testCase.GrpcReq.Headers.PseudoHeaders[":authority"], err = utils.ReplaceGrpcHost(testCase.GrpcReq.Headers.PseudoHeaders[":authority"], newHost)
		if err != nil {
			utils.LogError(r.logger, err, fmt.Sprintf("failed to replace host to %s", logContext))
			return err
		}
		r.logger.Debug("", zap.Any(fmt.Sprintf("replaced %s", logContext), testCase.GrpcReq.Headers.PseudoHeaders[":authority"]))
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
