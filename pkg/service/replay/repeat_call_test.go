package replay

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A repeated call is named with the first other test that consumed its
// recording, never its own test, and its next step is written again with that
// test; a recording no other test consumed stays unnamed, and a call with no
// note is left as it was. The end-of-run rows say the same.
func TestMarkRepeatOfNamesTheEarlierTest(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	note := func(mock string) *models.RepeatNote {
		return models.NewRepeatNote(mock, rec, rec, rec.Add(6*time.Second), rec.Add(2*time.Second))
	}
	results := []models.TestResult{
		{TestCaseID: "test-9", FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{
			{ActualSummary: "GET /count", ClosestMock: "mock-7", Repeat: note("mock-7")},
			{ActualSummary: "GET /alone", ClosestMock: "mock-8", Repeat: note("mock-8")},
			{ActualSummary: "GET /other", NextSteps: "as it was"},
		}}},
	}
	consumers := map[string][]string{"mock-7": {"test-9", "test-3", "test-5"}, "mock-8": {"test-9"}}
	store := NewTestFailureStore()
	for _, c := range results[0].FailureInfo.UnmatchedCalls {
		store.AddUnmatchedCallForTest("set-0", "test-9", c)
	}
	keep := results[0].FailureInfo.UnmatchedCalls[0].Repeat

	named := markRepeatOf(results, consumers)
	calls := results[0].FailureInfo.UnmatchedCalls
	if calls[0].Repeat.Test != "test-3" || !strings.Contains(calls[0].NextSteps, "test test-3's call") {
		t.Fatalf("the repeat is named %q: %s", calls[0].Repeat.Test, calls[0].NextSteps)
	}
	if keep.Test != "" {
		t.Fatal("naming the test wrote into the note the agent's report shares")
	}
	if calls[1].Repeat.Test != "" || !strings.Contains(calls[1].NextSteps, "an earlier test of this set") {
		t.Fatalf("a recording only its own test consumed was named %q", calls[1].Repeat.Test)
	}
	if calls[2].NextSteps != "as it was" {
		t.Fatalf("a call with no note was rewritten: %q", calls[2].NextSteps)
	}
	if len(named) != 2 || !strings.Contains(named[0], "test-9: GET /count repeats mock-7, answered for test-3 6.00s earlier at replay, 2.00s apart at record") {
		t.Fatalf("the set log lines are %q", named)
	}

	store.nameRepeats("set-0", results)
	rows := store.GetFailures()
	if rows[0].MismatchReport.Repeat.Test != "test-3" || !strings.Contains(rows[0].MismatchReport.NextSteps, "test-3") {
		t.Fatalf("the end-of-run row says %+v", rows[0].MismatchReport.Repeat)
	}
	if rows[1].MismatchReport.Repeat.Test != "" || rows[2].MismatchReport.NextSteps != "as it was" {
		t.Fatalf("rows that name no earlier test changed: %+v, %q", rows[1].MismatchReport.Repeat, rows[2].MismatchReport.NextSteps)
	}
}
