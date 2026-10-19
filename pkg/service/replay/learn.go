package replay

import (
	"context"
	"errors"
	"sort"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// learning reports whether this replay runs the learn step (design v14 §7.2,
// row L; package matcher/learn): record's full round, which the record that
// runs it marks in memory (config.Test.Learn), under the proven switch.
func (r *Replayer) learning() bool {
	return provenswitch.On() && r.config.Test.Learn
}

// learningPool reports whether the learn step may pair the set's pool mocks
// too: only in a full record (config.Test.LearnPool).
func (r *Replayer) learningPool() bool {
	return r.learning() && r.config.Test.LearnPool
}

// judgedSet reports whether a test set's round gave it a verdict, which
// judging reads: PASSED or FAILED (a set keploy marks not judged,
// Replayer.markNotJudged, is INTERNAL_ERR), with a verdict for at least one
// of its tests (everyIgnored: none got one, everyTestIgnored). Only then is
// what its full round learned kept (persistLearned).
func judgedSet(status models.TestSetStatus, everyIgnored bool) bool {
	return !everyIgnored && (status == models.TestSetStatusPassed || status == models.TestSetStatusFailed)
}

// learnedPersister writes what the learn step learned onto a set's mocks and
// says what it added, by mock (the yaml mock store, PersistLearned).
type learnedPersister interface {
	PersistLearned(ctx context.Context, testSetID string, mockStates map[string]models.MockState) (map[string][]string, error)
}

// persistLearned writes what the learn step learned in a test set's replay
// (the consumed mocks' learned noise and variables) onto the set's
// recording, and says per test what it learned: "Record prints every field
// it learned, per test" (design §7.2 step 10). A mock no test consumed is
// named on its own.
func (r *Replayer) persistLearned(ctx context.Context, testSetID string, consumed map[string]models.MockState, mapping *models.Mapping) error {
	p, ok := r.mockDB.(learnedPersister)
	if !ok {
		return errors.New("keploy's mock store cannot write what the learn step learned")
	}
	added, err := p.PersistLearned(ctx, testSetID, consumed)
	if err != nil {
		return err
	}
	for _, line := range learnedByTest(added, mapping) {
		r.logger.Info("learned fields that changed between record and replay",
			zap.String("testSetID", testSetID), zap.String("test", line.test), zap.Strings("fields", line.fields))
	}
	return nil
}

// learnedLine is what the learn step learned for one test: each field as
// "<mock> <path> (<kind>)".
type learnedLine struct {
	test   string
	fields []string
}

// learnedByTest groups what was added per mock by the test whose mapping
// holds the mock, in the mapping's order; the mocks no test holds come last,
// under "" .
func learnedByTest(added map[string][]string, mapping *models.Mapping) []learnedLine {
	if len(added) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []learnedLine
	if mapping != nil {
		for _, tc := range mapping.TestCases {
			var fields []string
			for _, e := range tc.Mocks {
				for _, f := range added[e.Name] {
					fields = append(fields, e.Name+" "+f)
				}
				if len(added[e.Name]) > 0 {
					seen[e.Name] = true
				}
			}
			if len(fields) > 0 {
				out = append(out, learnedLine{test: tc.ID, fields: fields})
			}
		}
	}
	var rest []string
	for name := range added {
		if !seen[name] {
			for _, f := range added[name] {
				rest = append(rest, name+" "+f)
			}
		}
	}
	if len(rest) > 0 {
		sort.Strings(rest)
		out = append(out, learnedLine{fields: rest})
	}
	return out
}
