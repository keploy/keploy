package models

import "testing"

// The report's line for a test's background writes answered by their kind:
// how many, where, and of which kinds; nothing for a provisional answer or no
// answer.
func TestUncheckedLineCountsWritesAnsweredByKind(t *testing.T) {
	byKind := func(kind, where string) UnmatchedCall {
		return UnmatchedCall{Answer: AnswerByKind, WriteKind: kind, Kind: where}
	}
	for _, tc := range []struct {
		answers []UnmatchedCall
		want    string
	}{
		{nil, ""},
		{[]UnmatchedCall{{Answer: AnswerProvisional, WriteKind: "Mongo insert keploy.events"}}, ""},
		{[]UnmatchedCall{byKind("Mongo insert keploy.events", MissInTest)},
			"post-url-1: 1 background write answered inside its window, unchecked (Mongo insert keploy.events)"},
		{[]UnmatchedCall{byKind("Mongo insert keploy.events", MissInTest), byKind("Mongo delete keploy.events", MissInTest), byKind("Mongo insert keploy.events", MissInTest), byKind("Mongo insert keploy.events", MissOutsideTest)},
			"post-url-1: 3 background writes answered inside its window, unchecked (Mongo insert keploy.events x2, Mongo delete keploy.events); 1 background write answered in the gap before it, unchecked (Mongo insert keploy.events)"},
		// A by-kind answer that names no write kind is named by its protocol.
		{[]UnmatchedCall{{Answer: AnswerByKind, Protocol: "MongoDB", Kind: MissInTest}},
			"post-url-1: 1 background write answered inside its window, unchecked (MongoDB)"},
	} {
		if got := UncheckedLine("post-url-1", tc.answers); got != tc.want {
			t.Errorf("UncheckedLine(%+v) =\n %q\nwant\n %q", tc.answers, got, tc.want)
		}
	}
}
