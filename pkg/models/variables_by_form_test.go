package models

import "testing"

// A use's check by form (MockVariable.ByForm) survives a merge only where no
// learning checks that use exactly: a use one learning alone names keeps its
// check, one both name keeps it where both learned it in one form, and is
// checked exactly otherwise. Cloning shares no map.
func TestAUsesCheckByFormHoldsOnlyWhereNoLearningChecksItExactly(t *testing.T) {
	const at, other = "body.createdAt", "body.updatedAt"
	text := ClockOffset{Form: "text 9999-99-99a99:99:99a"}
	number := ClockOffset{Form: "number 9999999999"}
	for _, tc := range []struct {
		what       string
		aUse, bUse []string
		aBy, bBy   map[string]ClockOffset
		want       map[string]ClockOffset
	}{
		{what: "both check it in one form", aUse: []string{at}, aBy: map[string]ClockOffset{at: text}, bUse: []string{at}, bBy: map[string]ClockOffset{at: text}, want: map[string]ClockOffset{at: text}},
		{what: "a checks it, b exactly", aUse: []string{at}, aBy: map[string]ClockOffset{at: text}, bUse: []string{at}},
		{what: "a exactly, b checks it", aUse: []string{at}, bUse: []string{at}, bBy: map[string]ClockOffset{at: text}},
		{what: "two forms", aUse: []string{at}, aBy: map[string]ClockOffset{at: text}, bUse: []string{at}, bBy: map[string]ClockOffset{at: number}},
		{what: "a alone names it", aUse: []string{at}, aBy: map[string]ClockOffset{at: text}, bUse: []string{other}, want: map[string]ClockOffset{at: text}},
		{what: "b alone names it", aUse: []string{other}, bUse: []string{at}, bBy: map[string]ClockOffset{at: number}, want: map[string]ClockOffset{at: number}},
		{what: "neither checks one", aUse: []string{at}, bUse: []string{at}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got := MergeByForm(tc.aUse, tc.aBy, tc.bUse, tc.bBy)
			if len(got) != len(tc.want) {
				t.Fatalf("MergeByForm = %v; want %v", got, tc.want)
			}
			for p, o := range tc.want {
				if got[p] != o {
					t.Fatalf("MergeByForm = %v; want %v", got, tc.want)
				}
			}
			a := MockVariables{"v": {Recorded: "r", Use: tc.aUse, ByForm: tc.aBy}}
			b := MockVariables{"v": {Recorded: "r", Use: tc.bUse, ByForm: tc.bBy}}
			merged := MergeVariables(a, b)["v"].ByForm
			if len(merged) != len(tc.want) {
				t.Fatalf("MergeVariables keeps %v; want %v", merged, tc.want)
			}
		})
	}
	a := MockVariables{"v": {Recorded: "r", Use: []string{at}, ByForm: map[string]ClockOffset{at: text}}}
	c := a.Clone()
	c["v"].ByForm[at] = number
	if a["v"].ByForm[at] != text {
		t.Fatal("a clone's check by form is the original's map")
	}
}
