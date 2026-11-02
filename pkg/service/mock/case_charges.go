package mock

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// holding names every case whose span (its request to its response) holds
// at: the cases in flight when a call was made.
func holding(spans []models.ScopeWindow, at time.Time) []string {
	var names []string
	for _, w := range spans {
		if !at.Before(w.Start) && !at.After(w.End) {
			names = append(names, w.Name)
		}
	}
	return names
}

// sharedCall is a call made while two or more cases of its flow were in
// flight: the time alone cannot tell which one's handler made it, so
// correlateCases charges it to none of them.
type sharedCall struct {
	mock  capturedMock
	cases []string
}

// sharedLines says, a line a case in cases' order, the calls left out of
// that case's case_mocks because another case was in flight too when they
// were made, and the other cases. A published case lists its case_mocks as
// its own calls, and a pull puts them in its step's mapping entry: a call
// charged to two cases would make each expect a call that only one's
// handler makes.
func sharedLines(cases []capturedMock, shared []sharedCall) []string {
	type left struct {
		calls  []string
		others []string
	}
	byCase := map[string]*left{}
	for _, s := range shared {
		what := s.mock.call
		if what == "" {
			what = s.mock.kind
		}
		for _, c := range s.cases {
			l := byCase[c]
			if l == nil {
				l = &left{}
				byCase[c] = l
			}
			l.calls = append(l.calls, fmt.Sprintf("%s (%s)", s.mock.name, what))
			for _, o := range s.cases {
				if o != c && !slices.Contains(l.others, o) {
					l.others = append(l.others, o)
				}
			}
		}
	}
	var out []string
	for _, c := range cases {
		l := byCase[c.name]
		if l == nil {
			continue
		}
		slices.Sort(l.others)
		n := fmt.Sprintf("%d calls", len(l.calls))
		if len(l.calls) == 1 {
			n = "1 call"
		}
		out = append(out, fmt.Sprintf("case %s: case_mocks leaves out %s made while other cases were in flight too (%s), as the time alone cannot tell whose handler made them: %s",
			c.name, n, strings.Join(l.others, ", "), strings.Join(l.calls, ", ")))
	}
	return out
}

// saySharedCharges logs sharedLines, a line a case.
func (m *mockService) saySharedCharges(name string, cases []capturedMock, shared []sharedCall) {
	for _, l := range sharedLines(cases, shared) {
		m.logger.Info(l, zap.String("mock-set", name))
	}
}
