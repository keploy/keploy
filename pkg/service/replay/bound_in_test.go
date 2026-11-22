package replay

import (
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The mismatch store keeps where a variable miss's variables were bound
// (UnmatchedCall.BoundIn) on the report it gives, in a slice of its own.
func TestTheMismatchStoreKeepsWhereAVariableMissWasBound(t *testing.T) {
	s := NewTestFailureStore()
	call := models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "findAndModify entity_versions", MatchPhase: models.MatchPhaseVariable, BoundIn: []string{"mock-2065"}}
	s.AddUnmatchedCallForTest("set-0", "test-1", call)
	got := s.GetFailures()
	if len(got) != 1 || !slices.Equal(got[0].MismatchReport.BoundIn, []string{"mock-2065"}) {
		t.Fatalf("the store's report lost where the miss was bound: %+v", got)
	}
	call.BoundIn[0] = "mock-1"
	if got[0].MismatchReport.BoundIn[0] != "mock-2065" {
		t.Fatalf("the store's report changed with the call: %v", got[0].MismatchReport.BoundIn)
	}
}
