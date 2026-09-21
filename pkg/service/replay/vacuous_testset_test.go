package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A test-set that loaded cases and produced no outcome for any of them must not
// be reported as passed. See scoredNothing for the run that prompted this.
//
// LIMIT, stated so nobody reads more into this file than it proves: these pin
// the PREDICATE, not the call. Nothing here calls RunTestSet — it needs a live
// app, an agent and a report DB — so deleting the `if scoredNothing(...)` block
// in the report path would keep this file green. The predicate is where the
// judgement lives; the call site is one line, reviewed by eye.
func TestScoredNothing(t *testing.T) {
	cases := []struct {
		name                                        string
		status                                      models.TestSetStatus
		loaded, success, failure, ignored, obsolete int
		want                                        bool
	}{
		{"loaded four, scored none", models.TestSetStatusPassed, 4, 0, 0, 0, 0, true},
		{"one passed", models.TestSetStatusPassed, 4, 1, 0, 0, 0, false},
		{"one failed", models.TestSetStatusPassed, 4, 0, 1, 0, 0, false},
		// Ignoring every test is a deliberate choice, not a lost run.
		{"every test ignored", models.TestSetStatusPassed, 4, 0, 0, 4, 0, false},
		// Obsolete means the test RAN and answered; the mocks drifted. Calling
		// that an app fault aborts the remaining test-sets on native/docker-run.
		{"every test obsolete", models.TestSetStatusPassed, 4, 0, 0, 0, 4, false},
		{"obsolete alongside a pass", models.TestSetStatusPassed, 4, 1, 0, 0, 3, false},
		{"one of each", models.TestSetStatusPassed, 4, 1, 1, 1, 1, false},
		// An empty set returns earlier as NO_TESTS_TO_RUN; nothing to rescue.
		{"no test cases loaded", models.TestSetStatusPassed, 0, 0, 0, 0, 0, false},
		// Already downgraded: leave the more specific status alone. A user abort
		// lands here as USER_ABORT and must keep that name.
		{"already app-halted", models.TestSetStatusAppHalted, 4, 0, 0, 0, 0, false},
		{"already failed", models.TestSetStatusFailed, 4, 0, 0, 0, 0, false},
		{"already app-fault", models.TestSetStatusFaultUserApp, 4, 0, 0, 0, 0, false},
		{"user abort", models.TestSetStatusUserAbort, 4, 0, 0, 0, 0, false},
		{"still running", models.TestSetStatusRunning, 4, 0, 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scoredNothing(tc.status, tc.loaded, tc.success, tc.failure, tc.ignored, tc.obsolete)
			if got != tc.want {
				t.Fatalf("scoredNothing(%q, loaded=%d, s=%d, f=%d, i=%d, o=%d) = %v, want %v",
					tc.status, tc.loaded, tc.success, tc.failure, tc.ignored, tc.obsolete, got, tc.want)
			}
		})
	}
}

// The status it downgrades to must carry the app logs and a reason — that is
// the whole reason for choosing APP_FAULT over FAILED.
func TestVacuousRunStatusCarriesItsEvidence(t *testing.T) {
	status := models.TestSetStatusFaultUserApp
	if !shouldIncludeAppLogs(status) {
		t.Fatal("the status used for a scoreless run must carry the app logs")
	}
	// A scoreless run has no test results, which is the branch that renders the
	// startup-failure wording.
	if reason := describeTestSetFailure(status, nil); reason == "" {
		t.Fatal("the status used for a scoreless run must carry a failure reason")
	}
}
