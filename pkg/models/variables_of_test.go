package models

import "testing"

const (
	evID = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	suID = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
)

// Of goes with the bind it was learned at when two declarations of one
// variable merge: kept where both bind naming one value, dropped where they
// name two.
func TestVariables_OfGoesWithItsBindWhenVariablesMerge(t *testing.T) {
	a := MockVariables{"w": {Recorded: "4011", Bind: "body.id#2", Of: evID}}
	for _, c := range []struct {
		name string
		b    MockVariable
		want string
	}{
		{"a use only", MockVariable{Recorded: "4011", Use: []string{"body.x"}}, evID},
		{"a bind naming the same value", MockVariable{Recorded: "4011", Bind: "body.id#2", Of: evID}, evID},
		{"a bind naming another value", MockVariable{Recorded: "4011", Bind: "body.id#2", Of: suID}, ""},
		{"a bind naming none", MockVariable{Recorded: "4011", Bind: "body.id#2"}, ""},
	} {
		if got := MergeVariables(a, MockVariables{"w": c.b})["w"].Of; got != c.want {
			t.Errorf("%s: Of %q, want %q", c.name, got, c.want)
		}
	}
	if got := MergeVariables(MockVariables{"w": {Recorded: "4011", Use: []string{"body.x"}}}, a)["w"].Of; got != evID {
		t.Errorf("a bind taken from the second block: Of %q, want %q", got, evID)
	}
}
