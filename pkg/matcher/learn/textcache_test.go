package learn

import (
	"strings"
	"testing"
)

// A payload's text is read once for the set: every test's drifts read it
// through one cache, and the next set starts an empty one (its mock names,
// and so its texts, are its own).
func TestTrackerReadsAPayloadTextOnceForTheSet(t *testing.T) {
	tr := NewTracker()
	tr.Begin() // test 1
	first := tr.Drifts()
	text := `GET /a?next=%2Forders%2FA7K29QXM HTTP/1.1`
	extra := first.rawExtra(text)
	if !strings.Contains(extra, "/orders/A7K29QXM") {
		t.Fatalf("decoded form missing: %q", extra)
	}
	tr.End()
	tr.Begin() // test 2
	second := tr.Drifts()
	if second == first {
		t.Fatal("test 2 kept test 1's drifts")
	}
	if second.texts != first.texts {
		t.Fatal("test 2 reads the set's payload texts anew")
	}
	if _, ok := second.texts.extra[text]; !ok {
		t.Fatal("test 2 does not find the text test 1 read")
	}
	tr.End()
	tr.NewSet()
	tr.Begin()
	if tr.Drifts().texts == first.texts {
		t.Fatal("the next set kept the previous set's payload texts")
	}
}

// A drift cache made on its own (Classify with none) reads texts on its
// own, as before.
func TestADriftCacheOfItsOwnReadsItsOwnTexts(t *testing.T) {
	a, b := NewDriftCache(), NewDriftCache()
	if a.texts == nil || a.texts == b.texts {
		t.Fatal("two caches of their own share their texts")
	}
	toks := a.rawTokens("ORDER-7 shipped")
	if !toks[0]["ORDER"] || !toks[0]["7"] {
		t.Fatalf("words: %v", toks[0])
	}
}

// containsWholeClock reads a payload's clock reads only once the value
// stands in it, with the same answer as reading them first: a value absent
// is absent, one standing whole is found, and one only inside a longer clock
// read of its own form is not.
func TestContainsWholeClockAnswersAsBefore(t *testing.T) {
	for _, tc := range []struct {
		text, v string
		want    bool
	}{
		{"created 2026-10-22T14:18:04Z by job 7", "2026-10-21T09:00:00Z", false},
		{"created 2026-10-22T14:18:04Z by job 7", "2026-10-22T14:18:04Z", true},
		{"at 2026-10-22T14:18:04.274Z", "2026-10-22T14:18:04", false},
		{`{"day":"2026-10-22","at":"2026-10-22T14:18:04Z"}`, "2026-10-22", true},
	} {
		if got := containsWholeClock(tc.text, tc.v); got != tc.want {
			t.Errorf("containsWholeClock(%q, %q) = %v, want %v", tc.text, tc.v, got, tc.want)
		}
	}
}
