package replay

import (
	"context"
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
	g, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	var stopReason string
	defer func() {
		select {
		case <-ctx.Done():
			break
		default:
			err := utils.Stop(r.logger, stopReason)
			if err != nil {
				r.logger.Error("failed to stop replay", zap.Error(err))
			}
		}
		err := g.Wait()
		if err != nil {
			r.logger.Error("failed to stop recording", zap.Error(err))
		}
	}()
	testRunId, appId, err := r.BootReplay(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to boot replay: %v", err)
		r.logger.Error(stopReason, zap.Error(err))
		return fmt.Errorf(stopReason)
	}
	testSetIds, err := r.testDB.GetAllTestSetIds(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get all test set ids: %v", err)
		r.logger.Error(stopReason, zap.Error(err))
		return fmt.Errorf(stopReason)
	}

	testSetResult := false
	testRunResult := true
	abort := false
	for _, testSetId := range testSetIds {
		testSetStatus, err := r.RunTestSet(ctx, testSetId, testRunId, appId, false)
		if err != nil {
			stopReason = fmt.Sprintf("failed to run test set: %v", err)
			r.logger.Error(stopReason, zap.Error(err))
			return fmt.Errorf(stopReason)
		}
		switch testSetStatus {
		case models.TestSetStatusAppHalted:
			testSetResult = false
			abort = true
		case models.TestSetStatusFaultUserApp:
			testSetResult = false
			abort = true
		case models.TestSetStatusUserAbort:
			return nil
		case models.TestSetStatusFailed:
			testSetResult = false
		case models.TestSetStatusPassed:
			testSetResult = true
		}
		testRunResult = testRunResult && testSetResult
		if abort {
			break
		}
	}
	if !abort {
		r.printSummary(ctx, testRunResult)
	}
	return nil
}

func (r *replayer) BootReplay(ctx context.Context) (string, uint64, error) {
	testRunIds, err := r.reportDB.GetAllTestRunIds(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get all test run ids: %w", err)
	}

	newTestRunId := pkg.NewId(testRunIds, models.TestRunTemplateName)
	appId, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("failed to setup instrumentation: %w", err)
	}

	err = r.instrumentation.Hook(ctx, appId, models.HookOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("failed to start the hooks and proxy: %w", err)
	}

	return newTestRunId, appId, nil
}

func (r *replayer) GetAllTestSetIds(ctx context.Context) ([]string, error) {
	return r.testDB.GetAllTestSetIds(ctx)
}

func (r *replayer) RunTestSet(ctx context.Context, testSetId string, testRunId string, appId uint64, serveTest bool) (models.TestSetStatus, error) {
	var testLoopCtx, testLoopCtxCancel = context.WithCancel(ctx)
	defer testLoopCtxCancel()
	var runTestSetCtx, runTestSetCtxCancel = context.WithCancel(ctx)
	defer runTestSetCtxCancel()
	runTestSetErrGrp, runTestSetCtx := errgroup.WithContext(runTestSetCtx)
	runTestSetCtx = context.WithValue(runTestSetCtx, models.ErrGroupKey, runTestSetErrGrp)
	testLoopErrGrp, testLoopCtx := errgroup.WithContext(testLoopCtx)
	testLoopCtx = context.WithValue(testLoopCtx, models.ErrGroupKey, testLoopErrGrp)
	var mockErrChan <-chan error
	var appErrChan = make(chan models.AppError)
	var appErr models.AppError
	var success int
	var failure int
	testSetStatus := models.TestSetStatusPassed
	testSetStatusByErrChan := models.TestSetStatusRunning

	testCases, err := r.testDB.GetTestCases(runTestSetCtx, testSetId)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to get test cases: %w", err)
	}

	if len(testCases) == 0 {
		return models.TestSetStatusPassed, nil
	}

	testReport := &models.TestReport{
		Version: models.GetVersion(),
		Total:   len(testCases),
		Status:  string(models.TestStatusRunning),
	}

	// Inserting the report with status running
	err = r.reportDB.InsertReport(runTestSetCtx, testRunId, testSetId, testReport)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to insert report: %w", err)
	}

	filteredMocks, err := r.mockDB.GetFilteredMocks(runTestSetCtx, testSetId, time.Time{}, time.Now())
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to get filtered mocks: %w", err)
	}
	unfilteredMocks, err := r.mockDB.GetUnFilteredMocks(runTestSetCtx, testSetId, time.Time{}, time.Now())
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to get unfiltered mocks: %w", err)
	}
	err = r.instrumentation.SetMocks(runTestSetCtx, appId, filteredMocks, unfilteredMocks)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to set mocks: %w", err)
	}
	mockErrChan = r.instrumentation.MockOutgoing(runTestSetCtx, appId, models.OutgoingOptions{})
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to mock outgoing: %w", err)
	}

	if !serveTest {
		runTestSetErrGrp.Go(func() error {
			defer utils.Recover(r.logger)
			appErr = r.RunApplication(runTestSetCtx, appId, models.RunOptions{})
			if appErr.AppErrorType == models.ErrCtxCanceled {
				return nil
			}
			appErrChan <- appErr
			return nil
		})
	}

	time.Sleep(time.Duration(r.config.Test.Delay) * time.Second)

	exitLoop := false

	// Checking for errors in the mocking and application
	go func() {
		defer utils.Recover(r.logger)
		select {
		case err := <-mockErrChan:
			r.logger.Error("failed to mock outgoing", zap.Error(err))
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
				return
			case models.ErrInternal:
				testSetStatusByErrChan = models.TestSetStatusInternalErr
			default:
				testSetStatusByErrChan = models.TestSetStatusAppHalted
			}
			r.logger.Error("application failed to run", zap.Error(err))
		case <-ctx.Done():
			testSetStatusByErrChan = models.TestSetStatusUserAbort
		}
		exitLoop = true
		testLoopCtxCancel()
		err := testLoopErrGrp.Wait()
		if err != nil {
			r.logger.Error("error in testLoop go routine", zap.Error(err))
		}
	}()

	for _, testCase := range testCases {

		if exitLoop {
			testSetStatus = testSetStatusByErrChan
			break
		}

		testStatus := models.TestStatusPending
		var testResult *models.Result
		var testPass bool

		filteredMocks, err := r.mockDB.GetFilteredMocks(testLoopCtx, testSetId, testCase.HttpReq.Timestamp, testCase.HttpResp.Timestamp)
		if err != nil {
			return models.TestSetStatusFailed, fmt.Errorf("failed to get filtered mocks: %w", err)
		}
		unfilteredMocks, err := r.mockDB.GetUnFilteredMocks(testLoopCtx, testSetId, testCase.HttpReq.Timestamp, testCase.HttpResp.Timestamp)
		if err != nil {
			return models.TestSetStatusFailed, fmt.Errorf("failed to get unfiltered mocks: %w", err)
		}
		err = r.instrumentation.SetMocks(testLoopCtx, appId, filteredMocks, unfilteredMocks)
		if err != nil {
			return models.TestSetStatusFailed, fmt.Errorf("failed to set mocks: %w", err)
		}
		started := time.Now().UTC()
		resp, err := r.SimulateRequest(testLoopCtx, appId, testCase, testSetId)
		if err != nil && resp == nil {
			r.logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(testCase.Name)), zap.Any("testset id", models.HighlightFailingString(testSetId)), zap.Any("passed", models.HighlightFailingString("false")))
		} else {
			testPass, testResult = r.compareResp(testCase, resp, testSetId)
			if !testPass {
				r.logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(testCase.Name)), zap.Any("testset id", models.HighlightFailingString(testSetId)), zap.Any("passed", models.HighlightFailingString(testPass)))
			} else {
				r.logger.Info("result", zap.Any("testcase id", models.HighlightPassingString(testCase.Name)), zap.Any("testset id", models.HighlightPassingString(testSetId)), zap.Any("passed", models.HighlightPassingString(testPass)))
			}
			if testPass {
				testStatus = models.TestStatusPassed
				success++
			} else {
				testStatus = models.TestStatusFailed
				failure++
			}
		}

		// TODO remove this context canceled error is done
		if testResult != nil {

			testCaseResult := &models.TestResult{
				Kind:       models.HTTP,
				Name:       testSetId,
				Status:     testStatus,
				Started:    started.Unix(),
				Completed:  time.Now().UTC().Unix(),
				TestCaseID: testCase.Name,
				Req: models.HttpReq{
					Method:     testCase.HttpReq.Method,
					ProtoMajor: testCase.HttpReq.ProtoMajor,
					ProtoMinor: testCase.HttpReq.ProtoMinor,
					URL:        testCase.HttpReq.URL,
					URLParams:  testCase.HttpReq.URLParams,
					Header:     testCase.HttpReq.Header,
					Body:       testCase.HttpReq.Body,
					Binary:     testCase.HttpReq.Binary,
					Form:       testCase.HttpReq.Form,
					Timestamp:  testCase.HttpReq.Timestamp,
				},
				Res: models.HttpResp{
					StatusCode:    testCase.HttpResp.StatusCode,
					Header:        testCase.HttpResp.Header,
					Body:          testCase.HttpResp.Body,
					StatusMessage: testCase.HttpResp.StatusMessage,
					ProtoMajor:    testCase.HttpResp.ProtoMajor,
					ProtoMinor:    testCase.HttpResp.ProtoMinor,
					Binary:        testCase.HttpResp.Binary,
					Timestamp:     testCase.HttpResp.Timestamp,
				},
				Noise:  testCase.Noise,
				Result: *testResult,
			}
			err = r.reportDB.InsertTestCaseResult(testLoopCtx, testRunId, testSetId, testCase.Name, testCaseResult)
			if err != nil {
				return models.TestSetStatusFailed, fmt.Errorf("failed to insert test case result: %w", err)
			}
			if !testPass {
				testSetStatus = models.TestSetStatusFailed
			}
		}
	}

	testCaseResults, err := r.reportDB.GetTestCaseResults(runTestSetCtx, testRunId, testSetId)
	if err != nil {
		if runTestSetCtx.Err() != context.Canceled {
			return models.TestSetStatusFailed, fmt.Errorf("failed to get test case results: %w", err)
		}
	}

	testReport = &models.TestReport{
		TestSet: testSetId,
		Status:  string(testSetStatus),
		Total:   len(testCases),
		Success: success,
		Failure: failure,
		Tests:   testCaseResults,
		ID:      testRunId,
	}

	// final report should have reason for sudden stop the test run.
	reportCtx := context.WithoutCancel(runTestSetCtx)
	// write final report
	err = r.reportDB.InsertReport(reportCtx, testRunId, testSetId, testReport)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to insert report: %w", err)
	}

	// TODO Need to decide on whether to use global variable or not
	verdict := TestReportVerdict{
		total:  testReport.Total,
		failed: testReport.Failure,
		passed: testReport.Success,
	}

	completeTestReport[testSetId] = verdict
	totalTests += testReport.Total
	totalTestPassed += testReport.Success
	totalTestFailed += testReport.Failure

	runTestSetCtxCancel()
	err = runTestSetErrGrp.Wait()
	if err != nil {
		r.logger.Error("error in runTestSetErrGrp", zap.Error(err))
	}
	return testSetStatus, nil
}

func (r *replayer) GetTestSetStatus(ctx context.Context, testRunId string, testSetId string) (models.TestSetStatus, error) {
	testReport, err := r.reportDB.GetReport(ctx, testRunId, testSetId)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to get report: %w", err)
	}
	status, err := models.StringToTestSetStatus(testReport.Status)
	if err != nil {
		return models.TestSetStatusFailed, fmt.Errorf("failed to convert string to test set status: %w", err)
	}
	return status, nil
}

func (r *replayer) SimulateRequest(ctx context.Context, appId uint64, tc *models.TestCase, testSetId string) (*models.HttpResp, error) {
	switch tc.Kind {
	case models.HTTP:
		r.logger.Debug("Before simulating the request", zap.Any("Test case", tc))
		cmdType := utils.FindDockerCmd(r.config.Command)
		if cmdType == utils.Docker || cmdType == utils.DockerCompose {
			var err error

			userIp, err := r.instrumentation.GetAppIp(ctx, appId)
			if err != nil {
				r.logger.Error("failed to get the app ip", zap.Error(err))
				return nil, err
			}

			tc.HttpReq.URL, err = replaceHostToIP(tc.HttpReq.URL, userIp)
			if err != nil {
				r.logger.Error("failed to replace host to docker container's IP", zap.Error(err))
			}
			r.logger.Debug("", zap.Any("replaced URL in case of docker env", tc.HttpReq.URL))
		}
		r.logger.Debug(fmt.Sprintf("the url of the testcase: %v", tc.HttpReq.URL))
		resp, err := pkg.SimulateHttp(ctx, *tc, testSetId, r.logger, r.config.Test.ApiTimeout)
		r.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
		r.logger.Debug("After GetResp of the request", zap.Any("test case id", tc.Name))
		return resp, err
	}
	return nil, nil
}

func (r *replayer) compareResp(tc *models.TestCase, actualResponse *models.HttpResp, testSetId string) (bool, *models.Result) {

	noiseConfig := r.config.Test.GlobalNoise.Global
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetId]; ok {
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
		pp.Printf("\n <=========================================> \n  COMPLETE TESTRUN SUMMARY. \n\tTotal tests: %s\n"+"\tTotal test passed: %s\n"+"\tTotal test failed: %s\n", totalTests, totalTestPassed, totalTestFailed)
		pp.Printf("\n\tTest Suite Name\t\tTotal Test\tPassed\t\tFailed\t\n")
		for _, testSuiteName := range testSuiteNames {
			pp.Printf("\n\t%s\t\t%s\t\t%s\t\t%s", testSuiteName, completeTestReport[testSuiteName].total, completeTestReport[testSuiteName].passed, completeTestReport[testSuiteName].failed)
		}
		pp.Printf("\n<=========================================> \n\n")
		r.logger.Info("test run completed", zap.Bool("passed overall", testRunResult))
		if r.config.Test.Coverage {
			r.logger.Info("there is a opportunity to get the coverage here")
			coverCmd := exec.CommandContext(ctx, "go", "tool", "covdata", "percent", "-i="+os.Getenv("GOCOVERDIR"))
			output, err := coverCmd.Output()
			if err != nil {
				r.logger.Error("failed to get the coverage of the go binary", zap.Error(err), zap.Any("cmd", coverCmd.String()))
			}
			r.logger.Sugar().Infoln("\n", models.HighlightPassingString(string(output)))
			generateCovTxtCmd := exec.CommandContext(ctx, "go", "tool", "covdata", "textfmt", "-i="+os.Getenv("GOCOVERDIR"), "-o="+os.Getenv("GOCOVERDIR")+"/total-coverage.txt")
			output, err = generateCovTxtCmd.Output()
			if err != nil {
				r.logger.Error("failed to get the coverage of the go binary", zap.Error(err), zap.Any("cmd", coverCmd.String()))
			}
			if len(output) > 0 {
				r.logger.Sugar().Infoln("\n", models.HighlightFailingString(string(output)))
			}
		}
	}
}

func (r *replayer) RunApplication(ctx context.Context, appId uint64, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, appId, opts)
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
				r.logger.Error("failed to stop mock replay", zap.Error(err))
			}

		}
	}()

	filteredMocks, err := r.mockDB.GetFilteredMocks(ctx, "", time.Time{}, time.Now())
	if err != nil {
		stopReason = "failed to get filtered mocks"
		r.logger.Error(stopReason, zap.Error(err))
		return fmt.Errorf(stopReason)
	}
	unfilteredMocks, err := r.mockDB.GetUnFilteredMocks(ctx, "", time.Time{}, time.Now())
	if err != nil {
		stopReason = "failed to get unfiltered mocks"
		r.logger.Error(stopReason, zap.Error(err))
		return fmt.Errorf(stopReason)
	}

	_, appId, err := r.BootReplay(ctx)
	if err != nil {
		stopReason = "failed to boot replay"
		r.logger.Error(stopReason, zap.Error(err))
		return fmt.Errorf(stopReason)
	}

	err = r.instrumentation.SetMocks(ctx, appId, filteredMocks, unfilteredMocks)
	if err != nil {
		stopReason = "failed to set mocks"
		r.logger.Error(stopReason, zap.Error(err))
		return fmt.Errorf(stopReason)
	}
	<-ctx.Done()
	return nil
}
