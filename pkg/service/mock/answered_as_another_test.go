package mock

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A read of the calls no mock answered gives back the calls the agent
// answered with another call's recording apart: no miss, in order, and
// every other entry (a miss, an answer of another kind) stays a miss.
func TestSplitAnsweredAsAnother(t *testing.T) {
	read := []models.UnmatchedCall{
		{ActualSummary: "GET /a"},
		{ActualSummary: "GET /users/3", Answer: models.AnswerOtherID},
		{ActualSummary: "POST /search", Answer: models.AnswerClosestBody},
		{ActualSummary: "INSERT", Answer: models.AnswerByKind},
	}
	misses, asAnother := splitAnsweredAsAnother(read)
	if len(misses) != 2 || misses[0].ActualSummary != "GET /a" || misses[1].Answer != models.AnswerByKind {
		t.Fatalf("misses = %+v, want GET /a and the answer by kind", misses)
	}
	if len(asAnother) != 2 || asAnother[0].ActualSummary != "GET /users/3" || asAnother[1].ActualSummary != "POST /search" {
		t.Fatalf("asAnother = %+v, want GET /users/3 and POST /search", asAnother)
	}
}

// A run that served a call another call's recording is not counted as
// proven, whatever else it served, and its note names each such call whole,
// the first answeredAsAnotherMax of them, and how many more. With none it
// is proven as before.
func TestARunAnsweredWithAnotherCallsRecordingIsNotProven(t *testing.T) {
	call := models.UnmatchedCall{ActualSummary: "GET /users/3", Answer: models.AnswerOtherID, ClosestMock: "mock-1",
		Diff: "GET /users/3 was answered with the recording of GET /users/1"}
	ok, note := isolation(0, models.MissFail, replayCounts{consumed: 1, asAnother: []models.UnmatchedCall{call}}, nil)
	if ok || !strings.Contains(note, "1 call(s) were answered with the recording of another call") || !strings.Contains(note, call.Diff) || strings.Contains(note, "more") {
		t.Fatalf("isolation = (%v, %q), want not proven, naming the call and no more", ok, note)
	}
	if ok, note := isolation(0, models.MissFail, replayCounts{consumed: 1}, nil); !ok || note != "" {
		t.Fatalf("isolation with no such call = (%v, %q), want proven", ok, note)
	}
	many := make([]models.UnmatchedCall, answeredAsAnotherMax+2)
	for i := range many {
		many[i] = models.UnmatchedCall{ActualSummary: "GET /x", Answer: models.AnswerClosestBody, ClosestMock: "mock-x"}
	}
	_, note = isolation(0, models.MissFail, replayCounts{consumed: 1, asAnother: many}, nil)
	if strings.Count(note, "GET /x was answered with recording mock-x") != answeredAsAnotherMax || !strings.Contains(note, "and 2 more") {
		t.Fatalf("the note %q does not name the first %d and count the rest", note, answeredAsAnotherMax)
	}
	got := answeredAsAnotherReceipt([]models.UnmatchedCall{call})
	if len(got) != 1 || got[0].Call != "GET /users/3" || got[0].Mock != "mock-1" || got[0].Answer != models.AnswerOtherID || got[0].Answered != call.Diff {
		t.Fatalf("the receipt's entry is %+v", got)
	}
	if answeredAsAnotherReceipt(nil) != nil {
		t.Fatal("the receipt names calls with none")
	}
}
