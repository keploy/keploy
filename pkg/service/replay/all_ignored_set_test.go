package replay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// everyRow sets every result row the given status, as a hook that
// quarantines every test of a set does (keploy#4769's contract: every row).
func everyRow(status models.TestStatus) func([]models.TestResult) {
	return func(results []models.TestResult) {
		for i := range results {
			results[i].Status = status
		}
	}
}

// A set none of whose tests was verified, every one of them IGNORED, is no
// pass (design §8.2 rule 1). A set the run skips whole is reported IGNORED;
// under the proven switch, so is a set whose tests are skipped one by one or
// quarantined one by one by a hook. With the switch off it stays PASSED, and
// its report is what it was.
func TestRunTestSet_ASetWhoseEveryTestIsIgnoredIsIgnored(t *testing.T) {
	for _, tc := range []struct {
		name string
		prep func(h *prRun)
		// the set's status with the switch off, as today
		off models.TestSetStatus
	}{
		{"the skip list names every test", func(h *prRun) {
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-1", "test-2"}}
		}, models.TestSetStatusPassed},
		{"a hook quarantines every test of a passing set", func(h *prRun) {
			h.replayer.hookImpl = verdictHooks{set: everyRow(models.TestStatusIgnored)}
		}, models.TestSetStatusPassed},
		{"a hook quarantines every test of a failing set", func(h *prRun) {
			h.replayer.hookImpl = verdictHooks{prHooks: prHooks{wrongBody: map[string]bool{"test-1": true}}, set: everyRow(models.TestStatusIgnored)}
		}, models.TestSetStatusFailed},
	} {
		for _, on := range []bool{false, true} {
			t.Run(tc.name+"/"+switchName(on), func(t *testing.T) {
				setSwitch(t, on)
				h := newPartialRunHarness(t, 2, 0)
				tc.prep(h)
				core, logs := observer.New(zapcore.WarnLevel)
				h.replayer.logger = zap.New(core)

				status := h.run(t)
				rep := h.report.report
				said := logs.FilterMessageSnippet("nothing in the test-set was verified").All()
				if !on {
					if status != tc.off || rep.Status != string(tc.off) || len(said) != 0 {
						t.Fatalf("switch off: set %s, report %s, said %d; want %s as today, and nothing said", status, rep.Status, len(said), tc.off)
					}
					return
				}
				if status != models.TestSetStatusIgnored || rep.Status != string(models.TestSetStatusIgnored) {
					t.Fatalf("set %s, report %s; want IGNORED: no test of it was verified, so it did not pass", status, rep.Status)
				}
				if rep.Success != 0 || rep.Failure != 0 || rep.Obsolete != 0 || rep.Ignored != 2 || rep.Total != 2 {
					t.Fatalf("report total %d, passed %d, failed %d, obsolete %d, ignored %d; want 2 ignored of 2", rep.Total, rep.Success, rep.Failure, rep.Obsolete, rep.Ignored)
				}
				if len(rep.Tests) != 2 || rep.Tests[0].Status != models.TestStatusIgnored || rep.Tests[1].Status != models.TestStatusIgnored {
					t.Fatalf("rows %+v; want both IGNORED", rep.Tests)
				}
				if rep.FailureReason != "" {
					t.Fatalf("failure reason %q; an IGNORED set has none, as a set skipped whole has none", rep.FailureReason)
				}
				// The run's summary shows it as it shows a set skipped whole.
				v := h.replayer.completeTestReport["test-set-0"]
				if !v.status || v.ignored != 2 || v.passed != 0 || v.failed != 0 {
					t.Fatalf("run summary row %+v; want it shown as a set skipped whole is (status true, 2 ignored)", v)
				}
				if len(said) != 1 || said[0].ContextMap()["test-set"] != "test-set-0" {
					t.Fatalf("said %+v; want one warning naming the set", said)
				}
			})
		}
	}
}

// Only a set with no verdict at all is IGNORED: one test that passed, failed
// or was obsolete beside the ignored ones keeps the set's status.
func TestRunTestSet_ASetWithOneVerdictBesideIgnoredTestsKeepsItsStatus(t *testing.T) {
	setSwitch(t, true)
	for _, tc := range []struct {
		name  string
		hooks prHooks
		want  models.TestSetStatus
	}{
		{"one passed", prHooks{}, models.TestSetStatusPassed},
		{"one failed", prHooks{wrongBody: map[string]bool{"test-2": true}}, models.TestSetStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-1"}}
			h.replayer.hookImpl = tc.hooks
			if status := h.run(t); status != tc.want || h.report.report.Status != string(tc.want) {
				t.Fatalf("set %s, report %s; want %s", status, h.report.report.Status, tc.want)
			}
		})
	}

	// An OBSOLETE test ran and answered: its set verified something.
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.hookImpl = verdictHooks{set: func(results []models.TestResult) {
		everyRow(models.TestStatusIgnored)(results)
		setStatus("test-2", models.TestStatusObsolete)(results)
	}}
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("one obsolete beside one ignored: set %s, want PASSED", status)
	}
}

// A set that stopped part-way is APP_FAULT even when the only rows it has are
// IGNORED: the tests it never reached are unverified, which says more than
// IGNORED does. The IGNORED rule is the last word only on a set that would
// otherwise be PASSED.
func TestRunTestSet_APartialRunWithOnlyIgnoredRowsIsNotIgnored(t *testing.T) {
	setSwitch(t, true)
	// UpdateMockParams is called once before the loop and then once per test
	// that runs; test-1 is ignored before the loop, so the 2nd call is
	// test-2's: it is refused, and test-2 and test-3 never get a verdict.
	h := newPartialRunHarness(t, 3, 2)
	h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-1"}}

	status := h.run(t)
	rep := h.report.report
	if rep.Success+rep.Failure+rep.Obsolete != 0 || rep.Ignored != 1 {
		t.Fatalf("precondition: passed %d, failed %d, obsolete %d, ignored %d; want only test-1, ignored", rep.Success, rep.Failure, rep.Obsolete, rep.Ignored)
	}
	if status != models.TestSetStatusFaultUserApp || rep.Status != string(models.TestSetStatusFaultUserApp) {
		t.Fatalf("set %s, report %s; want APP_FAULT: the run stopped before test-2 and test-3", status, rep.Status)
	}
}

// unreadQuarantined is an agent whose consumed-mock read fails, with a hook
// that then quarantines every test.
type unreadQuarantined struct{ consumedReadFails }

func (unreadQuarantined) BeforeTestResult(_ context.Context, _, _ string, results []models.TestResult) error {
	everyRow(models.TestStatusIgnored)(results)
	return nil
}

// A set not judged (what a test consumed could not be read) stays INTERNAL_ERR
// when a hook then quarantines every test: not judged says more than IGNORED,
// and fails the run, so the IGNORED rule must not pre-empt it.
func TestRunTestSet_ANotJudgedSetWithOnlyIgnoredRowsIsNotIgnored(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 1, 0)
	mappedTo(h, sessionTierHTTP("mock-1"))
	h.replayer.hookImpl = unreadQuarantined{}

	status := h.run(t)
	rep := h.report.report
	if rep.Success+rep.Failure+rep.Obsolete != 0 || rep.Ignored != 1 {
		t.Fatalf("precondition: passed %d, failed %d, obsolete %d, ignored %d; want only test-1, quarantined", rep.Success, rep.Failure, rep.Obsolete, rep.Ignored)
	}
	if status != models.TestSetStatusInternalErr || !strings.Contains(rep.FailureReason, "not judged") || !strings.Contains(rep.FailureReason, "consumed") {
		t.Fatalf("set %s (%q); want INTERNAL_ERR, not judged: what test-1 consumed could not be read", status, rep.FailureReason)
	}
}

// --json prints an IGNORED set's report, as it printed it when the set was
// (wrongly) PASSED: a set does not drop out of the output for being IGNORED.
func TestRunTestSet_AnIgnoredSetIsStillPrintedAsJSON(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.JSONOutput = true
	h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-1", "test-2"}}

	out := captureStdout(t, func() { h.run(t) })
	var printed models.TestReport
	at := strings.Index(out, "{")
	if at < 0 {
		t.Fatalf("no JSON report printed:\n%s", out)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out[at:])), &printed); err != nil {
		t.Fatalf("the JSON report printed does not parse: %v\n%s", err, out)
	}
	if printed.TestSet != "test-set-0" || printed.Status != string(models.TestSetStatusIgnored) || printed.Ignored != 2 {
		t.Fatalf("printed %+v; want test-set-0, IGNORED, 2 ignored", printed)
	}
}

func TestEveryTestIgnored(t *testing.T) {
	for _, tc := range []struct {
		status                            models.TestSetStatus
		passed, failed, obsolete, ignored int
		want                              bool
	}{
		{models.TestSetStatusPassed, 0, 0, 0, 2, true},
		{models.TestSetStatusPassed, 1, 0, 0, 2, false},
		{models.TestSetStatusPassed, 0, 0, 1, 2, false},
		{models.TestSetStatusPassed, 0, 0, 0, 0, false}, // scoredNothing's case
		{models.TestSetStatusFailed, 0, 1, 0, 2, false},
		{models.TestSetStatusFaultUserApp, 0, 0, 0, 2, false},
		{models.TestSetStatusInternalErr, 0, 0, 0, 2, false},
		{models.TestSetStatusUserAbort, 0, 0, 0, 2, false},
	} {
		for _, on := range []bool{false, true} {
			setSwitch(t, on)
			want := tc.want && on
			if got := everyTestIgnored(tc.status, tc.passed, tc.failed, tc.obsolete, tc.ignored); got != want {
				t.Errorf("%s: everyTestIgnored(%s, %d, %d, %d, %d) = %v, want %v", switchName(on), tc.status, tc.passed, tc.failed, tc.obsolete, tc.ignored, got, want)
			}
		}
	}
}
