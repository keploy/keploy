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
	if r.Write != "" {
		t.Fatalf("a recording of a kind no parser tells a write of is named one: %q", r.Write)
	}
	MarkExtraCall(nil, &models.Mock{Name: "mock-2"}, 1)
	kept := &models.MockMismatchReport{ClosestMock: "mock-5"}
	if MarkExtraCall(kept, nil, 1); kept.MissKind != "" || kept.ClosestMock != "mock-5" {
		t.Fatalf("no recording to name changed the report: %+v", kept)
	}
}

// An extra call says what the recording it repeats writes, by that
// recording's kind's registration (models.CallWriteOf), so judging marks a
// repeated write and sets a repeated read aside; a read says nothing.
func TestAnExtraCallSaysWhatTheRecordingItRepeatsWrites(t *testing.T) {
	kind := models.Kind("extra-call-write-test")
	defer models.RegisterCallWrite(kind, func(m *models.Mock) (string, bool) {
		if m.Spec.Metadata["op"] == "update" {
			return "Test update keys", true
		}
		return "", false
	})()
	write := &models.MockMismatchReport{}
	MarkExtraCall(write, &models.Mock{Name: "mock-4", Kind: kind, Spec: models.MockSpec{Metadata: map[string]string{"op": "update"}}}, 1)
	if write.MissKind != models.MissExtraCall || write.ClosestMock != "mock-4" || write.Write != "Test update keys" {
		t.Fatalf("a repeated write is filed %+v", write)
	}
	read := &models.MockMismatchReport{Write: "stale"}
	MarkExtraCall(read, &models.Mock{Name: "mock-5", Kind: kind, Spec: models.MockSpec{Metadata: map[string]string{"op": "find"}}}, 1)
	if read.MissKind != models.MissExtraCall || read.Write != "" {
		t.Fatalf("a repeated read is filed %+v", read)
	}
}
