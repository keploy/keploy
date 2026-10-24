package models

import (
	"strings"
	"testing"
	"time"
)

// The note's gaps come from the times it is given: at replay from the earlier
// answer to this call, at record from the earlier recording to the start of
// this call's test, to the hundredth of a second and never below zero; the
// record gap is unknown without the test's recorded start.
func TestRepeatNoteGapsFromAControlledTimeline(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	answered := rec.Add(time.Hour)
	n := NewRepeatNote("mock-7", answered, rec, answered.Add(6213*time.Millisecond), rec.Add(3050*time.Millisecond))
	if n.Mock != "mock-7" || n.ReplayGapSeconds != 6.21 || n.RecordGapSeconds != 3.05 || !n.RecordGapKnown {
		t.Fatalf("note %+v, want 6.21s at replay and 3.05s at record", n)
	}
	if likely, known := n.ElapsedTimeExpiryLikely(); !likely || !known {
		t.Fatalf("a replay gap longer than the record gap is not an expiry: likely %v, known %v", likely, known)
	}
	if n := NewRepeatNote("m", answered, rec, answered.Add(-time.Millisecond), time.Time{}); n.ReplayGapSeconds != 0 || n.RecordGapKnown {
		t.Fatalf("a call a millisecond before its twin's answer, with no recorded start, gave %+v", n)
	}
	if _, known := NewRepeatNote("m", answered, rec, answered, time.Time{}).ElapsedTimeExpiryLikely(); known {
		t.Fatal("an unknown record gap was judged")
	}
	if likely, known := NewRepeatNote("m", answered, rec, answered.Add(time.Second), rec.Add(2*time.Second)).ElapsedTimeExpiryLikely(); likely || !known {
		t.Fatalf("a replay gap shorter than the record gap was judged likely %v (known %v)", likely, known)
	}
}

// The sentence names the recording, the test once it is named, both gaps,
// the cause that the gaps support, and the remedies; the cause is never
// claimed as certain.
func TestRepeatNoteExplains(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	expired := NewRepeatNote("mock-7", rec, rec, rec.Add(6210*time.Millisecond), rec.Add(3050*time.Millisecond))
	expired.Test = "get-count-3"
	got := expired.Explain("keploy record")
	for _, want := range []string{"identical to mock-7", "test get-count-3's call", "came 6.21s after", "began 3.05s after", "likely kept the earlier answer for a span of elapsed time", "'keploy record'", "could hide a bug"} {
		if !strings.Contains(got, want) {
			t.Errorf("the note of an expiry lacks %q: %s", want, got)
		}
	}
	unnamed := NewRepeatNote("mock-7", rec, rec, rec.Add(time.Second), time.Time{}).Explain("keploy record")
	if !strings.Contains(unnamed, "an earlier test of this set") || strings.Contains(unnamed, "at record this test began") || !strings.Contains(unnamed, "If the application keeps that answer") {
		t.Errorf("the note with no test and no record gap: %s", unnamed)
	}
	faster := NewRepeatNote("mock-7", rec, rec, rec.Add(time.Second), rec.Add(2*time.Second)).Explain("keploy record")
	if !strings.Contains(faster, "less likely cause") || strings.Contains(faster, "likely kept") {
		t.Errorf("the note of a replay gap shorter than at record: %s", faster)
	}
	if (*RepeatNote)(nil).Explain("keploy record") != "" || (*RepeatNote)(nil).Clone() != nil {
		t.Error("a nil note says something")
	}
}
