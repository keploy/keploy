package report

import (
<<<<<<< HEAD
=======
	"bufio"
	"bytes"
>>>>>>> main
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// MockReportDB is a mock implementation of ReportDB interface
type MockReportDB struct {
	mock.Mock
}

func (m *MockReportDB) GetAllTestRunIDs(ctx context.Context) ([]string, error) {
	args := m.Called(ctx)
	return args.Get(0).([]string), args.Error(1)
}

func (m *MockReportDB) GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error) {
	args := m.Called(ctx, testRunID, testSetID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.TestReport), args.Error(1)
}

// MockTestDB is a mock implementation of TestDB interface
type MockTestDB struct {
	mock.Mock
}

func (m *MockTestDB) GetReportTestSets(ctx context.Context, reportID string) ([]string, error) {
	args := m.Called(ctx, reportID)
	return args.Get(0).([]string), args.Error(1)
}

// TestNew_001 tests the New function creates a Report instance correctly
func TestNew_001(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	assert.NotNil(t, report)
	assert.Equal(t, logger, report.logger)
	assert.Equal(t, cfg, report.config)
	assert.Equal(t, mockReportDB, report.reportDB)
	assert.Equal(t, mockTestDB, report.testDB)
}

<<<<<<< HEAD
// TestGenerateReport_FromFilePath_002 tests generating report from a specified file path
func TestGenerateReport_FromFilePath_002(t *testing.T) {
	// Create temporary report file
	tempFile, err := os.CreateTemp("", "test_report_*.yaml")
	require.NoError(t, err)
	defer os.Remove(tempFile.Name())
=======
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

	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)

	err := r.GenerateReport(ctx)
	if err != nil {
		t.Fatalf("GenerateReport failed: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "COMPLETE TESTRUN SUMMARY") {
		t.Error("The report summary is missing the expected title")
	}
	if !strings.Contains(output, "Total tests: 2") {
		t.Error("The report summary did not correctly calculate the total number of tests")
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
>>>>>>> main

	reportContent := `
tests:
  - kind: Http
    name: test-case-1
    status: FAILED
    test_case_id: test-1
    result:
      status_code:
        normal: false
        expected: 200
        actual: 404
      headers_result: []
      body_result:
        - normal: false
          type: JSON
          expected: '{"success": true}'
          actual: '{"success": false}'
`
	_, err = tempFile.WriteString(reportContent)
	require.NoError(t, err)
	tempFile.Close()

	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			ReportPath: tempFile.Name(),
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	err = report.GenerateReport(context.Background())

	assert.NoError(t, err)
}

// TestGenerateReport_FromDatabase_003 tests generating report from database with failed tests
func TestGenerateReport_FromDatabase_003(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			SelectedTestSets: map[string][]string{
				"test-set-1": {},
			},
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Setup mock expectations
	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{"test-run-1", "test-run-2"}, nil)

	testReport := &models.TestReport{
		Tests: []models.TestResult{
			{
				Name:       "test-case-1",
				Status:     models.TestStatusFailed,
				TestCaseID: "test-1",
				Result: models.Result{
					StatusCode: models.IntResult{
						Normal:   false,
						Expected: 200,
						Actual:   404,
					},
					BodyResult: []models.BodyResult{
						{
							Normal:   false,
							Type:     models.JSON,
							Expected: `{"success": true}`,
							Actual:   `{"success": false}`,
						},
					},
				},
			},
		},
	}
	mockReportDB.On("GetReport", mock.Anything, "test-run-2", "test-set-1").Return(testReport, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)

	err := report.GenerateReport(context.Background())

	assert.NoError(t, err)
	mockReportDB.AssertExpectations(t)
}

// TestGenerateReport_NoTestRuns_004 tests behavior when no test runs exist
func TestGenerateReport_NoTestRuns_004(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{}, nil)
	mockTestDB.On("GetReportTestSets", mock.Anything, "").Return([]string{}, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)

	err := report.GenerateReport(context.Background())

	assert.NoError(t, err)
	mockReportDB.AssertExpectations(t)
	mockTestDB.AssertExpectations(t)
}

// TestGenerateReport_NoFailedTests_005 tests behavior when no failed tests exist
func TestGenerateReport_NoFailedTests_005(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			SelectedTestSets: map[string][]string{
				"test-set-1": {},
			},
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{"test-run-1"}, nil)

	// Return test report with only passed tests
	testReport := &models.TestReport{
		Tests: []models.TestResult{
			{
				Name:       "test-case-1",
				Status:     models.TestStatusPassed,
				TestCaseID: "test-1",
			},
		},
	}
	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "test-set-1").Return(testReport, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)

	err := report.GenerateReport(context.Background())

	assert.NoError(t, err)
	mockReportDB.AssertExpectations(t)
}

// TestGenerateReport_DatabaseError_006 tests handling of database errors
func TestGenerateReport_DatabaseError_006(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{}, errors.New("database connection failed"))

	report := New(logger, cfg, mockReportDB, mockTestDB)

	err := report.GenerateReport(context.Background())

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "database connection failed")
	mockReportDB.AssertExpectations(t)
}

// TestGenerateReport_NoSelectedTestSets_007 tests when no test sets are selected
func TestGenerateReport_NoSelectedTestSets_007(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{"test-run-1"}, nil)
	mockTestDB.On("GetReportTestSets", mock.Anything, "test-run-1").Return([]string{"test-set-1", "test-set-2"}, nil)

	// Mock both test sets
	testReport := &models.TestReport{
		Tests: []models.TestResult{
			{
				Name:       "test-case-1",
				Status:     models.TestStatusFailed,
				TestCaseID: "test-1",
				Result: models.Result{
					StatusCode: models.IntResult{
						Normal:   false,
						Expected: 200,
						Actual:   500,
					},
					BodyResult: []models.BodyResult{
						{
							Normal:   false,
							Type:     models.JSON,
							Expected: `{"error": false}`,
							Actual:   `{"error": true}`,
						},
					},
				},
			},
		},
	}
	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "test-set-1").Return(testReport, nil)
	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "test-set-2").Return(testReport, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)

	err := report.GenerateReport(context.Background())

	assert.NoError(t, err)
	mockReportDB.AssertExpectations(t)
	mockTestDB.AssertExpectations(t)
}

// TestGenerateReportFromFile_InvalidPath_008 tests with invalid file path
func TestGenerateReportFromFile_InvalidPath_008(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			ReportPath: "invalid/path/report.yaml",
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	err := report.GenerateReport(context.Background())

	assert.Error(t, err)
}

// TestGenerateReportFromFile_RelativePath_009 tests with relative path (should fail)
func TestGenerateReportFromFile_RelativePath_009(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			ReportPath: "report.yaml", // relative path
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	err := report.GenerateReport(context.Background())

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "report-path must be absolute")
}

// TestGenerateReportFromFile_InvalidYAML_010 tests with invalid YAML content
func TestGenerateReportFromFile_InvalidYAML_010(t *testing.T) {
	tempFile, err := os.CreateTemp("", "test_report_*.yaml")
	require.NoError(t, err)
	defer os.Remove(tempFile.Name())

	// Write invalid YAML
	_, err = tempFile.WriteString("invalid: yaml: content: [unclosed")
	require.NoError(t, err)
	tempFile.Close()

	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			ReportPath: tempFile.Name(),
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	err = report.GenerateReport(context.Background())

	assert.Error(t, err)
}

// TestGenerateReportFromFile_MissingTestsField_011 tests with missing tests field
func TestGenerateReportFromFile_MissingTestsField_011(t *testing.T) {
	tempFile, err := os.CreateTemp("", "test_report_*.yaml")
	require.NoError(t, err)
	defer os.Remove(tempFile.Name())

	// YAML without tests field
	reportContent := `
name: test-report
status: FAILED
`
	_, err = tempFile.WriteString(reportContent)
	require.NoError(t, err)
	tempFile.Close()

	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			ReportPath: tempFile.Name(),
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	err = report.GenerateReport(context.Background())

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "tests' is missing or empty")
}

// TestExtractTestSetIDs_012 tests extractTestSetIDs function
func TestExtractTestSetIDs_012(t *testing.T) {
	cfg := &config.Config{
		Report: config.Report{
			SelectedTestSets: map[string][]string{
				"test-set-1 ": {}, // with trailing space
				" test-set-2": {}, // with leading space
				"test-set-3":  {},
				"test-set-4":  {}, // should still be included
			},
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}
	logger := zap.NewNop()

	report := New(logger, cfg, mockReportDB, mockTestDB)
	testSetIDs := report.extractTestSetIDs()

	assert.Len(t, testSetIDs, 4)
	assert.Contains(t, testSetIDs, "test-set-1") // trimmed
	assert.Contains(t, testSetIDs, "test-set-2") // trimmed
	assert.Contains(t, testSetIDs, "test-set-3")
	assert.Contains(t, testSetIDs, "test-set-4")
}

// TestGetLatestTestRunID_013 tests getLatestTestRunID with numeric sorting
func TestGetLatestTestRunID_013(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Test numeric sorting
	testRuns := []string{"test-run-10", "test-run-2", "test-run-1", "test-run-20"}
	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return(testRuns, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	latestRunID, err := report.getLatestTestRunID(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "test-run-20", latestRunID)
	mockReportDB.AssertExpectations(t)
}

// TestGetLatestTestRunID_NonNumeric_014 tests getLatestTestRunID with non-numeric IDs
func TestGetLatestTestRunID_NonNumeric_014(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Test with non-numeric suffixes (should fall back to string sorting)
	testRuns := []string{"test-run-abc", "test-run-def", "test-run-xyz"}
	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return(testRuns, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	latestRunID, err := report.getLatestTestRunID(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "test-run-xyz", latestRunID) // Last alphabetically
	mockReportDB.AssertExpectations(t)
}

// TestGetLatestTestRunID_MixedFormat_015 tests getLatestTestRunID with mixed formats
func TestGetLatestTestRunID_MixedFormat_015(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Mix of numeric and non-numeric
	testRuns := []string{"test-run-5", "test-run-abc", "test-run-10", "test-run-def"}
	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return(testRuns, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	latestRunID, err := report.getLatestTestRunID(context.Background())

	require.NoError(t, err)
	// Non-numeric ones are treated as "less than" numeric ones in this sorting logic
	assert.Equal(t, "test-run-10", latestRunID)
	mockReportDB.AssertExpectations(t)
}

// TestCollectFailedTests_016 tests collectFailedTests function
func TestCollectFailedTests_016(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Test with suffix removal
	testSetIDs := []string{"test-set-1-report", "test-set-2"}

	testReport1 := &models.TestReport{
		Tests: []models.TestResult{
			{
				Name:       "failed-test-1",
				Status:     models.TestStatusFailed,
				TestCaseID: "test-1",
			},
			{
				Name:       "passed-test-1",
				Status:     models.TestStatusPassed,
				TestCaseID: "test-2",
			},
		},
	}

	testReport2 := &models.TestReport{
		Tests: []models.TestResult{
			{
				Name:       "failed-test-2",
				Status:     models.TestStatusFailed,
				TestCaseID: "test-3",
			},
		},
	}

	mockReportDB.On("GetReport", mock.Anything, "run-1", "test-set-1").Return(testReport1, nil)
	mockReportDB.On("GetReport", mock.Anything, "run-1", "test-set-2").Return(testReport2, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	failedTests, err := report.collectFailedTests(context.Background(), "run-1", testSetIDs)

	require.NoError(t, err)
	assert.Len(t, failedTests, 2) // Only failed tests should be returned
	assert.Equal(t, "failed-test-1", failedTests[0].Name)
	assert.Equal(t, "failed-test-2", failedTests[1].Name)
	mockReportDB.AssertExpectations(t)
}

// TestCollectFailedTests_NoResults_017 tests collectFailedTests with no results
func TestCollectFailedTests_NoResults_017(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	testSetIDs := []string{"test-set-1"}

	mockReportDB.On("GetReport", mock.Anything, "run-1", "test-set-1").Return(nil, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	failedTests, err := report.collectFailedTests(context.Background(), "run-1", testSetIDs)

	require.NoError(t, err)
	assert.Len(t, failedTests, 0)
	mockReportDB.AssertExpectations(t)
}

// TestCollectFailedTests_DatabaseError_018 tests collectFailedTests with database error
func TestCollectFailedTests_DatabaseError_018(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	testSetIDs := []string{"test-set-1", "test-set-2"}

	// First call succeeds, second fails
	testReport := &models.TestReport{
		Tests: []models.TestResult{
			{
				Name:       "failed-test-1",
				Status:     models.TestStatusFailed,
				TestCaseID: "test-1",
			},
		},
	}

	mockReportDB.On("GetReport", mock.Anything, "run-1", "test-set-1").Return(testReport, nil)
	mockReportDB.On("GetReport", mock.Anything, "run-1", "test-set-2").Return(nil, errors.New("db error"))

	report := New(logger, cfg, mockReportDB, mockTestDB)
	failedTests, err := report.collectFailedTests(context.Background(), "run-1", testSetIDs)

	// Should continue processing despite error and return partial results
	require.NoError(t, err)
	assert.Len(t, failedTests, 1)
	mockReportDB.AssertExpectations(t)
}

// TestExtractFailedTestsFromResults_019 tests extractFailedTestsFromResults function
func TestExtractFailedTestsFromResults_019(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	tests := []models.TestResult{
		{Name: "test1", Status: models.TestStatusFailed},
		{Name: "test2", Status: models.TestStatusPassed},
		{Name: "test3", Status: models.TestStatusFailed},
		{Name: "test4", Status: models.TestStatusIgnored},
		{Name: "test5", Status: models.TestStatusPending},
	}

	report := New(logger, cfg, mockReportDB, mockTestDB)
	failedTests := report.extractFailedTestsFromResults(tests)

	assert.Len(t, failedTests, 2)
	assert.Equal(t, "test1", failedTests[0].Name)
	assert.Equal(t, "test3", failedTests[1].Name)
}

// TestPrintSingleTestReport_FullBodyMode_020 tests printSingleTestReport with ShowFullBody enabled
func TestPrintSingleTestReport_FullBodyMode_020(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			ShowFullBody: true,
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	testResult := models.TestResult{
		Name:       "test-case-1",
		TestCaseID: "test-1",
		Status:     models.TestStatusFailed,
		Result: models.Result{
			StatusCode: models.IntResult{
				Normal:   false,
				Expected: 200,
				Actual:   500,
			},
			HeadersResult: []models.HeaderResult{
				{
					Normal: false,
					Expected: models.Header{
						Key:   "Content-Type",
						Value: []string{"application/json"},
					},
					Actual: models.Header{
						Key:   "Content-Type",
						Value: []string{"text/html"},
					},
				},
			},
			BodyResult: []models.BodyResult{
				{
					Normal:   false,
					Type:     models.JSON,
					Expected: `{"success": true}`,
					Actual:   `{"success": false}`,
				},
			},
		},
	}

	report := New(logger, cfg, mockReportDB, mockTestDB)
	err := report.printSingleTestReport(testResult)

	assert.NoError(t, err)
}

// TestPrintSingleTestReport_TableViewMode_021 tests printSingleTestReport with table view for JSON
func TestPrintSingleTestReport_TableViewMode_021(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			ShowFullBody: false, // Use table view
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	testResult := models.TestResult{
		Name:       "test-case-1",
		TestCaseID: "test-1",
		Status:     models.TestStatusFailed,
		Result: models.Result{
			StatusCode: models.IntResult{
				Normal:   true,
				Expected: 200,
				Actual:   200,
			},
			BodyResult: []models.BodyResult{
				{
					Normal:   false,
					Type:     models.JSON,
					Expected: `{"user": {"name": "John", "age": 30}}`,
					Actual:   `{"user": {"name": "Jane", "age": 25}}`,
				},
			},
		},
	}

	report := New(logger, cfg, mockReportDB, mockTestDB)
	err := report.printSingleTestReport(testResult)

	assert.NoError(t, err)
}

// TestPrintSingleTestReport_NonJSONBody_022 tests printSingleTestReport with non-JSON body
func TestPrintSingleTestReport_NonJSONBody_022(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			ShowFullBody: false,
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	testResult := models.TestResult{
		Name:       "test-case-1",
		TestCaseID: "test-1",
		Status:     models.TestStatusFailed,
		Result: models.Result{
			StatusCode: models.IntResult{
				Normal:   true,
				Expected: 200,
				Actual:   200,
			},
			BodyResult: []models.BodyResult{
				{
					Normal:   false,
					Type:     models.Plain,
					Expected: "Hello World",
					Actual:   "Hello Universe",
				},
			},
		},
	}

	report := New(logger, cfg, mockReportDB, mockTestDB)
	err := report.printSingleTestReport(testResult)

	assert.NoError(t, err)
}

// TestPrintSingleTestReport_InvalidJSONForTableView_023 tests fallback when JSON diff generation fails
func TestPrintSingleTestReport_InvalidJSONForTableView_023(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			ShowFullBody: false,
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	testResult := models.TestResult{
		Name:       "test-case-1",
		TestCaseID: "test-1",
		Status:     models.TestStatusFailed,
		Result: models.Result{
			StatusCode: models.IntResult{
				Normal:   true,
				Expected: 200,
				Actual:   200,
			},
			BodyResult: []models.BodyResult{
				{
					Normal:   false,
					Type:     models.JSON,
					Expected: `{"valid": "json"}`,
					Actual:   `{invalid json}`, // Invalid JSON should trigger fallback
				},
			},
		},
	}

	report := New(logger, cfg, mockReportDB, mockTestDB)
	err := report.printSingleTestReport(testResult)

	assert.NoError(t, err) // Should not fail, should use fallback
}

// TestCreateFormattedPrinter_024 tests createFormattedPrinter function
func TestCreateFormattedPrinter_024(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)
	printer := report.createFormattedPrinter()

	assert.NotNil(t, printer)
	assert.False(t, printer.WithLineInfo)
}

// TestGenerateTestHeader_025 tests generateTestHeader function
func TestGenerateTestHeader_025(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	testResult := models.TestResult{
		TestCaseID: "test-123",
	}

	report := New(logger, cfg, mockReportDB, mockTestDB)
	printer := report.createFormattedPrinter()
	header := report.generateTestHeader(testResult, printer)

	assert.Contains(t, header, "test-123")
	assert.Contains(t, header, "Testrun failed for testcase with id:")
}

// TestParseReportTests_ValidYAML_026 tests parseReportTests with valid YAML
func TestParseReportTests_ValidYAML_026(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	yamlData := `
tests:
  - kind: Http
    name: test-case-1
    status: FAILED
    test_case_id: test-1
  - kind: Http
    name: test-case-2
    status: PASSED
    test_case_id: test-2
`

	report := New(logger, cfg, mockReportDB, mockTestDB)
	tests, err := report.parseReportTests([]byte(yamlData))

	require.NoError(t, err)
	assert.Len(t, tests, 2)
	assert.Equal(t, "test-case-1", tests[0].Name)
	assert.Equal(t, models.TestStatusFailed, tests[0].Status)
	assert.Equal(t, "test-case-2", tests[1].Name)
	assert.Equal(t, models.TestStatusPassed, tests[1].Status)
}

// TestParseReportTests_InvalidYAML_027 tests parseReportTests with invalid YAML
func TestParseReportTests_InvalidYAML_027(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	invalidYaml := `
tests:
  - kind: Http
    name: test-case-1
    invalid: [unclosed
`

	report := New(logger, cfg, mockReportDB, mockTestDB)
	tests, err := report.parseReportTests([]byte(invalidYaml))

	assert.Error(t, err)
	assert.Nil(t, tests)
	assert.Contains(t, err.Error(), "failed to unmarshal report")
}

// TestParseReportTests_EmptyTests_028 tests parseReportTests with empty tests array
func TestParseReportTests_EmptyTests_028(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	yamlData := `
name: test-report
status: FAILED
tests: []
`

	report := New(logger, cfg, mockReportDB, mockTestDB)
	tests, err := report.parseReportTests([]byte(yamlData))

	assert.Error(t, err)
	assert.Nil(t, tests)
	assert.Contains(t, err.Error(), "tests' is missing or empty")
}

// TestPrintDefaultBodyDiff_029 tests printDefaultBodyDiff function
func TestPrintDefaultBodyDiff_029(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	bodyResult := models.BodyResult{
		Normal:   false,
		Type:     models.Plain,
		Expected: "Expected content",
		Actual:   "Actual content",
	}

	report := New(logger, cfg, mockReportDB, mockTestDB)
	err := report.printDefaultBodyDiff(bodyResult)

	assert.NoError(t, err)
}

// TestGenerateReport_LargeDataSet_030 tests performance with large number of failed tests
func TestGenerateReport_LargeDataSet_030(t *testing.T) {
	logger := zap.NewNop()
	cfg := &config.Config{
		Report: config.Report{
			SelectedTestSets: map[string][]string{
				"large-test-set": {},
			},
		},
	}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{"test-run-1"}, nil)

	// Create a large number of failed tests
	tests := make([]models.TestResult, 100)
	for i := 0; i < 100; i++ {
		tests[i] = models.TestResult{
			Name:       fmt.Sprintf("test-case-%d", i),
			Status:     models.TestStatusFailed,
			TestCaseID: fmt.Sprintf("test-%d", i),
			Result: models.Result{
				StatusCode: models.IntResult{
					Normal:   false,
					Expected: 200,
					Actual:   500,
				},
				BodyResult: []models.BodyResult{
					{
						Normal:   false,
						Type:     models.JSON,
						Expected: `{"success": true}`,
						Actual:   `{"success": false}`,
					},
				},
			},
		}
	}

	testReport := &models.TestReport{Tests: tests}
	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "large-test-set").Return(testReport, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	err := report.GenerateReport(context.Background())

	assert.NoError(t, err)
	mockReportDB.AssertExpectations(t)
}
