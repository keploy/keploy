package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A connection whose client sent nothing (models.MatchPhaseNoRequest) is
// reported for its next step but made no call, so it fails no test by itself:
// a dependency's TCP health check closes the same way.
func TestAConnectionThatSentNothingIsNoDataPlaneMiss(t *testing.T) {
	calls := []models.UnmatchedCall{
		{Protocol: "MySQL", MatchPhase: models.MatchPhaseNoRequest},
		{Protocol: "DNS"},
	}
	if n := dataPlaneMisses(calls); n != 0 {
		t.Fatalf("dataPlaneMisses = %d, want 0: neither a DNS lookup nor a connection that sent nothing is a call", n)
	}
	if got := dataPlaneCalls(calls); len(got) != 0 {
		t.Fatalf("dataPlaneCalls = %v, want none", got)
	}
	calls = append(calls, models.UnmatchedCall{Protocol: "MySQL", MatchPhase: models.MatchPhaseExhausted})
	if n := dataPlaneMisses(calls); n != 1 {
		t.Fatalf("dataPlaneMisses = %d, want 1: a MySQL call nothing answered is a miss", n)
	}
}
