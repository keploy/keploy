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

	named := markRepeatOf(results, consumers, nil)
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

// The gap at record is measured from the call's own test's recorded request
// where the replay has it, whether or not the agent knew the test's recorded
// start (exact serving stages no window); the agent's note is not written
// through. With no recorded request the agent's gap, or none, stays. The
// end-of-run rows carry the gap measured, named test or not.
func TestMarkRepeatOfMeasuresTheRecordGapFromTheTest(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	unknown := models.NewRepeatNote("mock-7", rec, rec, rec.Add(70*time.Second), time.Time{})
	results := []models.TestResult{
		{TestCaseID: "test-9", FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{
			{ActualSummary: "GET /count", ClosestMock: "mock-7", Repeat: unknown},
		}}},
		{TestCaseID: "test-12", FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{
			{ActualSummary: "GET /count", ClosestMock: "mock-7", Repeat: models.NewRepeatNote("mock-7", rec, rec, rec.Add(80*time.Second), rec.Add(3*time.Second))},
		}}},
	}
	store := NewTestFailureStore()
	store.AddUnmatchedCallForTest("set-0", "test-9", results[0].FailureInfo.UnmatchedCalls[0])
	tc := func(name string, at time.Time) *models.TestCase {
		return &models.TestCase{Name: name, HTTPReq: models.HTTPReq{Timestamp: at}}
	}
	// test-9 began 40 s after the recording at record, and no other test
	// consumed it (the note stays unnamed); test-12 has no recorded request
	// time.
	markRepeatOf(results, map[string][]string{"mock-7": {"test-9"}}, []*models.TestCase{tc("test-9", rec.Add(40*time.Second)), tc("test-12", time.Time{}), nil})
	got := results[0].FailureInfo.UnmatchedCalls[0]
	if !got.Repeat.RecordGapKnown || got.Repeat.RecordGapSeconds != 40 || !strings.Contains(got.NextSteps, "at record this test began 40.00s after it") {
		t.Fatalf("the gap at record is %+v: %s", got.Repeat, got.NextSteps)
	}
	if likely, known := got.Repeat.ElapsedTimeExpiryLikely(); !likely || !known {
		t.Fatalf("70s at replay after 40s at record is not read as a span that ran out: %v %v", likely, known)
	}
	if unknown.RecordGapKnown {
		t.Fatal("measuring the gap wrote into the note the agent's report shares")
	}
	if n := results[1].FailureInfo.UnmatchedCalls[0].Repeat; !n.RecordGapKnown || n.RecordGapSeconds != 3 {
		t.Fatalf("a test with no recorded request lost the agent's gap: %+v", n)
	}
	store.nameRepeats("set-0", results)
	if row := store.GetFailures()[0].MismatchReport; !row.Repeat.RecordGapKnown || row.Repeat.RecordGapSeconds != 40 {
		t.Fatalf("the end-of-run row's note is %+v", row.Repeat)
	}
}
