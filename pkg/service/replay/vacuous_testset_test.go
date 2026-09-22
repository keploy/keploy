package replay

import (
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A test-set that did not produce an outcome for every test it loaded must not
// be reported as passed. Two shapes: the set that scored nothing (the run that
// prompted the predicate) and the set that stopped part-way with the tests
// that never ran still counted in its total.
//
// LIMIT, stated so nobody reads more into this file than it proves: the report
// path itself is one line, a delegation to applyStoppedEarlyVerdict, and
// nothing here calls RunTestSet (it needs a live app, an agent and a report
// DB). What these tests pin is every decision that line can make: the
// predicate below, and the verdict helper's status and evidence.
func TestTestSetStoppedEarly(t *testing.T) {
	cases := []struct {
		name                                        string
		status                                      models.TestSetStatus
		loaded, success, failure, ignored, obsolete int
		want                                        bool
	}{
		// The zero case from the run that prompted the predicate: nothing at
		// all was verified.
		{"loaded four, scored none", models.TestSetStatusPassed, 4, 0, 0, 0, 0, true},
		// The partial cases: the loop stopped part-way and the tests that
		// never ran stay in the total. This is the "Total: 4, passed 2,
		// failed 0" report the issue is about.
		{"stopped part-way, two of four passed", models.TestSetStatusPassed, 4, 2, 0, 0, 0, true},
		{"stopped part-way, three of four passed", models.TestSetStatusPassed, 4, 3, 0, 0, 0, true},
		{"stopped part-way after a failure was scored", models.TestSetStatusPassed, 4, 1, 1, 0, 0, true},
		{"stopped part-way, some ignored", models.TestSetStatusPassed, 4, 2, 0, 1, 0, true},
		// A complete run: every loaded test produced exactly one outcome.
		{"every loaded test scored", models.TestSetStatusPassed, 4, 3, 1, 0, 0, false},
		{"every test ignored", models.TestSetStatusPassed, 4, 0, 0, 4, 0, false},
		{"every test obsolete", models.TestSetStatusPassed, 4, 0, 0, 0, 4, false},
		{"obsolete alongside a pass", models.TestSetStatusPassed, 4, 1, 0, 0, 3, false},
		{"one of each", models.TestSetStatusPassed, 4, 1, 1, 1, 1, false},
		// Defensive: counters that overshoot are not a partial run.
		{"more outcomes than loaded", models.TestSetStatusPassed, 4, 5, 0, 0, 0, false},
		// An empty set returns earlier as NO_TESTS_TO_RUN; nothing to rescue.
		{"no test cases loaded", models.TestSetStatusPassed, 0, 0, 0, 0, 0, false},
		// Already downgraded: leave the more specific status alone. A user
		// abort lands here as USER_ABORT and must keep that name.
		{"already app-halted", models.TestSetStatusAppHalted, 4, 0, 0, 0, 0, false},
		{"already failed", models.TestSetStatusFailed, 4, 0, 0, 0, 0, false},
		{"already app-fault", models.TestSetStatusFaultUserApp, 4, 0, 0, 0, 0, false},
		{"user abort", models.TestSetStatusUserAbort, 4, 0, 0, 0, 0, false},
		{"still running", models.TestSetStatusRunning, 4, 0, 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := testSetStoppedEarly(tc.status, tc.loaded, tc.success, tc.failure, tc.ignored, tc.obsolete)
			if got != tc.want {
				t.Fatalf("testSetStoppedEarly(%q, loaded=%d, s=%d, f=%d, i=%d, o=%d) = %v, want %v",
					tc.status, tc.loaded, tc.success, tc.failure, tc.ignored, tc.obsolete, got, tc.want)
			}
		})
	}
}

// The verdict the report path delegates to: the status it returns and the
// evidence it logs. Driven directly so neutering either half fails here.
func TestApplyStoppedEarlyVerdict(t *testing.T) {
	verdict := func(status models.TestSetStatus, loaded, success, failure, ignored, obsolete int) (models.TestSetStatus, *observer.ObservedLogs) {
		core, logs := observer.New(zap.ErrorLevel)
		r := &Replayer{logger: zap.New(core)}
		return r.applyStoppedEarlyVerdict("test-set-0", status, loaded, success, failure, ignored, obsolete), logs
	}

	t.Run("a partial run is never published as passed and names the gap", func(t *testing.T) {
		got, logs := verdict(models.TestSetStatusPassed, 4, 2, 0, 0, 0)
		if got != models.TestSetStatusFaultUserApp {
			t.Fatalf("partial run verdict = %q, want %q", got, models.TestSetStatusFaultUserApp)
		}
		if logs.Len() != 1 {
			t.Fatalf("partial run logged %d error records, want exactly 1", logs.Len())
		}
		entry := logs.All()[0]
		if !strings.Contains(entry.Message, "stopped before every loaded test produced a result") {
			t.Fatalf("partial-run message %q does not name the defect", entry.Message)
		}
		fields := entry.ContextMap()
		if fmt.Sprint(fields["test-cases-loaded"]) != "4" || fmt.Sprint(fields["test-cases-scored"]) != "2" {
			t.Fatalf("partial-run evidence = %v, want loaded=4 scored=2", fields)
		}
	})

	t.Run("the scoreless run keeps its own message", func(t *testing.T) {
		got, logs := verdict(models.TestSetStatusPassed, 4, 0, 0, 0, 0)
		if got != models.TestSetStatusFaultUserApp {
			t.Fatalf("scoreless run verdict = %q, want %q", got, models.TestSetStatusFaultUserApp)
		}
		if logs.Len() != 1 || !strings.Contains(logs.All()[0].Message, "no results at all") {
			t.Fatalf("scoreless run records = %v, want one 'no results at all' record", logs.All())
		}
	})

	t.Run("a complete run is left exactly as it was", func(t *testing.T) {
		got, logs := verdict(models.TestSetStatusPassed, 4, 3, 1, 0, 0)
		if got != models.TestSetStatusPassed {
			t.Fatalf("complete run verdict = %q, want PASSED untouched", got)
		}
		if logs.Len() != 0 {
			t.Fatalf("complete run logged %v, want silence", logs.All())
		}
	})

	t.Run("a status that is already specific is not overwritten", func(t *testing.T) {
		got, logs := verdict(models.TestSetStatusAppHalted, 4, 0, 0, 0, 0)
		if got != models.TestSetStatusAppHalted {
			t.Fatalf("app-halted verdict = %q, want APP_HALTED untouched", got)
		}
		if logs.Len() != 0 {
			t.Fatalf("app-halted run logged %v, want silence", logs.All())
		}
	})
}

// The status it downgrades to must carry the app logs and a reason — that is
// the whole reason for choosing APP_FAULT over FAILED.
func TestStoppedEarlyStatusCarriesItsEvidence(t *testing.T) {
	status := models.TestSetStatusFaultUserApp
	if !shouldIncludeAppLogs(status) {
		t.Fatal("the status used for an incomplete run must carry the app logs")
	}
	// A scoreless run has no test results, which is the branch that renders
	// the startup-failure wording.
	if reason := describeTestSetFailure(status, nil); reason == "" {
		t.Fatal("the status used for an incomplete run must carry a failure reason")
	}
	// A partial run has some results, which renders the stopped-mid-replay
	// wording; it must still produce one.
	if reason := describeTestSetFailure(status, []models.TestResult{{}}); reason == "" {
		t.Fatal("the status used for a partial run must carry a failure reason when tests did score")
	}
}
