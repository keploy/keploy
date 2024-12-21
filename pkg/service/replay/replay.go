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
}

func NewReplayer(logger *zap.Logger, testDB TestDB, mockDB MockDB, reportDB ReportDB, testSetConf TestSetConfig, telemetry Telemetry, instrumentation Instrumentation, auth service.Auth, storage Storage, config *config.Config) Service {
	// set the request emulator for simulating test case requests, if not set
	if HookImpl == nil {
		SetTestHooks(NewHooks(logger, config, testSetConf, storage, auth))
	}
	instrument := false
	if config.Command != "" {
		instrument = true
	}
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

	setupErrGrp, _ := errgroup.WithContext(ctx)
	_ = context.WithoutCancel(ctx)
	setupCtx := context.WithValue(ctx, models.ErrGroupKey, setupErrGrp)
	setupCtx, setupCtxCancel := context.WithCancel(setupCtx)

	var stopReason = "replay completed successfully"

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-ctx.Done():
			break
		default:
			unregister := models.UnregisterReq{
				ClientID: r.config.ClientID,
				Mode:     models.MODE_TEST,
			}
			err := r.instrumentation.UnregisterClient(ctx, unregister)
			if err != nil {
				utils.LogError(r.logger, err, "failed to unregister client")
			}
			time.Sleep(2 * time.Second)
			r.logger.Info("stopping Keploy", zap.String("reason", stopReason))
		}

		setupCtxCancel()
		err := setupErrGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop setup execution, that covers init container")
		}

		err = g.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop replaying")
		}
	}()

	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get all test set ids: %v", err)
		utils.LogError(r.logger, err, stopReason)
		return errors.New(stopReason)
	}

	if len(testSetIDs) == 0 {
		recordCmd := models.HighlightGrayString("keploy record")
		errMsg := fmt.Sprintf("No test sets found in the keploy folder. Please record testcases using %s command", recordCmd)
		utils.LogError(r.logger, err, errMsg)
		return fmt.Errorf(errMsg)
	}

	testRunID, err := r.GetNextTestRunID(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get next test run id: %v", err)
		utils.LogError(r.logger, err, stopReason)
		return errors.New(stopReason)
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

	inst, err := r.Instrument(setupCtx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to instrument: %v", err)
		utils.LogError(r.logger, err, stopReason)
		if ctx.Err() == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}

	var testSetResult bool
	testRunResult := true
	abortTestRun := false
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
			return fmt.Errorf(stopReason)
		}

		if !r.config.Test.SkipCoverage {
			err = os.Setenv("TESTSETID", testSet) // related to java coverage calculation
			if err != nil {
				r.config.Test.SkipCoverage = true
				r.logger.Warn("failed to set TESTSETID env variable, skipping coverage caluclation", zap.Error(err))
			}
		}

		testSetStatus, err := r.RunTestSet(ctx, testSet, testRunID, inst.ClientID, false)
		if err != nil {
			stopReason = fmt.Sprintf("failed to run test set: %v", err)
			utils.LogError(r.logger, err, stopReason)
			if ctx.Err() == context.Canceled {
				return err
			}
			return fmt.Errorf(stopReason)
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
		}

		if testSetStatus != models.TestSetStatusIgnored {
			testRunResult = testRunResult && testSetResult
			if abortTestRun {
				break
			}
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
	// Instrument will setup the environment and start the hooks and proxy
	clientID, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, DockerNetwork: r.config.NetworkName, CommandType: r.config.CommandType, DockerDelay: r.config.BuildDelay, Mode: models.MODE_TEST, EnableTesting: r.config.EnableTesting})
	if err != nil {
		stopReason := "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return &InstrumentState{}, fmt.Errorf(stopReason)
	}

	r.config.ClientID = clientID

	return &InstrumentState{ClientID: clientID}, nil
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

func (r *Replayer) RunTestSet(ctx context.Context, testSetID string, testRunID string, clientID uint64, serveTest bool) (models.TestSetStatus, error) {

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
		return models.TestSetStatusPassed, nil
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
		if strings.Contains(err.Error(), "no such file or directory") {
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
	var totalConsumedMocks = map[string]bool{}

	testSetStatus := models.TestSetStatusPassed
	testSetStatusByErrChan := models.TestSetStatusRunning

	r.logger.Info("running", zap.Any("test-set", models.HighlightString(testSetID)))

	cmdType := utils.CmdType(r.config.CommandType)
	var userIP string

	err = r.SetupOrUpdateMocks(runTestSetCtx, clientID, testSetID, models.BaseTime, time.Now(), Start)
	if err != nil {
		return models.TestSetStatusFailed, err
	}

	if r.instrument {
		if !serveTest {
			runTestSetErrGrp.Go(func() error {
				defer utils.Recover(r.logger)
				appErr = r.RunApplication(runTestSetCtx, clientID, models.RunOptions{})
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
			userIP, err = r.instrumentation.GetContainerIP(ctx, clientID)
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

		//No need to handle mocking when basepath is provided
		err := r.SetupOrUpdateMocks(runTestSetCtx, clientID, testSetID, testCase.HTTPReq.Timestamp, testCase.HTTPResp.Timestamp, Update)
		if err != nil {
			utils.LogError(r.logger, err, "failed to update mocks")
			break
		}

		if utils.IsDockerCmd(cmdType) {
			testCase.HTTPReq.URL, err = utils.ReplaceHost(testCase.HTTPReq.URL, userIP)
			if err != nil {
				utils.LogError(r.logger, err, "failed to replace host to docker container's IP")
				break
			}
			r.logger.Debug("", zap.Any("replaced URL in case of docker env", testCase.HTTPReq.URL))
		}

		// send the flag replace-host instead of sending the IP
		if r.config.Test.Host != "" {
			testCase.HTTPReq.URL, err = utils.ReplaceHost(testCase.HTTPReq.URL, r.config.Test.Host)
			if err != nil {
				utils.LogError(r.logger, err, "failed to replace host to provided host by the user")
				break
			}
		}

		if r.config.Test.Port != 0 {
			testCase.HTTPReq.URL, err = utils.ReplacePort(testCase.HTTPReq.URL, strconv.Itoa(int(r.config.Test.Port)))
		}

		started := time.Now().UTC()
		resp, loopErr := HookImpl.SimulateRequest(runTestSetCtx, clientID, testCase, testSetID)
		if loopErr != nil {
			utils.LogError(r.logger, err, "failed to simulate request")
			failure++
			continue
		}

		var consumedMocks []string
		if r.instrument {
			consumedMocks, err = r.instrumentation.GetConsumedMocks(runTestSetCtx, clientID)
			if err != nil {
				utils.LogError(r.logger, err, "failed to get consumed filtered mocks")
			}
			if r.config.Test.RemoveUnusedMocks {
				for _, mockName := range consumedMocks {
					totalConsumedMocks[mockName] = true
				}
			}
		}

		testPass, testResult = r.compareResp(testCase, resp, testSetID)
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
			testCaseResult := &models.TestResult{
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
				Res:          *resp,
				TestCasePath: filepath.Join(r.config.Path, testSetID),
				MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
				Noise:        testCase.Noise,
				Result:       *testResult,
			}
			loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
			if loopErr != nil {
				utils.LogError(r.logger, err, "failed to insert test case result")
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
		removeDoubleQuotes(utils.TemplatizedValues)
		// Write the templatized values to the yaml.
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

func (r *Replayer) SetupOrUpdateMocks(ctx context.Context, appID uint64, testSetID string, afterTime, beforeTime time.Time, action MockAction) error {

	if !r.instrument {
		r.logger.Debug("Keploy will not setup or update the mocks when base path is provided", zap.Any("base path", r.config.Test.BasePath))
		return nil
	}

	filteredMocks, unfilteredMocks, err := r.GetMocks(ctx, testSetID, afterTime, beforeTime)
	if err != nil {
		return err
	}

	if action == Start {
		err = r.instrumentation.MockOutgoing(ctx, appID, models.OutgoingOptions{
			Rules:          r.config.BypassRules,
			MongoPassword:  r.config.Test.MongoPassword,
			SQLDelay:       time.Duration(r.config.Test.Delay),
			FallBackOnMiss: r.config.Test.FallBackOnMiss,
			Mocking:        r.config.Test.Mocking,
		})
		if err != nil {
			utils.LogError(r.logger, err, "failed to mock outgoing")
			return err
		}
	}

	// this will be sent to the proxy
	err = r.instrumentation.SetMocks(ctx, appID, filteredMocks, unfilteredMocks)
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

func (r *Replayer) compareResp(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string) (bool, *models.Result) {

	noiseConfig := r.config.Test.GlobalNoise.Global
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
		noiseConfig = LeftJoinNoise(r.config.Test.GlobalNoise.Global, tsNoise)
	}
	return httpMatcher.Match(tc, actualResponse, noiseConfig, r.config.Test.IgnoreOrdering, r.logger)
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

func (r *Replayer) RunApplication(ctx context.Context, clientID uint64, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, clientID, opts)
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
				err = r.testDB.UpdateTestCase(ctx, v, testSetID)
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
		err = r.testDB.UpdateTestCase(ctx, testCase, testSetID)
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

func (r *Replayer) GetContainerIP(ctx context.Context, id uint64) (string, error) {
	return r.instrumentation.GetContainerIP(ctx, id)
}
