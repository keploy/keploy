package report

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A set whose end read a call that no mock answered, and that the agent could
// not place outside every test's window, is never a green JUnit file, every
// test of it passed: the suite carries a failed case naming the calls. Each
// is what keploy hands the report (TestReport.UnclaimedCalls): a call made in
// the last test's window and filed after its read (Kind in_test), or one
// whose time the agent does not say (no Kind).
func TestBuildJUnitSuites_UnclaimedCalls(t *testing.T) {
	passed := []models.TestResult{{TestCaseID: "tc-1", Name: "s", Status: models.TestStatusPassed, TimeTaken: "1ms"}}
	calls := []models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "POST /publish", Destination: "broker:9099", Kind: models.MissInTest},
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
	if want := "a call read only at the set's end got no recorded answer"; last.Failure.Message != want {
		t.Fatalf("failure message %q; want %q: one call's time keploy cannot place", last.Failure.Message, want)
	}
	for _, want := range []string{"made in the last test: HTTP POST /publish to broker:9099", "made at a time keploy cannot place: TCP to 127.0.0.1:9098"} {
		if !strings.Contains(last.Failure.Text, want) {
			t.Fatalf("failure text %q does not say %q", last.Failure.Text, want)
		}
	}
	if strings.Contains(last.Failure.Text+last.Failure.Message+last.Name, "after the last test") {
		t.Fatalf("case %+v says a call was made after the last test; such a call is background work, never here", last)
	}
	if s := buildJUnitSuites(map[string]*models.TestReport{"s": {Total: 1, Success: 1, Status: "PASSED", Tests: passed}}).Suites[0]; s.Tests != 1 || s.Failures != 0 {
		t.Fatalf("a set with no unclaimed call has %d cases, %d failed", s.Tests, s.Failures)
	}
}

// A call the agent placed outside every test's window (Kind outside_test)
// but gave no instant is not background work: the agent marks a call made
// after a test's read as background by when it was made (proxy.go's
// madeBetweenTests), and a miss with no time is never so marked. The set's
// end reads it as unclaimed, and the suite's failed case says where it was
// made, not that keploy cannot place it.
func TestBuildJUnitSuites_ACallPlacedOutsideEveryTestIsSaidAsMadeThere(t *testing.T) {
	passed := []models.TestResult{{TestCaseID: "tc-1", Name: "s", Status: models.TestStatusPassed, TimeTaken: "1ms"}}
	calls := []models.UnmatchedCall{{Protocol: "TCP", Destination: "127.0.0.1:9098", Kind: models.MissOutsideTest}}
	s := buildJUnitSuites(map[string]*models.TestReport{"s": {Total: 1, Success: 1, Status: "FAILED", Tests: passed, UnclaimedCalls: calls}}).Suites[0]
	last := s.Cases[len(s.Cases)-1]
	if last.Name != junitUnclaimedCase || last.Failure == nil {
		t.Fatalf("last case %+v, want %q failed", last, junitUnclaimedCase)
	}
	if want := "made outside every test: TCP to 127.0.0.1:9098"; !strings.Contains(last.Failure.Text, want) || strings.Contains(last.Failure.Text, "cannot place") {
		t.Fatalf("failure text %q; want it saying %q, not that keploy cannot place the call", last.Failure.Text, want)
	}
}

// Every call made in the last test's window (an extra call among them) is
// said so, and so is the case's message.
func TestBuildJUnitSuites_CallsMadeInTheLastTestAreSaidAsMadeThere(t *testing.T) {
	passed := []models.TestResult{{TestCaseID: "tc-1", Name: "s", Status: models.TestStatusPassed, TimeTaken: "1ms"}}
	calls := []models.UnmatchedCall{
		{Protocol: "Mongo", ActualSummary: "insert on events", Kind: models.MissInTest},
		{Protocol: "HTTP", ActualSummary: "POST /publish", Kind: models.MissExtraCall},
	}
	s := buildJUnitSuites(map[string]*models.TestReport{"s": {Total: 1, Success: 1, Status: "FAILED", Tests: passed, UnclaimedCalls: calls}}).Suites[0]
	last := s.Cases[len(s.Cases)-1]
	if want := "a call in the set's last test got no recorded answer, read only at the set's end"; last.Failure == nil || last.Failure.Message != want {
		t.Fatalf("failure %+v; want the message %q", last.Failure, want)
	}
	for _, want := range []string{"made in the last test: Mongo insert on events", "made in the last test: HTTP POST /publish"} {
		if !strings.Contains(last.Failure.Text, want) {
			t.Fatalf("failure text %q does not say %q", last.Failure.Text, want)
		}
	}
}
