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
		// Calls of a marked background series name the recordings of the
		// series that answered them, after the calls answered again.
		{[]UnmatchedCall{{Answer: AnswerBackgroundSeries, ClosestMock: "mock-12", Kind: MissInTest}},
			"post-url-1: 1 background series call answered on the run's clock, unchecked (mock-12)"},
		{[]UnmatchedCall{{Answer: AnswerBackgroundSeries, ClosestMock: "mock-12"}, {Answer: AnswerBackgroundAgain, ClosestMock: "mock-7"}, {Answer: AnswerBackgroundSeries, ClosestMock: "mock-12"}, {Answer: AnswerBackgroundSeries, ClosestMock: "mock-9"}},
			"post-url-1: 1 background call answered again, unchecked (mock-7); 3 background series calls answered on the run's clock, unchecked (mock-12 x2, mock-9)"},
		// Calls of the test's own series are named apart from a background
		// series, last, with what was checked of them: never "unchecked".
		{[]UnmatchedCall{{Answer: AnswerOwnSeries, ClosestMock: "beat-6", Kind: MissInTest}},
			"post-url-1: 1 call of its own series answered at any count, on the run's clock and its own id (beat-6)"},
		{[]UnmatchedCall{{Answer: AnswerOwnSeries, ClosestMock: "beat-6"}, {Answer: AnswerBackgroundSeries, ClosestMock: "mock-12"}, {Answer: AnswerOwnSeries, ClosestMock: "beat-6"}, {Answer: AnswerOwnSeries, ClosestMock: "beat-5"}},
			"post-url-1: 1 background series call answered on the run's clock, unchecked (mock-12); 3 calls of its own series answered at any count, on the run's clock and its own id (beat-6 x2, beat-5)"},
	} {
		if got := UncheckedLine("post-url-1", tc.answers); got != tc.want {
			t.Errorf("UncheckedLine(%+v) =\n %q\nwant\n %q", tc.answers, got, tc.want)
		}
	}
}
