package replay

import (
	"strings"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// F2: a call the application makes as it starts, before its set's first test,
// that no mock answered is the set's, no test's. Under the proven switch the
// agent hands it to the first test's read marked as made before the set
// (BeforeSet), and it fails the set, named, charging no test. It was dropped
// with a warning, and such a set passed.

// beforeFirst is the first test's read: the write the application made as it
// started, which no mock answered, handed over as the set's.
func beforeFirst() ([]models.UnmatchedCall, error) {
	return []models.UnmatchedCall{{Protocol: "Mongo", ActualSummary: "insert on events", Destination: "mongo:27017",
		Kind: models.MissOutsideTest, BeforeSet: true}}, nil
}

func TestRunTestSet_ACallBeforeTheFirstTestThatNoMockAnsweredFailsTheSet(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	var reads atomic.Int32
	h.instr.mockErrorsFor = counted(&reads, misses(beforeFirst))

	status := h.run(t)

	rep := h.report.report
	for _, r := range rep.Tests {
		if r.Status != models.TestStatusPassed {
			t.Fatalf("test %s is %s; the call before the first test is no test's", r.TestCaseID, r.Status)
		}
		if len(r.FailureInfo.UnmatchedCalls) != 0 {
			t.Fatalf("the call before the first test was charged to %s: %+v", r.TestCaseID, r.FailureInfo.UnmatchedCalls)
		}
	}
	if reads.Load() != 3 {
		t.Fatalf("%d reads of the agent's misses; want one per test and one after the last, no more", reads.Load())
	}
	if status != models.TestSetStatusFailed || rep.Status != string(models.TestSetStatusFailed) {
		t.Fatalf("status %s (report %s); a call before the first test that no mock answered must fail the set", status, rep.Status)
	}
	if len(rep.UnclaimedCalls) != 1 || rep.UnclaimedCalls[0].ActualSummary != "insert on events" || !rep.UnclaimedCalls[0].BeforeSet {
		t.Fatalf("unclaimed calls %+v; want the insert, made before the set", rep.UnclaimedCalls)
	}
	if reason := rep.FailureReason; !strings.Contains(reason, "before the set's first test") || !strings.Contains(reason, "Mongo insert on events to mongo:27017") {
		t.Fatalf("failure reason %q does not name the call", reason)
	}
	if h.replayer.notJudged("test-set-0") {
		t.Fatal("the set is marked not judged; it was judged, FAILED")
	}
}

// Made before the first test and after the last, both are named, and the set
// fails once.
func TestRunTestSet_CallsBeforeTheFirstTestAndAfterTheLastAreBothNamed(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.instr.mockErrorsFor = misses(beforeFirst, nil, afterLast)

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s, want FAILED", status)
	}
	rep := h.report.report
	if len(rep.UnclaimedCalls) != 2 {
		t.Fatalf("unclaimed calls %+v; want the insert and the publish", rep.UnclaimedCalls)
	}
	for _, want := range []string{"before the set's first test", "insert on events", "after the set's last test", "POST /publish"} {
		if !strings.Contains(rep.FailureReason, want) {
			t.Fatalf("failure reason %q does not say %q", rep.FailureReason, want)
		}
	}
}

// A DNS miss, or a connection that sent no request, made as the application
// started counts against no set either.
func TestRunTestSet_AControlPlaneMissBeforeTheFirstTestLeavesTheSetGreen(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.instr.mockErrorsFor = misses(func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{
			{Protocol: "DNS", ActualSummary: "A mongo.", BeforeSet: true, Kind: models.MissOutsideTest},
			{Protocol: "TCP", MatchPhase: models.MatchPhaseNoRequest, BeforeSet: true, Kind: models.MissOutsideTest},
		}, nil
	})
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s; a control-plane miss before the first test counts against no set", status)
	}
	if calls := h.report.report.UnclaimedCalls; len(calls) != 0 {
		t.Fatalf("unclaimed calls %+v; want none", calls)
	}
}

// What one set's application missed before its first test is not the next
// set's.
func TestBeforeSetCallsAreTheSetsOwn(t *testing.T) {
	var b setCalls
	calls, _ := beforeFirst()
	own := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /own"}
	rest := b.take(append(calls, own))
	if len(rest) != 1 || rest[0].ActualSummary != "GET /own" {
		t.Fatalf("the test keeps %+v; want only its own call", rest)
	}
	if got := b.failing(); len(got) != 1 {
		t.Fatalf("the set holds %+v; want the insert", got)
	}
	b.reset()
	if got := b.failing(); len(got) != 0 {
		t.Fatalf("after the reset the set holds %+v; want nothing", got)
	}
}

// A set run again (as a run's next set is) starts with none of the previous
// run's calls made before its first test.
func TestRunTestSet_ACallBeforeTheFirstTestIsNotTheNextSetsToo(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.instr.mockErrorsFor = misses(beforeFirst)
	if status := h.runServing(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s, want FAILED", status)
	}
	h.instr.mockErrorsFor = misses()
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("the next run of the set is %s (reason %q); the previous run's call before its first test is not its", status, h.report.report.FailureReason)
	}
}
