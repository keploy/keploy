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

// A test set FAILED for an incomplete capture, every test of it passed, is
// never a green JUnit file: the suite carries a failed "keploy capture" test
// case with the reason, besides the properties. An unverified capture only
// gets the properties; a set that was not checked gets neither. Either FAILED
// set, no case of which failed, carries the case of a set that did not pass
// (TestJUnitASetThatDidNotPassIsNeverAGreenSuite).
func TestBuildJUnitSuites_CaptureVerification(t *testing.T) {
	passed := []models.TestResult{{TestCaseID: "tc-1", Name: "s", Status: models.TestStatusPassed, TimeTaken: "1ms"}}
	for _, tc := range []struct {
		name         string
		cv           *models.CaptureVerification
		wantTests    int
		wantFailures int
		wantErrors   int
		wantProps    bool
	}{
		{"incomplete", &models.CaptureVerification{Status: models.CaptureIncompleteStatus, Reason: "capture incomplete: the agent's PID map refused 2 insert(s)"}, 2, 1, 0, true},
		{"unverified", &models.CaptureVerification{Status: models.CaptureUnverifiedStatus, Reason: "capture unverified: read failed"}, 2, 0, 1, true},
		{"not checked", nil, 2, 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suites := buildJUnitSuites(map[string]*models.TestReport{"s": {Total: 1, Success: 1, Status: "FAILED", Tests: passed, CaptureVerification: tc.cv}})
			s := suites.Suites[0]
			if s.Tests != tc.wantTests || s.Failures != tc.wantFailures || suites.Failures != tc.wantFailures || s.Errors != tc.wantErrors {
				t.Fatalf("suite tests %d failures %d (all %d) errors %d; want %d, %d and %d", s.Tests, s.Failures, suites.Failures, s.Errors, tc.wantTests, tc.wantFailures, tc.wantErrors)
			}
			if (s.Properties != nil) != tc.wantProps {
				t.Fatalf("properties %+v, want present %v", s.Properties, tc.wantProps)
			}
			if tc.wantFailures == 1 {
				last := s.Cases[len(s.Cases)-1]
				if last.Name != junitCaptureCase || last.Failure == nil || last.Failure.Text != tc.cv.Reason {
					t.Fatalf("last case %+v, want %q failed with the reason", last, junitCaptureCase)
				}
			}
			if tc.wantProps && (s.Properties.Property[0].Value != string(tc.cv.Status) || s.Properties.Property[1].Value != tc.cv.Reason) {
				t.Fatalf("properties %+v, want the status and the reason", s.Properties.Property)
			}
		})
	}
}

// A test set mock.real's rule 4 failed, every test of it passed, is never a
// green JUnit file: the suite carries a failed "keploy mock.real" case with
// the reason.
func TestBuildJUnitSuites_RealFailure(t *testing.T) {
	passed := []models.TestResult{{TestCaseID: "tc-1", Name: "s", Status: models.TestStatusPassed, TimeTaken: "1ms"}}
	why := "mock.real destination db:5432, which its test cases' recordings list, was never reached"
	s := buildJUnitSuites(map[string]*models.TestReport{"s": {Total: 1, Success: 1, Status: "FAILED", Tests: passed, RealFailure: why}}).Suites[0]
	if s.Tests != 2 || s.Failures != 1 {
		t.Fatalf("suite tests %d failures %d; want 2 and 1", s.Tests, s.Failures)
	}
	last := s.Cases[len(s.Cases)-1]
	if last.Name != junitRealCase || last.Failure == nil || last.Failure.Text != why {
		t.Fatalf("last case %+v, want %q failed with the reason", last, junitRealCase)
	}
	if s := buildJUnitSuites(map[string]*models.TestReport{"s": {Total: 1, Success: 1, Status: "PASSED", Tests: passed}}).Suites[0]; s.Tests != 1 || s.Failures != 0 {
		t.Fatalf("a set mock.real did not fail has %d cases, %d failed", s.Tests, s.Failures)
	}
}
