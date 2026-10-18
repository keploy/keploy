package report

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// P101: a set the calls its application made after its last test failed,
// every test of it passed, is never a green JUnit file: the suite carries a
// failed case naming the calls.
func TestBuildJUnitSuites_UnclaimedCalls(t *testing.T) {
	passed := []models.TestResult{{TestCaseID: "tc-1", Name: "s", Status: models.TestStatusPassed, TimeTaken: "1ms"}}
	calls := []models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "POST /publish", Destination: "broker:9099"},
		{Protocol: "TCP", Destination: "127.0.0.1:9098"},
	}
	s := buildJUnitSuites(map[string]*models.TestReport{"s": {Total: 1, Success: 1, Status: "FAILED", Tests: passed, UnclaimedCalls: calls}}).Suites[0]
	if s.Tests != 2 || s.Failures != 1 {
		t.Fatalf("suite tests %d failures %d; want 2 and 1", s.Tests, s.Failures)
	}
	last := s.Cases[len(s.Cases)-1]
	if last.Name != junitUnclaimedCase || last.Failure == nil {
		t.Fatalf("last case %+v, want %q failed", last, junitUnclaimedCase)
	}
	for _, want := range []string{"HTTP POST /publish to broker:9099", "TCP to 127.0.0.1:9098"} {
		if !strings.Contains(last.Failure.Text, want) {
			t.Fatalf("failure text %q does not name %q", last.Failure.Text, want)
		}
	}
	if s := buildJUnitSuites(map[string]*models.TestReport{"s": {Total: 1, Success: 1, Status: "PASSED", Tests: passed}}).Suites[0]; s.Tests != 1 || s.Failures != 0 {
		t.Fatalf("a set with no unclaimed call has %d cases, %d failed", s.Tests, s.Failures)
	}
}

// F2: a call the application made as it started, before the set's first test,
// is named as such in the same failed case.
func TestBuildJUnitSuites_ACallBeforeTheFirstTestIsNamedAsSuch(t *testing.T) {
	passed := []models.TestResult{{TestCaseID: "tc-1", Name: "s", Status: models.TestStatusPassed, TimeTaken: "1ms"}}
	calls := []models.UnmatchedCall{
		{Protocol: "Mongo", ActualSummary: "insert on events", BeforeSet: true, Kind: models.MissOutsideTest},
		{Protocol: "HTTP", ActualSummary: "POST /publish"},
	}
	s := buildJUnitSuites(map[string]*models.TestReport{"s": {Total: 1, Success: 1, Status: "FAILED", Tests: passed, UnclaimedCalls: calls}}).Suites[0]
	last := s.Cases[len(s.Cases)-1]
	if last.Failure == nil || strings.Contains(last.Failure.Message, "after the last test") {
		t.Fatalf("failure %+v; want a message for calls outside every test", last.Failure)
	}
	for _, want := range []string{"before the first test: Mongo insert on events", "after the last test: HTTP POST /publish"} {
		if !strings.Contains(last.Failure.Text, want) {
			t.Fatalf("failure text %q does not say %q", last.Failure.Text, want)
		}
	}
}
