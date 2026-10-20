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

// judgedSet reports whether a test set's round judged the set, as keploy
// record's judging reads a round (enterprise's judgeRoundOut, which writes
// nothing for a set it did not judge): the set is PASSED or FAILED (a set
// keploy marks not judged, Replayer.markNotJudged, is INTERNAL_ERR), and
// every test of the set (tests, by name: every one recorded, as judging
// counts them, whether or not test.selectedTests selected it) got a verdict.
// A test has one when a row of it in results is not IGNORED, and no row of
// it says keploy could not read or check all of what the test did
// (noVerdict). An ignored test (keploy.yml's test.ignoredTests, a
// before-test-result hook's IGNORED) has none, nor does a test the run never
// reached, selected or not; results not read back whole (complete false)
// vouch for none. Only a judged set keeps what its full round learned
// (persistLearned).
func judgedSet(status models.TestSetStatus, tests []string, results []models.TestResult, complete bool) bool {
	if !complete || len(tests) == 0 || status != models.TestSetStatusPassed && status != models.TestSetStatusFailed {
		return false
	}
	verdict := make(map[string]bool, len(tests))
	unread := map[string]bool{}
	for _, r := range results {
		switch {
		case r.Status == models.TestStatusIgnored:
		case noVerdict(r.FailureInfo):
			unread[r.TestCaseID] = true
		default:
			verdict[r.TestCaseID] = true
		}
	}
	for _, name := range tests {
		if !verdict[name] || unread[name] {
			return false
		}
	}
	return true
}

// noVerdict reports whether a test's row says keploy could not read, or
// check, all of what the test did: the calls no mock answered, the mocks it
// consumed, its captures against its dependency calls, what its agent could
// not capture, or a call keploy cannot capture at all. Such a test has no
// verdict, whatever its status, as enterprise's RoundOf reads it.
func noVerdict(fi models.FailureInfo) bool {
	return fi.UnmatchedCallsUnread || fi.TestMocksUnread || fi.VariablesUnchecked != "" || fi.CaptureUnread || len(fi.CannotCapture) > 0
}

// testNames are the names of a set's test cases.
func testNames(cases []*models.TestCase) []string {
	out := make([]string, 0, len(cases))
	for _, tc := range cases {
		if tc != nil {
			out = append(out, tc.Name)
		}
	}
	return out
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
