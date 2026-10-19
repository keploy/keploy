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

// A test set the run ignores whole used to be reported as counts with no rows,
// so neither the report nor JUnit could name the skipped tests or say why.
// Under the switch each test is one IGNORED row with its reason; with the
// switch off the report is what it was.
func TestRunTestSet_FullyIgnoredSetHasARowPerTest(t *testing.T) {
	for _, on := range []bool{false, true} {
		name := "switch off"
		if on {
			name = "switch on"
		}
		t.Run(name, func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "0")
			}
			h := newPartialRunHarness(t, 3, 0)
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {}}
			h.replayer.config.Test.SkipReasons = skipReasonsFor("test-set-0", map[string]string{
				"test-1": "flaky/replay_only: why one",
				"test-3": "manual: why three",
			})

			if status := h.run(t); status != models.TestSetStatusIgnored {
				t.Fatalf("status = %s, want IGNORED", status)
			}
			rep := h.report.report
			if rep.Total != 3 || rep.Ignored != 3 {
				t.Fatalf("total %d, ignored %d; want 3 and 3", rep.Total, rep.Ignored)
			}
			if !on {
				if len(rep.Tests) != 0 || len(h.report.results) != 0 {
					t.Fatalf("switch off: the report grew %d rows (%d stored); it must stay as it was", len(rep.Tests), len(h.report.results))
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

// A test the run skips inside a set that otherwise runs carries its reason
// under the switch, and none with it off.
func TestRunTestSet_IgnoredTestCarriesItsReason(t *testing.T) {
	for _, on := range []bool{false, true} {
		name := "switch off"
		if on {
			name = "switch on"
		}
		t.Run(name, func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "0")
			}
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-2"}}
			h.replayer.config.Test.SkipReasons = skipReasonsFor("test-set-0", map[string]string{"test-2": "manual: why two"})

			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status = %s, want PASSED", status)
			}
			want := ""
			if on {
				want = "manual: why two"
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
// run's summary, under the switch. With it off nothing is
// said and the summary lists nothing.
func TestRunTestSet_AQuarantinedTestIsSaidAndSummarised(t *testing.T) {
	reason := models.SkipReasonText("flaky", "replay_only", "failed the replay after record\nwith no keploy evidence")
	for _, on := range []bool{false, true} {
		name := "switch off"
		if on {
			name = "switch on"
		}
		t.Run(name, func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "0")
			}
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-2"}}
			h.replayer.config.Test.SkipReasons = skipReasonsFor("test-set-0", map[string]string{"test-2": reason})
			core, logs := observer.New(zap.DebugLevel)
			h.replayer.logger = zap.New(core)

			h.run(t)
			h.run(t) // a flaky-check attempt runs the set again: still said once
			said := logs.FilterMessageSnippet("skipping a quarantined test").All()
			summary := h.replayer.quarantinedSummary()
			if !on {
				if len(said) != 0 || summary != "" {
					t.Fatalf("switch off: said %d times, summary %q; want nothing", len(said), summary)
				}
				return
			}
			if len(said) != 1 || said[0].ContextMap()["testcase"] != "test-2" || said[0].ContextMap()["reason"] != reason {
				t.Fatalf("said %+v; want once, for test-2, with its reason", said)
			}
			if want := "test-set-0/test-2: flaky/replay_only: failed the replay after record with no keploy evidence\n"; !strings.Contains(summary, want) || !strings.Contains(summary, ": 1\n") {
				t.Fatalf("summary %q; want it to list test-2 on one line", summary)
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
	t.Setenv(provenswitch.Env, "0")
	if got := ignoredKind(grpcCase); got != models.HTTP {
		t.Fatalf("switch off: %s, want HTTP as before", got)
	}
	t.Setenv(provenswitch.Env, "1")
	if got := ignoredKind(grpcCase); got != models.GRPC_EXPORT {
		t.Fatalf("switch on: %s, want the test's own kind", got)
	}
}
