package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
				t.Setenv(provenswitch.Env, "")
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
				t.Setenv(provenswitch.Env, "")
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
