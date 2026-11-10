package replay

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
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
	said := logs.FilterMessageSnippet("a test of an earlier test set made a call that no mock answered").
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
	why := logs.FilterMessageSnippet("test run failed: the application made 1 call(s) in a test of an earlier test set").All()
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
