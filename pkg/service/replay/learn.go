package replay

import (
	"context"
	"errors"
	"sort"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
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
// says what it added, by mock, and what a person's entry kept from being
// applied (blocked) (the yaml mock store, PersistLearned).
type learnedPersister interface {
	PersistLearned(ctx context.Context, testSetID string, mockStates map[string]models.MockState) (added, blocked map[string][]string, err error)
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
	var by map[string]string
	if len(consumerOf) > 0 {
		by = consumerOf[0]
	}
	added, blocked, err := p.PersistLearned(ctx, testSetID, learn.StampStates(consumed, learnedStamp(mapping, by)))
	if err != nil {
		return err
	}
	for _, line := range learnedByTest(added, mapping, by) {
		r.logger.Info("learned fields that changed between record and replay",
			zap.String("testSetID", testSetID), zap.String("test", line.test), zap.Strings("fields", line.fields))
	}
	if fields := learn.BlockedFields(blocked); len(fields) > 0 {
		r.logger.Warn(learn.BlockedMessage,
			zap.String("testSetID", testSetID), zap.Strings("fields", fields), zap.String("next_step", learn.BlockedNextStep))
	}
	return nil
}

// learnedStamp is what record's full round writes into the about of each
// decision it learned (learn.Stamp): the test open when each mock's were
// learned is the test that made a shared recording's call (consumerOf),
// else the test whose mapping holds the mock; one neither names was
// learned between tests.
func learnedStamp(mapping *models.Mapping, consumerOf map[string]string) learn.Stamp {
	test := map[string]string{}
	if mapping != nil {
		for _, tc := range mapping.TestCases {
			for _, e := range tc.Mocks {
				if _, ok := test[e.Name]; !ok && tc.ID != "" {
					test[e.Name] = tc.ID
				}
			}
		}
	}
	for name, t := range consumerOf {
		if t != "" {
			test[name] = t
		}
	}
	return learn.Stamp{Round: models.AboutRoundFull, Run: learn.RunStarted(), Keploy: utils.Version, Test: test}
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
