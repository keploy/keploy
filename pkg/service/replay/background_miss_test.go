package replay

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Under the proven switch, a test whose only misses are background misses
// (the same request as one of the set's background recordings) passes; one
// of its own misses beside them still fails it. A background miss made before
// the set's first test fails no set either.
func TestABackgroundMissFailsNoTestAndNoSet(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	poll := models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "findAndModify ptw_endpoints", MatchPhase: models.MatchPhaseBody,
		Kind: models.MissExtraCall, Recorded: 1, Background: true}
	own := models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "find users", MatchPhase: models.MatchPhaseBody, Kind: models.MissInTest}
	results := []models.TestResult{
		{TestCaseID: "only-background", Status: models.TestStatusPassed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{poll, poll}}},
		{TestCaseID: "own-miss", Status: models.TestStatusPassed, FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{poll, own}}},
	}
	if failed := failTestsWithMisses(results); !reflect.DeepEqual(failed, []string{"own-miss"}) {
		t.Fatalf("failTestsWithMisses failed %v; want only the test with its own miss", failed)
	}
	if results[0].Status != models.TestStatusPassed || results[1].Status != models.TestStatusFailed {
		t.Fatalf("statuses %s, %s; want PASSED, FAILED", results[0].Status, results[1].Status)
	}
	if n := len(results[0].FailureInfo.UnmatchedCalls); n != 2 {
		t.Fatalf("the passing test lists %d background misses; want both kept, listed", n)
	}

	var set setCalls
	before := poll
	before.Kind, before.Recorded, before.BeforeSet = models.MissOutsideTest, 0, true
	ownBefore := own
	ownBefore.Kind, ownBefore.BeforeSet = models.MissOutsideTest, true
	if rest := set.take([]models.UnmatchedCall{before}); len(rest) != 0 {
		t.Fatalf("the set's background call stayed with the test: %+v", rest)
	}
	if f := set.failing(); len(f) != 0 {
		t.Fatalf("a background call before the set's first test fails the set: %+v", f)
	}
	set.take([]models.UnmatchedCall{ownBefore})
	if f := set.failing(); len(f) != 1 || f[0].Background {
		t.Fatalf("the set's own call before its first test: failing %+v; want it alone", f)
	}
}

// The run's mismatch store keeps the mark of a miss the agent read, so a
// green run's table leaves a background miss out as it leaves out every miss
// that counts against no test.
func TestTheMismatchStoreKeepsTheBackgroundMark(t *testing.T) {
	s := NewTestFailureStore()
	s.AddUnmatchedCallForTest("set-0", "test-1", models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "findAndModify ptw_endpoints", Background: true})
	s.AddUnmatchedCallForTest("set-0", "test-1", models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "find users"})
	got := s.GetFailures()
	if len(got) != 2 || !got[0].MismatchReport.Background || got[0].MismatchReport.CountsAgainstTest() ||
		got[1].MismatchReport.Background || !got[1].MismatchReport.CountsAgainstTest() {
		t.Fatalf("the store's reports lost or invented the mark: %+v, %+v", got[0].MismatchReport, got[1].MismatchReport)
	}
}
