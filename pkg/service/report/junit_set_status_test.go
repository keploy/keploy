package report

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// setReport is a test set with status and the given test cases, every one
// passed, out of total.
func setReport(status models.TestSetStatus, total int, passed ...string) map[string]*models.TestReport {
	rep := &models.TestReport{Status: string(status), Total: total, Success: len(passed), TimeTaken: "1s",
		FailureReason: "the reason the set says"}
	for _, id := range passed {
		rep.Tests = append(rep.Tests, models.TestResult{TestCaseID: id, Status: models.TestStatusPassed})
	}
	return map[string]*models.TestReport{"test-set-0": rep}
}

// A set whose status fails the run is never a green JUnit suite, with the
// switch on or off: one keploy erred on, one whose app faulted part-way
// (the cases it ran passed, the rest never ran), one whose app halted or
// whose script faulted, one with no tests to run, and a FAILED one no case
// of which failed each get a case of their own, an <error> with the set's
// reason. They read green while the run exited non-zero.
func TestJUnitASetThatDidNotPassIsNeverAGreenSuite(t *testing.T) {
	for _, tc := range []struct {
		status models.TestSetStatus
		total  int
		passed []string
	}{
		{models.TestSetStatusInternalErr, 2, []string{"test-1", "test-2"}},
		{models.TestSetStatusFaultUserApp, 3, []string{"test-1", "test-2"}},
		{models.TestSetStatusAppHalted, 3, []string{"test-1"}},
		{models.TestSetStatusFaultScript, 1, []string{"test-1"}},
		{models.TestSetStatusNoTestsToRun, 0, nil},
		{models.TestSetStatusFailed, 1, []string{"test-1"}},
		{models.TestSetStatusRunning, 3, nil},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			suites := buildJUnitSuites(setReport(tc.status, tc.total, tc.passed...))
			s := suites.Suites[0]
			if s.Errors != 1 || suites.Errors != 1 || s.Failures != 0 || len(s.Cases) != len(tc.passed)+1 {
				t.Fatalf("suite errors %d, file errors %d, failures %d, cases %d; want one error case more", s.Errors, suites.Errors, s.Failures, len(s.Cases))
			}
			c := s.Cases[len(s.Cases)-1]
			if c.Name != junitVerdictCase || c.Error == nil || c.Error.Type != "TestSetNotPassed" ||
				!strings.Contains(c.Error.Message, string(tc.status)) || c.Error.Text != "the reason the set says" ||
				strings.Contains(c.Error.Message, "though no test case of it failed") != (len(tc.passed) > 0) {
				t.Fatalf("case %+v; want %q, an error naming %s, with the set's reason", c, junitVerdictCase, tc.status)
			}
		})
	}
}

// A set that passed, was ignored, was stopped by its user, or whose report
// has no status renders as it did; so does a failed
// set a case of which failed, and a set run under the switch that keploy
// could not judge carries its one verdict case, not two.
func TestJUnitASetThatPassedOrSaysItFailedRendersAsBefore(t *testing.T) {
	for _, status := range []models.TestSetStatus{models.TestSetStatusPassed, models.TestSetStatusIgnored, models.TestSetStatusUserAbort, ""} {
		data, err := xml.Marshal(buildJUnitSuites(setReport(status, 1, "test-1")))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "errors=") || strings.Contains(string(data), junitVerdictCase) {
			t.Fatalf("%q: the suite changed:\n%s", status, data)
		}
	}

	failed := setReport(models.TestSetStatusFailed, 2, "test-1")
	failed["test-set-0"].Tests = append(failed["test-set-0"].Tests, models.TestResult{TestCaseID: "test-2", Status: models.TestStatusFailed})
	if s := buildJUnitSuites(failed).Suites[0]; s.Errors != 0 || s.Failures != 1 || len(s.Cases) != 2 {
		t.Fatalf("a failed set with a failed case: %+v; want it as before", s)
	}

	proven := setReport(models.TestSetStatusInternalErr, 1, "test-1")
	proven["test-set-0"].Proven = true
	if s := buildJUnitSuites(proven).Suites[0]; s.Errors != 1 || len(s.Cases) != 2 || s.Cases[1].Error == nil || s.Cases[1].Error.Type != "NotJudged" {
		t.Fatalf("a set run under the switch keploy could not judge: %+v; want its one verdict case", s)
	}
}

// keploy report --format junit --test-case renders only the cases asked for:
// a FAILED set a case of which failed, filtered to cases that passed, or to
// none of its cases, is not said to have failed with no case failing. A set
// that did not pass with no case failing carries its case however filtered.
func TestJUnitAFilteredSetIsJudgedOnAllItsCases(t *testing.T) {
	failed := setReport(models.TestSetStatusFailed, 2, "test-1")
	failed["test-set-0"].Tests = append(failed["test-set-0"].Tests, models.TestResult{TestCaseID: "test-2", Status: models.TestStatusFailed})
	explained := map[string]bool{"test-set-0": slices.ContainsFunc(failed["test-set-0"].Tests, redCase)}
	failed["test-set-0"].Tests = failed["test-set-0"].Tests[:1] // --test-case test-1
	if s := buildJUnitSuitesExplained(failed, explained).Suites[0]; s.Errors != 0 || len(s.Cases) != 1 {
		t.Fatalf("a failed set filtered to its passing case: %+v; want no did-not-pass case", s)
	}

	faulted := setReport(models.TestSetStatusFaultUserApp, 3, "test-1", "test-2")
	explained = map[string]bool{"test-set-0": slices.ContainsFunc(faulted["test-set-0"].Tests, redCase)}
	faulted["test-set-0"].Tests = faulted["test-set-0"].Tests[:1]
	if s := buildJUnitSuitesExplained(faulted, explained).Suites[0]; s.Errors != 1 || s.Cases[len(s.Cases)-1].Name != junitVerdictCase {
		t.Fatalf("a faulted set filtered: %+v; want its did-not-pass case", s)
	}
}

// keploy report --format junit --test-case, end to end: the FAILED set whose
// test-1 failed, filtered to test-2 that passed, gets no did-not-pass case.
func TestGenerateReport_JUnitFilteredFailedSetIsNotSaidToFailWithNoCaseFailing(t *testing.T) {
	rdb, tdb := newFakeReportFixture()
	rdb.reports["test-set-0"].Status = string(models.TestSetStatusFailed)
	r := New(zap.NewNop(), &config.Config{Report: config.Report{Format: "junit", TestCaseIDs: []string{"test-2"}}}, rdb, tdb)
	var buf bytes.Buffer
	r.out = bufio.NewWriter(&buf)
	if err := r.GenerateReport(context.Background()); err != nil {
		t.Fatalf("GenerateReport: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "TestSetNotPassed") || !strings.Contains(out, `name="test-2"`) {
		t.Fatalf("filtered to test-2:\n%s", out)
	}
}
