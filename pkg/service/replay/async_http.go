package replay

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type asyncHTTPResult struct {
	testCase        *models.TestCase
	started         time.Time
	httpResp        *models.HTTPResp
	testResult      *models.Result
	testPass        bool
	simErr          error
	mockNames       []string
	expectedMocks   []string
	mockSetMismatch bool
	consumedMocks   []models.MockState
}

type asyncResultCounters struct {
	success       *int
	failure       *int
	obsolete      *int
	testSetStatus *models.TestSetStatus
}

func (r *Replayer) runAsyncStreamingRequest(
	ctx context.Context,
	tc models.TestCase,
	testSetID string,
	started time.Time,
	expectedMocks []string,
	useMappingBased bool,
	isMappingEnabled bool,
	results chan<- asyncHTTPResult,
) {
	asyncRes := asyncHTTPResult{
		testCase:      &tc,
		started:       started,
		expectedMocks: expectedMocks,
	}

	resp, err := HookImpl.SimulateRequest(ctx, &tc, testSetID)
	if err != nil {
		asyncRes.simErr = err
		results <- asyncRes
		return
	}

	httpResp, ok := resp.(*models.HTTPResp)
	if !ok {
		asyncRes.simErr = fmt.Errorf("invalid response type for HTTP test case")
		results <- asyncRes
		return
	}
	asyncRes.httpResp = httpResp

	if r.instrument {
		consumed, err := HookImpl.GetConsumedMocks(ctx)
		if err != nil {
			utils.LogError(r.logger, err, "failed to get consumed filtered mocks")
		} else {
			asyncRes.consumedMocks = consumed
		}
	}

	mockNames := make([]string, 0, len(asyncRes.consumedMocks))
	for _, m := range asyncRes.consumedMocks {
		mockNames = append(mockNames, m.Name)
	}
	asyncRes.mockNames = mockNames

	hasExpectedMocks := len(expectedMocks) > 0
	if r.instrument && useMappingBased && isMappingEnabled && hasExpectedMocks {
		asyncRes.mockSetMismatch = !isMockSubset(mockNames, expectedMocks)
	}

	emitFailureLogs := !asyncRes.mockSetMismatch
	asyncRes.testPass, asyncRes.testResult = r.CompareHTTPResp(&tc, httpResp, testSetID, emitFailureLogs)
	results <- asyncRes
}

func (r *Replayer) processAsyncHTTPResult(
	ctx context.Context,
	testRunID string,
	testSetID string,
	asyncRes asyncHTTPResult,
	totalConsumedMocks map[string]models.MockState,
	actualTestMockMappings *models.Mapping,
	counters asyncResultCounters,
) error {
	if asyncRes.testCase == nil {
		return fmt.Errorf("async streaming testcase is nil")
	}

	if asyncRes.simErr != nil {
		utils.LogError(r.logger, asyncRes.simErr, "failed to simulate async streaming request. Check network connectivity, verify the server is responding correctly, or increase the API timeout if the stream is slow.")
		incrementCounter(counters.failure)
		setTestSetStatus(counters.testSetStatus, models.TestSetStatusFailed)
		testCaseResult := r.CreateFailedTestResult(asyncRes.testCase, testSetID, asyncRes.started, asyncRes.simErr.Error())
		return r.reportDB.InsertTestCaseResult(ctx, testRunID, testSetID, testCaseResult)
	}

	if asyncRes.httpResp == nil {
		incrementCounter(counters.failure)
		setTestSetStatus(counters.testSetStatus, models.TestSetStatusFailed)
		testCaseResult := r.CreateFailedTestResult(asyncRes.testCase, testSetID, asyncRes.started, "nil async streaming http response")
		return r.reportDB.InsertTestCaseResult(ctx, testRunID, testSetID, testCaseResult)
	}

	if r.instrument {
		for _, m := range asyncRes.consumedMocks {
			totalConsumedMocks[m.Name] = m
		}
	}

	upsertActualTestMockMapping(actualTestMockMappings, asyncRes.testCase.Name, asyncRes.mockNames)

	r.logger.Debug("Consumed Mocks", zap.Any("mocks", asyncRes.consumedMocks))

	if asyncRes.mockSetMismatch {
		if asyncRes.testPass {
			r.logger.Debug("mock mapping mismatch ignored because testcase passed",
				zap.String("testcase", asyncRes.testCase.Name),
				zap.String("testset", testSetID),
				zap.Strings("expectedMocks", asyncRes.expectedMocks),
				zap.Strings("actualMocks", asyncRes.mockNames))
		} else {
			r.logger.Error("mock mapping mismatch detected; marking testcase as obsolete. Re-record the test case to update the mock mappings, or verify that the application's external dependencies have not changed.",
				zap.String("testcase", asyncRes.testCase.Name),
				zap.String("testset", testSetID),
				zap.Strings("expectedMocks", asyncRes.expectedMocks),
				zap.Strings("actualMocks", asyncRes.mockNames))
			mockMismatchFailures.AddFailure(testSetID, asyncRes.testCase.Name, asyncRes.expectedMocks, asyncRes.mockNames)
		}
	}

	if !asyncRes.testPass {
		r.logger.Info("result", zap.String("testcase id", models.HighlightFailingString(asyncRes.testCase.Name)), zap.String("testset id", models.HighlightFailingString(testSetID)), zap.String("passed", models.HighlightFailingString(asyncRes.testPass)))
	} else {
		r.logger.Info("result", zap.String("testcase id", models.HighlightPassingString(asyncRes.testCase.Name)), zap.String("testset id", models.HighlightPassingString(testSetID)), zap.String("passed", models.HighlightPassingString(asyncRes.testPass)))
	}

	var testStatus models.TestStatus
	if asyncRes.testPass {
		testStatus = models.TestStatusPassed
		incrementCounter(counters.success)
	} else if asyncRes.mockSetMismatch {
		testStatus = models.TestStatusObsolete
		incrementCounter(counters.obsolete)
	} else {
		testStatus = models.TestStatusFailed
		incrementCounter(counters.failure)
		setTestSetStatus(counters.testSetStatus, models.TestSetStatusFailed)
	}

	if asyncRes.testResult == nil {
		return fmt.Errorf("test result is nil for async testcase: %s", asyncRes.testCase.Name)
	}

	testCaseResult := &models.TestResult{
		Kind:       models.HTTP,
		Name:       testSetID,
		Status:     testStatus,
		Started:    asyncRes.started.Unix(),
		Completed:  time.Now().UTC().Unix(),
		TestCaseID: asyncRes.testCase.Name,
		Req: models.HTTPReq{
			Method:     asyncRes.testCase.HTTPReq.Method,
			ProtoMajor: asyncRes.testCase.HTTPReq.ProtoMajor,
			ProtoMinor: asyncRes.testCase.HTTPReq.ProtoMinor,
			URL:        asyncRes.testCase.HTTPReq.URL,
			URLParams:  asyncRes.testCase.HTTPReq.URLParams,
			Header:     asyncRes.testCase.HTTPReq.Header,
			Body:       asyncRes.testCase.HTTPReq.Body,
			Binary:     asyncRes.testCase.HTTPReq.Binary,
			Form:       asyncRes.testCase.HTTPReq.Form,
			Timestamp:  asyncRes.testCase.HTTPReq.Timestamp,
		},
		Res:          *asyncRes.httpResp,
		TestCasePath: filepath.Join(r.config.Path, testSetID),
		MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
		Noise:        asyncRes.testCase.Noise,
		Result:       *asyncRes.testResult,
		TimeTaken:    time.Since(asyncRes.started).String(),
	}

	if testStatus == models.TestStatusFailed && asyncRes.testResult.FailureInfo.Risk != models.None {
		testCaseResult.FailureInfo.Risk = asyncRes.testResult.FailureInfo.Risk
		testCaseResult.FailureInfo.Category = asyncRes.testResult.FailureInfo.Category
	}

	insertStart := time.Now()
	err := r.reportDB.InsertTestCaseResult(ctx, testRunID, testSetID, testCaseResult)
	if time.Since(insertStart) > 50*time.Millisecond {
		r.logger.Debug("Slow InsertTestCaseResult", zap.Duration("duration", time.Since(insertStart)))
	}
	return err
}

func drainAsyncHTTPResults(asyncHTTPResults <-chan asyncHTTPResult, block bool, handler func(asyncHTTPResult) error) error {
	for {
		if block {
			asyncRes, ok := <-asyncHTTPResults
			if !ok {
				return nil
			}
			if err := handler(asyncRes); err != nil {
				return err
			}
			continue
		}

		select {
		case asyncRes, ok := <-asyncHTTPResults:
			if !ok {
				return nil
			}
			if err := handler(asyncRes); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func upsertActualTestMockMapping(actualTestMockMappings *models.Mapping, testCaseID string, mockNames []string) {
	if actualTestMockMappings == nil || testCaseID == "" || len(mockNames) == 0 {
		return
	}

	for i := range actualTestMockMappings.Tests {
		if actualTestMockMappings.Tests[i].ID == testCaseID {
			existing := actualTestMockMappings.Tests[i].Mocks.ToSlice()
			actualTestMockMappings.Tests[i].Mocks = models.FromSlice(append(existing, mockNames...))
			return
		}
	}

	actualTestMockMappings.Tests = append(actualTestMockMappings.Tests, models.Test{
		ID:    testCaseID,
		Mocks: models.FromSlice(mockNames),
	})
}

func incrementCounter(counter *int) {
	if counter == nil {
		return
	}
	*counter = *counter + 1
}

func setTestSetStatus(status *models.TestSetStatus, next models.TestSetStatus) {
	if status == nil {
		return
	}
	*status = next
}
