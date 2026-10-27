package replay

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The rule both ways out of Start use to record a failing run.
//
// It exists as a function because the two used to disagree: the ordinary end
// of the loop set utils.ErrCode = 1 for a red run, and the user-abort case
// returned with no code at all -- so a `keploy test` whose suite had already
// gone red and was then interrupted exited 0, and CI recorded a pass.
//
// What this covers is the RULE. Start's loop has no test seam today (it needs
// a live testDB, reportDB, instrumentation and hooks), so that the abort case
// calls this is not covered here; the enterprise side pins the other half of
// the path in TestRunRoot_InterruptKeepsACodeAlreadyArmed.
func TestArmRunExitCode(t *testing.T) {
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })

	utils.ErrCode = 0
	armRunExitCode(nil, true, 0)
	if utils.ErrCode != 0 {
		t.Errorf("a run where every test passed exited %d", utils.ErrCode)
	}

	utils.ErrCode = 0
	armRunExitCode(nil, false, 0)
	if utils.ErrCode != 1 {
		t.Errorf("a run with a failed test set exited %d, want 1", utils.ErrCode)
	}

	// The case the abort path actually hits. testRunResult is folded in AFTER
	// the switch the abort returns from, so it is still true when the set
	// being interrupted is the one with the failures in it -- which is every
	// single-test-set suite, the common shape. Only the failure count knows.
	utils.ErrCode = 0
	armRunExitCode(nil, true, 3)
	if utils.ErrCode != 1 {
		t.Errorf("a run interrupted with 3 tests already failed exited %d, want 1", utils.ErrCode)
	}

	// ...and the other way round, so neither signal can be dropped: a set can
	// fail without any test failing (the app never came up, no tests to run).
	utils.ErrCode = 0
	armRunExitCode(nil, false, 0)
	if utils.ErrCode != 1 {
		t.Errorf("a set that failed with no failing test exited %d, want 1", utils.ErrCode)
	}

	// A run that never reached the loop at all -- no tests recorded, the
	// report store unreadable, the agent never healthy. `keploy test` throws
	// that error away so cobra will not print usage over a failure already
	// reported, which left nothing to carry it to the exit status.
	utils.ErrCode = 0
	armRunExitCode(errors.New("no test sets found"), true, 0)
	if utils.ErrCode != 1 {
		t.Errorf("a run that never started exited %d, want 1", utils.ErrCode)
	}

	// A run whose error still carries a tag ends with that tag's code, not a
	// bare 1.
	utils.ErrCode = 0
	armRunExitCode(fmt.Errorf("%w: the keploy agent could not start", utils.ErrPrivilegeRequired), true, 0)
	if utils.ErrCode != utils.ExitPrivilegeRequired {
		t.Errorf("a run whose agent lacked privileges exited %d, want %d", utils.ErrCode, utils.ExitPrivilegeRequired)
	}

	// A code already armed is MORE specific than this one -- a wrapped
	// runner's own status, or one of Keploy's own (utils/exitcodes.go) --
	// and overwriting it with a flat 1 tells the caller the tests failed
	// when the truth was a privilege failure or a refused session.
	utils.ErrCode = 5
	armRunExitCode(nil, false, 9)
	if utils.ErrCode != 5 {
		t.Errorf("an armed 5 became %d", utils.ErrCode)
	}
}

// armStartExitCode is the rule Start's every way out records the run with
// (OBS-ctrlc, the class of P240: keploy#4700 for keploy mock replay).
//
// Under the proven switch a run its user's signal stopped before its verdict
// did not verify the suite, whatever the tests it reached showed: it exits
// 128+N for the signal, never 0. A failure it reached keeps its 1. A stop
// once the run has its verdict changes nothing, and a cancel that is no
// signal's arms nothing.
//
// With the switch off a stopped run with nothing wrong still exits 0, the
// CLI's contract. But a test that got no verdict before the stop (the one
// such test with the switch off: a wrong answer demoted to OBSOLETE for a
// stale mapping) does not pass, as a failed test does not: its set would not
// be judged, and would fail the run, had the run ended on its own. The stop
// took its set to USER_ABORT, where that rule does not run, and the run
// exited 0: a false green on the default path. Under the switch a stop
// before the verdict exits 128+N whatever it reached, so a test with no
// verdict, which a read the stop cut short also makes, changes nothing.
func TestArmStartExitCode(t *testing.T) {
	const sigint, sigterm = 2, 15
	for _, c := range []struct {
		name           string
		on             bool
		stopped        bool
		signal         int
		err            error
		testRunResult  bool
		verdictReached bool
		failed         int
		// noVerdict marks test-2 of test-set-0 as having no verdict.
		noVerdict bool
		armed     int
		want      int
		// said: keploy says the run was stopped before it judged it;
		// saidNoVerdict: that it was stopped after a test with no verdict.
		said, saidNoVerdict bool
	}{
		{name: "on: stopped before the verdict", on: true, stopped: true, signal: sigint, testRunResult: true, want: 130, said: true},
		{name: "on: SIGTERM before the verdict", on: true, stopped: true, signal: sigterm, testRunResult: true, want: 143, said: true},
		{name: "on: stopped before the verdict, Start's error the stop's", on: true, stopped: true, signal: sigint, err: context.Canceled, testRunResult: true, want: 130, said: true},
		{name: "on: stopped before the verdict after a test with no verdict", on: true, stopped: true, signal: sigint, testRunResult: true, noVerdict: true, want: 130, said: true},
		{name: "on: stopped before the verdict after a failed test", on: true, stopped: true, signal: sigint, testRunResult: true, failed: 1, want: 1},
		{name: "on: stopped before the verdict after a failed set", on: true, stopped: true, signal: sigint, want: 1},
		{name: "on: stopped before the verdict, a code armed", on: true, stopped: true, signal: sigint, testRunResult: true, armed: 3, want: 3},
		{name: "on: stopped once the run has its verdict", on: true, stopped: true, signal: sigint, testRunResult: true, verdictReached: true, want: 0},
		{name: "on: a cancel that is no signal's", on: true, stopped: true, testRunResult: true, want: 0},
		{name: "on: not stopped, though a signal was noted", on: true, signal: sigint, testRunResult: true, want: 0},
		{name: "on: not stopped, a test with no verdict", on: true, testRunResult: true, verdictReached: true, noVerdict: true, want: 0},
		{name: "off: stopped after a test with no verdict", stopped: true, signal: sigint, testRunResult: true, noVerdict: true, want: 1, saidNoVerdict: true},
		{name: "off: stopped after a test with no verdict, once the run has its verdict", stopped: true, signal: sigint, testRunResult: true, verdictReached: true, noVerdict: true, want: 1, saidNoVerdict: true},
		{name: "off: stopped after a failed set and a test with no verdict", stopped: true, signal: sigint, noVerdict: true, want: 1},
		{name: "off: stopped with nothing wrong", stopped: true, signal: sigint, testRunResult: true, want: 0},
		{name: "off: stopped with nothing wrong, Start's error the stop's", stopped: true, signal: sigint, err: context.Canceled, testRunResult: true, want: 0},
		{name: "off: stopped after a failed test", stopped: true, signal: sigint, testRunResult: true, failed: 2, want: 1},
		{name: "off: not stopped, a test with no verdict, its set passed", testRunResult: true, verdictReached: true, noVerdict: true, want: 0},
		{name: "off: not stopped, an error", err: errors.New("no test sets found"), testRunResult: true, want: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			setSwitch(t, c.on)
			saved := utils.ErrCode
			t.Cleanup(func() { utils.ErrCode = saved })
			utils.ErrCode = c.armed
			t.Cleanup(utils.SetInterruptSignalForTest(c.signal))
			core, logs := observer.New(zap.InfoLevel)
			r := &Replayer{logger: zap.New(core), totalTestFailed: c.failed}
			if c.noVerdict {
				r.markNoVerdict("test-set-0", []string{"test-2"})
			}
			r.armStartExitCode(c.stopped, c.err, c.testRunResult, c.verdictReached)
			if utils.ErrCode != c.want {
				t.Fatalf("exit code %d, want %d", utils.ErrCode, c.want)
			}
			if n := logs.FilterMessageSnippet("the run was stopped before keploy judged it").Len(); (n == 1) != c.said || n > 1 {
				t.Fatalf("keploy said %d times that the run was stopped before it judged it; want it said: %v", n, c.said)
			}
			said := logs.FilterMessageSnippet("the run was stopped after tests of the test-set got no verdict")
			if (said.Len() == 1) != c.saidNoVerdict || said.Len() > 1 {
				t.Fatalf("keploy said %d times that the run was stopped after tests with no verdict; want it said: %v", said.Len(), c.saidNoVerdict)
			}
			if c.saidNoVerdict {
				f := said.All()[0].ContextMap()
				if f["test-set"] != "test-set-0" || fmt.Sprint(f["tests"]) != "[test-2]" {
					t.Fatalf("the line names %v of %v; want test-2 of test-set-0", f["tests"], f["test-set"])
				}
			}
		})
	}
}

// stopHooks stops the run, as its user's signal does, as the named test is
// sent (send), or once the run has its verdict (afterRun: the run's
// after-test-run hook), and otherwise answers as the hooks it wraps.
type stopHooks struct {
	TestHooks
	stop     func()
	send     string
	afterRun bool
}

func (h stopHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	if tc.Name == h.send {
		h.stop()
	}
	return h.TestHooks.SimulateRequest(ctx, tc, testSetID)
}

func (h stopHooks) AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error {
	if h.afterRun {
		h.stop()
	}
	return h.TestHooks.AfterTestRun(ctx, testRunID, testSetIDs, coverage)
}

// startStopped runs Start over h's set as keploy test does, with arm wiring
// the run's stop into the harness's fakes, and returns the code the process
// exits with and the set's status. The stop cancels the root context (cause
// context.Canceled) after NewCtx's handler has noted signal (0 for a cancel
// that is no signal's).
func startStopped(t *testing.T, h *prRun, signal int, arm func(stop func())) (int, models.TestSetStatus) {
	t.Helper()
	h.replayer.config.Command = "./app"
	h.replayer.config.CommandType = string(utils.Native)
	h.replayer.config.Test.MaxFlakyChecks = 1
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })
	utils.ErrCode = 0
	// The signal stays noted for the rest of the run, as NewCtx's handler
	// notes it for the life of the process.
	t.Cleanup(utils.SetInterruptSignalForTest(0))
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	arm(func() {
		utils.SetInterruptSignalForTest(signal)
		stop()
	})
	if err := h.replayer.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Start: %v", err)
	}
	h.report.mu.Lock()
	defer h.report.mu.Unlock()
	if h.report.report == nil {
		t.Fatal("the set wrote no report")
	}
	return utils.ErrCode, models.TestSetStatus(h.report.report.Status)
}

// demoteTest2 makes test-2 of h, a set of n tests, a wrong answer demoted to
// OBSOLETE for a stale mapping (switch off): it left a recorded mock of its
// mapping unconsumed. Every other test consumes its own mock.
func demoteTest2(h *prRun, hooks *prHooks, n int) {
	pinWindows(h)
	h.mappings.exists = true
	h.mappings.onDisk = map[string][]models.MockEntry{}
	consumed := map[string][]models.MockState{}
	for i := 1; i <= n; i++ {
		name, own := fmt.Sprintf("test-%d", i), fmt.Sprintf("own-%d", i)
		h.mappings.onDisk[name] = mongoEntries(own)
		consumed[name] = []models.MockState{mongoAt(own, i*1000+50)}
	}
	h.mappings.onDisk["test-2"] = mongoEntries("own-2", "own-2b")
	hooks.wrongBody = map[string]bool{"test-2": true}
	hooks.consumedFor = consumedByWindow(h, consumed)
}

// A keploy test its user stops does not exit 0 where it reached no verdict,
// end to end through Start (armStartExitCode is the rule).
//
// Under the switch the set the stop ends is USER_ABORT, whatever its tests
// showed, and the run exits 128+N for the signal: a test whose misses could
// not be read, which leaves its set not judged when it ends on its own,
// exited 0 once a Ctrl+C came after it, and so did a set whose tests all
// passed so far.
//
// With the switch off which status the stopped set ends with is a race
// between the set's end and the watch of its application, which says
// USER_ABORT. A wrong answer demoted to OBSOLETE before the stop has no
// verdict either way: the set is not judged when its end wins (1), and the
// run exited 0 when the watch won (on main 7db6d02ef, 41 of 60 runs of the
// 3-test case and 47 of 60 of the 4-test one ended USER_ABORT, each with
// exit 0; how often the watch wins is the race's, and changes with the
// host's load). It now exits 1 for both. A run stopped with nothing wrong
// exits 0 for both, as before. TestArmStartExitCode holds the rule for each
// status.
func TestAStoppedRunWithNoVerdictDoesNotExitZero(t *testing.T) {
	const sigint, sigterm = 2, 15
	unreadAtTest2 := func(func()) func() ([]models.UnmatchedCall, error) { return misses(nil, failedRead) }
	for _, c := range []struct {
		name   string
		on     bool
		signal int
		// demoted makes test-2 a wrong answer demoted to OBSOLETE (switch
		// off); wrong a wrong answer that fails.
		demoted, wrong bool
		// reads is the agent's misses per test. stopAt is the test whose
		// send the stop comes at; afterRun stops the run once it has its
		// verdict; atEnd as the set reads its tests' results; none of them
		// is no stop.
		reads           func(stop func()) func() ([]models.UnmatchedCall, error)
		stopAt          string
		afterRun, atEnd bool
		// tests is the set's size, 3 when 0.
		tests int
		// statuses are the statuses the set may end with.
		statuses []models.TestSetStatus
		want     int
	}{
		{name: "switch on: test-2's misses unread, no stop", on: true, signal: sigint, reads: unreadAtTest2,
			statuses: []models.TestSetStatus{models.TestSetStatusInternalErr}, want: 1},
		{name: "switch on: test-2's misses unread, the stop at test-3", on: true, signal: sigint, reads: unreadAtTest2, stopAt: "test-3",
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort}, want: 130},
		{name: "switch on: every test judged so far, the stop at test-3", on: true, signal: sigint, stopAt: "test-3",
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort}, want: 130},
		{name: "switch on: the stop cuts test-3's read of its misses short", on: true, signal: sigint,
			reads: func(stop func()) func() ([]models.UnmatchedCall, error) {
				return misses(nil, nil, func() ([]models.UnmatchedCall, error) { stop(); return nil, context.Canceled })
			},
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort}, want: 130},
		{name: "switch on: the stop as the set reads its results", on: true, signal: sigint, atEnd: true,
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort}, want: 130},
		{name: "switch on: SIGTERM at test-3", on: true, signal: sigterm, stopAt: "test-3",
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort}, want: 143},
		{name: "switch on: test-2 failed before the stop", on: true, signal: sigint, wrong: true, stopAt: "test-3",
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort}, want: 1},
		{name: "switch on: the stop once the run has its verdict", on: true, signal: sigint, afterRun: true,
			statuses: []models.TestSetStatus{models.TestSetStatusPassed}, want: 0},
		{name: "switch on: a cancel that is no signal's", on: true, signal: 0, stopAt: "test-3",
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort}, want: 0},
		{name: "switch off: test-2 a demoted wrong answer, no stop", signal: sigint, demoted: true,
			statuses: []models.TestSetStatus{models.TestSetStatusInternalErr}, want: 1},
		{name: "switch off: test-2 a demoted wrong answer, the stop at test-3", signal: sigint, demoted: true, stopAt: "test-3",
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort, models.TestSetStatusInternalErr}, want: 1},
		{name: "switch off: test-2 a demoted wrong answer, the stop at test-3 of 4", signal: sigint, demoted: true, stopAt: "test-3", tests: 4,
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort, models.TestSetStatusInternalErr}, want: 1},
		{name: "switch off: every test judged so far, the stop at test-3 of 4", signal: sigint, stopAt: "test-3", tests: 4,
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort, models.TestSetStatusPassed}, want: 0},
		{name: "switch on: test-2's misses unread, the stop at test-3 of 4", on: true, signal: sigint, reads: unreadAtTest2, stopAt: "test-3", tests: 4,
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort}, want: 130},
		{name: "switch off: test-2 failed before the stop", signal: sigint, wrong: true, stopAt: "test-3",
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort, models.TestSetStatusFailed}, want: 1},
		{name: "switch off: every test judged so far, the stop at test-3", signal: sigint, stopAt: "test-3",
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort, models.TestSetStatusPassed}, want: 0},
		{name: "switch off: test-2's misses unread, the stop at test-3", signal: sigint, reads: unreadAtTest2, stopAt: "test-3",
			statuses: []models.TestSetStatus{models.TestSetStatusUserAbort, models.TestSetStatusPassed}, want: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			setSwitch(t, c.on)
			n := c.tests
			if n == 0 {
				n = 3
			}
			h := newPartialRunHarness(t, n, 0)
			hooks := prHooks{}
			if c.wrong {
				hooks.wrongBody = map[string]bool{"test-2": true}
			}
			if c.demoted {
				demoteTest2(h, &hooks, n)
			}
			code, status := startStopped(t, h, c.signal, func(stop func()) {
				h.replayer.hookImpl = stopHooks{TestHooks: hooks, stop: stop, send: c.stopAt, afterRun: c.afterRun}
				if c.reads != nil {
					h.instr.mockErrorsFor = c.reads(stop)
				}
				if c.atEnd {
					h.report.onRead = stop
				}
			})
			if !slices.Contains(c.statuses, status) {
				t.Fatalf("the set is %s; the case needs it one of %v", status, c.statuses)
			}
			if code != c.want {
				t.Fatalf("the set is %s, and the run exits %d; want %d", status, code, c.want)
			}
		})
	}
}

// A run's marks of tests with no verdict are its own: a run after one that
// demoted a wrong answer, on the same replayer, is not failed by that one's
// when its user stops it with nothing wrong (switch off).
func TestAStoppedRunIsNotFailedByAnEarlierRunsNoVerdict(t *testing.T) {
	setSwitch(t, false)
	h := newPartialRunHarness(t, 3, 0)
	hooks := prHooks{}
	demoteTest2(h, &hooks, 3)
	if code, status := startStopped(t, h, 2, func(func()) { h.replayer.hookImpl = hooks }); code != 1 || status != models.TestSetStatusInternalErr {
		t.Fatalf("the first run exits %d with its set %s; the case needs 1 and INTERNAL_ERR", code, status)
	}
	if got := h.replayer.noVerdicts(); fmt.Sprint(got) != "map[test-set-0:[test-2]]" {
		t.Fatalf("the first run marks %v; the case needs test-2 of test-set-0", got)
	}
	h.report.mu.Lock()
	h.report.results, h.report.report = nil, nil
	h.report.mu.Unlock()
	code, status := startStopped(t, h, 2, func(stop func()) { h.replayer.hookImpl = stopHooks{TestHooks: prHooks{}, stop: stop, send: "test-3"} })
	if got := h.replayer.noVerdicts(); len(got) != 0 {
		t.Fatalf("the second run, with every answer right, marks %v", got)
	}
	if code != 0 {
		t.Fatalf("the second run, stopped with nothing wrong, exits %d with its set %s; want 0", code, status)
	}
}
