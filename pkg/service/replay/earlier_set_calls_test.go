package replay

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// lateOfSetA is set B's first test's read: set A's test-2's own call, which
// keploy decided only after set A had ended, naming test-2 as an earlier
// set's test (models.UnmatchedCall.EarlierSetTest), as the agent hands it.
func lateOfSetA() ([]models.UnmatchedCall, error) {
	return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /orders", Destination: "orders:80",
		Kind: models.MissOutsideTest, EarlierSetTest: "test-2"}}, nil
}

// A call set A's last test made that keploy decided only after set A had
// ended reaches set B's first read naming its test. It is no call of set B,
// nor of set B's tests: set B passes, charging none of them, and the run
// fails with it, naming the call, test-2 and set A. Taken as made before set
// B's first test (models.UnmatchedCall.BeforeSet), it would fail set B, which
// did not make it.
func TestRunTestSet_AnEarlierSetsTestsLateCallFailsTheRunNotTheSetThatReadIt(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	core, logs := observer.New(zap.ErrorLevel)
	h.replayer.logger = zap.New(core)
	h.set = "test-set-a"
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("set A is %s (reason %q), want PASSED", status, h.report.report.FailureReason)
	}
	h.instr.mockErrorsFor = misses(lateOfSetA)
	h.set = "test-set-b"
	status := h.runServing(t)

	rep := h.report.report
	if status != models.TestSetStatusPassed || rep.Status != string(models.TestSetStatusPassed) {
		t.Fatalf("set B is %s (report %s, reason %q); set A's test-2's call is no call of set B", status, rep.Status, rep.FailureReason)
	}
	for _, r := range rep.Tests {
		if r.Status != models.TestStatusPassed || len(r.FailureInfo.UnmatchedCalls) != 0 {
			t.Fatalf("set B's %s is %s with %+v; set A's test-2's call was charged to it", r.TestCaseID, r.Status, r.FailureInfo.UnmatchedCalls)
		}
	}
	if len(rep.UnclaimedCalls) != 0 {
		t.Fatalf("set B's unclaimed calls %+v; set A's test-2's call is not set B's", rep.UnclaimedCalls)
	}
	late := h.replayer.earlierSetCalls.failing()
	if len(late) != 1 || late[0].set != "test-set-a" || late[0].call.EarlierSetTest != "test-2" {
		t.Fatalf("the run holds %+v; want set A's test-2's call, made in test-set-a", late)
	}
	said := logs.FilterMessageSnippet("an earlier test set made a call that no mock answered").
		FilterField(zap.String("made-in-test", "test-2")).
		FilterField(zap.String("made-in-test-set", "test-set-a")).
		FilterField(zap.String("read-in-test-set", "test-set-b"))
	if said.Len() != 1 {
		t.Fatalf("the call was said %d times naming test-2, test-set-a and test-set-b; want once: %+v", said.Len(), logs.All())
	}

	// The run's verdict: red, whatever its sets', naming the call, the test
	// and the set it was made in.
	if h.replayer.passesEarlierSetCalls(true) {
		t.Fatal("the run passes with set A's test-2's call no mock answered")
	}
	why := logs.FilterMessageSnippet("test run failed: the application made 1 call(s) in an earlier test set").All()
	if len(why) != 1 || !strings.Contains(why[0].Message, "HTTP POST /orders to orders:80 (made in test-2 of test-set-a)") {
		t.Fatalf("the run's failure said %+v; want the call, test-2 and test-set-a named once", why)
	}
}

// A run's verdict counts only the calls of earlier sets' tests that count
// against a test, and only the run's: with none it stands, a DNS one leaves
// it, and a new run forgets the previous run's.
func TestEarlierSetCallsFailOnlyTheRunTheyWereReadIn(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	r := &Replayer{logger: zap.New(core)}
	if !r.passesEarlierSetCalls(true) || r.passesEarlierSetCalls(false) {
		t.Fatal("with no earlier set's call the run's verdict changed")
	}
	r.earlierSetCalls.begin("test-set-a")
	r.earlierSetCalls.begin("test-set-b")
	own := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /own"}
	rest := r.takeEarlierSetCalls([]models.UnmatchedCall{
		own,
		{Protocol: "DNS", ActualSummary: "A orders.", Kind: models.MissOutsideTest, EarlierSetTest: "test-2"},
	})
	if len(rest) != 1 || rest[0].ActualSummary != "GET /own" {
		t.Fatalf("the read keeps %+v; want the test's own call alone", rest)
	}
	if !r.passesEarlierSetCalls(true) {
		t.Fatal("a DNS miss of an earlier set's test failed the run")
	}
	if n := logs.Len(); n != 0 {
		t.Fatalf("a DNS miss of an earlier set's test was said %d times as failing the run: %+v", n, logs.All())
	}
	if got := r.takeEarlierSetCalls([]models.UnmatchedCall{own}); len(got) != 1 || got[0].ActualSummary != "GET /own" {
		t.Fatalf("a read with no earlier set's call keeps %+v; want it whole", got)
	}
	calls, _ := lateOfSetA()
	r.takeEarlierSetCalls(calls)
	if r.passesEarlierSetCalls(true) {
		t.Fatal("the run passes with an earlier set's test's call no mock answered")
	}
	if got := r.earlierSetCalls.failing(); len(got) != 1 || got[0].set != "test-set-a" {
		t.Fatalf("the run holds %+v; want the call, made in test-set-a, the set run before test-set-b", got)
	}
	r.earlierSetCalls.reset()
	if !r.passesEarlierSetCalls(true) {
		t.Fatal("a new run fails for the previous run's call")
	}
	r.earlierSetCalls.begin("test-set-c")
	r.takeEarlierSetCalls(calls)
	if got := r.earlierSetCalls.failing(); len(got) != 1 || got[0].set != "" {
		t.Fatalf("the run holds %+v; a call read in a run's first set names no set before it", got)
	}
	if why := earlierSetCallsWhy(r.earlierSetCalls.failing()); !strings.Contains(why, "(made in test-2 of an earlier test set)") {
		t.Fatalf("the run's failure says %q; want test-2 of an earlier test set", why)
	}
}

// A set whose first test's read did not return the call hands it over at its
// end read: the run's still, not the set's.
func TestRunTestSet_AnEarlierSetsTestsLateCallAtTheSetsEndFailsTheRunNotTheSet(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.set = "test-set-a"
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("set A is %s (reason %q), want PASSED", status, h.report.report.FailureReason)
	}
	// Set B's two tests read nothing; its end read returns the call.
	h.instr.mockErrorsFor = misses(nil, nil, lateOfSetA)
	h.set = "test-set-b"
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("set B is %s (reason %q); set A's test-2's call is no call of set B", status, h.report.report.FailureReason)
	}
	if calls := h.report.report.UnclaimedCalls; len(calls) != 0 {
		t.Fatalf("set B's unclaimed calls %+v; set A's test-2's call is not set B's", calls)
	}
	if late := h.replayer.earlierSetCalls.failing(); len(late) != 1 || late[0].set != "test-set-a" {
		t.Fatalf("the run holds %+v; want set A's test-2's call, made in test-set-a", late)
	}
}

// setsReportDB is the harness's report store, keeping each set's report status
// by its set's name: a run of two sets writes two reports.
type setsReportDB struct {
	*prReportDB
	statuses map[string]string
}

func (f *setsReportDB) InsertReport(ctx context.Context, run, set string, rep *models.TestReport) error {
	f.mu.Lock()
	f.statuses[set] = rep.Status
	f.mu.Unlock()
	return f.prReportDB.InsertReport(ctx, run, set, rep)
}

// startEarlierSetRun runs Start over test-set-0 and test-set-1, two sets of the
// harness's two tests, with keploy test's settings, and returns the code the
// process exits with and each set's report status. Each run starts with its
// sets' statuses cleared.
func startEarlierSetRun(t *testing.T, h *prRun, reports *setsReportDB) (int, map[string]string) {
	t.Helper()
	reports.mu.Lock()
	reports.statuses = map[string]string{}
	reports.mu.Unlock()
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })
	utils.ErrCode = 0
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	if err := h.replayer.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	reports.mu.Lock()
	defer reports.mu.Unlock()
	return utils.ErrCode, maps.Clone(reports.statuses)
}

// keploy test, over a whole run: a call test-set-0's test-2 made, which keploy
// decided only after test-set-0 had ended, reaches test-set-1's first read
// naming its test. Both sets pass, as neither is charged with it, and the
// run exits non-zero for it, naming the call, test-2 and test-set-0: removed
// from every set's report, it is in the run's verdict alone. A call the
// parser also held past its window (Held) is taken the same way, not as the
// reading set's held call: it was made in an earlier set's test. A second
// run on the same Replayer, with no such call, passes: the first run's call
// is not the second's.
func TestStart_AnEarlierSetsTestsLateCallFailsTheRunWithEverySetPassed(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("held=%v", held), func(t *testing.T) {
			provenOn(t, true)
			h := newPartialRunHarness(t, 2, 0)
			reports := &setsReportDB{prReportDB: h.report}
			h.replayer.reportDB = reports
			h.replayer.testDB = twoPRSetsDB{&prTestDB{cases: h.cases}}
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1
			core, logs := observer.New(zap.ErrorLevel)
			h.replayer.logger = zap.New(core)
			// The first read of test-set-1 returns test-set-0's test-2's call.
			var handed atomic.Bool
			h.instr.mockErrorsFor = func() ([]models.UnmatchedCall, error) {
				if h.replayer.earlierSetCalls.reading() != "test-set-1" || handed.Swap(true) {
					return nil, nil
				}
				calls, _ := lateOfSetA()
				calls[0].Held = held
				return calls, nil
			}

			code, statuses := startEarlierSetRun(t, h, reports)
			if !handed.Load() {
				t.Fatal("test-set-1 read nothing: the case needs its first read to return the call")
			}
			for _, set := range []string{"test-set-0", "test-set-1"} {
				if statuses[set] != string(models.TestSetStatusPassed) {
					t.Fatalf("%s is %q (all %v); neither set is charged with test-set-0's test-2's call", set, statuses[set], statuses)
				}
			}
			if code == 0 {
				t.Fatal("the run exits 0 with test-set-0's test-2's call no mock answered")
			}
			why := logs.FilterMessageSnippet("test run failed: the application made 1 call(s) in an earlier test set").All()
			if len(why) != 1 || !strings.Contains(why[0].Message, "HTTP POST /orders to orders:80 (made in test-2 of test-set-0)") {
				t.Fatalf("the run's failure said %+v; want the call, test-2 and test-set-0 named once", why)
			}

			// The same Replayer runs again, and no read returns such a call.
			h.instr.mockErrorsFor = nil
			code, statuses = startEarlierSetRun(t, h, reports)
			if statuses["test-set-0"] != string(models.TestSetStatusPassed) || statuses["test-set-1"] != string(models.TestSetStatusPassed) {
				t.Fatalf("the second run's sets are %v; want both PASSED", statuses)
			}
			if code != 0 {
				t.Fatalf("the second run, with no earlier set's call, exits %d: the first run's call failed it", code)
			}
		})
	}
}

// A call of an earlier set made while no test's view named one (between two
// of its tests, or after its last), which keploy decided only after that set
// ended, reaches the run's reads naming no test and how many sets back it was
// made (EarlierSet): the read does not keep it as its set's or its test's, and
// the run fails with it, naming the set the agent counts back to and no test.
// A count past the sets the run began names none.
func TestAnEarlierSetsCallOfNoTestFailsTheRunNamingTheSetItWasMadeIn(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	r := &Replayer{logger: zap.New(core)}
	for _, set := range []string{"test-set-a", "test-set-b", "test-set-c"} {
		r.earlierSetCalls.begin(set)
	}
	own := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /own"}
	twoBack := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /between-tests-of-a", Kind: models.MissOutsideTest, EarlierSet: 2}
	oneBack := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /after-the-last-of-b", Kind: models.MissOutsideTest, EarlierSet: 1}
	tooFar := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /before-the-run", Kind: models.MissOutsideTest, EarlierSet: 3}
	rest := r.takeEarlierSetCalls([]models.UnmatchedCall{twoBack, own, oneBack, tooFar})
	if len(rest) != 1 || rest[0].ActualSummary != "GET /own" {
		t.Fatalf("the read keeps %+v; want the running set's own call alone", rest)
	}
	if r.passesEarlierSetCalls(true) {
		t.Fatal("the run passes with an earlier set's call of no test that no mock answered")
	}
	got := r.earlierSetCalls.failing()
	if len(got) != 3 || got[0].set != "test-set-a" || got[1].set != "test-set-b" || got[2].set != "" {
		t.Fatalf("the run holds %+v; want the calls made in test-set-a, test-set-b and no set the run began", got)
	}
	why := earlierSetCallsWhy(got)
	for _, want := range []string{"(made in no test (between two of its tests, or after its last) of test-set-a)", "(made in no test (between two of its tests, or after its last) of test-set-b)", "of an earlier test set)"} {
		if !strings.Contains(why, want) {
			t.Fatalf("the run's failure says %q; want %q", why, want)
		}
	}
	if n := logs.FilterField(zap.String("made-in-test-set", "test-set-a")).FilterField(zap.String("read-in-test-set", "test-set-c")).Len(); n != 1 {
		t.Fatalf("the call made in test-set-a was said %d times as failing the run, read in test-set-c; want once: %+v", n, logs.All())
	}
}
