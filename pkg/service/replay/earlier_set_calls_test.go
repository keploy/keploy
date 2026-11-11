package replay

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	rec := &earlierSetCallRecorder{TestHooks: h.replayer.GetTestHooks()}
	h.replayer.SetTestHooks(rec)
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
	// The hooks are told it, made in set A and read in set B: a judge reads
	// it there, since no set's report holds it.
	if told := rec.all(); len(told) != 1 || told[0].set != "test-set-a" || told[0].readIn != "test-set-b" || told[0].call.EarlierSetTest != "test-2" {
		t.Fatalf("the hooks were told %+v; want set A's test-2's call, read in set B", told)
	}
	// Said once, in the documented form of a call outside every test, named
	// under the test and the set it was made in.
	said := logs.FilterMessageSnippet("an earlier test set made a call that no mock answered").All()
	if len(said) != 1 {
		t.Fatalf("the call was said %d times; want once: %+v", len(said), logs.All())
	}
	f := said[0].ContextMap()
	for k, want := range map[string]string{
		"kind": models.KindOutsideEveryTest, "made_in": "test-2", "test-set": "test-set-a", "read-in-test-set": "test-set-b",
		"protocol": "HTTP", "call": "POST /orders", "destination": "orders:80", "after": "", "before": "", "closest": "",
		"facts": "no recording of this call in the set",
		"next_step": "record the test set again in its place: rm -rf -- " + utils.ShellWord(filepath.Join(h.replayer.config.Path, "test-set-a")) +
			" && keploy record --path " + utils.ShellWord(filepath.Dir(h.replayer.config.Path)) + " --metadata name=test-set-a",
	} {
		if got := f[k]; got != want {
			t.Errorf("line field %s = %v; want %q", k, got, want)
		}
	}

	// The run's verdict: red, whatever its sets', naming the call, the test
	// and the set it was made in.
	if h.replayer.passesEarlierSetCalls(true) {
		t.Fatal("the run passes with set A's test-2's call no mock answered")
	}
	why := logs.FilterMessageSnippet("test run failed: the application made 1 call(s) in an earlier test set").All()
	if len(why) != 1 || !strings.Contains(why[0].Message, "HTTP POST /orders to orders:80 (made in test-2 of test-set-a)") ||
		!strings.Contains(why[0].Message, "("+models.KindOutsideEveryTest+")") {
		t.Fatalf("the run's failure said %+v; want the call, test-2 and test-set-a named once, as outside every test", why)
	}
}

// earlierSetCallRecorder is the run's hooks with EarlierSetCallHook: what a
// judge is told of the calls of earlier sets' tests that fail the run.
type earlierSetCallRecorder struct {
	TestHooks
	mu   sync.Mutex
	told []earlierSetCallTold
}

type earlierSetCallTold struct {
	set, readIn string
	call        models.UnmatchedCall
}

func (h *earlierSetCallRecorder) EarlierSetCall(set, readIn string, call models.UnmatchedCall) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.told = append(h.told, earlierSetCallTold{set: set, readIn: readIn, call: call})
}

func (h *earlierSetCallRecorder) all() []earlierSetCallTold {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]earlierSetCallTold(nil), h.told...)
}

// A run's verdict counts only the calls of earlier sets' tests that count
// against a test, and only the run's: with none it stands, a DNS one leaves
// it, and a new run forgets the previous run's. The hooks are told each that
// fails the run, with the set it was made in and the set whose read returned
// it, and of no other call.
func TestEarlierSetCallsFailOnlyTheRunTheyWereReadIn(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	rec := &earlierSetCallRecorder{}
	r := &Replayer{logger: zap.New(core), hookImpl: rec}
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
	if told := rec.all(); len(told) != 0 {
		t.Fatalf("the hooks were told %+v; a DNS miss of an earlier set's test fails no run", told)
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
	if told := rec.all(); len(told) != 1 || told[0].set != "test-set-a" || told[0].readIn != "test-set-b" ||
		told[0].call.EarlierSetTest != "test-2" || told[0].call.ActualSummary != "POST /orders" {
		t.Fatalf("the hooks were told %+v; want test-2's POST /orders, made in test-set-a, read in test-set-b", told)
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
	if told := rec.all(); len(told) != 2 || told[1].set != "" || told[1].readIn != "test-set-c" {
		t.Fatalf("the hooks were told %+v; want the call read in a run's first set, test-set-c, naming no set before it", told)
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
	// None has a time: each is unplaced, failing the run all the same.
	for _, want := range []string{"(made in no test of test-set-a, at a time keploy cannot place among its tests)", "(made in no test of test-set-b, at a time keploy cannot place among its tests)", "(made in no test of an earlier test set, at a time keploy cannot place among its tests)"} {
		if !strings.Contains(why, want) {
			t.Fatalf("the run's failure says %q; want %q", why, want)
		}
	}
	// Said in the documented form of a call outside every test: kind, the set
	// it was made in, no test, and the set whose read returned it.
	if n := logs.FilterField(zap.String("kind", models.KindOutsideEveryTest)).FilterField(zap.String("test-set", "test-set-a")).
		FilterField(zap.String("made_in", "")).FilterField(zap.String("read-in-test-set", "test-set-c")).Len(); n != 1 {
		t.Fatalf("the call made in test-set-a was said %d times as failing the run outside every test, of no test, read in test-set-c; want once: %+v", n, logs.All())
	}
}

// failingSetReportDB is the harness's report store, refusing the report of
// the set fail names: that set's RunTestSet ends after its mocks are staged,
// before any of its tests runs.
type failingSetReportDB struct {
	*prReportDB
	fail string
}

func (f *failingSetReportDB) InsertReport(ctx context.Context, run, set string, rep *models.TestReport) error {
	if set == f.fail {
		return fmt.Errorf("the report store refused %s's report", set)
	}
	return f.prReportDB.InsertReport(ctx, run, set, rep)
}

// The agent counts a test set at its mocks' staging, and names an earlier
// set's call by how many sets back from the reading one it was made in
// (models.UnmatchedCall.EarlierSet). The replay counts each set where the
// agent does, at its staging, so a set that ends after its staging and before
// any of its tests runs is counted on both sides: with sets [P, X, H, Y], H
// ended so, X's call of no test that Y's first read returns two sets back is
// named as X's, never as P's. The native and the docker compose path stage a
// set each in their own place, and each counts it there.
func TestASetEndedAfterItsStagingIsCountedAsTheAgentCountsIt(t *testing.T) {
	provenOn(t, true)
	for _, path := range []struct {
		name    string
		harness func(*testing.T) *prRun
	}{
		{"native", func(t *testing.T) *prRun { return newPartialRunHarness(t, 2, 0) }},
		{"compose", func(t *testing.T) *prRun { return composeHarness(t, &composeInstr{}) }},
	} {
		t.Run(path.name, func(t *testing.T) { aSetEndedAfterItsStagingIsCountedAsTheAgentCountsIt(t, path.harness(t)) })
	}
}

func aSetEndedAfterItsStagingIsCountedAsTheAgentCountsIt(t *testing.T, h *prRun) {
	core, logs := observer.New(zap.ErrorLevel)
	h.replayer.logger = zap.New(core)
	h.replayer.reportDB = &failingSetReportDB{prReportDB: h.report, fail: "test-set-h"}
	for _, set := range []string{"test-set-p", "test-set-x"} {
		h.set = set
		if status := h.runServing(t); status != models.TestSetStatusPassed {
			t.Fatalf("%s is %s (reason %q), want PASSED", set, status, h.report.report.FailureReason)
		}
	}
	stagings := func() int {
		h.instr.mu.Lock()
		defer h.instr.mu.Unlock()
		n := 0
		for _, p := range h.instr.allParams {
			if p.AfterTime.Equal(models.BaseTime) {
				n++
			}
		}
		return n
	}
	staged := stagings()
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	if status, err := h.replayer.RunTestSet(ctx, "test-set-h", "test-run-0", true); err == nil || status != models.TestSetStatusFailed {
		t.Fatalf("precondition: test-set-h ended %s, %v; want FAILED by its refused report", status, err)
	}
	if stagings() == staged {
		t.Fatal("precondition: test-set-h's mocks were not staged before its report was refused")
	}
	var handed atomic.Bool
	h.instr.mockErrorsFor = func() ([]models.UnmatchedCall, error) {
		if h.replayer.earlierSetCalls.reading() != "test-set-y" || handed.Swap(true) {
			return nil, nil
		}
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /orders", Destination: "orders:80",
			Kind: models.MissOutsideTest, EarlierSet: 2}}, nil
	}
	h.set = "test-set-y"
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("test-set-y is %s (reason %q); test-set-x's call is no call of test-set-y", status, h.report.report.FailureReason)
	}
	if !handed.Load() {
		t.Fatal("test-set-y read nothing: the case needs its first read to return the call")
	}
	if late := h.replayer.earlierSetCalls.failing(); len(late) != 1 || late[0].set != "test-set-x" {
		t.Fatalf("the run holds %+v; want test-set-x's call, two sets back from test-set-y", late)
	}
	said := logs.FilterMessageSnippet("an earlier test set made a call that no mock answered").
		FilterField(zap.String("kind", models.KindOutsideEveryTest)).
		FilterField(zap.String("test-set", "test-set-x")).
		FilterField(zap.String("read-in-test-set", "test-set-y"))
	if said.Len() != 1 {
		t.Fatalf("the call was said %d times naming test-set-x and test-set-y; want once: %+v", said.Len(), logs.All())
	}
}

// spanAt is a test's span from a to b, offsets from at0.
func spanAt(at0 time.Time, a, b time.Duration) testSpan {
	return testSpan{began: at0.Add(a), ended: at0.Add(b)}
}

// An earlier set's call of no test is placed among the tests of the attempt
// that made it by its time alone: before the first test that began after
// it, after the test that ended last at or before it, or both. A call keploy
// cannot place so (no time, no spans, a time inside a test's span) names no
// test there: keploy would guess the test it was made in. The tests are named
// against their order in time, so a placement by name order is told apart.
func TestPlaceByTimePlacesACallAmongOneAttemptsTests(t *testing.T) {
	at0 := time.Date(2026, 11, 11, 6, 0, 0, 0, time.UTC)
	spans := map[string]testSpan{
		"c-first":  spanAt(at0, 0, 10*time.Millisecond),
		"b-second": spanAt(at0, 20*time.Millisecond, 30*time.Millisecond),
		"a-third":  spanAt(at0, 40*time.Millisecond, 50*time.Millisecond),
	}
	for _, c := range []struct {
		name          string
		at            time.Time
		spans         map[string]testSpan
		after, before string
	}{
		{"before the first test", at0.Add(-5 * time.Millisecond), spans, "", "c-first"},
		{"between two tests", at0.Add(15 * time.Millisecond), spans, "c-first", "b-second"},
		{"between the last two", at0.Add(35 * time.Millisecond), spans, "b-second", "a-third"},
		{"as a test ends", at0.Add(10 * time.Millisecond), spans, "c-first", "b-second"},
		{"after the last test", at0.Add(60 * time.Millisecond), spans, "a-third", ""},
		{"inside a test", at0.Add(25 * time.Millisecond), spans, "", ""},
		{"as a test begins", at0.Add(20 * time.Millisecond), spans, "", ""},
		{"with no time", time.Time{}, spans, "", ""},
		{"with no spans", at0.Add(15 * time.Millisecond), nil, "", ""},
	} {
		after, before := placeByTime(c.at, c.spans)
		if after != c.after || before != c.before {
			t.Errorf("a call made %s is placed after %q and before %q; want after %q and before %q", c.name, after, before, c.after, c.before)
		}
	}
}

// An earlier set's call of no test is placed by the spans of the attempt of
// that set that made it, as the agent counts back: not by a later attempt's
// of the same set, nor by a test of another set run since, and the one of
// its test is named under its test, wherever its time falls. Each is said in
// the documented form, after and before naming that set's tests, and the
// run's failure says where each was made.
func TestAnEarlierSetsCallOfNoTestIsPlacedByTheAttemptThatMadeIt(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	r := &Replayer{logger: zap.New(core)}
	at0 := time.Date(2026, 11, 11, 6, 0, 0, 0, time.UTC)
	e := &r.earlierSetCalls
	e.noteSpan("test-set-a", "test-0", spanAt(at0, -20*time.Millisecond, -10*time.Millisecond))
	e.begin("test-set-a")
	e.noteSpan("test-set-a", "test-1", spanAt(at0, 0, 10*time.Millisecond))
	e.noteSpan("test-set-a", "test-2", spanAt(at0, 20*time.Millisecond, 30*time.Millisecond))
	e.noteSpan("test-set-z", "test-9", spanAt(at0, 12*time.Millisecond, 14*time.Millisecond))
	// test-set-a again: its second attempt's tests come later.
	e.begin("test-set-a")
	e.noteSpan("test-set-a", "test-1", spanAt(at0, 100*time.Millisecond, 110*time.Millisecond))
	e.noteSpan("test-set-a", "test-2", spanAt(at0, 120*time.Millisecond, 130*time.Millisecond))
	e.begin("test-set-b")
	between := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "DELETE /sessions", Destination: "auth:8080",
		Kind: models.MissOutsideTest, EarlierSet: 2, At: at0.Add(15 * time.Millisecond)}
	before := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /warm", Kind: models.MissOutsideTest,
		EarlierSet: 1, At: at0.Add(15 * time.Millisecond)}
	after := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /flush", Kind: models.MissOutsideTest,
		EarlierSet: 1, At: at0.Add(140 * time.Millisecond)}
	named := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /orders", Kind: models.MissOutsideTest,
		EarlierSet: 2, EarlierSetTest: "test-2", At: at0.Add(15 * time.Millisecond)}
	r.takeEarlierSetCalls([]models.UnmatchedCall{between, before, after, named})
	got := e.failing()
	if len(got) != 4 {
		t.Fatalf("the run holds %+v; want the four calls", got)
	}
	for i, want := range []struct{ set, after, before string }{
		{"test-set-a", "test-1", "test-2"},
		{"test-set-a", "", "test-1"},
		{"test-set-a", "test-2", ""},
		{"test-set-a", "", ""},
	} {
		if c := got[i]; c.set != want.set || c.call.MadeAfter != want.after || c.call.MadeBefore != want.before {
			t.Errorf("%s is held made in %q after %q and before %q; want made in %q after %q and before %q",
				c.call.ActualSummary, c.set, c.call.MadeAfter, c.call.MadeBefore, want.set, want.after, want.before)
		}
	}
	said := logs.FilterField(zap.String("kind", models.KindOutsideEveryTest)).FilterField(zap.String("test-set", "test-set-a")).
		FilterField(zap.String("call", "DELETE /sessions")).FilterField(zap.String("made_in", "")).
		FilterField(zap.String("after", "test-1")).FilterField(zap.String("before", "test-2")).
		FilterField(zap.String("read-in-test-set", "test-set-b"))
	if said.Len() != 1 {
		t.Fatalf("the call between test-1 and test-2 of test-set-a was said %d times in the documented form; want once: %+v", said.Len(), logs.All())
	}
	why := earlierSetCallsWhy(got)
	for _, want := range []string{
		"DELETE /sessions to auth:8080 (made in no test of test-set-a, after test test-1 ended and before test test-2 began)",
		"GET /warm (made in no test of test-set-a, before its first test test-1 began)",
		"POST /flush (made in no test of test-set-a, after its last test test-2 ended)",
		"POST /orders (made in test-2 of test-set-a)",
	} {
		if !strings.Contains(why, want) {
			t.Errorf("the run's failure says %q; want %q", why, want)
		}
	}
}

// Through keploy test's run of two sets: the run notes when it sent each of
// set A's tests' requests and read their answers, so a call set A made between
// its two tests, which keploy decided only after set A had ended, is placed
// between them when set B's first read returns it, and said so.
func TestRunTestSet_AnEarlierSetsCallOfNoTestIsPlacedAmongThatSetsTests(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	core, logs := observer.New(zap.ErrorLevel)
	h.replayer.logger = zap.New(core)
	h.set = "test-set-a"
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("set A is %s (reason %q), want PASSED", status, h.report.report.FailureReason)
	}
	e := &h.replayer.earlierSetCalls
	e.mu.Lock()
	spans := maps.Clone(e.spans[len(e.spans)-1])
	e.mu.Unlock()
	one, two := spans["test-1"], spans["test-2"]
	if one.ended.IsZero() || !two.began.After(one.ended) {
		t.Fatalf("set A's spans are %+v; want test-1 ended before test-2 began", spans)
	}
	// The agent's time carries no monotonic reading.
	at := one.ended.Add(two.began.Sub(one.ended) / 2).Round(0)
	h.instr.mockErrorsFor = misses(func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "DELETE /sessions", Destination: "auth:8080",
			Kind: models.MissOutsideTest, EarlierSet: 1, At: at}}, nil
	})
	h.set = "test-set-b"
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("set B is %s (reason %q); set A's call is no call of set B", status, h.report.report.FailureReason)
	}
	if late := e.failing(); len(late) != 1 || late[0].set != "test-set-a" || late[0].call.MadeAfter != "test-1" || late[0].call.MadeBefore != "test-2" {
		t.Fatalf("the run holds %+v; want set A's call, placed after test-1 and before test-2", late)
	}
	if n := logs.FilterField(zap.String("kind", models.KindOutsideEveryTest)).FilterField(zap.String("test-set", "test-set-a")).
		FilterField(zap.String("after", "test-1")).FilterField(zap.String("before", "test-2")).Len(); n != 1 {
		t.Fatalf("set A's call was said %d times placed between test-1 and test-2; want once: %+v", n, logs.All())
	}
	if h.replayer.passesEarlierSetCalls(true) {
		t.Fatal("the run passes with set A's call no mock answered")
	}
}
