package models

import (
	"strings"
	"testing"
	"time"
)

// The note's gaps come from the times it is given: at replay from the earlier
// answer to this call, or, once anchored, to this call's test's start; at
// record from the recording it is anchored on to the start of this call's
// test; to the hundredth of a second and never below zero. The record gap is
// unknown until it is anchored, and a likely cause is judged only from gaps
// that both run to the test's start.
func TestRepeatNoteGapsFromAControlledTimeline(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	answered := rec.Add(time.Hour)
	n := anchoredNote("mock-7", answered, rec, answered.Add(6213*time.Millisecond), rec.Add(3050*time.Millisecond))
	if n.Mock != "mock-7" || n.ReplayGapSeconds != 6.21 || n.RecordGapSeconds != 3.05 || !n.RecordGapKnown {
		t.Fatalf("note %+v, want 6.21s at replay and 3.05s at record", n)
	}
	if likely, known := n.ElapsedTimeExpiryLikely(); !likely || !known {
		t.Fatalf("a replay gap longer than the record gap is not an expiry: likely %v, known %v", likely, known)
	}
	if n := NewRepeatNote("m", answered, rec, answered.Add(-1500*time.Millisecond)); n.ReplayGapSeconds != 0 || n.RecordGapKnown || n.ReplayGapFromStart {
		t.Fatalf("a call 1.5 s before its twin's answer, not anchored, gave %+v", n)
	}
	if n := anchoredNote("m", answered, rec, answered.Add(-1500*time.Millisecond), rec.Add(-2*time.Second)); n.ReplayGapSeconds != 0 || n.RecordGapSeconds != 0 || !n.RecordGapKnown || !n.ReplayGapFromStart {
		t.Fatalf("a test that began before its twin's answer and before its recording, anchored, gave %+v", n)
	}
	// Anchored at record only: the replay gap still runs to the call.
	half := NewRepeatNote("m", answered, rec, answered.Add(9*time.Second))
	half.Anchor(time.Time{}, rec.Add(2*time.Second), "m", rec)
	if _, known := half.ElapsedTimeExpiryLikely(); known || !half.RecordGapKnown || half.ReplayGapFromStart {
		t.Fatalf("gaps that run to a call and to a test's start were judged: %+v", half)
	}
	if _, known := half.ElapsedTimeFreshLikely(); known {
		t.Fatal("the mirror judged gaps that run to a call and to a test's start")
	}
	// Anchored on another recording, the record gap runs from that one.
	other := NewRepeatNote("mock-7", answered, rec, time.Time{})
	other.Anchor(answered.Add(6*time.Second), rec.Add(30*time.Second), "mock-9", rec.Add(28*time.Second))
	if other.RecordGapSeconds != 2 || other.RecordedLast != "mock-9" || other.ReplayGapSeconds != 6 {
		t.Fatalf("anchored on mock-9, recorded 2 s before the test began: %+v", other)
	}
	if likely, known := other.ElapsedTimeExpiryLikely(); !likely || !known {
		t.Fatalf("6 s at replay against 2 s at record is not an expiry: %v %v", likely, known)
	}
	// An anchor with no name or no time measures nothing at record.
	for _, a := range []struct {
		name string
		at   time.Time
	}{{"", rec}, {"mock-9", time.Time{}}} {
		n := NewRepeatNote("mock-7", answered, rec, time.Time{})
		n.Anchor(answered.Add(time.Second), rec.Add(30*time.Second), a.name, a.at)
		if n.RecordGapKnown || n.RecordedLast != "" {
			t.Fatalf("an anchor %q at %v measured %+v", a.name, a.at, n)
		}
	}
	if _, known := NewRepeatNote("m", answered, rec, answered).ElapsedTimeExpiryLikely(); known {
		t.Fatal("an unknown record gap was judged")
	}
	if likely, known := anchoredNote("m", answered, rec, answered.Add(time.Second), rec.Add(2*time.Second)).ElapsedTimeExpiryLikely(); likely || !known {
		t.Fatalf("a replay gap shorter than the record gap was judged likely %v (known %v)", likely, known)
	}
	// Equal gaps support neither cause: a span measured in elapsed time that
	// the recording stayed inside and the replay passed (or the other way)
	// needs the two gaps to differ, and to the hundredth they do not.
	even := anchoredNote("m", answered, rec, answered.Add(3050*time.Millisecond), rec.Add(3050*time.Millisecond))
	if likely, known := even.ElapsedTimeExpiryLikely(); likely || !known {
		t.Fatalf("equal gaps (%+v) were judged an expiry: likely %v, known %v", even, likely, known)
	}
	if likely, known := even.ElapsedTimeFreshLikely(); likely || !known {
		t.Fatalf("equal gaps (%+v) were judged a fresh answer: likely %v, known %v", even, likely, known)
	}
}

// The sentence names the recording, the test once it is named, both gaps,
// the cause that the gaps support, and the remedies; the cause is never
// claimed as certain.
func TestRepeatNoteExplains(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	expired := anchoredNote("mock-7", rec, rec, rec.Add(6210*time.Millisecond), rec.Add(3050*time.Millisecond))
	expired.Test = "get-count-3"
	got := expired.Explain("keploy record")
	for _, want := range []string{"identical to mock-7", "test get-count-3's call", "At replay this test began 6.21s after that call", "at record this test began 3.05s after it,", "likely kept the earlier answer for a span of elapsed time", "'keploy record'", "could hide a bug"} {
		if !strings.Contains(got, want) {
			t.Errorf("the note of an expiry lacks %q: %s", want, got)
		}
	}
	unnamed := NewRepeatNote("mock-7", rec, rec, rec.Add(time.Second)).Explain("keploy record")
	if !strings.Contains(unnamed, "an earlier test of this set") || !strings.Contains(unnamed, "At replay this call came 1.00s after that one") || strings.Contains(unnamed, "at record this test began") || !strings.Contains(unnamed, "If the application keeps that answer") {
		t.Errorf("the note with no test and no record gap: %s", unnamed)
	}
	refilled := NewRepeatNote("mock-7", rec, rec, time.Time{})
	refilled.Anchor(rec.Add(6*time.Second), rec.Add(30*time.Second), "mock-9", rec.Add(28*time.Second))
	if s := refilled.Explain("keploy record"); !strings.Contains(s, "at record this test began 2.00s after mock-9, the newest identical recording made before it,") || !strings.Contains(s, "likely kept") {
		t.Errorf("the note anchored on another recording: %s", s)
	}
	if s := refilled.ExplainUnmade("keploy record"); !strings.Contains(s, "at record it began 2.00s after mock-9, the newest identical recording made before it.") {
		t.Errorf("the unmade note anchored on another recording: %s", s)
	}
	if b := refilled.Brief(false); b != "identical to mock mock-7, which answered an earlier test's call 6.00s earlier at replay and 2.00s earlier at record (from mock-9): likely an answer the application keeps for a span of elapsed time" {
		t.Errorf("the brief anchored on another recording is %q", b)
	}
	faster := anchoredNote("mock-7", rec, rec, rec.Add(time.Second), rec.Add(2*time.Second)).Explain("keploy record")
	if !strings.Contains(faster, "less likely cause") || strings.Contains(faster, "likely kept") {
		t.Errorf("the note of a replay gap shorter than at record: %s", faster)
	}
	if (*RepeatNote)(nil).Explain("keploy record") != "" || (*RepeatNote)(nil).Clone() != nil {
		t.Error("a nil note says something")
	}
}

// Two HTTP recordings have one fingerprint when they sent the same method,
// URL (the query in any order), X-Amz-Target and body, whatever their other
// headers; any of those that differs, or a mock of another kind, gives none
// in common. A mock whose payload a read left on disk answers as it did in
// memory.
func TestRequestFingerprintNamesOneIdenticalRequest(t *testing.T) {
	mk := func(method, u, target, body string, header map[string]string) *Mock {
		h := map[string]string{"X-Trace": "1"}
		for k, v := range header {
			h[k] = v
		}
		if target != "" {
			h["x-amz-target"] = target
		}
		return &Mock{Name: "m", Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: Method(method), URL: u, Header: h, Body: body}}}
	}
	base := mk("POST", "http://sap:8080/odata/Rates?b=2&a=1", "Svc.Get", `{"k":1}`, nil).RequestFingerprint()
	if base == "" {
		t.Fatal("an HTTP recording has no fingerprint")
	}
	if got := mk("POST", "http://sap:8080/odata/Rates?a=1&b=2", "Svc.Get", `{"k":1}`, map[string]string{"X-Trace": "2", "Date": "now"}).RequestFingerprint(); got != base {
		t.Error("the query's order or another header split one request in two")
	}
	for name, m := range map[string]*Mock{
		"method": mk("PUT", "http://sap:8080/odata/Rates?b=2&a=1", "Svc.Get", `{"k":1}`, nil),
		"host":   mk("POST", "http://sap2:8080/odata/Rates?b=2&a=1", "Svc.Get", `{"k":1}`, nil),
		"query":  mk("POST", "http://sap:8080/odata/Rates?b=3&a=1", "Svc.Get", `{"k":1}`, nil),
		"target": mk("POST", "http://sap:8080/odata/Rates?b=2&a=1", "Svc.Put", `{"k":1}`, nil),
		"body":   mk("POST", "http://sap:8080/odata/Rates?b=2&a=1", "Svc.Get", `{"k":2}`, nil),
	} {
		if m.RequestFingerprint() == base {
			t.Errorf("a request with another %s has the same fingerprint", name)
		}
	}
	a := &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: "http://x/", Body: "a|", Binary: "b"}}}
	b := &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: "http://x/", Body: "a", Binary: "|b"}}}
	if a.RequestFingerprint() == b.RequestFingerprint() {
		t.Error("two splits of the same bytes between body and binary are one request")
	}
	if fp := (&Mock{Kind: Postgres, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: "http://x/"}}}).RequestFingerprint(); fp != "" {
		t.Error("a mock of another kind has a fingerprint")
	}
	onDisk := mk("POST", "http://sap:8080/odata/Rates?b=2&a=1", "Svc.Get", `{"k":1}`, nil)
	onDisk.LeavePayloadOnDisk(&fileSource{}, PayloadAt{Name: "m"})
	if onDisk.Spec.HTTPReq != nil || onDisk.RequestFingerprint() != base {
		t.Error("a mock whose payload is on disk lost its request's fingerprint")
	}
}

// The mirror's sentence, on a recorded call the replay never made, names the
// recording and test it is identical to, both gaps, and the cause the gaps
// support: a replay gap shorter than at record is the likely sign.
func TestRepeatNoteExplainsAnUnmadeCall(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	fresh := anchoredNote("mock-2", rec, rec, rec.Add(1500*time.Millisecond), rec.Add(70*time.Second))
	fresh.Test = "get-rates-1"
	if likely, known := fresh.ElapsedTimeFreshLikely(); !likely || !known {
		t.Fatalf("a replay gap shorter than at record is not fresh: %v %v", likely, known)
	}
	got := fresh.ExplainUnmade("keploy record")
	for _, want := range []string{"call identical to mock-2", "test get-rates-1's call", "never made it", "began 1.50s after", "at record it began 70.00s after", "likely still held the earlier answer", "'keploy record'"} {
		if !strings.Contains(got, want) {
			t.Errorf("the unmade note lacks %q: %s", want, got)
		}
	}
	slower := anchoredNote("mock-2", rec, rec, rec.Add(80*time.Second), rec.Add(70*time.Second))
	if likely, _ := slower.ElapsedTimeFreshLikely(); likely {
		t.Fatal("a replay gap longer than at record was judged fresh")
	}
	if s := slower.ExplainUnmade("keploy record"); !strings.Contains(s, "less likely cause") || strings.Contains(s, "likely still held") {
		t.Errorf("the unmade note of a slower replay: %s", s)
	}
	if s := NewRepeatNote("mock-2", rec, rec, rec.Add(time.Second)).ExplainUnmade("keploy record"); !strings.Contains(s, "an earlier test of this set") || strings.Contains(s, "at record it began") || !strings.Contains(s, "If the application keeps that answer") {
		t.Errorf("the unmade note with no test and no record gap: %s", s)
	}
	if _, known := (*RepeatNote)(nil).ElapsedTimeFreshLikely(); known || (*RepeatNote)(nil).ExplainUnmade("x") != "" || (*RepeatNote)(nil).Brief(true) != "" {
		t.Error("a nil note says something")
	}
	if b := fresh.Brief(true); b != "identical to mock mock-2, which answered test get-rates-1's call 1.50s earlier at replay and 70.00s earlier at record: likely an answer the application keeps for a span of elapsed time" {
		t.Errorf("the unmade brief is %q", b)
	}
	if b := fresh.Brief(false); strings.Contains(b, "likely") {
		t.Errorf("a miss whose replay gap is shorter claims a likely cause: %q", b)
	}
	if b := NewRepeatNote("mock-2", rec, rec, rec.Add(time.Second)).Brief(false); b != "identical to mock mock-2, which answered an earlier test's call 1.00s earlier at replay" {
		t.Errorf("the brief with no test and no record gap is %q", b)
	}
}

// anchoredNote is the note of a call identical to mock, which answered at
// answered and was recorded at rec, in a test that began at replayStart at
// this replay and at recordStart at record, measured from mock itself
// (RepeatNote.Anchor).
func anchoredNote(mock string, answered, rec, replayStart, recordStart time.Time) *RepeatNote {
	n := NewRepeatNote(mock, answered, rec, time.Time{})
	n.Anchor(replayStart, recordStart, mock, rec)
	return n
}

// A miss's next step is the note's Explain, unless the call is also a
// recording made elsewhere (not owned, with a closest): then it names that
// recording first, with the test it was recorded in when known, keeps
// Explain's words whole, and says judging reads it as the repeat. A nil note
// says nothing.
func TestExplainMissNamesANotOwnedRecordingBesideTheRepeat(t *testing.T) {
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	n := NewRepeatNote("mock-7", rec, rec, rec.Add(6*time.Second))
	n.Test = "test-3"
	plain := n.Explain("keploy record")
	if got := n.ExplainMiss(false, "mock-w", "test-5", "keploy record"); got != plain {
		t.Fatalf("an owned call's next step is %q, want Explain's", got)
	}
	if got := n.ExplainMiss(true, "", "test-5", "keploy record"); got != plain {
		t.Fatalf("a not-owned call with no closest got %q, want Explain's", got)
	}
	got := n.ExplainMiss(true, "mock-w", "test-5", "keploy record")
	if !strings.HasPrefix(got, "This request is mock-w, a recording keploy made in test test-5's window,") || !strings.Contains(got, plain) || !strings.HasSuffix(got, "does not move mock-w to the test set's worker scope, and sets the test aside as a repeat of an earlier recorded call only when its confirming round sees the same again.") {
		t.Fatalf("a not-owned repeat's next step is %q", got)
	}
	if got := n.ExplainMiss(true, "mock-w", "", "keploy record"); !strings.HasPrefix(got, "This request is mock-w, a recording keploy made between tests or in another test's window,") {
		t.Fatalf("with no test named it says %q", got)
	}
	var none *RepeatNote
	if got := none.ExplainMiss(true, "mock-w", "test-5", "keploy record"); got != "" {
		t.Fatalf("a nil note says %q", got)
	}
}
