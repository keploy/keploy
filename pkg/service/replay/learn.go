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
//
// consumerOf, when set, is the test that made each shared recording's call
// (models.SetShares.Pins): what was learned on one is said under that test,
// not under the test whose request came first.
func (r *Replayer) persistLearned(ctx context.Context, testSetID string, consumed map[string]models.MockState, mapping *models.Mapping, consumerOf ...map[string]string) error {
	p, ok := r.mockDB.(learnedPersister)
	if !ok {
		return errors.New("keploy's mock store cannot write what the learn step learned")
	}
	added, err := p.PersistLearned(ctx, testSetID, consumed)
	if err != nil {
		return err
	}
	var by map[string]string
	if len(consumerOf) > 0 {
		by = consumerOf[0]
	}
	for _, line := range learnedByTest(added, mapping, by) {
		r.logger.Info("learned fields that changed between record and replay",
			zap.String("testSetID", testSetID), zap.String("test", line.test), zap.Strings("fields", line.fields))
	}
	writeLearnedFiles(r.logger, r.mockDB, testSetID, consumed)
	return nil
}

// learnedFilesWriter writes a set's learned files (the yaml mock store,
// WriteLearnedFiles): learned.yaml from its mocks, learned.local.yaml with
// record's full round's values.
type learnedFilesWriter interface {
	WriteLearnedFiles(testSetID string, mockStates map[string]models.MockState) error
}

// writeLearnedFiles writes the set's learned files once what the learn step
// learned is on its mocks (design r9 §6): they are derived from the mocks and
// say, for a person or an LLM, what was learned and how to undo it. A store
// that writes none writes none, and one that fails says so: the recording is
// written all the same.
func writeLearnedFiles(logger *zap.Logger, db interface{}, testSetID string, consumed map[string]models.MockState) {
	w, ok := db.(learnedFilesWriter)
	if !ok {
		return
	}
	if err := w.WriteLearnedFiles(testSetID, consumed); err != nil {
		logger.Warn("what the learn step learned is on the set's mocks, but its learned files were not written", zap.String("testSetID", testSetID), zap.Error(err))
	}
}

// learnedLine is what the learn step learned for one test: each field as
// "<mock> <path> (<kind>)".
type learnedLine struct {
	test   string
	fields []string
}

// learnedByTest groups what was added per mock by the test whose mapping
// holds the mock, in the mapping's order; the mocks no test holds come last,
// under "" . A mock consumerOf names (a shared recording, pinned to the test
// that made its call) is that test's, wherever the mapping lists it: its
// lines follow the mapping's, by test.
func learnedByTest(added map[string][]string, mapping *models.Mapping, consumerOf ...map[string]string) []learnedLine {
	if len(added) == 0 {
		return nil
	}
	var by map[string]string
	if len(consumerOf) > 0 {
		by = consumerOf[0]
	}
	seen := map[string]bool{}
	var out []learnedLine
	at := map[string]int{}
	addLine := func(test string, fields []string) {
		if i, ok := at[test]; ok {
			out[i].fields = append(out[i].fields, fields...)
			return
		}
		at[test] = len(out)
		out = append(out, learnedLine{test: test, fields: fields})
	}
	if mapping != nil {
		for _, tc := range mapping.TestCases {
			var fields []string
			for _, e := range tc.Mocks {
				if by[e.Name] != "" {
					continue
				}
				for _, f := range added[e.Name] {
					fields = append(fields, e.Name+" "+f)
				}
				if len(added[e.Name]) > 0 {
					seen[e.Name] = true
				}
			}
			if len(fields) > 0 {
				addLine(tc.ID, fields)
			}
		}
	}
	var pinned []string
	for name := range added {
		if by[name] != "" {
			pinned = append(pinned, name)
		}
	}
	sort.Strings(pinned)
	sort.SliceStable(pinned, func(i, j int) bool { return by[pinned[i]] < by[pinned[j]] })
	for _, name := range pinned {
		var fields []string
		for _, f := range added[name] {
			fields = append(fields, name+" "+f)
		}
		seen[name] = true
		addLine(by[name], fields)
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
