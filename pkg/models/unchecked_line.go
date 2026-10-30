package models

import (
	"fmt"
	"strings"
)

// namedPart is UncheckedLine's part for the test's calls answered with
// answer: how many, and the recording each was answered with, in the order
// they first were, said as what says it ("%d background %s answered again,
// unchecked (%s)": the count, call or calls, the recordings); "" when
// answers holds none.
//
//	3 background calls answered again, unchecked (mock-7 x2, mock-3)
func namedPart(answers []UnmatchedCall, answer, what string) string {
	n := 0
	var names []string
	count := map[string]int{}
	for _, a := range answers {
		if a.Answer != answer {
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
	return fmt.Sprintf(what, n, noun, strings.Join(names, ", "))
}

// UncheckedLine is the report's line for test's background writes answered by
// their kind (AnswerByKind): how many, where (inside its window,
// or in the gap before it), and of which write kinds, in the order they were
// first answered; then its background calls answered again
// (AnswerBackgroundAgain), then its calls of a background series answered
// from the series (AnswerBackgroundSeries), then its calls of its own series
// answered at any count (AnswerOwnSeries: their clock reads and id checked,
// their count not), each naming the recordings that answered (namedPart); ""
// when answers holds none of these. keploy test prints it as each
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
	for _, named := range []struct{ answer, what string }{
		{AnswerBackgroundAgain, "%d background %s answered again, unchecked (%s)"},
		{AnswerBackgroundSeries, "%d background series %s answered on the run's clock, unchecked (%s)"},
		{AnswerOwnSeries, "%d %s of its own series answered at any count, on the run's clock and its own id (%s)"},
	} {
		if part := namedPart(answers, named.answer, named.what); part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return test + ": " + strings.Join(parts, "; ")
}
