package integrations

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The recording a call made once more repeats is the test's copy of it
// recorded last: by the request's timestamp, then by name at one timestamp,
// whatever order the copies come in.
func TestTheRecordingAnExtraCallRepeatsIsTheCopyRecordedLast(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	mk := func(name string, recorded time.Time) *models.Mock {
		m := &models.Mock{Name: name}
		m.Spec.ReqTimestampMock = recorded
		return m
	}
	a, b, c := mk("mock-9", at), mk("mock-3", at.Add(time.Second)), mk("mock-4", at.Add(time.Second))
	for _, copies := range [][]*models.Mock{{a, b, c}, {c, b, a}, {b, nil, c, a}} {
		if got := RepeatedRecording(copies); got != c {
			t.Fatalf("of %v the last recorded is mock-4 (mock-3's timestamp, a later name), got %v", copies, got)
		}
	}
	if got := RepeatedRecording([]*models.Mock{a}); got != a {
		t.Fatalf("one copy: got %v", got)
	}
	if got := RepeatedRecording(nil); got != nil {
		t.Fatalf("no copy: got %v", got)
	}
}

// MarkExtraCall files the miss as one call more than recorded and names the
// recording it repeats, dropping what the report said of another recording.
func TestAnExtraCallsReportNamesOnlyTheRecordingItRepeats(t *testing.T) {
	r := &models.MockMismatchReport{ClosestMock: "mock-5", Diff: "mock-5 differs", ClosestMockReq: "GET /all",
		FieldDiffs: []models.MockFieldDiff{{Path: "url.path"}}, Differs: []string{"url.path"}, NextSteps: "declare url.path as noise"}
	MarkExtraCall(r, &models.Mock{Name: "mock-2"}, 2)
	if r.MissKind != models.MissExtraCall || r.Recorded != 2 || r.ClosestMock != "mock-2" || len(r.FieldDiffs)+len(r.Differs) != 0 || r.ClosestMockReq != "" ||
		strings.Contains(r.Diff+r.NextSteps, "mock-5") || strings.Contains(r.NextSteps, "declare") || !strings.Contains(r.NextSteps, "2 time(s)") {
		t.Fatalf("got %+v", r)
	}
	MarkExtraCall(nil, &models.Mock{Name: "mock-2"}, 1)
	kept := &models.MockMismatchReport{ClosestMock: "mock-5"}
	if MarkExtraCall(kept, nil, 1); kept.MissKind != "" || kept.ClosestMock != "mock-5" {
		t.Fatalf("no recording to name changed the report: %+v", kept)
	}
}
