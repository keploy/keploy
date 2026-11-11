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
