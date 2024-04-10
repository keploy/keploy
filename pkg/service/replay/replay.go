package replay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

var completeTestReport = make(map[string]TestReportVerdict)
var totalTests int
var totalTestPassed int
var totalTestFailed int

// emulator contains the struct instance that implements RequestEmulator interface. This is done for
// attaching the objects dynamically as plugins.
var emulator RequestEmulator

func SetTestUtilInstance(instance RequestEmulator) {
	emulator = instance
}

type Replayer struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	reportDB        ReportDB
	configDB        ConfigDB
	telemetry       Telemetry
	instrumentation Instrumentation
	config          config.Config
}

func NewReplayer(logger *zap.Logger, testDB TestDB, mockDB MockDB, reportDB ReportDB, configDB ConfigDB, telemetry Telemetry, instrumentation Instrumentation, config config.Config) Service {
	// set the request emulator for simulating test case requests, if not set
	if emulator == nil {
		SetTestUtilInstance(NewTestUtils(config.Test.APITimeout, logger))
	}

	return &Replayer{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		reportDB:        reportDB,
		configDB:        configDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          config,
	}
}

func (r *Replayer) Start(ctx context.Context) error {

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	g, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)

	var stopReason = "replay completed successfully"
	var hookCancel context.CancelFunc

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-ctx.Done():
			break
		default:
			err := utils.Stop(r.logger, stopReason)
			if err != nil {
				utils.LogError(r.logger, err, "failed to stop recording")
			}
		}
		if hookCancel != nil {
			hookCancel()
		}
		err := g.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop recording")
		}
	}()

	// BootReplay will start the hooks and proxy and return the testRunID and appID
	testRunID, appID, hookCancel, err := r.BootReplay(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to boot replay: %v", err)
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}

	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get all test set ids: %v", err)
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}

	testSetResult := false
	testRunResult := true
	abortTestRun := false

	testSetCommands := make(map[string]string)
	for _, testSetID := range testSetIDs {
		config, _ := r.configDB.GetConfig(ctx, testSetID)
		if err != nil {
			testSetCommands[testSetID] = r.config.Command
		} else {
			testSetCommands[testSetID] = config.Cmd
		}
	}

	// Sort testSetIDs based on the frequency of commands
	sortedTestSetIDs := sortTestSetsByCmdFrequency(testSetCommands)

	var previousCmd string
	var appCtxCancel context.CancelFunc
	var stopApp bool
	var restartApp bool
	var appErrChan = make(chan models.AppError, 1)
	var runAppErrGrp *errgroup.Group
	var appErr models.AppError
	var appCtx context.Context

	defer func() {
		if appCtxCancel != nil {
			appCtxCancel()
		}
		if runAppErrGrp != nil {
			err := runAppErrGrp.Wait()
			if err != nil {
				utils.LogError(r.logger, err, "error in runAppErrGrp")
			}
		}
	}()

	for index, testSetID := range sortedTestSetIDs {

		if _, ok := r.config.Test.SelectedTests[testSetID]; !ok && len(r.config.Test.SelectedTests) != 0 {
			continue
		}

		currentCmd := testSetCommands[testSetID]

		if (index == len(sortedTestSetIDs)-1) || currentCmd != testSetCommands[sortedTestSetIDs[index+1]] {
			stopApp = true
		}

		err := r.SetUpRunTestSet(ctx, testSetID, appID)
		if err != nil {
			stopReason = fmt.Sprintf("failed to setup run test set: %v", err)
			utils.LogError(r.logger, err, stopReason)
			testSetResult = false
			if stopApp {
				if appCtxCancel != nil {
					appCtxCancel()
				}
				if runAppErrGrp != nil {
					err := runAppErrGrp.Wait()
					if err != nil {
						utils.LogError(r.logger, err, "error in runAppErrGrp")
					}
				}
			}
			continue
		}

		if previousCmd != currentCmd || restartApp {
			err := r.instrumentation.UpdateAppInfo(ctx, appID, currentCmd, models.SetupOptions{Container: r.config.ContainerName, DockerNetwork: r.config.NetworkName, DockerDelay: r.config.BuildDelay})
			if err != nil {
				utils.LogError(r.logger, err, "failed to setup instrumentation")
				continue
			}
			appCtx, appCtxCancel = context.WithCancel(ctx)
			runAppErrGrp, _ := errgroup.WithContext(appCtx)
			runAppErrGrp.Go(func() error {
				defer utils.Recover(r.logger)
				appErr = r.RunApplication(appCtx, appID, models.RunOptions{})
				if appErr.AppErrorType == models.ErrCtxCanceled {
					return nil
				}
				appErrChan <- appErr
				return nil
			})
		}
		previousCmd = currentCmd

		testSetStatus, total, passed, failed, err := r.RunTestSet(ctx, testSetID, testRunID, appID, appErrChan)
		if total != 0 && testSetStatus != models.TestSetStatusUserAbort {
			verdict := TestReportVerdict{
				total:  total,
				failed: passed,
				passed: failed,
				status: testSetStatus == models.TestSetStatusPassed,
			}

			completeTestReport[testSetID] = verdict
			totalTests += total
			totalTestPassed += passed
			totalTestFailed += failed

			if testSetStatus == models.TestSetStatusPassed {
				pp.SetColorScheme(models.PassingColorScheme)
			} else {
				pp.SetColorScheme(models.FailingColorScheme)
			}
			if _, err := pp.Printf("\n <=========================================> \n  TESTRUN SUMMARY. For test-set: %s\n"+"\tTotal tests: %s\n"+"\tTotal test passed: %s\n"+"\tTotal test failed: %s\n <=========================================> \n\n", testSetID, total, passed, failed); err != nil {
				utils.LogError(r.logger, err, "failed to print testrun summary")
			}

			r.telemetry.TestSetRun(passed, failed, testSetID, string(testSetStatus))
		}
		if err != nil {
			stopReason = fmt.Sprintf("failed to run test set: %v", err)
			utils.LogError(r.logger, err, stopReason)
			if err == context.Canceled {
				return err
			}
			if stopApp {
				if appCtxCancel != nil {
					appCtxCancel()
				}
				if runAppErrGrp != nil {
					err := runAppErrGrp.Wait()
					if err != nil {
						utils.LogError(r.logger, err, "error in runAppErrGrp")
					}
				}
			}
			continue
		}
		switch testSetStatus {
		case models.TestSetStatusAppHalted:
			restartApp = true
			testSetResult = false
		case models.TestSetStatusInternalErr:
			restartApp = true
			testSetResult = false
		case models.TestSetStatusFaultUserApp:
			restartApp = true
			testSetResult = false
		case models.TestSetStatusUserAbort:
			return nil
		case models.TestSetStatusFailed:
			restartApp = false
			testSetResult = false
		case models.TestSetStatusPassed:
			restartApp = false
			testSetResult = true
		}
		testRunResult = testRunResult && testSetResult
		if stopApp {
			appCtxCancel()
			if runAppErrGrp != nil {
				err := runAppErrGrp.Wait()
				if err != nil {
					utils.LogError(r.logger, err, "error in runAppErrGrp")
				}
			}
		}
	}

	testRunStatus := "fail"
	if testRunResult {
		testRunStatus = "pass"
	}
	r.telemetry.TestRun(totalTestPassed, totalTestFailed, len(testSetIDs), testRunStatus)

	if !abortTestRun {
		r.printSummary(ctx, testRunResult)
	}
	return nil
}

func (r *Replayer) BootReplay(ctx context.Context) (string, uint64, context.CancelFunc, error) {

	var cancel context.CancelFunc

	testRunIDs, err := r.reportDB.GetAllTestRunIDs(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", 0, nil, err
		}
		return "", 0, nil, fmt.Errorf("failed to get all test run ids: %w", err)
	}

	newTestRunID := pkg.NewID(testRunIDs, models.TestRunTemplateName)

	appID, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{Container: r.config.ContainerName, DockerNetwork: r.config.NetworkName, DockerDelay: r.config.BuildDelay})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", 0, nil, err
		}
		return "", 0, nil, fmt.Errorf("failed to setup instrumentation: %w", err)
	}

	// starting the hooks and proxy
	select {
	case <-ctx.Done():
		return "", 0, nil, context.Canceled
	default:
		hookCtx := context.WithoutCancel(ctx)
		hookCtx, cancel = context.WithCancel(hookCtx)
		err = r.instrumentation.Hook(hookCtx, appID, models.HookOptions{Mode: models.MODE_TEST})
		if err != nil {
			cancel()
			if errors.Is(err, context.Canceled) {
				return "", 0, nil, err
			}
			return "", 0, nil, fmt.Errorf("failed to start the hooks and proxy: %w", err)
		}
	}

	return newTestRunID, appID, cancel, nil
}

func (r *Replayer) GetAllTestSetIDs(ctx context.Context) ([]string, error) {
	return r.testDB.GetAllTestSetIDs(ctx)
}

func (r *Replayer) SetUpRunTestSet(ctx context.Context, testSetID string, appID uint64) error {

	filteredMocks, err := r.mockDB.GetFilteredMocks(ctx, testSetID, models.BaseTime, time.Now())
	if err != nil {
		utils.LogError(r.logger, err, "failed to get filtered mocks")
		return err
	}

	unfilteredMocks, err := r.mockDB.GetUnFilteredMocks(ctx, testSetID, models.BaseTime, time.Now())
	if err != nil {
		utils.LogError(r.logger, err, "failed to get unfiltered mocks")
		return err
	}

	err = r.instrumentation.MockOutgoing(ctx, appID, models.OutgoingOptions{
		Rules:         r.config.BypassRules,
		MongoPassword: r.config.Test.MongoPassword,
		SQLDelay:      time.Duration(r.config.Test.Delay),
	})
	if err != nil {
		utils.LogError(r.logger, err, "failed to mock outgoing")
		return err
	}

	err = r.instrumentation.SetMocks(ctx, appID, filteredMocks, unfilteredMocks)
	if err != nil {
		utils.LogError(r.logger, err, "failed to set mocks")
		return err
	}

	return nil
}

func (r *Replayer) RunTestSet(ctx context.Context, testSetID string, testRunID string, appID uint64, appErrChan chan models.AppError) (models.TestSetStatus, int, int, int, error) {

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	runTestSetErrGrp, runTestSetCtx := errgroup.WithContext(ctx)
	runTestSetCtx = context.WithValue(runTestSetCtx, models.ErrGroupKey, runTestSetErrGrp)

	runTestSetCtx, runTestSetCtxCancel := context.WithCancel(runTestSetCtx)

	exitLoopChan := make(chan bool, 2)
	defer func() {
		runTestSetCtxCancel()
		err := runTestSetErrGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "error in testLoopErrGrp")
		}
		close(exitLoopChan)
	}()

	var success int
	var failure int
	var totalConsumedMocks = map[string]bool{}

	testSetStatus := models.TestSetStatusPassed
	testSetStatusByErrChan := models.TestSetStatusRunning

	r.logger.Info("running", zap.Any("test-set", models.HighlightString(testSetID)))

	testCases, err := r.testDB.GetTestCases(runTestSetCtx, testSetID)
	if err != nil {
		return models.TestSetStatusFailed, 0, 0, 0, fmt.Errorf("failed to get test cases: %w", err)
	}

	if len(testCases) == 0 {
		return models.TestSetStatusPassed, 0, 0, 0, nil
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
		return testSetStatusByErrChan, len(testCases), 0, len(testCases), nil
	}

	selectedTests := ArrayToMap(r.config.Test.SelectedTests[testSetID])

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
		return models.TestSetStatusFailed, len(testCases), 0, len(testCases), nil
	}

	// var to exit the loop
	var exitLoop bool
	// var to store the error in the loop
	var loopErr error

	for _, testCase := range testCases {

		if _, ok := selectedTests[testCase.Name]; !ok && len(selectedTests) != 0 {
			continue
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

		filteredMocks, loopErr := r.mockDB.GetFilteredMocks(runTestSetCtx, testSetID, testCase.HTTPReq.Timestamp, testCase.HTTPResp.Timestamp)
		if loopErr != nil {
			utils.LogError(r.logger, err, "failed to get filtered mocks")
			break
		}
		unfilteredMocks, loopErr := r.mockDB.GetUnFilteredMocks(runTestSetCtx, testSetID, testCase.HTTPReq.Timestamp, testCase.HTTPResp.Timestamp)
		if loopErr != nil {
			utils.LogError(r.logger, err, "failed to get unfiltered mocks")
			break
		}

		loopErr = r.instrumentation.SetMocks(runTestSetCtx, appID, filteredMocks, unfilteredMocks)
		if loopErr != nil {
			utils.LogError(r.logger, err, "failed to set mocks")
			break
		}

		started := time.Now().UTC()

		cmdType := utils.FindDockerCmd(r.config.Command)

		if cmdType == utils.Docker || cmdType == utils.DockerCompose {

			userIP, err := r.instrumentation.GetAppIP(ctx, appID)
			if err != nil {
				utils.LogError(r.logger, err, "failed to get the app ip")
				break
			}

			testCase.HTTPReq.URL, err = replaceHostToIP(testCase.HTTPReq.URL, userIP)
			if err != nil {
				utils.LogError(r.logger, err, "failed to replace host to docker container's IP")
				break
			}
			r.logger.Debug("", zap.Any("replaced URL in case of docker env", testCase.HTTPReq.URL))
		}

		resp, loopErr := emulator.SimulateRequest(runTestSetCtx, appID, testCase, testSetID)
		if loopErr != nil {
			utils.LogError(r.logger, err, "failed to simulate request")
			break
		}

		consumedMocks, err := r.instrumentation.GetConsumedMocks(runTestSetCtx, appID)
		if err != nil {
			utils.LogError(r.logger, err, "failed to get consumed filtered mocks")
		}
		if r.config.Test.RemoveUnusedMocks {
			for _, mockName := range consumedMocks {
				totalConsumedMocks[mockName] = true
			}
		}

		testPass, testResult = r.compareResp(testCase, resp, testSetID)
		if !testPass {
			// log the consumed mocks during the test run of the test case for test set
			r.logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(testCase.Name)), zap.Any("testset id", models.HighlightFailingString(testSetID)), zap.Any("passed", models.HighlightFailingString(testPass)), zap.Any("consumed mocks", consumedMocks))
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
				Res: models.HTTPResp{
					StatusCode:    testCase.HTTPResp.StatusCode,
					Header:        testCase.HTTPResp.Header,
					Body:          testCase.HTTPResp.Body,
					StatusMessage: testCase.HTTPResp.StatusMessage,
					ProtoMajor:    testCase.HTTPResp.ProtoMajor,
					ProtoMinor:    testCase.HTTPResp.ProtoMinor,
					Binary:        testCase.HTTPResp.Binary,
					Timestamp:     testCase.HTTPResp.Timestamp,
				},
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
	}

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
		Tests:   testCaseResults,
	}

	// final report should have reason for sudden stop of the test run so this should get canceled
	reportCtx := context.WithoutCancel(runTestSetCtx)
	err = r.reportDB.InsertReport(reportCtx, testRunID, testSetID, testReport)
	if err != nil {
		utils.LogError(r.logger, err, "failed to insert report")
		return models.TestSetStatusInternalErr, testReport.Total, testReport.Success, testReport.Failure, fmt.Errorf("failed to insert report")
	}

	// remove the unused mocks by the test cases of a testset
	if r.config.Test.RemoveUnusedMocks && testSetStatus == models.TestSetStatusPassed {
		r.logger.Debug("consumed mocks from the completed testset", zap.Any("for test-set", testSetID), zap.Any("consumed mocks", totalConsumedMocks))
		// delete the unused mocks from the data store
		err = r.mockDB.UpdateMocks(runTestSetCtx, testSetID, totalConsumedMocks)
		if err != nil {
			utils.LogError(r.logger, err, "failed to delete unused mocks")
		}
	}

	return testSetStatus, testReport.Total, testReport.Success, testReport.Failure, nil
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
	return match(tc, actualResponse, noiseConfig, r.config.Test.IgnoreOrdering, r.logger)
}

func (r *Replayer) printSummary(ctx context.Context, testRunResult bool) {
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
		if _, err := pp.Printf("\n <=========================================> \n  COMPLETE TESTRUN SUMMARY. \n\tTotal tests: %s\n"+"\tTotal test passed: %s\n"+"\tTotal test failed: %s\n", totalTests, totalTestPassed, totalTestFailed); err != nil {
			utils.LogError(r.logger, err, "failed to print test run summary")
			return
		}
		if _, err := pp.Printf("\n\tTest Suite Name\t\tTotal Test\tPassed\t\tFailed\t\n"); err != nil {
			utils.LogError(r.logger, err, "failed to print test suite summary")
			return
		}
		for _, testSuiteName := range testSuiteNames {
			if completeTestReport[testSuiteName].status {
				pp.SetColorScheme(models.PassingColorScheme)
			} else {
				pp.SetColorScheme(models.FailingColorScheme)
			}
			if _, err := pp.Printf("\n\t%s\t\t%s\t\t%s\t\t%s", testSuiteName, completeTestReport[testSuiteName].total, completeTestReport[testSuiteName].passed, completeTestReport[testSuiteName].failed); err != nil {
				utils.LogError(r.logger, err, "failed to print test suite details")
				return
			}
		}
		if _, err := pp.Printf("\n<=========================================> \n\n"); err != nil {
			utils.LogError(r.logger, err, "failed to print separator")
			return
		}
		r.logger.Info("test run completed", zap.Bool("passed overall", testRunResult))
		if r.config.Test.GoCoverage {
			r.logger.Info("there is a opportunity to get the coverage here")
			coverCmd := exec.CommandContext(ctx, "go", "tool", "covdata", "percent", "-i="+os.Getenv("GOCOVERDIR"))
			output, err := coverCmd.Output()
			if err != nil {
				utils.LogError(r.logger, err, "failed to get the coverage of the go binary", zap.Any("cmd", coverCmd.String()))
			}
			r.logger.Sugar().Infoln("\n", models.HighlightPassingString(string(output)))
			generateCovTxtCmd := exec.CommandContext(ctx, "go", "tool", "covdata", "textfmt", "-i="+os.Getenv("GOCOVERDIR"), "-o="+os.Getenv("GOCOVERDIR")+"/total-coverage.txt")
			output, err = generateCovTxtCmd.Output()
			if err != nil {
				utils.LogError(r.logger, err, "failed to get the coverage of the go binary", zap.Any("cmd", coverCmd.String()))
			}
			if len(output) > 0 {
				r.logger.Sugar().Infoln("\n", models.HighlightFailingString(string(output)))
			}
		}
	}
}

func (r *Replayer) RunApplication(ctx context.Context, appID uint64, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, appID, opts)
}

func (r *Replayer) ProvideMocks(ctx context.Context) error {
	var stopReason string
	var hookCancel context.CancelFunc
	defer func() {
		select {
		case <-ctx.Done():
			return
		default:
			err := utils.Stop(r.logger, stopReason)
			if err != nil {
				utils.LogError(r.logger, err, "failed to stop mock replay")
			}
		}
		if hookCancel != nil {
			hookCancel()
		}
	}()

	filteredMocks, err := r.mockDB.GetFilteredMocks(ctx, "", time.Time{}, time.Now())
	if err != nil {
		stopReason = "failed to get filtered mocks"
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}

	unfilteredMocks, err := r.mockDB.GetUnFilteredMocks(ctx, "", time.Time{}, time.Now())
	if err != nil {
		stopReason = "failed to get unfiltered mocks"
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}

	_, appID, hookCancel, err := r.BootReplay(ctx)
	if err != nil {
		stopReason = "failed to boot replay"
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}

	err = r.instrumentation.SetMocks(ctx, appID, filteredMocks, unfilteredMocks)
	if err != nil {
		stopReason = "failed to set mocks"
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}
	<-ctx.Done()
	return nil
}

func sortTestSetsByCmdFrequency(testSetCommands map[string]string) []string {
	// Count the frequency of each command
	cmdFrequency := make(map[string]int)
	for _, cmd := range testSetCommands {
		cmdFrequency[cmd]++
	}

	// Create a slice of test set IDs sorted by command frequency
	sortedTestSetIDs := make([]string, 0, len(testSetCommands))
	for testSetID := range testSetCommands {
		sortedTestSetIDs = append(sortedTestSetIDs, testSetID)
	}
	sort.Slice(sortedTestSetIDs, func(i, j int) bool {
		cmdI, cmdJ := testSetCommands[sortedTestSetIDs[i]], testSetCommands[sortedTestSetIDs[j]]
		return cmdFrequency[cmdI] > cmdFrequency[cmdJ] // Sort in descending order of frequency
	})

	return sortedTestSetIDs
}
