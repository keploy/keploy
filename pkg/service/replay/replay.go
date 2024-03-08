package replay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

type replayer struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	reportDB        ReportDB
	telemetry       Telemetry
	instrumentation Instrumentation
	config          config.Config
}

func NewReplayer(logger *zap.Logger, testDB TestDB, mockDB MockDB, reportDB ReportDB, telemetry Telemetry, instrumentation Instrumentation, config config.Config) Service {
	return &replayer{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		reportDB:        reportDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          config,
	}
}

func (r *replayer) Start(ctx context.Context) error {

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	g, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	var stopReason string
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
		err := g.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop recording")
		}
	}()

	// BootReplay will start the hooks and proxy and return the testRunID and appID
	testRunID, appID, err := r.BootReplay(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to boot replay: %v", err)
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get all test set ids: %v", err)
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	testSetResult := false
	testRunResult := true
	abortTestRun := false
	for _, testSetID := range testSetIDs {
		testSetStatus, err := r.RunTestSet(ctx, testSetID, testRunID, appID, false)
		if err != nil {
			stopReason = fmt.Sprintf("failed to run test set: %v", err)
			utils.LogError(r.logger, err, stopReason)
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
		}
		testRunResult = testRunResult && testSetResult
		if abortTestRun {
			break
		}
	}
	if !abortTestRun {
		r.printSummary(ctx, testRunResult)
	}
	return nil
}

func (r *replayer) BootReplay(ctx context.Context) (string, uint64, error) {

	testRunIDs, err := r.reportDB.GetAllTestRunIDs(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", 0, err
		}
		return "", 0, fmt.Errorf("failed to get all test run ids: %w", err)
	}

	newTestRunID := pkg.NewID(testRunIDs, models.TestRunTemplateName)

	appID, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", 0, err
		}
		return "", 0, fmt.Errorf("failed to setup instrumentation: %w", err)
	}

	// starting the hooks and proxy
	select {
	case <-ctx.Done():
		return "", 0, context.Canceled
	default:
		err = r.instrumentation.Hook(ctx, appID, models.HookOptions{})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return "", 0, err
			}
			return "", 0, fmt.Errorf("failed to start the hooks and proxy: %w", err)
		}
	}

	return newTestRunID, appID, nil
}

func (r *replayer) GetAllTestSetIDs(ctx context.Context) ([]string, error) {
	return r.testDB.GetAllTestSetIDs(ctx)
}

func (r *replayer) RunTestSet(ctx context.Context, testSetID string, testRunID string, appID uint64, serveTest bool) (models.TestSetStatus, error) {

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

	var mockErrChan <-chan error
	var appErrChan = make(chan models.AppError)
	var appErr models.AppError
	var success int
	var failure int

	testSetStatus := models.TestSetStatusPassed
	testSetStatusByErrChan := models.TestSetStatusRunning

	testCases, err := r.testDB.GetTestCases(runTestSetCtx, testSetID)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to get test cases: %w", err)
	}

	if len(testCases) == 0 {
		return models.TestSetStatusPassed, nil
	}

	filteredMocks, err := r.mockDB.GetFilteredMocks(runTestSetCtx, testSetID, time.Time{}, time.Now())
	if err != nil {
		utils.LogError(r.logger, err, "failed to get filtered mocks")
		return models.TestSetStatusFailed, err
	}
	unfilteredMocks, err := r.mockDB.GetUnFilteredMocks(runTestSetCtx, testSetID, time.Time{}, time.Now())
	if err != nil {
		utils.LogError(r.logger, err, "failed to get unfiltered mocks")
		return models.TestSetStatusFailed, err
	}
	err = r.instrumentation.SetMocks(runTestSetCtx, appID, filteredMocks, unfilteredMocks)
	if err != nil {
		utils.LogError(r.logger, err, "failed to set mocks")
		return models.TestSetStatusFailed, err
	}
	mockErrChan = r.instrumentation.MockOutgoing(runTestSetCtx, appID, models.OutgoingOptions{})
	if err != nil {
		utils.LogError(r.logger, err, "failed to mock outgoing")
		return models.TestSetStatusFailed, err
	}

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
		case err := <-mockErrChan:
			utils.LogError(r.logger, err, "failed to mock outgoing")
			testSetStatusByErrChan = models.TestSetStatusFailed
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
	}

	// Inserting the initial report for the test set
	testReport := &models.TestReport{
		Version: models.GetVersion(),
		Total:   len(testCases),
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

	for _, testCase := range testCases {

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
		resp, loopErr := r.SimulateRequest(runTestSetCtx, appID, testCase, testSetID)
		if loopErr != nil {
			utils.LogError(r.logger, err, "failed to simulate request")
			break
		}
		testPass, testResult = r.compareResp(testCase, resp, testSetID)
		if !testPass {
			r.logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(testCase.Name)), zap.Any("testset id", models.HighlightFailingString(testSetID)), zap.Any("passed", models.HighlightFailingString(testPass)))
		} else {
			r.logger.Info("result", zap.Any("testcase id", models.HighlightPassingString(testCase.Name)), zap.Any("testset id", models.HighlightPassingString(testSetID)), zap.Any("passed", models.HighlightPassingString(testPass)))
		}
		if testPass {
			testStatus = models.TestStatusPassed
			success++
		} else {
			testStatus = models.TestStatusFailed
			failure++
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
				TestCasePath: r.config.Path,
				MockPath:     r.config.Path,
				Noise:        testCase.Noise,
				Result:       *testResult,
			}
			loopErr = r.reportDB.InsertTestCaseResult(runTestSetCtx, testRunID, testSetID, testCaseResult)
			if loopErr != nil {
				utils.LogError(r.logger, err, "failed to insert test case result")
				break
			}
			if !testPass {
				testSetStatus = models.TestSetStatusFailed
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

	// Checking errors for fina iteration
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
		Total:   len(testCases),
		Success: success,
		Failure: failure,
		Tests:   testCaseResults,
	}

	// final report should have reason for sudden stop of the test run so this should get canceled
	reportCtx := context.WithoutCancel(runTestSetCtx)
	err = r.reportDB.InsertReport(reportCtx, testRunID, testSetID, testReport)
	if err != nil {
		utils.LogError(r.logger, err, "failed to insert report")
		return models.TestSetStatusInternalErr, fmt.Errorf("failed to insert report")
	}

	// TODO Need to decide on whether to use global variable or not
	verdict := TestReportVerdict{
		total:  testReport.Total,
		failed: testReport.Failure,
		passed: testReport.Success,
	}

	completeTestReport[testSetID] = verdict
	totalTests += testReport.Total
	totalTestPassed += testReport.Success
	totalTestFailed += testReport.Failure

	runTestSetCtxCancel()
	err = runTestSetErrGrp.Wait()
	if err != nil {
		utils.LogError(r.logger, err, "error in runTestSetErrGrp")
		return models.TestSetStatusInternalErr, fmt.Errorf("error in runTestSetErrGrp")
	}
	return testSetStatus, nil
}

func (r *replayer) GetTestSetStatus(ctx context.Context, testRunID string, testSetID string) (models.TestSetStatus, error) {
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

func (r *replayer) SimulateRequest(ctx context.Context, appID uint64, tc *models.TestCase, testSetID string) (*models.HTTPResp, error) {
	switch tc.Kind {
	case models.HTTP:
		r.logger.Debug("Before simulating the request", zap.Any("Test case", tc))
		cmdType := utils.FindDockerCmd(r.config.Command)
		if cmdType == utils.Docker || cmdType == utils.DockerCompose {
			var err error

			userIP, err := r.instrumentation.GetAppIP(ctx, appID)
			if err != nil {
				utils.LogError(r.logger, err, "failed to get the app ip")
				return nil, err
			}

			tc.HTTPReq.URL, err = replaceHostToIP(tc.HTTPReq.URL, userIP)
			if err != nil {
				utils.LogError(r.logger, err, "failed to replace host to docker container's IP")
			}
			r.logger.Debug("", zap.Any("replaced URL in case of docker env", tc.HTTPReq.URL))
		}
		r.logger.Debug(fmt.Sprintf("the url of the testcase: %v", tc.HTTPReq.URL))
		resp, err := pkg.SimulateHTTP(ctx, *tc, testSetID, r.logger, r.config.Test.APITimeout)
		r.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
		r.logger.Debug("After GetResp of the request", zap.Any("test case id", tc.Name))
		return resp, err
	}
	return nil, nil
}

func (r *replayer) compareResp(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string) (bool, *models.Result) {

	noiseConfig := r.config.Test.GlobalNoise.Global
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetID]; ok {
		noiseConfig = LeftJoinNoise(r.config.Test.GlobalNoise.Global, tsNoise)
	}
	return match(tc, actualResponse, noiseConfig, r.config.Test.IgnoreOrdering, r.logger)
}

func (r *replayer) printSummary(ctx context.Context, testRunResult bool) {
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
		if r.config.Test.Coverage {
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

func (r *replayer) RunApplication(ctx context.Context, appID uint64, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, appID, opts)
}

func (r *replayer) ProvideMocks(ctx context.Context) error {
	var stopReason string
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
	}()

	filteredMocks, err := r.mockDB.GetFilteredMocks(ctx, "", time.Time{}, time.Now())
	if err != nil {
		stopReason = "failed to get filtered mocks"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}
	unfilteredMocks, err := r.mockDB.GetUnFilteredMocks(ctx, "", time.Time{}, time.Now())
	if err != nil {
		stopReason = "failed to get unfiltered mocks"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	_, appID, err := r.BootReplay(ctx)
	if err != nil {
		stopReason = "failed to boot replay"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	err = r.instrumentation.SetMocks(ctx, appID, filteredMocks, unfilteredMocks)
	if err != nil {
		stopReason = "failed to set mocks"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}
	<-ctx.Done()
	return nil
}
