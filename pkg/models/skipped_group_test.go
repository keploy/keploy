package models

import (
	"encoding/json"
	"testing"
)

// A miss the closing hooks of a group made after keploy skipped a test of it
// counts against no test, whatever else it is; and the group crosses the wire
// to the CLI, where every verdict reads CountsAgainstTest.
func TestAMissOfASkippedGroupsClosingHooksCountsAgainstNoTest(t *testing.T) {
	for _, c := range []struct {
		name string
		miss UnmatchedCall
		want bool
	}{
		{"an HTTP miss", UnmatchedCall{Protocol: "HTTP"}, true},
		{"a variable that did not hold", UnmatchedCall{Protocol: "HTTP", MatchPhase: MatchPhaseVariable}, true},
		{"made by a skipped group's closing hooks", UnmatchedCall{Protocol: "HTTP", MatchPhase: MatchPhaseVariable, SkippedGroup: "spec/Group"}, false},
		{"a DNS lookup", UnmatchedCall{Protocol: "DNS"}, false},
	} {
		if got := c.miss.CountsAgainstTest(); got != c.want {
			t.Errorf("%s: CountsAgainstTest = %v, want %v", c.name, got, c.want)
		}
	}
	b, err := json.Marshal(UnmatchedCall{Protocol: "HTTP", SkippedGroup: "spec/Group"})
	if err != nil {
		t.Fatal(err)
	}
	var back UnmatchedCall
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.SkippedGroup != "spec/Group" || back.CountsAgainstTest() {
		t.Fatalf("the group did not cross the wire: %s", b)
	}
	if b, _ := json.Marshal(UnmatchedCall{Protocol: "HTTP"}); string(b) != `{"protocol":"HTTP"}` {
		t.Fatalf("a miss of no skipped group carries the field: %s", b)
	}
}
