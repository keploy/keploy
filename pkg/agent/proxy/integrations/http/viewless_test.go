package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
)

// A miss whose view a test's begin or end replaced before keploy finished
// diagnosing it (viewless) keeps nothing it read of a recording: the report
// had named its closest, mock-own1, with a value-drift diff, a rendering of
// that recording's request, the diff's field reasons and a destination
// scope its compared set decided (none of it targets the call's
// destination). It keeps none of them, and its next steps are those of a
// miss compared with no recording at its phase, not the hint its compared
// set gave. The call's own summary and rendering stay.
func TestAViewlessMissKeepsNothingItReadOfARecording(t *testing.T) {
	r := mismatch.NewReport(mismatch.ProtocolHTTP, "GET /orders").
		WithDestination("deps.example").
		WithComparedDestinations([]string{"other.example"}).
		WithPhase(models.MatchPhaseExhausted, 2).
		WithClosest("mock-own1", []models.MockFieldDiff{{Path: "query.page", Expected: "1", Actual: "2", Kind: models.DiffKindValueChanged}}).
		WithRenderedRequests("GET /orders?page=1", "GET /orders?page=2").Build()
	r.Differs, r.FieldReasons = []string{"query.page"}, []models.FieldReason{{Field: "query.page", Why: "drifted"}}
	plain := mismatch.NewReport(mismatch.ProtocolHTTP, "GET /orders").
		WithDestination("deps.example").
		WithPhase(models.MatchPhaseExhausted, 2).Build()
	if r.NextSteps == plain.NextSteps || r.DestinationScope == plain.DestinationScope || r.Diff == "" {
		t.Fatalf("precondition: the report's next steps %q and scope %q are a plain miss's (%q, %q), or it has no diff", r.NextSteps, r.DestinationScope, plain.NextSteps, plain.DestinationScope)
	}
	viewless(r)
	if r.ClosestMock != "" || r.ClosestMockReq != "" || r.Diff != "" || r.FieldDiffs != nil || r.Differs != nil || r.FieldReasons != nil {
		t.Errorf("the viewless miss keeps closest %q, its rendering %q, diff %q, fields %+v, differs %v, reasons %+v; want none", r.ClosestMock, r.ClosestMockReq, r.Diff, r.FieldDiffs, r.Differs, r.FieldReasons)
	}
	if r.NextSteps != plain.NextSteps || r.DestinationScope != plain.DestinationScope {
		t.Errorf("the viewless miss's next steps %q and scope %q; want a plain miss's %q and %q", r.NextSteps, r.DestinationScope, plain.NextSteps, plain.DestinationScope)
	}
	if r.ActualSummary != "GET /orders" || r.ReceivedReq != "GET /orders?page=2" || r.MatchPhase != models.MatchPhaseExhausted || r.CandidateCount != 2 {
		t.Errorf("the viewless miss lost the call's own summary %q, rendering %q, phase %q or count %d", r.ActualSummary, r.ReceivedReq, r.MatchPhase, r.CandidateCount)
	}
}
