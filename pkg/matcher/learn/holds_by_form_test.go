package learn

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A use's check by form (models.MockVariable.ByForm) passes a whole value
// that is a time of its form, under the time default only; a word's check,
// a check with no form, a value of another form and a value that is no time
// (an ObjectId) do not pass. MergeVariable keeps a use's check only where
// both learnings checked it so.
func TestAUseCheckedByFormPassesOnlyATimeOfItsFormUnderTheTimeDefault(t *testing.T) {
	text := Leaf{Type: String, Value: "2026-11-11T18:35:46Z"}
	o := models.ClockOffset{Form: TimeForm(text)}
	next := Leaf{Type: String, Value: "2026-11-12T01:00:01Z"}
	for _, tc := range []struct {
		what     string
		o        models.ClockOffset
		live     Leaf
		timeForm bool
		want     bool
	}{
		{what: "the next second", o: o, live: next, timeForm: true, want: true},
		{what: "the next second under strict times", o: o, live: next},
		{what: "no form", o: models.ClockOffset{}, live: next, timeForm: true},
		{what: "a word's check", o: models.ClockOffset{Form: o.Form, Word: models.ClockWord{Start: 1, End: 2}}, live: next, timeForm: true},
		{what: "another form", o: o, live: Leaf{Type: String, Value: "2026-11-12 01:00:01Z"}, timeForm: true},
		{what: "an epoch number", o: o, live: Leaf{Type: Number, Value: "1794429703"}, timeForm: true},
		{what: "an ObjectId", o: o, live: Leaf{Type: String, Value: "6732f0f0c3d4e5f60123aaaa"}, timeForm: true},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if got := HoldsByForm(tc.o, tc.live, tc.timeForm); got != tc.want {
				t.Fatalf("HoldsByForm(%+v, %q, %v) = %v; want %v", tc.o, tc.live.Value, tc.timeForm, got, tc.want)
			}
		})
	}
	const at = "body.createdAt"
	checked := models.MockVariable{Recorded: "r", Use: []string{at}, ByForm: map[string]models.ClockOffset{at: o}}
	exact := models.MockVariable{Recorded: "r", Use: []string{at}}
	if got := MergeVariable(checked, checked).ByForm; got[at] != o {
		t.Fatalf("two learnings that check %s by form merge to %v; want it kept", at, got)
	}
	if got := MergeVariable(checked, exact).ByForm; len(got) != 0 {
		t.Fatalf("a learning that checks %s exactly merges to %v; want it exact", at, got)
	}
	if got := MergeVariable(exact, checked).ByForm; len(got) != 0 {
		t.Fatalf("a learning that checks %s exactly merges to %v; want it exact", at, got)
	}
}

// A use of a value the bound call answered whole, at any leaf of its answer
// (placed or not), may be a copy of that answer: AnsweredWhole says so, and
// the use keeps its exact bind. The call's request, another call's answer,
// and an answer leaf of another value say nothing.
func TestAValueTheBoundCallAnsweredWholeIsAnsweredWhole(t *testing.T) {
	r := Leaf{Path: "body.createdAt", Type: String, Value: "2026-11-11T18:35:46Z"}
	seen := func(mock string, side Side, place, value string) []Earlier {
		return []Earlier{{Mock: mock, Seen: []Seen{{Side: side, Place: place, Leaf: Leaf{Path: "body.at", Type: String, Value: value}}}}}
	}
	for _, tc := range []struct {
		what    string
		earlier []Earlier
		want    bool
	}{
		{what: "answered at a placed leaf", earlier: seen("mock-1", Response, "body.at", r.Value), want: true},
		{what: "answered at a leaf with no place", earlier: seen("mock-1", Response, "", r.Value), want: true},
		{what: "sent in its request only", earlier: seen("mock-1", Request, "body.at", r.Value)},
		{what: "another call answered it", earlier: seen("mock-0", Response, "body.at", r.Value)},
		{what: "its answer holds another value", earlier: seen("mock-1", Response, "body.at", "2026-11-11T18:35:47Z")},
	} {
		if got := AnsweredWhole(tc.earlier, "mock-1", r); got != tc.want {
			t.Errorf("%s: AnsweredWhole = %v; want %v", tc.what, got, tc.want)
		}
	}
}
