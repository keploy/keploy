package replay

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// realInstr is the harness's agent with mock.real: the REAL rows every test
// case's end reads, and what the set's run reached.
type realInstr struct {
	*prInstr
	each models.RealTest
	run  models.RealRun
}

func (f *realInstr) PushRealDeps(context.Context, []string) error { return nil }
func (f *realInstr) KeepRealDeps(context.Context, []string) error { return nil }
func (f *realInstr) RealRun(context.Context) (models.RealRun, error) {
	return f.run, nil
}
func (f *realInstr) RealTest(context.Context) (models.RealTest, error) {
	return f.each, nil
}

// Rule 4 (design v14 §8.2) through RunTestSet: a destination a test case's
// recording lists must be reached in the set's run. One its test cases used
// and that never answered is told apart from one nobody used: the first is
// down (or never answers), the second no longer needed or never started.
func TestRunTestSet_RealDestinationsTheRecordingsListMustBeReached(t *testing.T) {
	used := models.RealTest{Real: []models.RealUse{{Dest: "db:5432", Conns: 1}}}
	for _, c := range []struct {
		name    string
		each    models.RealTest
		reached []string
		status  models.TestSetStatus
		why     string
	}{
		{"reached", used, []string{"db:5432"}, models.TestSetStatusPassed, ""},
		{"used, never answered", used, nil, models.TestSetStatusFailed, "mock.real destination db:5432, which its test cases' recordings list, was used but never answered in this test set"},
		{"never used", models.RealTest{}, nil, models.TestSetStatusFailed, "mock.real destination db:5432, which its test cases' recordings list, was never reached"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Mock.Real = []string{"db:5432"}
			h.cases[0].Real = []string{"db:5432"}
			h.replayer.instrumentation = &realInstr{prInstr: h.instr, each: c.each, run: models.RealRun{Reached: c.reached}}

			status := h.run(t)

			if status != c.status {
				t.Fatalf("status %s, want %s (reason %q)", status, c.status, h.report.report.FailureReason)
			}
			if reason := h.report.report.FailureReason; c.why != "" && !strings.Contains(reason, c.why) {
				t.Fatalf("failure reason %q does not say %q", reason, c.why)
			}
			if len(h.report.report.Tests) != 2 {
				t.Fatalf("the report has %d tests, want 2", len(h.report.report.Tests))
			}
			for _, r := range h.report.report.Tests {
				if got := r.Real; len(c.each.Real) > 0 && (len(got) != 1 || got[0].Dest != "db:5432") {
					t.Fatalf("%s's REAL rows %v, want db:5432", r.TestCaseID, got)
				}
			}
			// Rule 4 is mock.real's own failure on the report, which JUnit
			// fails a case for; no capture is said incomplete for it.
			if got := h.report.report.RealFailure; (c.why == "") != (got == "") || !strings.Contains(got, c.why) {
				t.Fatalf("the report's mock.real failure %q, want %q", got, c.why)
			}
			if cv := h.report.report.CaptureVerification; cv != nil {
				t.Fatalf("capture verification %+v for a set mock.real's rule 4 decided", cv)
			}
		})
	}
}

// Rule 2 (design v14 §8.2) through RunTestSet: a connection keploy could
// neither capture nor relay as real outside every test case is the set's
// capture verdict: the set fails, the report's capture verification says
// incomplete with what to change, and the prune and JUnit follow it.
func TestRunTestSet_AConnectionKeployCouldNotCaptureOutsideEveryTestFailsTheSetsCapture(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 1, 0)
	h.replayer.config.Mock.Real = []string{"db:5432"}
	h.replayer.instrumentation = &realInstr{prInstr: h.instr, run: models.RealRun{
		Incomplete: []models.CaptureIncomplete{{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: "list the address in mock.real"}}}}

	status := h.run(t)

	if status != models.TestSetStatusFailed {
		t.Fatalf("status %s, want FAILED (reason %q)", status, h.report.report.FailureReason)
	}
	cv := h.report.report.CaptureVerification
	if cv == nil || cv.Status != models.CaptureIncompleteStatus || !strings.Contains(cv.Reason, "10.0.0.7:443: list the address in mock.real") {
		t.Fatalf("capture verification %+v, want incomplete naming the connection", cv)
	}
	if !strings.Contains(h.report.report.FailureReason, "keploy could neither capture nor relay as real 1 connection(s)") {
		t.Fatalf("failure reason %q does not say what keploy could not capture", h.report.report.FailureReason)
	}
	if h.report.report.RealFailure != "" {
		t.Fatalf("mock.real failure %q for a set whose listed destinations were all reached", h.report.report.RealFailure)
	}
}
