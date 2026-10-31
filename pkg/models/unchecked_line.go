package models

import (
	"fmt"
	"strings"
)

// againPart is UncheckedLine's part for the test's background calls answered
// again (AnswerBackgroundAgain): how many, and the marked recordings each
// repeated, in the order they first did; "" when answers holds none.
//
//	3 background calls answered again, unchecked (mock-7 x2, mock-3)
func againPart(answers []UnmatchedCall) string {
	n := 0
	var names []string
	count := map[string]int{}
	for _, a := range answers {
		if a.Answer != AnswerBackgroundAgain {
			continue
		}
		n++
		if count[a.ClosestMock] == 0 {
			names = append(names, a.ClosestMock)
		}
		count[a.ClosestMock]++
	}
	if n == 0 {
		return ""
	}
	for i, name := range names {
		if c := count[name]; c > 1 {
			names[i] = fmt.Sprintf("%s x%d", name, c)
		}
	}
	noun := "calls"
	if n == 1 {
		noun = "call"
	}
	return fmt.Sprintf("%d background %s answered again, unchecked (%s)", n, noun, strings.Join(names, ", "))
}

// UncheckedLine is the report's line for test's background writes answered by
// their kind (AnswerByKind): how many, where (inside its window,
// or in the gap before it), and of which write kinds, in the order they were
// first answered; then its background calls answered again (againPart); ""
// when answers holds neither. keploy test prints it as each
// set ends, and keploy report under the test's status, from the test's
// TestResult.Answers.
//
//	post-url-1: 1 background write answered inside its window, unchecked (Mongo insert keploy.events)
func UncheckedLine(test string, answers []UnmatchedCall) string {
	type where struct {
		n     int
		kinds []string
		count map[string]int
	}
	inside, gap := &where{count: map[string]int{}}, &where{count: map[string]int{}}
	for _, a := range answers {
		if a.Answer != AnswerByKind {
			continue
		}
		w := inside
		if a.Kind == MissOutsideTest {
			w = gap
		}
		kind := a.WriteKind
		if kind == "" {
			kind = a.Protocol
		}
		w.n++
		if w.count[kind] == 0 {
			w.kinds = append(w.kinds, kind)
		}
		w.count[kind]++
	}
	var parts []string
	for _, w := range []struct {
		*where
		place string
	}{{inside, "inside its window"}, {gap, "in the gap before it"}} {
		if w.n == 0 {
			continue
		}
		kinds := make([]string, 0, len(w.kinds))
		for _, k := range w.kinds {
			if c := w.count[k]; c > 1 {
				k = fmt.Sprintf("%s x%d", k, c)
			}
			kinds = append(kinds, k)
		}
		noun := "writes"
		if w.n == 1 {
			noun = "write"
		}
		parts = append(parts, fmt.Sprintf("%d background %s answered %s, unchecked (%s)", w.n, noun, w.place, strings.Join(kinds, ", ")))
	}
	if again := againPart(answers); again != "" {
		parts = append(parts, again)
	}
	if len(parts) == 0 {
		return ""
	}
	return test + ": " + strings.Join(parts, "; ")
}

// BeforeWriteLines are the report's lines for test's reads answered from
// before a write the replay had not made yet (AnswerBeforeUnmadeWrite), one
// per read, in the order they were answered: the call, then the agent's
// sentence naming the write and the recording that answered (NextSteps);
// nil when answers holds none. keploy test prints them as each set ends,
// and keploy report under the test's status, from the test's
// TestResult.Answers.
//
//	post-url-1: MongoDB find oauth.blocklist: an extra read before mock-7, answered from mock-3
func BeforeWriteLines(test string, answers []UnmatchedCall) []string {
	var out []string
	for _, a := range answers {
		if a.Answer != AnswerBeforeUnmadeWrite {
			continue
		}
		why := a.NextSteps
		if why == "" {
			from := a.ClosestMock
			if from == "" {
				from = "a recording made before it"
			}
			why = "an extra read before a write the replay had not made yet, answered from " + from
		}
		line := test + ": "
		if call := strings.TrimSpace(a.Protocol + " " + a.ActualSummary); call != "" {
			line += call + ": "
		}
		out = append(out, line+why)
	}
	return out
}
