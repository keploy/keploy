package replay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A test set that stops part-way is not a pass. scoredNothing catches only the
// all-or-nothing case — with at least one test scored its four-zero predicate is
// false — and because a failure takes the `default` arm of the scoring switch,
// an all-passing or all-obsolete partial run kept its PASSED status. The report
// then showed the gap and still called it green (`Total tests: 4, passed 2,
// failed 0`), so the natural reading was that two tests were skipped for some
// legitimate reason (#4618).
func TestCycleIncomplete(t *testing.T) {
	cases := []struct {
		name                                string
		success, failure, obsolete, skipped int
		intended                            int
		incomplete                          bool
	}{
		{name: "every intended test passed", success: 4, intended: 4},
		{name: "every intended test produced some verdict", success: 2, failure: 1, obsolete: 1, intended: 4},
		{name: "all failed is still complete", failure: 4, intended: 4},
		{name: "all obsolete is still complete", obsolete: 4, intended: 4},
		{
			name: "stopped after two of four — the defect", success: 2, intended: 4, incomplete: true,
		},
		{
			// A shortfall of ONE. Without this an off-by-one in the comparison
			// goes unnoticed, and a run that drops its last test still reports
			// green — the quietest form of the bug.
			name: "stopped after three of four", success: 3, intended: 4, incomplete: true,
		},
		{
			name: "three of four, mixed verdicts", success: 1, failure: 1, obsolete: 1, intended: 4, incomplete: true,
		},
		{
			name:     "stopped before any ran — scoredNothing's case, also incomplete",
			intended: 4, incomplete: true,
		},
		{
			// A retry cycle runs only the previously-passing tests, so `intended`
			// shrinks with it. Comparing against the LOADED count instead would
			// call every retry cycle incomplete.
			name: "a retry cycle that completes its smaller set", success: 2, intended: 2,
		},
		{name: "nothing intended is not incomplete", intended: 0},

		// A test the loop deliberately passes over records no verdict, and that
		// is not a fault. Reading it as one would mark a healthy set APP_FAULT,
		// which aborts every REMAINING test-set on the native and docker-run
		// paths (shouldAbortTestRun) — strictly worse than the bug being fixed.
		{name: "a deliberately skipped test is not a shortfall", success: 3, skipped: 1, intended: 4},
		{name: "every test skipped is not a shortfall", skipped: 4, intended: 4},
		{
			// Skips must not mask a real shortfall either: one skipped, one
			// scored, two never reached.
			name: "a skip alongside a genuine shortfall", success: 1, skipped: 1, intended: 4, incomplete: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cycleIncomplete(tc.success, tc.failure, tc.obsolete, tc.skipped, tc.intended)
			if got != tc.incomplete {
				t.Fatalf("cycleIncomplete(s=%d,f=%d,o=%d,skip=%d, intended=%d) = %v; want %v",
					tc.success, tc.failure, tc.obsolete, tc.skipped, tc.intended, got, tc.incomplete)
			}
		})
	}
}

// The retry rewind must also mark the AGENT's history stale. The rewind it
// performs reaches the CLI's map only; under KEPLOY_AGENT_OWNS_CONSUMED the
// agent filters from its own, which nothing rewinds, so cycle 2 would be
// filtered against cycle 1 and every single-use mock the retry needs would
// already be marked consumed (#4622).
func TestRewindConsumedForRetryCycleMarksAgentHistoryStale(t *testing.T) {
	r := &Replayer{}
	baseline := map[string]models.MockState{"mock-1": {Name: "mock-1"}}
	total := map[string]models.MockState{
		"mock-1": {Name: "mock-1"},
		"mock-2": {Name: "mock-2", Usage: models.Deleted}, // consumed during cycle 1
	}
	across := map[string]models.MockState{}

	rewound := r.rewindConsumedForRetryCycle(total, baseline, across)

	if !r.agentHistoryStaleForSet.Load() {
		t.Fatal("the retry rewind did not mark the agent's history stale; cycle 2 would be filtered " +
			"against cycle 1 and fail with match_phase=no_mocks")
	}
	if _, stillConsumed := rewound["mock-2"]; stillConsumed {
		t.Fatal("the cycle's own consumption survived the rewind; single-use mocks would not be servable again")
	}
	if _, kept := rewound["mock-1"]; !kept {
		t.Fatal("the baseline was not restored")
	}
	if _, folded := across["mock-2"]; !folded {
		t.Fatal("the finished cycle's consumption was not folded into the across-cycles record; the " +
			"post-loop telemetry and PersistMockNoise readers would be incomplete")
	}
}

// A run that stopped early for a reason that ALREADY has an honest diagnosis
// must keep it. `stoppedEarly` is true for every early exit, including an app
// that crashed mid-run — so keying the reason string off it deleted the correct
// message for the most common way APP_HALTED is reached. The reason must key
// off whether the partial-run downgrade actually fired.
func TestAppHaltedKeepsItsOwnFailureReason(t *testing.T) {
	results := []models.TestResult{{Name: "test-1"}, {Name: "test-2"}}

	got := describeTestSetFailure(models.TestSetStatusAppHalted, results, runShape{})
	if !strings.Contains(got, "application stopped during replay") {
		t.Fatalf("an app that crashed mid-run reports %q; it must keep its own diagnosis rather than "+
			"being told to check keploy's logs", got)
	}

	// And a set that never produced a result keeps the startup wording.
	got = describeTestSetFailure(models.TestSetStatusAppHalted, nil, runShape{})
	if !strings.Contains(got, "application startup failed") {
		t.Fatalf("a set with no results reports %q; want the startup-failure wording", got)
	}

	// The partial-run wording is reserved for the downgrade itself.
	got = describeTestSetFailure(models.TestSetStatusFaultUserApp, results, runShape{stoppedEarly: true, downgradedFromPassed: true})
	if !strings.Contains(got, "not") || !strings.Contains(got, "verified") {
		t.Fatalf("the partial-run downgrade reports %q; it must say the missing tests went unverified", got)
	}
	if strings.Contains(got, "application stopped during replay") {
		t.Fatalf("the partial-run downgrade blames the application: %q", got)
	}
}

// A set that both FAILED a test and stopped early must say the second half too.
// The report read `Total 4, passed 0, failed 1` with an empty reason, so a
// reader concluded the other three were skipped on purpose — the #4618
// misreading, on a red set instead of a green one.
func TestFailedAndPartialSaysTheRestWereUnverified(t *testing.T) {
	results := []models.TestResult{{Name: "test-1"}}

	got := describeTestSetFailure(models.TestSetStatusFailed, results, runShape{stoppedEarly: true})
	if got == "" {
		t.Fatal("a set that failed a test AND stopped early reports no reason at all; the tests it " +
			"never reached read as deliberately skipped")
	}
	if !strings.Contains(got, "not") || !strings.Contains(got, "verified") {
		t.Fatalf("reason %q does not say the missing tests went unverified", got)
	}

	// A set that failed and ran everything keeps its silence: the per-test
	// failures are the report, and a note here would be noise on every red run.
	if got := describeTestSetFailure(models.TestSetStatusFailed, results, runShape{}); got != "" {
		t.Fatalf("a complete failing run gained a reason string: %q", got)
	}
}

// An app that crashed mid-set has BOTH facts, and the reader needs both: the
// app stopped, and the remaining tests were never verified. Keeping only the
// first is what the app-crash guard pins; keeping only the second is what E1
// was. This pins that neither is dropped.
func TestAppCrashReasonCarriesBothFacts(t *testing.T) {
	results := []models.TestResult{{Name: "test-1"}}

	got := describeTestSetFailure(models.TestSetStatusAppHalted, results, runShape{stoppedEarly: true})
	if !strings.Contains(got, "application stopped during replay") {
		t.Fatalf("the application diagnosis was dropped: %q", got)
	}
	if !strings.Contains(got, "unverified") {
		t.Fatalf("the reason does not mention the tests that never ran: %q", got)
	}
}

// The two causes of an untrustworthy agent history have different lifetimes,
// and conflating them is what made one retry in test-set 0 disable
// KEPLOY_AGENT_OWNS_CONSUMED for every remaining set.
func TestAgentHistoryLatchesHaveSeparateLifetimes(t *testing.T) {
	t.Run("a retry rewind is cleared by the next set's MockOutgoing", func(t *testing.T) {
		r := &Replayer{logger: zap.NewNop(), instrumentation: &latchInstr{}}
		r.rewindConsumedForRetryCycle(
			map[string]models.MockState{}, map[string]models.MockState{}, map[string]models.MockState{})

		if !r.agentConsumedHistoryUnusable() {
			t.Fatal("the rewind did not disqualify the agent's history; cycle 2 would be filtered " +
				"against cycle 1 and fail with match_phase=no_mocks")
		}
		if err := r.mockOutgoingForTestSet(context.Background(), models.OutgoingOptions{}); err != nil {
			t.Fatalf("MockOutgoing: %v", err)
		}
		if r.agentConsumedHistoryUnusable() {
			t.Fatal("the retry-rewind staleness survived the test-set boundary; the agent wipes its " +
				"per-name history in ResetForReplaySession there, so there is nothing left to be stale")
		}
	})

	t.Run("a replaced agent stays disqualified across the boundary", func(t *testing.T) {
		r := &Replayer{logger: zap.NewNop(), instrumentation: &latchInstr{}}
		r.agentHistoryIncompleteForRun.Store(true)

		if err := r.mockOutgoingForTestSet(context.Background(), models.OutgoingOptions{}); err != nil {
			t.Fatalf("MockOutgoing: %v", err)
		}
		if !r.agentConsumedHistoryUnusable() {
			t.Fatal("a replacement agent's missing history was cleared at a set boundary; what it lost " +
				"is gone for the run, so the CLI's map stays the only complete record")
		}
	})

	t.Run("a failed MockOutgoing clears nothing", func(t *testing.T) {
		r := &Replayer{logger: zap.NewNop(), instrumentation: &latchInstr{err: errors.New("agent refused")}}
		r.agentHistoryStaleForSet.Store(true)

		if err := r.mockOutgoingForTestSet(context.Background(), models.OutgoingOptions{}); err == nil {
			t.Fatal("expected the error through")
		}
		if !r.agentConsumedHistoryUnusable() {
			t.Fatal("staleness was dropped although the agent never answered — the clear must follow " +
				"the round trip that wipes the agent's history, not merely the attempt")
		}
	})
}

// latchInstr answers only MockOutgoing; anything else panics on the nil
// embedded interface rather than quietly returning a zero value.
type latchInstr struct {
	Instrumentation
	err error
}

func (f *latchInstr) MockOutgoing(context.Context, models.OutgoingOptions) error { return f.err }
