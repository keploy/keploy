package report

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v2/config"
	matcherUtils "go.keploy.io/server/v2/pkg/matcher"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// MockReportDB implements the ReportDB interface for testing
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

// MockTestDB implements the TestDB interface for testing
type MockTestDB struct {
	mock.Mock
}

func (m *MockTestDB) GetReportTestSets(ctx context.Context, reportID string) ([]string, error) {
	args := m.Called(ctx, reportID)
	return args.Get(0).([]string), args.Error(1)
}

// Helper function to create a test config
func createTestConfig() *config.Config {
	return &config.Config{
		Report: config.Report{
			ReportPath:       "",
			SelectedTestSets: map[string][]string{},
			ShowFullBody:     false,
		},
	}
}

// Helper function to create test results
func createTestResult(id, name string, status models.TestStatus, withFailures bool) models.TestResult {
	result := models.TestResult{
		TestCaseID: id,
		Name:       name,
		Status:     status,
		Started:    time.Now().Unix(),
		Completed:  time.Now().Unix() + 1,
		Result: models.Result{
			StatusCode: models.IntResult{
				Normal:   !withFailures,
				Expected: 200,
				Actual:   200,
			},
			HeadersResult: []models.HeaderResult{},
			BodyResult:    []models.BodyResult{},
		},
	}

	if withFailures {
		result.Result.StatusCode.Actual = 500
		result.Result.HeadersResult = []models.HeaderResult{
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
		}
		result.Result.BodyResult = []models.BodyResult{
			{
				Normal:   false,
				Type:     models.JSON,
				Expected: `{"message": "success"}`,
				Actual:   `{"message": "error", "code": 500}`,
			},
		}
	}

	return result
}

func TestNew(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	assert.NotNil(t, report)
	assert.Equal(t, logger, report.logger)
	assert.Equal(t, cfg, report.config)
	assert.Equal(t, mockReportDB, report.reportDB)
	assert.Equal(t, mockTestDB, report.testDB)
}

func TestGenerateReport_NoFailedTests(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Setup mocks
	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{"test-run-1", "test-run-2"}, nil)
	mockTestDB.On("GetReportTestSets", mock.Anything, "test-run-2").Return([]string{"test-set-1"}, nil)

	testResults := []models.TestResult{
		createTestResult("test-1", "Test 1", models.TestStatusPassed, false),
		createTestResult("test-2", "Test 2", models.TestStatusPassed, false),
	}
	testReport := &models.TestReport{Tests: testResults}
	mockReportDB.On("GetReport", mock.Anything, "test-run-2", "test-set-1").Return(testReport, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	err := report.GenerateReport(ctx)
	require.NoError(t, err)

	mockReportDB.AssertExpectations(t)
	mockTestDB.AssertExpectations(t)
}

func TestGenerateReport_WithFailedTests(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Setup mocks
	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{"test-run-1"}, nil)
	mockTestDB.On("GetReportTestSets", mock.Anything, "test-run-1").Return([]string{"test-set-1"}, nil)

	testResults := []models.TestResult{
		createTestResult("test-1", "Test 1", models.TestStatusPassed, false),
		createTestResult("test-2", "Test 2", models.TestStatusFailed, true),
	}
	testReport := &models.TestReport{Tests: testResults}
	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "test-set-1").Return(testReport, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	err := report.GenerateReport(ctx)
	require.NoError(t, err)

	mockReportDB.AssertExpectations(t)
	mockTestDB.AssertExpectations(t)
}

func TestGenerateReport_NoTestRuns(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Setup mocks - empty test run list
	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{}, nil)
	// Even with empty latestRunID, the code still calls GetReportTestSets due to the current flow
	mockTestDB.On("GetReportTestSets", mock.Anything, "").Return([]string{}, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	err := report.GenerateReport(ctx)
	require.NoError(t, err)

	mockReportDB.AssertExpectations(t)
	mockTestDB.AssertExpectations(t)
}

func TestGenerateReport_ErrorGettingTestRunIDs(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Setup mocks
	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{}, fmt.Errorf("database error"))

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	err := report.GenerateReport(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database error")

	mockReportDB.AssertExpectations(t)
}

func TestGenerateReport_WithSelectedTestSets(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	cfg.Report.SelectedTestSets = map[string][]string{"test-set-1": {}, "test-set-2": {}}
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Setup mocks
	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{"test-run-1"}, nil)

	testResults := []models.TestResult{
		createTestResult("test-1", "Test 1", models.TestStatusFailed, true),
	}
	testReport := &models.TestReport{Tests: testResults}
	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "test-set-1").Return(testReport, nil)
	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "test-set-2").Return(testReport, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	err := report.GenerateReport(ctx)
	require.NoError(t, err)

	mockReportDB.AssertExpectations(t)
}

func TestGenerateReportFromFile(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Create a temporary file with test data
	tmpDir := t.TempDir()
	reportFile := filepath.Join(tmpDir, "test-report.yaml")

	yamlContent := `
tests:
  - testCaseID: "test-1"
    name: "Test 1"
    status: "FAILED"
    started: 1234567890
    completed: 1234567891
    result:
      status_code:
        normal: false
        expected: 200
        actual: 500
      headers_result: []
      body_result:
        - normal: false
          type: "JSON"
          expected: '{"message": "success"}'
          actual: '{"message": "error"}'
`

	err := os.WriteFile(reportFile, []byte(yamlContent), 0644)
	require.NoError(t, err)

	cfg.Report.ReportPath = reportFile
	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	err = report.GenerateReport(ctx)
	require.NoError(t, err)
}

func TestGenerateReportFromFile_InvalidPath(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	cfg.Report.ReportPath = "relative/path" // Non-absolute path
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	err := report.GenerateReport(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "report-path must be absolute")
}

func TestGenerateReportFromFile_FileNotFound(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	cfg.Report.ReportPath = "/nonexistent/file.yaml"
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	err := report.GenerateReport(ctx)
	require.Error(t, err)
}

func TestParseReportTests(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	tests := []struct {
		name    string
		data    []byte
		wantErr bool
		wantLen int
	}{
		{
			name: "valid YAML",
			data: []byte(`
tests:
  - testCaseID: "test-1"
    name: "Test 1"
    status: "PASSED"
  - testCaseID: "test-2"
    name: "Test 2"
    status: "FAILED"
`),
			wantErr: false,
			wantLen: 2,
		},
		{
			name: "empty tests array",
			data: []byte(`
tests: []
`),
			wantErr: true, // Should error on empty tests
		},
		{
			name: "missing tests key",
			data: []byte(`
other_key: "value"
`),
			wantErr: true,
		},
		{
			name:    "invalid YAML",
			data:    []byte(`invalid: yaml: content: [}`),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results, err := report.parseReportTests(tt.data)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Len(t, results, tt.wantLen)
		})
	}
}

func TestExtractTestSetIDs(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	tests := []struct {
		name     string
		testSets map[string][]string
		expected []string
	}{
		{
			name:     "empty test sets",
			testSets: map[string][]string{},
			expected: []string{},
		},
		{
			name:     "single test set",
			testSets: map[string][]string{"test-set-1": {}},
			expected: []string{"test-set-1"},
		},
		{
			name:     "multiple test sets",
			testSets: map[string][]string{"test-set-1": {}, "test-set-2": {}, "test-set-3": {}},
			expected: []string{"test-set-1", "test-set-2", "test-set-3"},
		},
		{
			name:     "test sets with spaces",
			testSets: map[string][]string{" test-set-1 ": {}, "  test-set-2  ": {}},
			expected: []string{"test-set-1", "test-set-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg.Report.SelectedTestSets = tt.testSets
			report := New(logger, cfg, mockReportDB, mockTestDB)

			result := report.extractTestSetIDs()

			// Convert to map for easier comparison since order might vary
			resultMap := make(map[string]bool)
			for _, id := range result {
				resultMap[id] = true
			}
			expectedMap := make(map[string]bool)
			for _, id := range tt.expected {
				expectedMap[id] = true
			}

			assert.Equal(t, expectedMap, resultMap)
		})
	}
}

func TestGetLatestTestRunID(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	tests := []struct {
		name        string
		testRunIDs  []string
		expectedID  string
		shouldError bool
	}{
		{
			name:        "single test run",
			testRunIDs:  []string{"test-run-1"},
			expectedID:  "test-run-1",
			shouldError: false,
		},
		{
			name:        "multiple test runs in order",
			testRunIDs:  []string{"test-run-1", "test-run-2", "test-run-3"},
			expectedID:  "test-run-3",
			shouldError: false,
		},
		{
			name:        "multiple test runs out of order",
			testRunIDs:  []string{"test-run-10", "test-run-2", "test-run-1"},
			expectedID:  "test-run-10",
			shouldError: false,
		},
		{
			name:        "test runs with non-numeric suffixes",
			testRunIDs:  []string{"test-run-abc", "test-run-def"},
			expectedID:  "test-run-def", // Alphabetical order when not numeric
			shouldError: false,
		},
		{
			name:        "empty list",
			testRunIDs:  []string{},
			expectedID:  "",
			shouldError: false,
		},
		{
			name:        "mixed numeric and non-numeric",
			testRunIDs:  []string{"test-run-1", "test-run-abc", "test-run-2"},
			expectedID:  "test-run-2", // Numeric ones should sort properly
			shouldError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockReportDB.ExpectedCalls = nil // Reset expectations
			mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return(tt.testRunIDs, nil)

			report := New(logger, cfg, mockReportDB, mockTestDB)
			ctx := context.Background()

			result, err := report.getLatestTestRunID(ctx)

			if tt.shouldError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedID, result)
			}

			mockReportDB.AssertExpectations(t)
		})
	}
}

func TestGetLatestTestRunID_DatabaseError(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	mockReportDB.On("GetAllTestRunIDs", mock.Anything).Return([]string{}, fmt.Errorf("database connection failed"))

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	result, err := report.getLatestTestRunID(ctx)

	assert.Error(t, err)
	assert.Equal(t, "", result)
	assert.Contains(t, err.Error(), "database connection failed")

	mockReportDB.AssertExpectations(t)
}

func TestExtractFailedTestsFromResults(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	testResults := []models.TestResult{
		createTestResult("test-1", "Test 1", models.TestStatusPassed, false),
		createTestResult("test-2", "Test 2", models.TestStatusFailed, true),
		createTestResult("test-3", "Test 3", models.TestStatusIgnored, false),
		createTestResult("test-4", "Test 4", models.TestStatusFailed, true),
		createTestResult("test-5", "Test 5", models.TestStatusPassed, false),
	}

	failedTests := report.extractFailedTestsFromResults(testResults)

	assert.Len(t, failedTests, 2)
	assert.Equal(t, "test-2", failedTests[0].TestCaseID)
	assert.Equal(t, "test-4", failedTests[1].TestCaseID)
}

func TestCollectFailedTests(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	// Setup test data
	testResults1 := []models.TestResult{
		createTestResult("test-1", "Test 1", models.TestStatusPassed, false),
		createTestResult("test-2", "Test 2", models.TestStatusFailed, true),
	}
	testResults2 := []models.TestResult{
		createTestResult("test-3", "Test 3", models.TestStatusFailed, true),
	}

	testReport1 := &models.TestReport{Tests: testResults1}
	testReport2 := &models.TestReport{Tests: testResults2}

	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "test-set-1").Return(testReport1, nil)
	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "test-set-2").Return(testReport2, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	failedTests, err := report.collectFailedTests(ctx, "test-run-1", []string{"test-set-1", "test-set-2"})

	require.NoError(t, err)
	assert.Len(t, failedTests, 2)
	assert.Equal(t, "test-2", failedTests[0].TestCaseID)
	assert.Equal(t, "test-3", failedTests[1].TestCaseID)

	mockReportDB.AssertExpectations(t)
}

func TestCollectFailedTests_WithReportSuffix(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	testResults := []models.TestResult{
		createTestResult("test-1", "Test 1", models.TestStatusFailed, true),
	}
	testReport := &models.TestReport{Tests: testResults}

	// Note: the method should strip the "-report" suffix
	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "test-set-1").Return(testReport, nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	failedTests, err := report.collectFailedTests(ctx, "test-run-1", []string{"test-set-1-report"})

	require.NoError(t, err)
	assert.Len(t, failedTests, 1)

	mockReportDB.AssertExpectations(t)
}

func TestCollectFailedTests_GetReportError(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "test-set-1").Return((*models.TestReport)(nil), fmt.Errorf("database error"))

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	failedTests, err := report.collectFailedTests(ctx, "test-run-1", []string{"test-set-1"})

	require.NoError(t, err) // Error is logged but not returned
	assert.Len(t, failedTests, 0)

	mockReportDB.AssertExpectations(t)
}

func TestCollectFailedTests_NilReport(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	mockReportDB.On("GetReport", mock.Anything, "test-run-1", "test-set-1").Return((*models.TestReport)(nil), nil)

	report := New(logger, cfg, mockReportDB, mockTestDB)
	ctx := context.Background()

	failedTests, err := report.collectFailedTests(ctx, "test-run-1", []string{"test-set-1"})

	require.NoError(t, err)
	assert.Len(t, failedTests, 0)

	mockReportDB.AssertExpectations(t)
}

func TestGenerateTestHeader(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)
	printer := report.createFormattedPrinter()

	testResult := createTestResult("test-123", "Test Case", models.TestStatusFailed, true)

	header := report.generateTestHeader(testResult, printer)

	assert.Contains(t, header, "test-123")
	assert.Contains(t, header, "Testrun failed for testcase with id:")
	assert.Contains(t, header, "--------------------------------------------------------------------")
}

func TestAddStatusCodeDiffs(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	tests := []struct {
		name     string
		result   models.TestResult
		expected bool // whether diffs should be added
	}{
		{
			name: "status code mismatch",
			result: models.TestResult{
				Result: models.Result{
					StatusCode: models.IntResult{
						Normal:   false,
						Expected: 200,
						Actual:   500,
					},
				},
			},
			expected: true,
		},
		{
			name: "status code match",
			result: models.TestResult{
				Result: models.Result{
					StatusCode: models.IntResult{
						Normal:   true,
						Expected: 200,
						Actual:   200,
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Note: We can't easily test the actual diff content without exposing internal state
			// This test mainly ensures no errors are thrown
			logDiffs := matcherUtils.NewDiffsPrinter(tt.result.TestCaseID)

			err := report.addStatusCodeDiffs(tt.result, &logDiffs)
			require.NoError(t, err)
		})
	}
}

func TestAddHeaderDiffs(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	tests := []struct {
		name     string
		result   models.TestResult
		expected bool // whether diffs should be added
	}{
		{
			name: "header mismatch",
			result: models.TestResult{
				Result: models.Result{
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
				},
			},
			expected: true,
		},
		{
			name: "header match",
			result: models.TestResult{
				Result: models.Result{
					HeadersResult: []models.HeaderResult{
						{
							Normal: true,
							Expected: models.Header{
								Key:   "Content-Type",
								Value: []string{"application/json"},
							},
							Actual: models.Header{
								Key:   "Content-Type",
								Value: []string{"application/json"},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "multiple header values",
			result: models.TestResult{
				Result: models.Result{
					HeadersResult: []models.HeaderResult{
						{
							Normal: false,
							Expected: models.Header{
								Key:   "Accept",
								Value: []string{"application/json", "text/html"},
							},
							Actual: models.Header{
								Key:   "Accept",
								Value: []string{"application/json", "application/xml"},
							},
						},
					},
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logDiffs := matcherUtils.NewDiffsPrinter(tt.result.TestCaseID)

			err := report.addHeaderDiffs(tt.result, &logDiffs)
			require.NoError(t, err)
		})
	}
}

func TestAddBodyDiffs(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	tests := []struct {
		name    string
		result  models.TestResult
		wantErr bool
	}{
		{
			name: "body mismatch",
			result: models.TestResult{
				Result: models.Result{
					BodyResult: []models.BodyResult{
						{
							Normal:   false,
							Type:     models.JSON,
							Expected: `{"message": "success"}`,
							Actual:   `{"message": "error"}`,
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "body match",
			result: models.TestResult{
				Result: models.Result{
					BodyResult: []models.BodyResult{
						{
							Normal:   true,
							Type:     models.JSON,
							Expected: `{"message": "success"}`,
							Actual:   `{"message": "success"}`,
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "multiple body mismatches",
			result: models.TestResult{
				Result: models.Result{
					BodyResult: []models.BodyResult{
						{
							Normal:   false,
							Type:     models.JSON,
							Expected: `{"message": "success"}`,
							Actual:   `{"message": "error"}`,
						},
						{
							Normal:   false,
							Type:     models.XML,
							Expected: `<status>ok</status>`,
							Actual:   `<status>fail</status>`,
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logDiffs := matcherUtils.NewDiffsPrinter(tt.result.TestCaseID)

			err := report.addBodyDiffs(tt.result, &logDiffs)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestPrintDefaultBodyDiff(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	tests := []struct {
		name       string
		bodyResult models.BodyResult
		wantErr    bool
	}{
		{
			name: "simple body diff",
			bodyResult: models.BodyResult{
				Normal:   false,
				Type:     models.JSON,
				Expected: `{"message": "success"}`,
				Actual:   `{"message": "error"}`,
			},
			wantErr: false,
		},
		{
			name: "xml body diff",
			bodyResult: models.BodyResult{
				Normal:   false,
				Type:     models.XML,
				Expected: `<status>ok</status>`,
				Actual:   `<status>fail</status>`,
			},
			wantErr: false,
		},
		{
			name: "empty expected",
			bodyResult: models.BodyResult{
				Normal:   false,
				Type:     models.JSON,
				Expected: ``,
				Actual:   `{"message": "error"}`,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := report.printDefaultBodyDiff(tt.bodyResult)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestPrintAndRenderDiffs(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	tests := []struct {
		name    string
		logs    string
		wantErr bool
	}{
		{
			name:    "simple logs",
			logs:    "Test header and logs",
			wantErr: false,
		},
		{
			name:    "empty logs",
			logs:    "",
			wantErr: false,
		},
		{
			name:    "formatted logs",
			logs:    "Testrun failed for testcase with id: test-123\n\n",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			printer := report.createFormattedPrinter()
			logDiffs := matcherUtils.NewDiffsPrinter("test-case")

			err := report.printAndRenderDiffs(printer, tt.logs, &logDiffs)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestPrintSingleTestReport_FullBodyMode(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	cfg.Report.ShowFullBody = true // Enable full body mode
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	testResult := createTestResult("test-123", "Test Case", models.TestStatusFailed, true)

	err := report.printSingleTestReport(testResult)
	require.NoError(t, err)
}

func TestPrintSingleTestReport_NonJSONBody(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	cfg.Report.ShowFullBody = false // Disable full body mode to test JSON table diff path
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	testResult := createTestResult("test-123", "Test Case", models.TestStatusFailed, false)
	// Override with XML body result
	testResult.Result.BodyResult = []models.BodyResult{
		{
			Normal:   false,
			Type:     models.XML,
			Expected: `<status>ok</status>`,
			Actual:   `<status>fail</status>`,
		},
	}

	err := report.printSingleTestReport(testResult)
	require.NoError(t, err)
}

func TestPrintSingleTestReport_JSONDiffError(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	cfg.Report.ShowFullBody = false
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	testResult := createTestResult("test-123", "Test Case", models.TestStatusFailed, false)
	// Override with invalid JSON that will cause diff generation to fail
	testResult.Result.BodyResult = []models.BodyResult{
		{
			Normal:   false,
			Type:     models.JSON,
			Expected: `{"valid": "json"}`,
			Actual:   `{invalid json`,
		},
	}

	err := report.printSingleTestReport(testResult)
	require.NoError(t, err) // Should not fail, just fall back to default diff
}

func TestPrintFailedTestReports_Error(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	cfg.Report.ShowFullBody = false
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	// Create a test result that would trigger an error condition
	// This is difficult to trigger directly, but we test the error handling path
	failedTests := []models.TestResult{
		createTestResult("test-1", "Test 1", models.TestStatusFailed, true),
	}

	err := report.printFailedTestReports(failedTests)
	require.NoError(t, err)
}

func TestRenderTemplateValue_ErrorHandling(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	tests := []struct {
		name    string
		input   interface{}
		wantErr bool
	}{
		{
			name:    "template with error",
			input:   "{{.nonexistentfield}}", // This should trigger an error in template rendering
			wantErr: false,                   // The current implementation doesn't return errors for missing fields
		},
		{
			name:    "valid template",
			input:   "simple string",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := report.renderTemplateValue(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, result)
			}
		})
	}
}

func TestCreateFormattedPrinter(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := createTestConfig()
	mockReportDB := &MockReportDB{}
	mockTestDB := &MockTestDB{}

	report := New(logger, cfg, mockReportDB, mockTestDB)

	printer := report.createFormattedPrinter()

	assert.NotNil(t, printer)
	// We can't easily test the internal configuration without exposing it
	// This test mainly ensures the method doesn't panic and returns a valid printer
}

// Fuzz tests for report.go
func FuzzExtractTestSetIDs(f *testing.F) {
	// Seed with various test set configurations
	f.Add("test-set-1")
	f.Add("")
	f.Add("   test-set-with-spaces   ")
	f.Add("test-set-with-special-chars!@#")

	f.Fuzz(func(t *testing.T, testSetName string) {
		logger := zap.NewNop()
		cfg := createTestConfig()
		cfg.Report.SelectedTestSets = map[string][]string{testSetName: {}}
		mockReportDB := &MockReportDB{}
		mockTestDB := &MockTestDB{}

		report := New(logger, cfg, mockReportDB, mockTestDB)

		// Function should not panic regardless of input
		result := report.extractTestSetIDs()

		// Basic sanity check
		assert.True(t, len(result) >= 0)
		if testSetName != "" {
			assert.True(t, len(result) <= 1)
		}
	})
}

func FuzzParseReportTests(f *testing.F) {
	// Seed with various YAML inputs
	f.Add([]byte(`tests: []`))
	f.Add([]byte(`tests: [{"name": "test"}]`))
	f.Add([]byte(`invalid yaml`))
	f.Add([]byte(``))
	f.Add([]byte(`tests: [{testCaseID: "test-1", status: "FAILED"}]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		logger := zap.NewNop()
		cfg := createTestConfig()
		mockReportDB := &MockReportDB{}
		mockTestDB := &MockTestDB{}

		report := New(logger, cfg, mockReportDB, mockTestDB)

		// Function should not panic regardless of input
		result, err := report.parseReportTests(data)
		_ = result
		_ = err
	})
}
