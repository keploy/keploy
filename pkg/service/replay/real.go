package replay

import (
	"context"
	"errors"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// RealRunHook is an optional TestHooks extension: it is told, under the
// proven switch with a mock.real list, what each test set's run saw of
// mock.real beyond its test cases (the destinations it reached, and what
// keploy could not capture outside them), and the destinations the
// recordings of its test cases that ran list, for a verdict of its own
// (design v14 §8.2 rules 2 and 4).
type RealRunHook interface {
	SetRealRun(testSetID string, run *models.RealRun, listed []string)
}

// realTestAgent is an agent client that reads a test case's mock.real use
// at its end (GET /agent/real/test).
type realTestAgent interface {
	RealTest(ctx context.Context) (models.RealTest, error)
}

// readRealTest adds to a test case's read of its misses, under the proven
// switch with a mock.real list, what it did with mock.real (design v14 §4):
// its REAL rows, and each connection keploy could neither capture nor relay
// as real while it ran, charged as a miss is (capture_incomplete, §3.2), so
// it fails the test with what to change. A read that fails leaves the test
// without a verdict, as a failed read of its misses does.
func (r *Replayer) readRealTest(ctx context.Context, read *testMisses) {
	if !provenswitch.On() || len(r.mockReal()) == 0 || r.noAgent {
		return
	}
	a, ok := r.instrumentation.(realTestAgent)
	if !ok {
		read.err = errors.Join(read.err, errors.New("this keploy's agent client cannot read a test case's mock.real use"))
		return
	}
	got, err := a.RealTest(ctx)
	if err != nil {
		// What the test's capture was cannot be told: as a loss count that
		// could not be read, the test has no verdict.
		read.realErr = err
		return
	}
	read.real = got.Real
	read.realLost = models.RealUncapturedLosses(got.Incomplete)
}

// realListedBy is the mock.real destinations the recordings of the test
// cases that ran list (TestCase.Real), sorted, each once: a test case that
// did not run (skipped, ignored) asks for nothing.
func realListedBy(cases []*models.TestCase, results []models.TestResult) []string {
	ran := map[string]bool{}
	for _, r := range results {
		if r.Status == models.TestStatusPassed || r.Status == models.TestStatusFailed || r.Status == models.TestStatusObsolete {
			ran[r.TestCaseID] = true
		}
	}
	var out []string
	for _, tc := range cases {
		if tc != nil && ran[tc.Name] {
			out = append(out, tc.Real...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// realUsedBy is the mock.real destinations the test cases' REAL rows say
// they used (TestResult.Real), sorted, each once.
func realUsedBy(results []models.TestResult) []string {
	var out []string
	for _, r := range results {
		for _, u := range r.Real {
			if u.Conns > 0 {
				out = append(out, u.Dest)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// realSetFailure is why mock.real fails a set's run (rule 4, design v14
// §8.2), "" when it does not: a destination the recordings of its test cases
// that ran list that the run never reached (CI must start it, or the test no
// longer needs it and is re-recorded), or one its test cases used (used,
// their REAL rows) that never answered in the set (down, or a protocol that
// never answers). What keploy could not capture outside every test case
// (rule 2) is the set's capture verdict instead (RunTestSet).
func realSetFailure(run *models.RealRun, listed, used []string) string {
	var why []string
	for _, d := range listed {
		switch {
		case slices.Contains(run.Reached, d):
		case slices.Contains(used, d):
			// Its connection carried the set's bytes, and it never
			// answered: dead, or a protocol that never answers.
			why = append(why, "mock.real destination "+d+", which its test cases' recordings list, was used but never answered in this test set: check that it is up where the tests run (a destination whose protocol never answers, a write-only sink, cannot show keploy it was reached: take it out of mock.real so keploy records and mocks it)")
		default:
			why = append(why, "mock.real destination "+d+", which its test cases' recordings list, was never reached: start it where the tests run, or re-record the tests that no longer use it")
		}
	}
	return strings.Join(why, "; ")
}

// mockReal is keploy.yml mock.real as the replayer reads it: only under the
// proven switch, and none without a config.
func (r *Replayer) mockReal() []string {
	if !provenswitch.On() || r.config == nil {
		return nil
	}
	return r.config.Mock.Real
}
