package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/k0kubun/pp/v3"
	"github.com/wI2L/jsondiff"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/graph/model"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

var completeTestReport = make(map[string]TestReportVerdict)
var totalTests int
var totalTestPassed int
var totalTestFailed int
var aborted = errors.New("aborted")

type replayer struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	reportDB        ReportDB
	telemetry       Telemetry
	instrumentation Instrumentation
	config          config.Config
	mutex           sync.Mutex
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

func (r *replayer) Replay(ctx context.Context) error {
	var stopReason = "User stopped replay"
	testRunId, appId, err := r.BootReplay(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to boot replay: %v", err)
		r.logger.Error(stopReason, zap.Error(err))
		return errors.New("failed to execute replay due to error in booting replay")
	}
	testSetIds, err := r.testDB.GetAllTestSetIds(ctx)
	if err != nil {
		stopReason = fmt.Sprintf("failed to get all test set ids: %v", err)
		r.logger.Error(stopReason, zap.Error(err))
		return errors.New("failed to execute replay due to error in getting all test set ids")
	}
	testRes := false
	result := true
	exitLoop := false

	for _, testSetId := range testSetIds {
		testSetStatus, err := r.RunTestSet(ctx, testSetId, testRunId, appId)
		if err != nil {
			stopReason = fmt.Sprintf("failed to run test set: %v", err)
			r.logger.Error(stopReason, zap.Error(err))
			return errors.New("failed to execute replay due to error in running test set")
		}
		switch testSetStatus {
		case models.TestRunStatusAppHalted:
			testRes = false
			exitLoop = true
		case models.TestRunStatusFaultUserApp:
			testRes = false
			exitLoop = true
		case models.TestRunStatusUserAbort:
			return nil
		case models.TestRunStatusFailed:
			testRes = false
		case models.TestRunStatusPassed:
			testRes = true
		}
		result = result && testRes
		if exitLoop {
			break
		}
	}
	// Sorting completeTestReport map according to testSuiteName (Keys)
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

	if totalTests > 0 {
		pp.Printf("\n <=========================================> \n  COMPLETE TESTRUN SUMMARY. \n\tTotal tests: %s\n"+"\tTotal test passed: %s\n"+"\tTotal test failed: %s\n", totalTests, totalTestPassed, totalTestFailed)
		pp.Printf("\n\tTest Suite Name\t\tTotal Test\tPassed\t\tFailed\t\n")
		for _, testSuiteName := range testSuiteNames {
			pp.Printf("\n\t%s\t\t%s\t\t%s\t\t%s", testSuiteName, completeTestReport[testSuiteName].total, completeTestReport[testSuiteName].passed, completeTestReport[testSuiteName].failed)
		}
		pp.Printf("\n<=========================================> \n\n")
		r.logger.Info("test run completed", zap.Bool("passed overall", result))
		if r.config.Test.Coverage {
			r.logger.Info("there is a opportunity to get the coverage here")
			coverCmd := exec.Command("go", "tool", "covdata", "percent", "-i="+os.Getenv("GOCOVERDIR"))
			output, err := coverCmd.Output()
			if err != nil {
				r.logger.Error("failed to get the coverage of the go binary", zap.Error(err), zap.Any("cmd", coverCmd.String()))
			}
			r.logger.Sugar().Infoln("\n", models.HighlightPassingString(string(output)))
			generateCovTxtCmd := exec.Command("go", "tool", "covdata", "textfmt", "-i="+os.Getenv("GOCOVERDIR"), "-o="+os.Getenv("GOCOVERDIR")+"/total-coverage.txt")
			output, err = generateCovTxtCmd.Output()
			if err != nil {
				r.logger.Error("failed to get the coverage of the go binary", zap.Error(err), zap.Any("cmd", coverCmd.String()))
			}
			if len(output) > 0 {
				r.logger.Sugar().Infoln("\n", models.HighlightFailingString(string(output)))
			}
		}
	}
	utils.Stop(r.logger, "replay completed")
	return nil
}

func (r *replayer) BootReplay(ctx context.Context) (string, uint64, error) {
	testRunIds, err := r.reportDB.GetAllTestRunIds(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get all test run ids: %w", err)
	}
	testRunId := pkg.NewId(testRunIds, models.TestRunTemplateName)

	appId, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("failed to setup instrumentation: %w", err)
	}

	err = r.instrumentation.Hook(ctx, appId, models.HookOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("failed to start the hooks and proxy: %w", err)
	}

	return testRunId, appId, nil
}

func (r *replayer) GetAllTestSetIds(ctx context.Context) ([]string, error) {
	return r.testDB.GetAllTestSetIds(ctx)
}

func (r *replayer) RunTestSet(ctx context.Context, testSetId string, testRunId string, appId uint64) (models.TestRunStatus, error) {
	var mockErrChan <-chan error
	var appErrChan = make(chan models.AppError)
	var appErr models.AppError
	var success int
	var failure int
	var testSetStatus models.TestRunStatus
	var simulateCtx, simulateCtxCancel = context.WithCancel(context.Background())

	testCases, err := r.testDB.GetTestCases(ctx, testSetId)
	if err != nil {
		return model.TestSetStatus{}, fmt.Errorf("failed to get test cases: %w", err)
	}
	// TODO for 0 test case
	filteredMocks, err := r.mockDB.GetFilteredMocks(ctx, testSetId, time.Time{}, time.Now())
	if err != nil {
		return model.TestSetStatus{}, fmt.Errorf("failed to get filtered mocks: %w", err)
	}
	unfilteredMocks, err := r.mockDB.GetUnFilteredMocks(ctx, testSetId, time.Time{}, time.Now())
	if err != nil {
		return model.TestSetStatus{}, fmt.Errorf("failed to get unfiltered mocks: %w", err)
	}
	err = r.instrumentation.SetMocks(ctx, appId, filteredMocks, unfilteredMocks)
	if err != nil {
		return model.TestSetStatus{}, fmt.Errorf("failed to set mocks: %w", err)
	}
	mockErrChan = r.instrumentation.MockOutgoing(ctx, appId, models.IncomingOptions{})
	if err != nil {
		return model.TestSetStatus{}, fmt.Errorf("failed to mock outgoing: %w", err)
	}
	go func() {
		appErr = r.instrumentation.Run(ctx, appId, models.RunOptions{})
		appErrChan <- appErr
	}()
	time.Sleep(time.Duration(r.config.Test.Delay) * time.Second)
	go func() {
		select {
		case err := <-mockErrChan:
			r.logger.Error("failed to mock outgoing", zap.Error(err))
			testSetStatus = models.TestRunStatusFailed
		case err := <-appErrChan:
			switch err.AppErrorType {
			case models.ErrCommandError:
				testSetStatus = models.TestRunStatusFaultUserApp
			case models.ErrUnExpected:
				testSetStatus = models.TestRunStatusAppHalted
			default:
				testSetStatus = models.TestRunStatusAppHalted
			}
		case <-ctx.Done():
			testSetStatus = models.TestRunStatusAppHalted
		}
		simulateCtxCancel()
	}()
	for _, testCase := range testCases {
		var testStatus models.TestStatus
		var testResult *models.Result
		var testPass bool

		filteredMocks, err := r.mockDB.GetFilteredMocks(ctx, testSetId, testCase.Spec.HttpReq.TimeStamp, testCase.Spec.HttpResp.TimeStamp)
		if err != nil {
			return model.TestSetStatus{}, fmt.Errorf("failed to get filtered mocks: %w", err)
		}
		unfilteredMocks, err := r.mockDB.GetUnFilteredMocks(ctx, testSetId, testCase.Spec.HttpReq.TimeStamp, testCase.Spec.HttpResp.TimeStamp)
		if err != nil {
			return model.TestSetStatus{}, fmt.Errorf("failed to get unfiltered mocks: %w", err)
		}
		err = r.instrumentation.SetMocks(ctx, appId, filteredMocks, unfilteredMocks)
		if err != nil {
			return model.TestSetStatus{}, fmt.Errorf("failed to set mocks: %w", err)
		}
		started := time.Now().UTC()
		resp, err := r.SimulateRequest(simulateCtx, testCase, testSetId)
		if err != nil && resp == nil {
			r.logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(testCase.Name)), zap.Any("testset id", models.HighlightFailingString(testSetId)), zap.Any("passed", models.HighlightFailingString("false")))
		} else {
			testPass, testResult = r.compareResp(testCase, resp, testSetId)
			if !testPass {
				r.logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(tc.Name)), zap.Any("testset id", models.HighlightFailingString(testSetId)), zap.Any("passed", models.HighlightFailingString(testPass)))
			} else {
				r.logger.Info("result", zap.Any("testcase id", models.HighlightPassingString(tc.Name)), zap.Any("testset id", models.HighlightPassingString(testSetId)), zap.Any("passed", models.HighlightPassingString(testPass)))
			}
			testStatus = models.TestStatusPending
			if testPass {
				testStatus = models.TestStatusPassed
				success++
			} else {
				testStatus = models.TestStatusFailed
				failure++
			}
		}

		testCaseResult := models.TestResult{
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
		r.reportDB.InsertTestCaseResult(ctx, testRunId, testSetId, testCase.Name, testCaseResult)
		if !testPass {
			testSetStatus = models.TestRunStatusFailed
		}
	}
	testCaseResults, err := r.reportDB.GetTestCaseResults(ctx, testRunId, testSetId)
	if err != nil {
		return model.TestSetStatus{}, fmt.Errorf("failed to get test case results: %w", err)
	}
	testReport := models.TestReport{
		TestSet: testSetId,
		Status:  string(testSetStatus),
		Total:   len(testCases),
		Success: success,
		Failure: failure,
		Tests:   testCaseResults,
		ID:      testRunId,
	}
	err = r.reportDB.InsertReport(ctx, testRunId, testSetId, testReport)
	if err != nil {
		return model.TestSetStatus{}, fmt.Errorf("failed to insert report: %w", err)
	}

	verdict := TestReportVerdict{total: testReport.Total, failed: testReport.Failure, passed: testReport.Success}

	completeTestReport[testSetId] = verdict
	totalTests += testReport.Total
	totalTestPassed += testReport.Success
	totalTestFailed += testReport.Failure

	return testSetStatus, nil
}

func (r *replayer) GetTestSetStatus(ctx context.Context, testRunId string, testSetId string) (model.TestSetStatus, error) {
	testReport, err := r.reportDB.GetReport(ctx, testRunId, testSetId)
	if err != nil {
		return model.TestSetStatus{}, fmt.Errorf("failed to get report: %w", err)
	}
	return model.TestSetStatus{
		Status: testReport.Status,
	}, nil
}

func (r *replayer) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetId string) (*models.HttpResp, error) {
	switch tc.Kind {
	case models.HTTP:
		started := time.Now().UTC()
		r.logger.Debug("Before simulating the request", zap.Any("Test case", tc))
		cmdType := utils.FindDockerCmd(r.config.Command)
		if cmdType == utils.Docker || cmdType == utils.DockerCompose {
			var err error
			tc.HttpReq.URL, err = replaceHostToIP(tc.HttpReq.URL, cfg.UserIP)
			if err != nil {
				r.logger.Error("failed to replace host to docker container's IP", zap.Error(err))
			}
			r.logger.Debug("", zap.Any("replaced URL in case of docker env", tc.HttpReq.URL))
		}
		r.logger.Debug(fmt.Sprintf("the url of the testcase: %v", tc.HttpReq.URL))
		resp, err := pkg.SimulateHttp(*tc, testSetId, r.logger, r.config.Test.ApiTimeout)
		r.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
		r.logger.Debug("After GetResp of the request", zap.Any("test case id", tc.Name))
		return resp, err
	}
}

func (r *replayer) compareResp(tc models.TestCase, actualResponse *models.HttpResp, testSetId string) (bool, *models.Result) {

	noiseConfig := r.config.Test.GlobalNoise.Global
	if tsNoise, ok := r.config.Test.GlobalNoise.Testsets[testSetId]; ok {
		noiseConfig = LeftJoinNoise(r.config.Test.GlobalNoise.Global, tsNoise)
	}

	bodyType := models.BodyTypePlain
	if json.Valid([]byte(actualResponse.Body)) {
		bodyType = models.BodyTypeJSON
	}
	pass := true
	hRes := &[]models.HeaderResult{}

	res := &models.Result{
		StatusCode: models.IntResult{
			Normal:   false,
			Expected: tc.HttpResp.StatusCode,
			Actual:   actualResponse.StatusCode,
		},
		BodyResult: []models.BodyResult{{
			Normal:   false,
			Type:     bodyType,
			Expected: tc.HttpResp.Body,
			Actual:   actualResponse.Body,
		}},
	}
	noise := tc.Noise

	var (
		bodyNoise   = noiseConfig["body"]
		headerNoise = noiseConfig["header"]
	)

	if bodyNoise == nil {
		bodyNoise = map[string][]string{}
	}
	if headerNoise == nil {
		headerNoise = map[string][]string{}
	}

	for field, regexArr := range noise {
		a := strings.Split(field, ".")
		if len(a) > 1 && a[0] == "body" {
			x := strings.Join(a[1:], ".")
			bodyNoise[x] = regexArr
		} else if a[0] == "header" {
			headerNoise[a[len(a)-1]] = regexArr
		}
	}

	// stores the json body after removing the noise
	cleanExp, cleanAct := tc.HttpResp.Body, actualResponse.Body
	var jsonComparisonResult jsonComparisonResult
	if !Contains(MapToArray(noise), "body") && bodyType == models.BodyTypeJSON {
		//validate the stored json
		validatedJSON, err := ValidateAndMarshalJson(r.logger, &cleanExp, &cleanAct)
		if err != nil {
			return false, res
		}
		if validatedJSON.isIdentical {
			jsonComparisonResult, err = JsonDiffWithNoiseControl(validatedJSON, bodyNoise, r.config.Test.IgnoreOrdering)
			pass = jsonComparisonResult.isExact
			if err != nil {
				return false, res
			}
		} else {
			pass = false
		}

		// debug log for cleanExp and cleanAct
		r.logger.Debug("cleanExp", zap.Any("", cleanExp))
		r.logger.Debug("cleanAct", zap.Any("", cleanAct))
	} else {
		if !Contains(MapToArray(noise), "body") && tc.HttpResp.Body != actualResponse.Body {
			pass = false
		}
	}

	res.BodyResult[0].Normal = pass

	if !CompareHeaders(pkg.ToHttpHeader(tc.HttpResp.Header), pkg.ToHttpHeader(actualResponse.Header), hRes, headerNoise) {

		pass = false
	}

	res.HeadersResult = *hRes
	if tc.HttpResp.StatusCode == actualResponse.StatusCode {
		res.StatusCode.Normal = true
	} else {

		pass = false
	}

	if !pass {
		logDiffs := NewDiffsPrinter(tc.Name)

		logger := pp.New()
		logger.WithLineInfo = false
		logger.SetColorScheme(models.FailingColorScheme)
		var logs = ""

		logs = logs + logger.Sprintf("Testrun failed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.Name)

		// ------------ DIFFS RELATED CODE -----------
		if !res.StatusCode.Normal {
			logDiffs.PushStatusDiff(fmt.Sprint(res.StatusCode.Expected), fmt.Sprint(res.StatusCode.Actual))
		}

		var (
			actualHeader   = map[string][]string{}
			expectedHeader = map[string][]string{}
			unmatched      = true
		)

		for _, j := range res.HeadersResult {
			if !j.Normal {
				unmatched = false
				actualHeader[j.Actual.Key] = j.Actual.Value
				expectedHeader[j.Expected.Key] = j.Expected.Value
			}
		}

		if !unmatched {
			for i, j := range expectedHeader {
				logDiffs.PushHeaderDiff(fmt.Sprint(j), fmt.Sprint(actualHeader[i]), i, headerNoise)
			}
		}

		if !res.BodyResult[0].Normal {
			if json.Valid([]byte(actualResponse.Body)) {
				patch, err := jsondiff.Compare(tc.HttpResp.Body, actualResponse.Body)
				if err != nil {
					r.logger.Warn("failed to compute json diff", zap.Error(err))
				}
				for _, op := range patch {
					keyStr := op.Path
					if len(keyStr) > 1 && keyStr[0] == '/' {
						keyStr = keyStr[1:]
					}
					if jsonComparisonResult.matches {
						logDiffs.hasarrayIndexMismatch = true
						logDiffs.PushFooterDiff(strings.Join(jsonComparisonResult.differences, ", "))
					}
					logDiffs.PushBodyDiff(fmt.Sprint(op.OldValue), fmt.Sprint(op.Value), bodyNoise)

				}
			} else {
				logDiffs.PushBodyDiff(fmt.Sprint(tc.HttpResp.Body), fmt.Sprint(actualResponse.Body), bodyNoise)
			}
		}
		r.mutex.Lock()
		logger.Printf(logs)
		err := logDiffs.Render()
		if err != nil {
			r.logger.Error("failed to render the diffs", zap.Error(err))
		}

		r.mutex.Unlock()

	} else {
		logger := pp.New()
		logger.WithLineInfo = false
		logger.SetColorScheme(models.PassingColorScheme)
		var log2 = ""
		log2 += logger.Sprintf("Testrun passed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.Name)
		r.mutex.Lock()
		logger.Printf(log2)
		r.mutex.Unlock()

	}

	return pass, res
}
