package models

import "testing"

// A clock read goes with the bind it was read at: a's bind keeps a's mark,
// a bind taken from b keeps b's, and a variable both bind is a clock read only
// when both say so.
func TestMergeVariablesKeepsAClockReadWithItsBind(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b MockVariable
		want bool
	}{
		{"a's bind, marked", MockVariable{Recorded: "r", Bind: "body.at", Clock: true}, MockVariable{Recorded: "r", Use: []string{"url.query.since"}}, true},
		{"a bind taken from b, marked", MockVariable{Recorded: "r", Use: []string{"url.query.since"}}, MockVariable{Recorded: "r", Bind: "body.at", Clock: true}, true},
		{"both bind, both marked", MockVariable{Recorded: "r", Bind: "body.at", Clock: true}, MockVariable{Recorded: "r", Bind: "body.at", Clock: true}, true},
		{"both bind, one marked", MockVariable{Recorded: "r", Bind: "body.at", Clock: true}, MockVariable{Recorded: "r", Bind: "body.at"}, false},
		{"a use marked is no clock read", MockVariable{Recorded: "r", Bind: "body.at"}, MockVariable{Recorded: "r", Use: []string{"url.query.since"}, Clock: true}, false},
	} {
		got := MergeVariables(MockVariables{"at": c.a}, MockVariables{"at": c.b})["at"]
		if got.Clock != c.want {
			t.Errorf("%s: merged Clock %v, want %v", c.name, got.Clock, c.want)
		}
	}
}
