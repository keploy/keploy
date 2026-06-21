package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// Regression for the missing-mock bug: auto-replay's prune (UpdateMocks) keeps
// only passing-test consumed mocks (+ config/startup), so a FAILED test's mapped
// mocks were deleted while its mapping was still written (UpdateTestMapping=true)
// → the mapping dangles → those mocks are missing on every later replay (both
// the legacy and smart-set cloud-replay paths fail). Confirmed on the real
// staging recording (test-set ...vhncm: 7 mongo mocks on the first test, mapped
// but absent from mocks.json).
//
// The fix keys mock-preservation on "did the test PASS" instead of "was it
// EXECUTED". This test shows both behaviours through the same helper:
//   - skip-set = every EXECUTED test (old)  → a failed test's mocks are DROPPED
//   - skip-set = PASSED tests only   (fix)  → a failed test's mocks are KEPT
func TestRetainNonPassingTestMocks_FailedTestMocksSurvivePrune(t *testing.T) {
	expected := map[string][]models.MockEntry{
		"test-failed": {{Name: "mock-104", Kind: "Mongo"}, {Name: "mock-105", Kind: "Mongo"}},
		"test-passed": {{Name: "mock-200", Kind: "Mongo"}},
		"test-notrun": {{Name: "mock-300", Kind: "Http"}},
	}

	// OLD behaviour (the bug): preserve only NOT-EXECUTED tests, i.e. skip every
	// executed test — passed AND failed. The failed test's mocks are not kept,
	// so the prune deletes them while the mapping still references them.
	oldSkip := map[string]bool{"test-passed": true, "test-failed": true}
	keepOld := map[string]models.MockState{"mock-200": {Name: "mock-200"}}
	retainNonPassingTestMocks(expected, oldSkip, keepOld)
	if _, ok := keepOld["mock-104"]; ok {
		t.Fatal("sanity: under the OLD executed-based behaviour, a failed test's mock should NOT be preserved")
	}

	// NEW behaviour (the fix): preserve every NON-PASSING test, i.e. skip only
	// PASSED tests. The failed and not-run tests' mapped mocks survive the prune.
	newSkip := map[string]bool{"test-passed": true}
	keepNew := map[string]models.MockState{"mock-200": {Name: "mock-200"}}
	added := retainNonPassingTestMocks(expected, newSkip, keepNew)

	for _, n := range []string{"mock-104", "mock-105", "mock-300"} {
		if _, ok := keepNew[n]; !ok {
			t.Errorf("FIX: non-passing test's mapped mock %q must be preserved from pruning so its mapping doesn't dangle, but it was dropped", n)
		}
	}
	if _, ok := keepNew["mock-200"]; !ok {
		t.Error("a passed test's already-kept mock must remain in the keep-set")
	}
	if added != 3 {
		t.Errorf("expected 3 newly-preserved mocks (mock-104/105/300), got %d", added)
	}
}
