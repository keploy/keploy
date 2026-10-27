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
		// A background call answered again names the recording it repeated.
		{[]UnmatchedCall{{Answer: AnswerBackgroundAgain, ClosestMock: "mock-7", Kind: MissInTest}},
			"post-url-1: 1 background call answered again, unchecked (mock-7)"},
		{[]UnmatchedCall{{Answer: AnswerBackgroundAgain, ClosestMock: "mock-7"}, byKind("Mongo insert keploy.events", MissInTest), {Answer: AnswerBackgroundAgain, ClosestMock: "mock-3"}, {Answer: AnswerBackgroundAgain, ClosestMock: "mock-7"}},
			"post-url-1: 1 background write answered inside its window, unchecked (Mongo insert keploy.events); 3 background calls answered again, unchecked (mock-7 x2, mock-3)"},
	} {
		if got := UncheckedLine("post-url-1", tc.answers); got != tc.want {
			t.Errorf("UncheckedLine(%+v) =\n %q\nwant\n %q", tc.answers, got, tc.want)
		}
	}
}
