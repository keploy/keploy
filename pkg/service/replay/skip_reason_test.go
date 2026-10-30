package replay

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func skipReasonsFor(set string, reasons map[string]string) map[string]map[string]string {
	return map[string]map[string]string{set: reasons}
}

// skipCase is one way a run reaches a skip: the proven switch on or off, and
// whether the run's caller gave the set's tests a reason.
type skipCase struct {
	name     string
	on       bool
	reasoned bool
}

// skipCases: under the switch the gate gives the reasons; with it off only a
// caller that skips for a reason does (a cloud replay skipping a test with no
// mapping entry), and every other run must stay what it was.
var skipCases = []skipCase{
	{name: "switch on", on: true, reasoned: true},
	{name: "switch off, a caller gave reasons", on: false, reasoned: true},
	{name: "switch off, no reason", on: false, reasoned: false},
}

func (c skipCase) setSwitch(t *testing.T) {
	t.Helper()
	if c.on {
		t.Setenv(provenswitch.Env, "1")
	} else {
		t.Setenv(provenswitch.Env, "")
	}
}

// A test set the run ignores whole used to be reported as counts with no rows,
// so neither the report nor JUnit could name the skipped tests or say why.
// Under the switch, or when the run's caller gave a reason for the set's
// tests, each test is one IGNORED row with its reason. With the switch off and
// no reason for the set, the report is what it was: a reason given for another
// set does not change it.
func TestRunTestSet_FullyIgnoredSetHasARowPerTest(t *testing.T) {
	for _, c := range skipCases {
		t.Run(c.name, func(t *testing.T) {
			c.setSwitch(t)
			h := newPartialRunHarness(t, 3, 0)
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {}}
			if c.reasoned {
				h.replayer.config.Test.SkipReasons = skipReasonsFor("test-set-0", map[string]string{
					"test-1": "flaky/replay_only: why one",
					"test-3": "manual: why three",
				})
			} else {
				h.replayer.config.Test.SkipReasons = skipReasonsFor("test-set-1", map[string]string{"test-1": "manual: another set's"})
			}

			if status := h.run(t); status != models.TestSetStatusIgnored {
				t.Fatalf("status = %s, want IGNORED", status)
			}
			rep := h.report.report
			if rep.Total != 3 || rep.Ignored != 3 {
				t.Fatalf("total %d, ignored %d; want 3 and 3", rep.Total, rep.Ignored)
			}
			if !c.on && !c.reasoned {
				if len(rep.Tests) != 0 || len(h.report.results) != 0 {
					t.Fatalf("switch off, no reason: the report grew %d rows (%d stored); it must stay as it was", len(rep.Tests), len(h.report.results))
				}
				return
			}
			want := map[string]string{"test-1": "flaky/replay_only: why one", "test-2": "", "test-3": "manual: why three"}
			if len(rep.Tests) != len(want) || len(h.report.results) != len(want) {
				t.Fatalf("rows %d (stored %d), want %d", len(rep.Tests), len(h.report.results), len(want))
			}
			for _, row := range rep.Tests {
				reason, known := want[row.TestCaseID]
				if !known || row.Status != models.TestStatusIgnored || row.SkipReason != reason || row.Name != "test-set-0" {
					t.Fatalf("row %+v; want IGNORED with reason %q", row, reason)
				}
				delete(want, row.TestCaseID)
			}
		})
	}
}

// The run's own selection decides which tests exist: a test it did not select
// is not reported as skipped.
func TestRunTestSet_FullyIgnoredSetRowsFollowTheSelection(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 3, 0)
	h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {}}
	h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {"test-2"}}

	h.run(t)
	rep := h.report.report
	if rep.Total != 1 || rep.Ignored != 1 || len(rep.Tests) != 1 || rep.Tests[0].TestCaseID != "test-2" {
		t.Fatalf("total %d, ignored %d, rows %+v; want the one selected test", rep.Total, rep.Ignored, rep.Tests)
	}
}

// A test the run skips inside a set that otherwise runs carries the reason its
// caller gave, with the switch on or off, and none when no caller gave one.
func TestRunTestSet_IgnoredTestCarriesItsReason(t *testing.T) {
	for _, c := range skipCases {
		t.Run(c.name, func(t *testing.T) {
			c.setSwitch(t)
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-2"}}
			want := ""
			if c.reasoned {
				want = "manual: why two"
				h.replayer.config.Test.SkipReasons = skipReasonsFor("test-set-0", map[string]string{"test-2": want})
			}

			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status = %s, want PASSED", status)
			}
			found := false
			for _, row := range h.report.report.Tests {
				if row.TestCaseID != "test-2" {
					if row.SkipReason != "" {
						t.Fatalf("a test that ran carries a skip reason: %+v", row)
					}
					continue
				}
				found = true
				if row.Status != models.TestStatusIgnored || row.SkipReason != want {
					t.Fatalf("ignored row %+v; want IGNORED with reason %q", row, want)
				}
			}
			if !found {
				t.Fatal("no row for the ignored test")
			}
		})
	}
}

// A test skipped for a reason is said as it is skipped (at Debug: the run's
// summary is where it is listed), once, with its reason, and listed in the
// run's summary, with the switch on or off. Only under the switch do the line
// and the summary's heading name it. A test skipped with no reason is not
// said, and the summary lists nothing.
func TestRunTestSet_AQuarantinedTestIsSaidAndSummarised(t *testing.T) {
	reason := models.SkipReasonText("flaky", "replay_only", "failed the replay after record\nwith no keploy evidence")
	for _, c := range skipCases {
		t.Run(c.name, func(t *testing.T) {
			c.setSwitch(t)
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-2"}}
			if c.reasoned {
				h.replayer.config.Test.SkipReasons = skipReasonsFor("test-set-0", map[string]string{"test-2": reason})
			}
			core, logs := observer.New(zap.DebugLevel)
			h.replayer.logger = zap.New(core)

			h.run(t)
			h.run(t) // a flaky-check attempt runs the set again: still said once
			said := logs.FilterMessageSnippet("skipping a ").All()
			summary := h.replayer.quarantinedSummary()
			if !c.reasoned {
				if len(said) != 0 || summary != "" {
					t.Fatalf("no reason: said %d times, summary %q; want nothing", len(said), summary)
				}
				return
			}
			line, heading := "skipping a test for a reason", "\tTests skipped by keploy for a reason: 1\n"
			if c.on {
				line, heading = "skipping a quarantined test (KEPLOY_PROVEN=1)", "\tQuarantined tests, skipped by keploy (KEPLOY_PROVEN=1): 1\n"
			}
			if len(said) != 1 || said[0].Message != line || said[0].ContextMap()["testcase"] != "test-2" || said[0].ContextMap()["reason"] != reason {
				t.Fatalf("said %+v; want %q once, for test-2, with its reason", said, line)
			}
			if want := "test-set-0/test-2: flaky/replay_only: failed the replay after record with no keploy evidence\n"; !strings.Contains(summary, heading) || !strings.Contains(summary, want) {
				t.Fatalf("summary %q; want the heading %q and test-2 on one line", summary, heading)
			}
		})
	}
}

// The run's printed summary carries the list, with and without ANSI.
func TestPrintSummaryListsTheQuarantinedTests(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, noANSI := range []bool{false, true} {
		h := newPartialRunHarness(t, 1, 0)
		h.replayer.config.DisableANSI = noANSI
		h.replayer.totalTests = 1
		h.replayer.completeTestReport["test-set-0"] = TestReportVerdict{total: 1, ignored: 1, status: true}
		h.replayer.noteQuarantined("test-set-0", "test-1", "manual: waits on PAY-12")
		out := captureStdout(t, func() { h.replayer.printSummary(t.Context(), false) })
		if !strings.Contains(out, "test-set-0/test-1: manual: waits on PAY-12") {
			t.Fatalf("DisableANSI=%v: the summary does not list the quarantined test:\n%s", noANSI, out)
		}
	}
}

// An IGNORED result is of the test's own kind under the switch, and HTTP with
// it off, as before.
func TestIgnoredKind(t *testing.T) {
	grpcCase := &models.TestCase{Kind: models.GRPC_EXPORT}
	t.Setenv(provenswitch.Env, "")
	if got := ignoredKind(grpcCase); got != models.HTTP {
		t.Fatalf("switch off: %s, want HTTP as before", got)
	}
	t.Setenv(provenswitch.Env, "1")
	if got := ignoredKind(grpcCase); got != models.GRPC_EXPORT {
		t.Fatalf("switch on: %s, want the test's own kind", got)
	}
}
