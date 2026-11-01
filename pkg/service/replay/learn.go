package replay

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"

	"go.keploy.io/server/v3/pkg/matcher/learn"
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
// elsewhere (learnedElsewhere) is written onto the recordings no call of
// the round consumed and said apart, under owner's test (such a recording
// is in no test's consumed list), or the set's worker scope for one the
// plan serves there (exactPlan.isWorker); on one the round consumed it is
// voided and said so (learn.OwnPairingWins).
//
// consumerOf, when set, is the test that made each shared recording's call
// (models.SetShares.Pins): what was learned on one is said under that test,
// not under the test whose request came first.
func (r *Replayer) persistLearned(ctx context.Context, testSetID string, consumed map[string]models.MockState, elsewhere []models.MockState, owner map[string]string, mapping *models.Mapping, consumerOf ...map[string]string) error {
	p, ok := r.mockDB.(learnedPersister)
	if !ok {
		return errors.New("keploy's mock store cannot write what the learn step learned")
	}
	kept, voided := learn.OwnPairingWins(func(name string) bool { _, ok := consumed[name]; return ok }, elsewhere)
	states := consumed
	if len(kept) > 0 {
		states = make(map[string]models.MockState, len(consumed)+len(kept))
		maps.Copy(states, consumed)
		for _, st := range learn.WithLearnedElsewhere(nil, kept) {
			states[st.Name] = st
		}
	}
	added, err := p.PersistLearned(ctx, testSetID, states)
	if err != nil {
		return err
	}
	// What a recording no call consumed got at the clock reads other
	// windows taught it is said apart, never as its owner's or the pool's.
	added, taught := learn.SplitTaught(added, kept, owner)
	var by map[string]string
	if len(consumerOf) > 0 {
		by = consumerOf[0]
	}
	for _, line := range learnedByTest(added, mapping, by) {
		r.logger.Info("learned fields that changed between record and replay",
			zap.String("testSetID", testSetID), zap.String("test", line.test), zap.Strings("fields", line.fields))
	}
	learn.SayLearnedElsewhere(r.logger, taught, voided, owner, r.exact.isWorker, zap.String("testSetID", testSetID))
	return nil
}

// learnedElsewhere is what the agent's learn step taught the set's
// recordings from calls made in other windows than theirs, for learning
// only (learnedElsewhereReader), less what a window taught whose test's
// request the agent did not say it read, or a window it did not know the
// test of while any such test ran (learn.TaughtInAny): a value the
// application took from that request may read as a clock read, so it is
// kept as what that test's own recordings learned is (readLearning), not at
// all. nil from an instrumentation that cannot say, and, said in a warning,
// when the read failed: none of it served a call or decides a verdict, so a
// round that cannot read it keeps what its consumed recordings learned.
func (r *Replayer) learnedElsewhere(ctx context.Context, testSetID string) []models.MockState {
	learned := learn.ReadLearnedElsewhere(ctx, r.instrumentation, r.logger, zap.String("testSetID", testSetID))
	unread := r.requestsUnread.set()
	var out []models.MockState
	for _, st := range learned {
		if learn.TaughtInAny(st.TaughtIn, unread) {
			r.logger.Warn("keploy did not keep what a call made in another window taught a recording: the agent did not say it read the request of the test whose window the call came in, so a value the application took from that request may have been learned as a clock read; the recording keeps comparing those fields",
				zap.String("testSetID", testSetID), zap.String("mock", st.Name), zap.Strings("notKept", slices.Sorted(maps.Keys(st.Clocks))),
				zap.String("taughtIn", learn.SayWindows(st.TaughtIn)), zap.String("next_step", learningNotKeptRemedy()))
			continue
		}
		out = append(out, st)
	}
	return out
}

// ownerOf is, by recording, the test whose recorded mapping lists it
// (expected, each test's mocks).
func ownerOf(expected map[string][]models.MockEntry) map[string]string {
	out := map[string]string{}
	for test, entries := range expected {
		for _, e := range entries {
			out[e.Name] = test
		}
	}
	return out
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
