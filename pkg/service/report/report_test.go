package report

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
)

// TestNew tests the constructor function
func TestNew(t *testing.T) {
	logger := newTestLogger()
	cfg := &config.Config{}
	mockReportDB := &mockReportDB{}
	mockTestDB := &mockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	if report == nil {
		t.Fatal("New() returned nil")
	}
	if report.logger != logger {
		t.Error("logger not set correctly")
	}
	if report.config != cfg {
		t.Error("config not set correctly")
	}
	if report.reportDB != mockReportDB {
		t.Error("reportDB not set correctly")
	}
	if report.testDB != mockTestDB {
		t.Error("testDB not set correctly")
	}
	if report.out == nil {
		t.Error("buffered writer not initialized")
	}
	if report.printer == nil {
		t.Error("pretty printer not initialized")
	}
}

// TestCollectReports tests the collectReports method
func TestCollectReports(t *testing.T) {
	ctx := context.Background()
	runID := "test-run-1"
	testSetIDs := []string{"test-set-1-report", "test-set-2-report"}

	mockReportDB := &mockReportDB{
		GetReportFunc: func(ctx context.Context, runID, testSetID string) (*models.TestReport, error) {
			if testSetID == "test-set-1" {
				return &models.TestReport{
					Name:    "test-set-1",
					Total:   2,
					Success: 1,
					Failure: 1,
				}, nil
			}
			if testSetID == "test-set-2" {
				return &models.TestReport{
					Name:    "test-set-2",
					Total:   3,
					Success: 2,
					Failure: 1,
				}, nil
			}
			return nil, errors.New("report not found")
		},
	}

	r := &Report{
		logger:   newTestLogger(),
		reportDB: mockReportDB,
	}

	reports, err := r.collectReports(ctx, runID, testSetIDs)
	if err != nil {
		t.Fatalf("collectReports failed: %v", err)
	}

	if len(reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(reports))
	}

	if reports["test-set-1"].Total != 2 {
		t.Error("test-set-1 report not collected correctly")
	}
	if reports["test-set-2"].Total != 3 {
		t.Error("test-set-2 report not collected correctly")
	}
}

// TestCollectReports_WithCancellation tests context cancellation
func TestCollectReports_WithCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	r := &Report{
		logger: newTestLogger(),
	}

	_, err := r.collectReports(ctx, "test-run-1", []string{"test-set-1"})
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestCollectReports_NoReports tests when no reports are found
func TestCollectReports_NoReports(t *testing.T) {
	ctx := context.Background()
	mockReportDB := &mockReportDB{
		GetReportFunc: func(ctx context.Context, runID, testSetID string) (*models.TestReport, error) {
			return nil, errors.New("no reports found")
		},
	}

	r := &Report{
		logger:   newTestLogger(),
		reportDB: mockReportDB,
	}

	_, err := r.collectReports(ctx, "test-run-1", []string{"test-set-1"})
	if err == nil {
		t.Fatal("expected error when no reports found")
	}
	if !strings.Contains(err.Error(), "no reports found for summary") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestPrintSpecificTestCases tests printing specific test cases
func TestPrintSpecificTestCases(t *testing.T) {
	ctx := context.Background()
	runID := "test-run-1"
	testSetIDs := []string{"test-set-1-report"}
	ids := []string{"test-1", "test-2"}

	mockReportDB := &mockReportDB{
		GetReportFunc: func(ctx context.Context, runID, testSetID string) (*models.TestReport, error) {
			return &models.TestReport{
				Name: "test-set-1",
				Tests: []models.TestResult{
					{TestCaseID: "test-1", Status: models.TestStatusPassed, Name: "Test 1", TimeTaken: "1s"},
					{TestCaseID: "test-2", Status: models.TestStatusFailed, Name: "Test 2", TimeTaken: "2s"},
					{TestCaseID: "test-3", Status: models.TestStatusPassed, Name: "Test 3", TimeTaken: "1s"},
				},
			}, nil
		},
	}

	r := &Report{
		logger:   newTestLogger(),
		config:   &config.Config{},
		reportDB: mockReportDB,
		out:      bufio.NewWriterSize(os.Stdout, 4096),
	}

	err := r.printSpecificTestCases(ctx, runID, testSetIDs, ids)
	if err != nil {
		t.Fatalf("printSpecificTestCases failed: %v", err)
	}
}

// TestGetLatestTestRunID tests getting the latest test run ID
func TestGetLatestTestRunID(t *testing.T) {
	ctx := context.Background()

	mockReportDB := &mockReportDB{
		GetAllTestRunIDsFn: func(ctx context.Context) ([]string, error) {
			return []string{"test-run-1", "test-run-10", "test-run-2"}, nil
		},
	}

	r := &Report{
		logger:   newTestLogger(),
		reportDB: mockReportDB,
	}

	latestID, err := r.getLatestTestRunID(ctx)
	if err != nil {
		t.Fatalf("getLatestTestRunID failed: %v", err)
	}

	if latestID != "test-run-10" {
		t.Fatalf("expected test-run-10, got %s", latestID)
	}
}

// TestGetLatestTestRunID_NoRuns tests when no test runs exist
func TestGetLatestTestRunID_NoRuns(t *testing.T) {
	ctx := context.Background()

	mockReportDB := &mockReportDB{
		GetAllTestRunIDsFn: func(ctx context.Context) ([]string, error) {
			return []string{}, nil
		},
	}

	r := &Report{
		logger:   newTestLogger(),
		reportDB: mockReportDB,
	}

	latestID, err := r.getLatestTestRunID(ctx)
	if err != nil {
		t.Fatalf("getLatestTestRunID failed: %v", err)
	}

	if latestID != "" {
		t.Fatalf("expected empty string, got %s", latestID)
	}
}

// TestCollectFailedTests tests collecting failed tests
func TestCollectFailedTests(t *testing.T) {
	ctx := context.Background()
	runID := "test-run-1"
	testSetIDs := []string{"test-set-1-report"}

	mockReportDB := &mockReportDB{
		GetReportFunc: func(ctx context.Context, runID, testSetID string) (*models.TestReport, error) {
			return &models.TestReport{
				Tests: []models.TestResult{
					{TestCaseID: "test-1", Status: models.TestStatusPassed},
					{TestCaseID: "test-2", Status: models.TestStatusFailed},
					{TestCaseID: "test-3", Status: models.TestStatusFailed},
				},
			}, nil
		},
	}

	r := &Report{
		logger:   newTestLogger(),
		reportDB: mockReportDB,
	}

	failedTests, err := r.collectFailedTests(ctx, runID, testSetIDs)
	if err != nil {
		t.Fatalf("collectFailedTests failed: %v", err)
	}

	if len(failedTests) != 2 {
		t.Fatalf("expected 2 failed tests, got %d", len(failedTests))
	}

	if failedTests[0].TestCaseID != "test-2" || failedTests[1].TestCaseID != "test-3" {
		t.Fatal("failed tests not collected correctly")
	}
}

// TestCollectFailedTests_WithCancellation tests context cancellation during failed test collection
func TestCollectFailedTests_WithCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	r := &Report{
		logger: newTestLogger(),
	}

	_, err := r.collectFailedTests(ctx, "test-run-1", []string{"test-set-1"})
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestExtractTestSetIDs tests extracting test set IDs from config
func TestExtractTestSetIDs(t *testing.T) {
	cfg := &config.Config{
		Report: config.Report{
			SelectedTestSets: map[string][]string{
				"test-set-1": {},
				"test-set-2": {},
			},
		},
	}

	r := &Report{
		config: cfg,
	}

	testSetIDs := r.extractTestSetIDs()
	if len(testSetIDs) != 2 {
		t.Fatalf("expected 2 test set IDs, got %d", len(testSetIDs))
	}

	// Check that both test sets are present (order may vary due to map iteration)
	found := make(map[string]bool)
	for _, id := range testSetIDs {
		found[id] = true
	}

	if !found["test-set-1"] || !found["test-set-2"] {
		t.Fatal("test set IDs not extracted correctly")
	}
}

// TestGenerateReport_Summary tests generating a summary report
func TestGenerateReport_Summary(t *testing.T) {
	ctx := context.Background()

	mockReportDB := &mockReportDB{
		GetAllTestRunIDsFn: func(ctx context.Context) ([]string, error) {
			return []string{"test-run-1"}, nil
		},
		GetReportFunc: func(ctx context.Context, runID, testSetID string) (*models.TestReport, error) {
			return &models.TestReport{
				Name:      "test-set-1",
				Total:     2,
				Success:   1,
				Failure:   1,
				TimeTaken: "2s",
				Tests: []models.TestResult{
					{TestCaseID: "test-1", Status: models.TestStatusPassed, TimeTaken: "1s"},
					{TestCaseID: "test-2", Status: models.TestStatusFailed, TimeTaken: "1s"},
				},
			}, nil
		},
	}

	mockTestDB := &mockTestDB{
		GetReportTestSetsFn: func(ctx context.Context, runID string) ([]string, error) {
			return []string{"test-set-1-report"}, nil
		},
	}

	cfg := &config.Config{
		Report: config.Report{
			Summary: true,
		},
	}

	r := New(newTestLogger(), cfg, mockReportDB, mockTestDB)

	err := r.GenerateReport(ctx)
	if err != nil {
		t.Fatalf("GenerateReport failed: %v", err)
	}
}

// TestGenerateReport_WithCancellation tests report generation with context cancellation
func TestGenerateReport_WithCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	r := &Report{
		logger: newTestLogger(),
		config: &config.Config{},
	}

	err := r.GenerateReport(ctx)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestGenerateReportFromFile tests generating report from file
func TestGenerateReportFromFile(t *testing.T) {
	ctx := context.Background()

	// Create a temporary report file
	tempDir := t.TempDir()
	reportFile := filepath.Join(tempDir, "test-report.yaml")

	reportContent := `
version: api.keploy.io/v1beta1
name: test-report
status: PASSED
success: 1
failure: 1
total: 2
time_taken: "2s"
tests:
  - kind: TestResult
    name: test-1
    status: PASSED
    testCaseID: test-1
    time_taken: "1s"
  - kind: TestResult
    name: test-2
    status: FAILED
    testCaseID: test-2
    time_taken: "1s"
    result:
      status_code:
        normal: false
        expected: 200
        actual: 404
      headers_result: []
      body_result:
        - normal: false
          type: JSON
          expected: '{"status": "ok"}'
          actual: '{"status": "error"}'
`

	err := os.WriteFile(reportFile, []byte(reportContent), 0644)
	if err != nil {
		t.Fatalf("failed to create test report file: %v", err)
	}

	cfg := &config.Config{
		Report: config.Report{
			ReportPath: reportFile,
		},
	}

	r := New(newTestLogger(), cfg, nil, nil)

	err = r.GenerateReport(ctx)
	if err != nil {
		t.Fatalf("GenerateReportFromFile failed: %v", err)
	}
}

// TestGenerateReportFromFile_InvalidPath tests generating report from invalid file path
func TestGenerateReportFromFile_InvalidPath(t *testing.T) {
	ctx := context.Background()

	cfg := &config.Config{
		Report: config.Report{
			ReportPath: "relative/path/to/report.yaml", // Not absolute
		},
	}

	r := New(newTestLogger(), cfg, nil, nil)

	err := r.GenerateReport(ctx)
	if err == nil {
		t.Fatal("expected error for relative path")
	}
	if !strings.Contains(err.Error(), "report-path must be absolute") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestGenerateReportFromFile_NonExistentFile tests generating report from non-existent file
func TestGenerateReportFromFile_NonExistentFile(t *testing.T) {
	ctx := context.Background()

	cfg := &config.Config{
		Report: config.Report{
			ReportPath: "/non/existent/file.yaml",
		},
	}

	r := New(newTestLogger(), cfg, nil, nil)

	err := r.GenerateReport(ctx)
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

// TestRenderSingleFailedTest tests rendering a single failed test
func TestRenderSingleFailedTest(t *testing.T) {
	testResult := models.TestResult{
		Name:       "test-1",
		TestCaseID: "test-case-1",
		Status:     models.TestStatusFailed,
		Result: models.Result{
			StatusCode: models.IntResult{
				Normal:   false,
				Expected: 200,
				Actual:   404,
			},
			HeadersResult: []models.HeaderResult{
				{
					Normal:   false,
					Expected: models.Header{Key: "Content-Type", Value: []string{"application/json"}},
					Actual:   models.Header{Key: "Content-Type", Value: []string{"text/plain"}},
				},
			},
			BodyResult: []models.BodyResult{
				{
					Normal:   false,
					Type:     models.JSON,
					Expected: `{"status": "ok"}`,
					Actual:   `{"status": "error"}`,
				},
			},
		},
	}

	r := &Report{
		logger: newTestLogger(),
		config: &config.Config{},
	}

	var sb strings.Builder
	err := r.renderSingleFailedTest(&sb, testResult)
	if err != nil {
		t.Fatalf("renderSingleFailedTest failed: %v", err)
	}

	output := sb.String()
	if !strings.Contains(output, "test-1/test-case-1") {
		t.Error("test name/ID not found in output")
	}
	if !strings.Contains(output, "CHANGES WITHIN THE RESPONSE BODY") {
		t.Error("body changes section not found in output")
	}
}

// TestGenerateTestHeader tests generating test header
func TestGenerateTestHeader(t *testing.T) {
	testResult := models.TestResult{
		Name:       "test-1",
		TestCaseID: "test-case-1",
	}

	r := New(newTestLogger(), &config.Config{}, nil, nil)
	header := r.generateTestHeader(testResult, r.printer)

	if !strings.Contains(header, "test-1") {
		t.Error("test name not found in header")
	}
	if !strings.Contains(header, "test-case-1") {
		t.Error("test case ID not found in header")
	}
	if !strings.Contains(header, "Testrun failed") {
		t.Error("failure message not found in header")
	}
}

// TestRenderTemplateValue tests rendering template values
func TestRenderTemplateValue(t *testing.T) {
	r := &Report{
		logger: newTestLogger(),
	}

	// Test with simple string value
	value := "test-value"
	result, err := r.renderTemplateValue(value)
	if err != nil {
		t.Fatalf("renderTemplateValue failed: %v", err)
	}

	if result != value {
		t.Fatalf("expected %v, got %v", value, result)
	}
}

// TestProcessLegacySummary tests processing summary for legacy format
func TestProcessLegacySummary(t *testing.T) {
	tests := []models.TestResult{
		{TestCaseID: "test-1", Status: models.TestStatusPassed, TimeTaken: "1s"},
		{TestCaseID: "test-2", Status: models.TestStatusFailed, TimeTaken: "2s"},
		{TestCaseID: "test-3", Status: models.TestStatusPassed, TimeTaken: "1s"},
	}

	r := &Report{
		logger: newTestLogger(),
		out:    bufio.NewWriterSize(os.Stdout, 4096),
	}

	err := r.processLegacySummary(tests)
	if err != nil {
		t.Fatalf("processLegacySummary failed: %v", err)
	}
}

// TestProcessLegacyTestCaseFiltering tests filtering specific test cases from legacy format
func TestProcessLegacyTestCaseFiltering(t *testing.T) {
	tests := []models.TestResult{
		{TestCaseID: "test-1", Status: models.TestStatusPassed, Name: "Test 1", TimeTaken: "1s"},
		{TestCaseID: "test-2", Status: models.TestStatusFailed, Name: "Test 2", TimeTaken: "2s"},
		{TestCaseID: "test-3", Status: models.TestStatusPassed, Name: "Test 3", TimeTaken: "1s"},
	}

	r := &Report{
		logger: newTestLogger(),
		config: &config.Config{
			Report: config.Report{
				TestCaseIDs: []string{"test-1", "test-3"},
			},
		},
		out: bufio.NewWriterSize(os.Stdout, 4096),
	}

	err := r.processLegacyTestCaseFiltering(tests)
	if err != nil {
		t.Fatalf("processLegacyTestCaseFiltering failed: %v", err)
	}
}

// TestProcessLegacyFailedTests tests processing failed tests from legacy format
func TestProcessLegacyFailedTests(t *testing.T) {
	ctx := context.Background()
	tests := []models.TestResult{
		{TestCaseID: "test-1", Status: models.TestStatusPassed, Name: "Test 1", TimeTaken: "1s"},
		{
			TestCaseID: "test-2",
			Status:     models.TestStatusFailed,
			Name:       "Test 2",
			TimeTaken:  "2s",
			Result: models.Result{
				StatusCode: models.IntResult{Normal: false, Expected: 200, Actual: 404},
				BodyResult: []models.BodyResult{
					{Normal: false, Type: models.JSON, Expected: `{"ok": true}`, Actual: `{"ok": false}`},
				},
			},
		},
	}

	r := &Report{
		logger: newTestLogger(),
		config: &config.Config{},
		out:    bufio.NewWriterSize(os.Stdout, 4096),
	}

	err := r.processLegacyFailedTests(ctx, tests)
	if err != nil {
		t.Fatalf("processLegacyFailedTests failed: %v", err)
	}
}
