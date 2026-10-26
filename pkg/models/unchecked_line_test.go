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

// The report's lines for a test's reads answered from before a write the
// replay had not made yet: one per read, in order, naming the call, the
// write and the recording that answered; nothing for an answer of another
// kind, a miss, or no answer.
func TestBeforeWriteLinesNameEachReadAnsweredFromBeforeAWrite(t *testing.T) {
	before := func(call, why string) UnmatchedCall {
		return UnmatchedCall{Protocol: "MongoDB", ActualSummary: call, Answer: AnswerBeforeUnmadeWrite, ClosestMock: "mock-3", NextSteps: why}
	}
	for _, tc := range []struct {
		answers []UnmatchedCall
		want    []string
	}{
		{nil, nil},
		{[]UnmatchedCall{{Answer: AnswerByKind, WriteKind: "Mongo insert keploy.events"}, {Answer: AnswerProvisional, ClosestMock: "mock-3"}, {Protocol: "MongoDB", NextSteps: "a miss"}}, nil},
		{[]UnmatchedCall{before("find oauth.blocklist", "an extra read before mock-7, answered from mock-3"), {Answer: AnswerByKind, WriteKind: "Mongo insert keploy.events"}, before("find oauth.clients", "an extra read before mock-9, answered from mock-8")},
			[]string{"post-url-1: MongoDB find oauth.blocklist: an extra read before mock-7, answered from mock-3", "post-url-1: MongoDB find oauth.clients: an extra read before mock-9, answered from mock-8"}},
		// An answer without its sentence still names the recording, if any.
		{[]UnmatchedCall{{Answer: AnswerBeforeUnmadeWrite, ClosestMock: "mock-3"}, {Answer: AnswerBeforeUnmadeWrite}},
			[]string{"post-url-1: an extra read before a write the replay had not made yet, answered from mock-3", "post-url-1: an extra read before a write the replay had not made yet, answered from a recording made before it"}},
	} {
		got := BeforeWriteLines("post-url-1", tc.answers)
		if len(got) != len(tc.want) {
			t.Fatalf("BeforeWriteLines(%+v) = %q, want %q", tc.answers, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("BeforeWriteLines(%+v)[%d] =\n %q\nwant\n %q", tc.answers, i, got[i], tc.want[i])
			}
		}
	}
}
