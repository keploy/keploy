package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// connRefusedResult builds a TestResult shaped exactly like CreateFailedTestResult
// produces for a request the app never answered: synthetic status_code 0 plus the
// AppConnectionError category on the TOP-LEVEL FailureInfo.
func connRefusedResult(name string) models.TestResult {
	return models.TestResult{
		Name:   name,
		Status: models.TestStatusFailed,
		FailureInfo: models.FailureInfo{
			Risk:     models.High,
			Category: []models.FailureCategory{models.AppConnectionError},
		},
	}
}

func mockDiffResult(name string) models.TestResult {
	return models.TestResult{
		Name:   name,
		Status: models.TestStatusFailed,
		FailureInfo: models.FailureInfo{
			Risk:     models.High,
			Category: []models.FailureCategory{models.StatusCodeChanged},
		},
	}
}

func passedResult(name string) models.TestResult {
	return models.TestResult{Name: name, Status: models.TestStatusPassed}
}

// TestAnyAppConnectionError verifies detection reads the top-level FailureInfo —
// Result.FailureInfo is tagged json:"-" and is empty after a report-DB round-trip,
// so keying off it would silently never fire.
func TestAnyAppConnectionError(t *testing.T) {
	if !anyAppConnectionError([]models.TestResult{passedResult("a"), connRefusedResult("b")}) {
		t.Fatal("expected AppConnectionError to be detected on the top-level FailureInfo")
	}
	if anyAppConnectionError([]models.TestResult{passedResult("a"), mockDiffResult("b")}) {
		t.Fatal("a genuine mock-diff failure must not be reported as an app connection error")
	}
	if anyAppConnectionError(nil) {
		t.Fatal("no results must not report an app connection error")
	}

	// Category set ONLY on the non-serialized Result.FailureInfo must NOT count:
	// this pins the field-path choice so a refactor can't silently regress it.
	buried := models.TestResult{Name: "x", Status: models.TestStatusFailed}
	buried.Result.FailureInfo.Category = []models.FailureCategory{models.AppConnectionError}
	if anyAppConnectionError([]models.TestResult{buried}) {
		t.Fatal("must read TestResult.FailureInfo, not the json:\"-\" Result.FailureInfo")
	}
}

// TestShouldSkipPruning_AppUnreachableRunNeverPrunes is the regression test for the
// data-loss incident: 9 recorded test cases, every one failing with connection
// refused because the replay pod's app container crash-looped. RemoveUnusedMocks=true
// and PreserveFailedMocks=false (exactly what k8s-proxy auto-replay sets), which
// previously left skipPruning=false — so UpdateMocks ran with an EMPTY keep-set and
// deleted the recorded mocks. A crashed container must never destroy mocks.
func TestShouldSkipPruning_AppUnreachableRunNeverPrunes(t *testing.T) {
	results := make([]models.TestResult, 0, 9)
	for _, n := range []string{"get-users-1", "post-users-1", "get-users-by-id-1", "get-users-by-id-2",
		"get-users-by-id-3", "get-actuator-health-1", "get-api-students-1", "get-employees-1", "get-books-1"} {
		results = append(results, connRefusedResult(n))
	}

	// PreserveFailedMocks=false is the incident configuration.
	if !shouldSkipPruning(0, len(results), 0, false, results) {
		t.Fatal("an all-connection-refused run must skip pruning even with PreserveFailedMocks=false")
	}
	// ...and the guard must not depend on that flag being set.
	if !shouldSkipPruning(0, len(results), 0, true, results) {
		t.Fatal("an all-connection-refused run must skip pruning with PreserveFailedMocks=true too")
	}
}

// TestShouldSkipPruning_ZeroPassingNeverPrunes covers the general zero-signal case:
// no passing test means an empty keep-set, which carries no information about which
// mocks are needed — regardless of WHY the tests failed.
func TestShouldSkipPruning_ZeroPassingNeverPrunes(t *testing.T) {
	allMockDiff := []models.TestResult{mockDiffResult("a"), mockDiffResult("b")}
	if !shouldSkipPruning(0, 2, 0, false, allMockDiff) {
		t.Fatal("zero passing tests must skip pruning: an empty keep-set is 'no information', not 'delete everything'")
	}
	if !shouldSkipPruning(0, 0, 0, false, nil) {
		t.Fatal("a run with no results at all must skip pruning")
	}
}

// TestShouldSkipPruning_PruningStillWorks guards against over-correction: a healthy
// run with real signal must still prune, or RemoveUnusedMocks silently becomes a
// no-op and mocks accumulate forever.
func TestShouldSkipPruning_PruningStillWorks(t *testing.T) {
	// All passing, no failures: the canonical prune case.
	allPass := []models.TestResult{passedResult("a"), passedResult("b")}
	if shouldSkipPruning(2, 0, 0, false, allPass) {
		t.Fatal("a fully passing run must prune unused mocks")
	}
	// Mixed pass/fail with PreserveFailedMocks=false: there IS a keep-set from the
	// passing tests, so pruning is justified (pre-existing behaviour, preserved).
	mixed := []models.TestResult{passedResult("a"), mockDiffResult("b")}
	if shouldSkipPruning(1, 1, 0, false, mixed) {
		t.Fatal("a run with passing tests must still prune when PreserveFailedMocks is false")
	}
	// Same input, PreserveFailedMocks=true: the opt-in safety net still applies.
	if !shouldSkipPruning(1, 1, 0, true, mixed) {
		t.Fatal("PreserveFailedMocks must still skip pruning when a test failed")
	}
	// Obsolete alone must also trip PreserveFailedMocks.
	if !shouldSkipPruning(1, 0, 1, true, allPass) {
		t.Fatal("PreserveFailedMocks must skip pruning when a test is obsolete")
	}
}

// TestShouldSkipPruning_PassingRunWithAConnectionErrorIsStillZeroSignal verifies a
// partially-unreachable run does not prune: if any request never reached the app,
// that test's mocks look "unused" purely because of the transport failure, and
// pruning would delete them.
func TestShouldSkipPruning_PassingRunWithAConnectionErrorIsStillZeroSignal(t *testing.T) {
	mixed := []models.TestResult{passedResult("a"), connRefusedResult("b")}
	if !shouldSkipPruning(1, 1, 0, false, mixed) {
		t.Fatal("a run containing any app-connection failure must not prune, even if other tests passed")
	}
}

// TestShouldPrune_WiresTheGuardIntoTheDecision pins the WIRING, not just the
// predicate.
//
// The data-loss bug is the whole conjunction — a correct shouldSkipPruning that
// is not consulted deletes mocks exactly as before. Testing only the predicate
// leaves that gap: disconnecting it at the call site kept every other test in
// this file green. shouldPrune is the single expression RunTestSet evaluates, so
// asserting on it covers the guard AND its wiring.
func TestShouldPrune_WiresTheGuardIntoTheDecision(t *testing.T) {
	appUnreachable := []models.TestResult{{
		Status:      models.TestStatusFailed,
		FailureInfo: models.FailureInfo{Category: []models.FailureCategory{models.AppConnectionError}},
	}}
	healthy := []models.TestResult{{Status: models.TestStatusPassed}}

	tests := []struct {
		name              string
		removeUnusedMocks bool
		instrument        bool
		success, failure  int
		results           []models.TestResult
		want              bool
		why               string
	}{
		{
			name: "app unreachable must not prune", removeUnusedMocks: true, instrument: true,
			success: 0, failure: 3, results: appUnreachable, want: false,
			why: "the guard must reach the decision: every request failed to connect, so nothing " +
				"vouches for any mock and pruning would destroy the recording over an infra fault",
		},
		{
			name: "healthy passing run still prunes", removeUnusedMocks: true, instrument: true,
			success: 3, failure: 0, results: healthy, want: true,
			why: "RemoveUnusedMocks is a documented feature; the guard must not disable it",
		},
		{
			name: "feature off never prunes", removeUnusedMocks: false, instrument: true,
			success: 3, failure: 0, results: healthy, want: false,
			why: "pruning is opt-in",
		},
		{
			name: "not instrumenting never prunes", removeUnusedMocks: true, instrument: false,
			success: 3, failure: 0, results: healthy, want: false,
			why: "without instrumentation there is no trustworthy consumed-mock set at all",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldPrune(tt.removeUnusedMocks, tt.instrument, tt.success, tt.failure, 0, false, tt.results)
			if got != tt.want {
				t.Errorf("shouldPrune = %v, want %v: %s", got, tt.want, tt.why)
			}
		})
	}
}
