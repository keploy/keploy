package report

import (
	"encoding/xml"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestBuildJUnitSuites_Basic(t *testing.T) {
	reports := map[string]*models.TestReport{
		"test-set-1": {
			Total:     3,
			Success:   2,
			Failure:   1,
			TimeTaken: "1.5s",
			Tests: []models.TestResult{
				{
					TestCaseID: "tc-1",
					Name:       "test-set-1",
					Status:     models.TestStatusPassed,
					TimeTaken:  "500ms",
				},
				{
					TestCaseID: "tc-2",
					Name:       "test-set-1",
					Status:     models.TestStatusPassed,
					TimeTaken:  "400ms",
				},
				{
					TestCaseID: "tc-3",
					Name:       "test-set-1",
					Kind:       models.HTTP,
					Status:     models.TestStatusFailed,
					TimeTaken:  "600ms",
					Result: models.Result{
						StatusCode: models.IntResult{
							Normal:   false,
							Expected: 200,
							Actual:   500,
						},
					},
					FailureInfo: models.FailureInfo{
						Risk: models.High,
					},
				},
			},
		},
	}

	suites := buildJUnitSuites(reports)

	if suites.Tests != 3 {
		t.Errorf("expected 3 total tests, got %d", suites.Tests)
	}
	if suites.Failures != 1 {
		t.Errorf("expected 1 failure, got %d", suites.Failures)
	}
	if len(suites.Suites) != 1 {
		t.Fatalf("expected 1 suite, got %d", len(suites.Suites))
	}

	suite := suites.Suites[0]
	if suite.Name != "test-set-1" {
		t.Errorf("expected suite name 'test-set-1', got %q", suite.Name)
	}
	if suite.Tests != 3 {
		t.Errorf("expected 3 tests in suite, got %d", suite.Tests)
	}
	if suite.Failures != 1 {
		t.Errorf("expected 1 failure in suite, got %d", suite.Failures)
	}

	// Check passed test has no failure
	if suite.Cases[0].Failure != nil {
		t.Error("expected no failure for passed test tc-1")
	}
	if suite.Cases[0].Skipped != nil {
		t.Error("expected no skip for passed test tc-1")
	}

	// Check failed test
	failedCase := suite.Cases[2]
	if failedCase.Failure == nil {
		t.Fatal("expected failure for tc-3")
	}
	if !strings.Contains(failedCase.Failure.Message, "HIGH-RISK") {
		t.Errorf("expected HIGH-RISK in failure message, got %q", failedCase.Failure.Message)
	}
	if !strings.Contains(failedCase.Failure.Text, "status: expected 200, got 500") {
		t.Errorf("expected status diff in failure text, got %q", failedCase.Failure.Text)
	}
}

func TestBuildJUnitSuites_ObsoleteSkipped(t *testing.T) {
	reports := map[string]*models.TestReport{
		"test-set-1": {
			Total:    2,
			Success:  1,
			Obsolete: 1,
			Tests: []models.TestResult{
				{
					TestCaseID: "tc-1",
					Status:     models.TestStatusPassed,
				},
				{
					TestCaseID: "tc-2",
					Status:     models.TestStatusObsolete,
				},
			},
		},
	}

	suites := buildJUnitSuites(reports)
	suite := suites.Suites[0]

	if suite.Skipped != 1 {
		t.Errorf("expected 1 skipped, got %d", suite.Skipped)
	}
	if suite.Cases[1].Skipped == nil {
		t.Error("expected skip element for obsolete test")
	}
	if suite.Cases[1].Skipped.Message != "obsolete test case" {
		t.Errorf("expected 'obsolete test case' message, got %q", suite.Cases[1].Skipped.Message)
	}
}

func TestBuildJUnitSuites_IgnoredSkipped(t *testing.T) {
	reports := map[string]*models.TestReport{
		"test-set-1": {
			Total:   2,
			Success: 1,
			Tests: []models.TestResult{
				{
					TestCaseID: "tc-1",
					Status:     models.TestStatusPassed,
				},
				{
					TestCaseID: "tc-2",
					Status:     models.TestStatusIgnored,
				},
			},
		},
	}

	suites := buildJUnitSuites(reports)
	suite := suites.Suites[0]

	if suite.Skipped != 1 {
		t.Errorf("expected 1 skipped for ignored test, got %d", suite.Skipped)
	}
	if suite.Cases[1].Skipped == nil {
		t.Fatal("expected skip element for ignored test")
	}
	if suite.Cases[1].Skipped.Message != "ignored test case" {
		t.Errorf("expected 'ignored test case' message, got %q", suite.Cases[1].Skipped.Message)
	}
}

func TestBuildJUnitSuites_ValidXML(t *testing.T) {
	reports := map[string]*models.TestReport{
		"test-set-1": {
			Total:     1,
			Success:   1,
			TimeTaken: "100ms",
			Tests: []models.TestResult{
				{
					TestCaseID: "tc-1",
					Name:       "test-set-1",
					Status:     models.TestStatusPassed,
					TimeTaken:  "100ms",
				},
			},
		},
	}

	suites := buildJUnitSuites(reports)
	data, err := xml.MarshalIndent(suites, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal JUnit XML: %v", err)
	}

	xmlStr := xml.Header + string(data)

	if !strings.Contains(xmlStr, `<?xml version="1.0"`) {
		t.Error("expected XML declaration")
	}
	if !strings.Contains(xmlStr, `<testsuites`) {
		t.Error("expected <testsuites> root element")
	}
	if !strings.Contains(xmlStr, `<testsuite`) {
		t.Error("expected <testsuite> element")
	}
	if !strings.Contains(xmlStr, `<testcase`) {
		t.Error("expected <testcase> element")
	}

	// Verify it's valid XML by unmarshaling back
	var parsed junitTestSuites
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("generated XML is not valid: %v", err)
	}
	if parsed.Tests != 1 {
		t.Errorf("round-trip: expected 1 test, got %d", parsed.Tests)
	}
}

func TestFmtSeconds(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"1.5s", "1.500"},
		{"100ms", "0.100"},
		{"2m30s", "150.000"},
		{"", "0.000"},
	}

	for _, tt := range tests {
		got := fmtTestTime(tt.input)
		if got != tt.expected {
			t.Errorf("fmtTestTime(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
