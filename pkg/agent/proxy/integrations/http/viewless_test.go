package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/matcher/learn"
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

// The learn step's reason on a miss whose view went before it was diagnosed
// is told by its kind alone: its code (request_changed, for judging) and
// the code's sentence, never its text, which names the test's mock-own1
// the learn step read in that view, nor its next step, which speaks of that
// recording. A miss whose view stayed keeps both, and a moved learn miss
// keeps telling the move, which names no recording.
func TestAViewlessMissTellsItsLearnReasonByItsKindAlone(t *testing.T) {
	const why = "keploy cannot tell which recordings of the test set came before mock-own1, which has no recorded time, so it cannot tell whether what changed came from one"
	plain := func() *models.MockMismatchReport {
		return mismatch.NewReport(mismatch.ProtocolHTTP, "POST /orders").
			WithDestination("deps.example").
			WithPhase(models.MatchPhaseExhausted, 2).Build()
	}
	miss := &learnMiss{res: learn.Result{Outcome: learn.Miss, Index: -1, Reason: models.MissRequestChanged, Why: why}, steps: placeSteps}
	stayed := plain()
	miss.explain(stayed, false)
	if !strings.Contains(stayed.NextSteps, why) || !strings.Contains(stayed.NextSteps, placeSteps) {
		t.Fatalf("precondition: the miss whose view stayed reads %q; want the reason %q and its next step", stayed.NextSteps, why)
	}
	gone := plain()
	base := gone.NextSteps
	miss.explain(gone, true)
	const kind = "keploy's learn step missed this request. The request changed since it was recorded in a way keploy does not learn (request_changed)."
	if want := strings.TrimSpace(kind + " " + base); gone.Reason != models.MissRequestChanged || gone.NextSteps != want {
		t.Errorf("the viewless miss's learn reason reads code %q, next steps %q; want %q and %q", gone.Reason, gone.NextSteps, models.MissRequestChanged, want)
	}
	moved := &learnMiss{res: learn.Result{Outcome: learn.Miss, Index: -1, Why: "keploy record's full round read this request in test-1, and the replay ended test-1 before keploy could answer it"}, moved: true}
	r := plain()
	moved.explain(r, true)
	if !strings.HasPrefix(r.NextSteps, moved.res.Why+": keploy learns nothing of a call in a round it was not made in") || r.Reason != "" {
		t.Errorf("a moved learn miss on a viewless report reads code %q, next steps %q; want the move told, with no code", r.Reason, r.NextSteps)
	}
}
