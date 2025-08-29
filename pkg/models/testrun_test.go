package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTestReport_GetKind_001 tests the GetKind method of TestReport to ensure it returns the correct kind string.
func TestTestReport_GetKind_001(t *testing.T) {
	// Arrange
	testReport := &TestReport{}

	// Act
	kind := testReport.GetKind()

	// Assert
	assert.Equal(t, "TestReport", kind, "Expected kind to be 'TestReport'")
}

// TestTestResult_GetKind_002 tests the GetKind method of TestResult to ensure it returns the correct kind string.
func TestTestResult_GetKind_002(t *testing.T) {
	// Arrange
	testResult := &TestResult{
		Kind: "Http",
	}

	// Act
	kind := testResult.GetKind()

	// Assert
	assert.Equal(t, "Http", kind, "Expected kind to be 'Http'")
}

// TestStringToTestSetStatus_ValidAndInvalidInputs_003 tests the StringToTestSetStatus function for valid and invalid inputs.
func TestStringToTestSetStatus_ValidAndInvalidInputs_003(t *testing.T) {
	// Arrange
	validInputs := map[string]TestSetStatus{
		"RUNNING":         TestSetStatusRunning,
		"FAILED":          TestSetStatusFailed,
		"PASSED":          TestSetStatusPassed,
		"APP_HALTED":      TestSetStatusAppHalted,
		"USER_ABORT":      TestSetStatusUserAbort,
		"APP_FAULT":       TestSetStatusFaultUserApp,
		"INTERNAL_ERR":    TestSetStatusInternalErr,
		"NO_TESTS_TO_RUN": TestSetStatusNoTestsToRun,
	}
	invalidInput := "INVALID"

	// Act & Assert for valid inputs
	for input, expected := range validInputs {
		status, err := StringToTestSetStatus(input)
		assert.NoError(t, err, "Expected no error for valid input")
		assert.Equal(t, expected, status, "Expected status to match")
	}

	// Act & Assert for invalid input
	status, err := StringToTestSetStatus(invalidInput)
	assert.Error(t, err, "Expected an error for invalid input")
	assert.Equal(t, TestSetStatus(""), status, "Expected status to be empty for invalid input")
}
