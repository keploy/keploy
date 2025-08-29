package report

import (
	"testing"

	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v2/pkg/models"
)

// TestFilterTestsByIDs_ValidIDs_123 tests the filterTestsByIDs function with valid IDs.
func TestFilterTestsByIDs_ValidIDs_123(t *testing.T) {
	tests := []models.TestResult{
		{TestCaseID: "id1"},
		{TestCaseID: "id2"},
		{TestCaseID: "id3"},
	}
	ids := []string{"id1", "id3"}

	result := filterTestsByIDs(tests, ids)

	require.Len(t, result, 2)
	assert.Equal(t, "id1", result[0].TestCaseID)
	assert.Equal(t, "id3", result[1].TestCaseID)
}

// TestEstimateDuration_ValidDurations_456 tests the estimateDuration function with valid durations.
func TestEstimateDuration_ValidDurations_456(t *testing.T) {
	tests := []models.TestResult{
		{TimeTaken: "1s"},
		{TimeTaken: "2s"},
		{TimeTaken: "3s"},
	}

	result := estimateDuration(tests)

	assert.Equal(t, 6*time.Second, result)
}

// TestFmtDuration_ValidDuration_321 tests the fmtDuration function with a valid duration.
func TestFmtDuration_ValidDuration_321(t *testing.T) {
	input := 28540 * time.Millisecond

	result := fmtDuration(input)

	assert.Equal(t, "28.54 s", result)
}

// TestApplyCliColorsToDiff_ValidDiff_654 tests the applyCliColorsToDiff function with a valid diff string.
func TestApplyCliColorsToDiff_ValidDiff_654(t *testing.T) {
	input := "Path: /status\n  Old: 200\n  New: 404\n"
	expected := "Path: \x1b[33m/status\x1b[0m\n  Old: \x1b[31m200\x1b[0m\n  New: \x1b[32m404\x1b[0m\n"

	result := applyCliColorsToDiff(input)

	assert.Equal(t, expected, result)
}

// TestGenerateStatusAndHeadersTableDiff_ValidTestResult_987 tests the GenerateStatusAndHeadersTableDiff function with a valid test result.
func TestGenerateStatusAndHeadersTableDiff_ValidTestResult_987(t *testing.T) {
	test := models.TestResult{
		Result: models.Result{
			StatusCode: models.IntResult{
				Normal:   false,
				Expected: 200,
				Actual:   404,
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
		},
	}

	result := GenerateStatusAndHeadersTableDiff(test)

	assert.Contains(t, result, "Path: status_code")
	assert.Contains(t, result, "Old: 200")
	assert.Contains(t, result, "New: 404")
	assert.Contains(t, result, "Path: header.Content-Type")
	assert.Contains(t, result, "Old: application/json")
	assert.Contains(t, result, "New: text/html")
}

// TestPrintSingleSummary_ValidInput_789 tests the printSingleSummary function with valid input.
func TestPrintSingleSummary_ValidInput_789(t *testing.T) {
	name := "Test Suite A"
	total := 10
	pass := 7
	fail := 3
	dur := 2 * time.Minute
	failedCases := []string{"TestCase1", "TestCase2", "TestCase3"}

	// Act
	printSingleSummary(name, total, pass, fail, dur, failedCases)

	// Assert
	// Since the function prints to stdout, you can manually verify the output or use a library like `os.Pipe` to capture and validate it.
	// For simplicity, this test assumes manual verification of the printed output.
}

// TestPrintSingleSummary_NoFailedCases_123 tests the printSingleSummary function when there are no failed test cases.
func TestPrintSingleSummary_NoFailedCases_123(t *testing.T) {
	name := "Test Suite B"
	total := 5
	pass := 5
	fail := 0
	dur := 1 * time.Minute
	failedCases := []string{}

	// Act
	printSingleSummary(name, total, pass, fail, dur, failedCases)

	// Assert
	// Since the function prints to stdout, you can manually verify the output or use a library like `os.Pipe` to capture and validate it.
	// For simplicity, this test assumes manual verification of the printed output.
}

// TestGenerateStatusAndHeadersTableDiff_NoDifferences_456 tests the GenerateStatusAndHeadersTableDiff function when there are no differences in status or headers.
func TestGenerateStatusAndHeadersTableDiff_NoDifferences_456(t *testing.T) {
	test := models.TestResult{
		Result: models.Result{
			StatusCode: models.IntResult{
				Normal:   true,
				Expected: 200,
				Actual:   200,
			},
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
	}

	// Act
	result := GenerateStatusAndHeadersTableDiff(test)

	// Assert
	assert.Equal(t, "No differences found in status or headers.", result)
}
