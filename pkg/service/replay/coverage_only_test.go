package replay

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// setsOfDB is a test DB whose test sets each hold their own tests; a set
// named in broken cannot be read.
type setsOfDB struct {
	*bpTestDB
	order  []string
	sets   map[string][]*models.TestCase
	broken map[string]bool
}

func (d *setsOfDB) GetAllTestSetIDs(context.Context) ([]string, error) { return d.order, nil }

func (d *setsOfDB) GetTestCases(_ context.Context, setID string) ([]*models.TestCase, error) {
	if d.broken[setID] {
		return nil, errors.New("the set's test cases cannot be read")
	}
	return d.sets[setID], nil
}

// coverageOnlyHooks are the run's hooks naming the sets in only coverage-only
// (CoverageOnlyHook), keeping the sets each AfterTestRun is handed.
type coverageOnlyHooks struct {
	TestHooks
	only  map[string]bool
	after [][]string
}

func (h *coverageOnlyHooks) CoverageOnly(testSetID string) bool { return h.only[testSetID] }

func (h *coverageOnlyHooks) AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, cov models.TestCoverage) error {
	h.after = append(h.after, testSetIDs)
	return h.TestHooks.AfterTestRun(ctx, testRunID, testSetIDs, cov)
}

// newCoverageOnlyRun is a run of two sets: test-set-0 holds test-1, which
// passes, and test-set-1 holds kept-1, which the app answers differently from
// its recording, so it fails. The hooks name the sets in only coverage-only.
func newCoverageOnlyRun(t *testing.T, only ...string) (*bpRun, *bpApp, *coverageOnlyHooks, *setsOfDB) {
	t.Helper()
	app := newBPApp(t)
	app.wrong["/kept-1"] = true
	run := newBasePathRun(t, app.URL)
	port := closedLocalPort(t)
	db := &setsOfDB{
		bpTestDB: run.tests,
		order:    []string{"test-set-0", "test-set-1"},
		sets: map[string][]*models.TestCase{
			"test-set-0": {bpTestCase("test-1", port)},
			"test-set-1": {bpTestCase("kept-1", port)},
		},
		broken: map[string]bool{},
	}
	run.replayer.testDB = db
	hooks := &coverageOnlyHooks{TestHooks: run.replayer.hookImpl, only: map[string]bool{}}
	for _, id := range only {
		hooks.only[id] = true
	}
	run.replayer.hookImpl = hooks
	return run, app, hooks, db
}

// asked counts the requests the app got for path.
func asked(app *bpApp, path string) int {
	n := 0
	for _, a := range app.asked() {
		if strings.HasSuffix(a, " "+path) {
			n++
		}
	}
	return n
}

// A set the hooks name coverage-only (enterprise cloud replay's kept list) runs,
// so its tests take their coverage, and its result counts in nothing of the
// run's: a failed test in it does not fail the run or its exit code, is not in
// its totals or summary, and AfterTestRun (the report upload) is not handed
// the set. Without the hook the same failing test fails the run.
func TestACoverageOnlySetCountsInNothingOfTheRun(t *testing.T) {
	t.Run("named coverage-only", func(t *testing.T) {
		run, app, hooks, _ := newCoverageOnlyRun(t, "test-set-1")
		code, err := run.start(t)
		if err != nil || code != 0 {
			t.Fatalf("a run whose only failing test is coverage-only ended %d, %v; want 0, nil", code, err)
		}
		if asked(app, "/kept-1") != 1 {
			t.Fatalf("the coverage-only test was sent %d times, want once (it runs for its coverage): %v", asked(app, "/kept-1"), app.asked())
		}
		r := run.replayer
		if r.totalTests != 1 || r.totalTestPassed != 1 || r.totalTestFailed != 0 {
			t.Fatalf("totals = %d tests, %d passed, %d failed; want the counted set's 1, 1, 0", r.totalTests, r.totalTestPassed, r.totalTestFailed)
		}
		if _, in := r.completeTestReport["test-set-1"]; in {
			t.Fatal("the coverage-only set is in the run's summary")
		}
		if want := [][]string{{"test-set-0"}}; !reflect.DeepEqual(hooks.after, want) {
			t.Fatalf("AfterTestRun was handed %v, want %v", hooks.after, want)
		}
		if len(run.said(0, "for code coverage only")) != 1 {
			t.Fatal("the run did not say once that it ran the set for code coverage only")
		}
	})
	t.Run("not named: the failing test fails the run", func(t *testing.T) {
		run, _, hooks, _ := newCoverageOnlyRun(t)
		code, err := run.start(t)
		if err != nil || code == 0 {
			t.Fatalf("a run with a failing counted test ended %d, %v; want non-zero, nil", code, err)
		}
		if want := [][]string{{"test-set-0", "test-set-1"}}; !reflect.DeepEqual(hooks.after, want) {
			t.Fatalf("AfterTestRun was handed %v, want %v", hooks.after, want)
		}
	})
}

// A coverage-only set runs after every counted set, whatever its name, and
// once, where a counted set runs every flaky-check attempt.
func TestACoverageOnlySetRunsLastAndOnce(t *testing.T) {
	run, app, _, _ := newCoverageOnlyRun(t, "test-set-0")
	run.replayer.config.Test.MaxFlakyChecks = 3
	app.wrong = map[string]bool{"/test-1": true} // the coverage-only set's test fails
	if code, err := run.start(t); err != nil || code != 0 {
		t.Fatalf("ended %d, %v; want 0, nil", code, err)
	}
	got := app.asked()
	if len(got) == 0 || !strings.HasSuffix(got[len(got)-1], " /test-1") {
		t.Fatalf("the coverage-only set did not run last: %v", got)
	}
	if asked(app, "/test-1") != 1 || asked(app, "/kept-1") != 3 {
		t.Fatalf("sent the coverage-only test %d times and the counted one %d; want 1 and 3: %v", asked(app, "/test-1"), asked(app, "/kept-1"), got)
	}
}

// A coverage-only set that cannot be run (its test cases unreadable) is said
// and passed over: the run goes on and ends as its counted sets say. A counted
// set that cannot be run still fails the run.
func TestACoverageOnlySetThatErrsDoesNotEndTheRun(t *testing.T) {
	run, _, hooks, db := newCoverageOnlyRun(t, "test-set-1")
	db.broken["test-set-1"] = true
	code, err := run.start(t)
	if err != nil || code != 0 {
		t.Fatalf("a run whose coverage-only set could not be read ended %d, %v; want 0, nil", code, err)
	}
	if want := [][]string{{"test-set-0"}}; !reflect.DeepEqual(hooks.after, want) {
		t.Fatalf("AfterTestRun was handed %v, want %v", hooks.after, want)
	}

	run, _, _, db = newCoverageOnlyRun(t)
	db.broken["test-set-1"] = true
	if _, err := run.start(t); err == nil {
		t.Fatal("a run whose counted set could not be read did not fail")
	}
}
