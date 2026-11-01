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
// elsewhere is what calls made in other windows than their recordings'
// taught them, for learning only (learnedElsewhere): their clock reads
// alone are written onto the recordings no call of the round consumed
// (learn.WithLearnedElsewhere), each said apart, under the test whose
// recording it is (owner, by the set's recorded mapping: such a recording
// is in no test's consumed list), with the windows that taught it. What
// such a call taught a recording the round consumed is voided, and said so:
// its own pairing wins (learn.OwnPairingWins). None of it is consumption.
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
	taughtIn := map[string][]models.TeachingWindow{}
	if len(kept) > 0 {
		states = maps.Clone(consumed)
		for _, st := range learn.WithLearnedElsewhere(nil, kept) {
			states[st.Name] = st
		}
		for _, st := range kept {
			taughtIn[st.Name] = append(taughtIn[st.Name], st.TaughtIn...)
		}
	}
	added, err := p.PersistLearned(ctx, testSetID, states)
	if err != nil {
		return err
	}
	// What a recording no call consumed got was taught in other windows
	// alone: said apart, never as its owner's or the pool's own.
	taught := map[string][]string{}
	for name := range taughtIn {
		if len(added[name]) > 0 {
			taught[name] = added[name]
		}
	}
	if len(taught) > 0 {
		added = maps.Clone(added)
		for name := range taught {
			delete(added, name)
		}
	}
	var by map[string]string
	if len(consumerOf) > 0 {
		by = consumerOf[0]
	}
	for _, line := range learnedByTest(added, mapping, by) {
		r.logger.Info("learned fields that changed between record and replay",
			zap.String("testSetID", testSetID), zap.String("test", line.test), zap.Strings("fields", line.fields))
	}
	for _, name := range slices.Sorted(maps.Keys(taught)) {
		ws := slices.Clone(taughtIn[name])
		sort.SliceStable(ws, func(i, j int) bool { return ws[i].At.Before(ws[j].At) })
		r.logger.Info("learned fields that changed between record and replay, from calls made in other windows than their recording's, for learning only",
			zap.String("testSetID", testSetID), zap.String("mock", name), zap.String("test", orStartupAndPool(owner[name])), zap.Strings("fields", taught[name]),
			zap.String("taughtIn", learn.SayWindows(ws)), zap.String("evidence", "a call of its shape in another window: no call of the round consumed the recording"))
	}
	for _, v := range voided {
		r.logger.Info("kept what a recording's own pairing learned and voided what a call made in another window taught it: a call of the round consumed the recording, and its own pairing wins",
			zap.String("testSetID", testSetID), zap.String("mock", v.Name), zap.String("test", orStartupAndPool(owner[v.Name])), zap.Strings("voided", slices.Sorted(maps.Keys(v.Clocks))),
			zap.String("taughtIn", learn.SayWindows(v.TaughtIn)), zap.String("evidence", "its own pairing in the round"))
	}
	return nil
}

// learnedElsewhereReader is an optional extension of Instrumentation: what
// record's full round's learn step taught recordings from calls made in
// other windows than theirs, for learning only (AgentClient's
// GetLearnedElsewhere).
type learnedElsewhereReader interface {
	GetLearnedElsewhere(ctx context.Context) ([]models.MockState, error)
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
	reader, ok := r.instrumentation.(learnedElsewhereReader)
	if !ok {
		return nil
	}
	learned, err := reader.GetLearnedElsewhere(ctx)
	if err != nil {
		r.logger.Warn("keploy could not read what record's full round learned onto recordings from calls made in other windows than theirs; it keeps what their own calls taught them, and a recording only another window's call taught compares every field",
			zap.String("testSetID", testSetID), zap.Error(err), zap.String("next_step", "record the test set again"))
		return nil
	}
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

// orStartupAndPool is test, or what the record calls the set's start-up and
// pool when it is "".
func orStartupAndPool(test string) string {
	if test == "" {
		return "the set's start-up and pool"
	}
	return test
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
