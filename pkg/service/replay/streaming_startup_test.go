package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The streaming path splits one list in two, and the split is the whole point:
// the agent must receive per-test PLUS startup names (so boot mocks stay in the
// pool), while the slice stored on streamingTest must stay per-test ONLY (it
// feeds isMockSubsetWithConfig).
//
// Merging at the build site instead would be a FALSE PASS, not a false
// mismatch: isMockSubsetWithConfig iterates CONSUMED and flags anything absent
// from the expected set, so a wider expected set can only report fewer
// mismatches. A per-test-lifetime mock consumed at boot lands in `startup:`,
// and having it in the expected set would tolerate that unexpected consumption
// — letting a test that should go OBSOLETE pass instead.
func TestStreamingStartupNamesGoToTheAgentNotTheSubsetCheck(t *testing.T) {
	tc := &models.TestCase{Name: "stream-1", Kind: models.HTTP}
	mappings := map[string][]models.MockEntry{
		"stream-1": {{Name: "mock-1"}, {Name: "mock-2"}},
	}
	startup := []string{"boot-0", "boot-1"}

	// What gets stored on the deferred entry — the subset-check side.
	deferred := newStreamingTest(tc, mappings)
	if len(deferred.expectedMocks) != 2 {
		t.Fatalf("expected per-test names only, got %v", deferred.expectedMocks)
	}
	for _, n := range deferred.expectedMocks {
		if n == "boot-0" || n == "boot-1" {
			t.Fatalf("startup names must NOT reach the subset check — that is a false pass: %v",
				deferred.expectedMocks)
		}
	}

	// What gets sent to the agent — the loader side, built at the send site.
	forAgent := models.MergeStartupMockNames(mappings[tc.Name], startup)
	want := []string{"mock-1", "mock-2", "boot-0", "boot-1"}
	if len(forAgent) != len(want) {
		t.Fatalf("agent list = %v, want %v", forAgent, want)
	}
	for i := range want {
		if forAgent[i] != want[i] {
			t.Fatalf("agent list = %v, want %v", forAgent, want)
		}
	}

	// The test case is copied, not aliased — the caller's loop variable is reused.
	if deferred.testCase == tc {
		t.Fatal("streamingTest must hold a copy, not the caller's pointer")
	}
	if deferred.testCase.Name != tc.Name {
		t.Fatalf("copy lost data: %+v", deferred.testCase)
	}
}

// A streaming test with no per-test mapping keeps an empty list, so the agent
// falls through to the wide pool rather than being narrowed to boot mocks.
func TestStreamingTestWithNoMappingStaysEmpty(t *testing.T) {
	tc := &models.TestCase{Name: "stream-2", Kind: models.HTTP}
	deferred := newStreamingTest(tc, map[string][]models.MockEntry{})
	if len(deferred.expectedMocks) != 0 {
		t.Fatalf("got %v", deferred.expectedMocks)
	}
	if n := models.MergeStartupMockNames(nil, []string{"boot-0"}); len(n) != 0 {
		t.Fatalf("agent list must stay empty so the wide-pool fallback applies, got %v", n)
	}
}
