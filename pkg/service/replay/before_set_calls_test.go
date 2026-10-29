package replay

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// F2 and item (b): a call the application makes as it starts, before its
// set's first test, that no mock answered is the set's, no test's. Under the
// proven switch the agent hands it to the first test's read marked as made
// before the set (BeforeSet); it is background work, failing nothing and
// listed for the set with where it was made (BackgroundWhy), unless the word
// rule says what changed in the first test may have come from its recorded
// answer (flowGuard): then it is that test's miss (ReachesTest). It failed
// the set until item (b) (founder 2026-10-29).

// beforeFirst is the first test's read: the write the application made as it
// started, which no mock answered, handed over as the set's.
func beforeFirst() ([]models.UnmatchedCall, error) {
	return []models.UnmatchedCall{{Protocol: "Mongo", ActualSummary: "insert on events", Destination: "mongo:27017",
		Kind: models.MissOutsideTest, BeforeSet: true}}, nil
}

func TestRunTestSet_ACallBeforeTheFirstTestThatNoMockAnsweredIsBackground(t *testing.T) {
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
	if status != models.TestSetStatusPassed || rep.Status != string(models.TestSetStatusPassed) {
		t.Fatalf("status %s (report %s, reason %q); a call before the first test that no mock answered is background work", status, rep.Status, rep.FailureReason)
	}
	if len(rep.UnclaimedCalls) != 0 {
		t.Fatalf("unclaimed calls %+v; want none", rep.UnclaimedCalls)
	}
	if len(rep.BackgroundCalls) != 1 || rep.BackgroundCalls[0].ActualSummary != "insert on events" || rep.BackgroundCalls[0].BackgroundWhy != madeBeforeFirst {
		t.Fatalf("background calls %+v; want the insert, saying it was made before the first test", rep.BackgroundCalls)
	}
}

// afterLastBackground is the set end's read: a call the agent saw made after
// it read the last test's misses, which no mock answered.
func afterLastBackground() ([]models.UnmatchedCall, error) {
	return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /publish", Destination: "broker:9099",
		Kind: models.MissOutsideTest, Background: true, ClosestMock: "mock-12"}}, nil
}

func TestRunTestSet_ACallAfterTheLastTestThatNoMockAnsweredIsBackground(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.instr.mockErrorsFor = misses(nil, nil, afterLastBackground)

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s (reason %q); a call made after the last test that no mock answered is background work", status, h.report.report.FailureReason)
	}
	rep := h.report.report
	if len(rep.UnclaimedCalls) != 0 {
		t.Fatalf("unclaimed calls %+v; want none", rep.UnclaimedCalls)
	}
	if len(rep.BackgroundCalls) != 1 || rep.BackgroundCalls[0].BackgroundWhy != madeAfterLast {
		t.Fatalf("background calls %+v; want the publish, saying it was made after the last test", rep.BackgroundCalls)
	}
}

// Made before the first test, a call is background work; one filed late in
// the last test's window (not marked as made after its read) still fails the
// set, named alone.
func TestRunTestSet_ACallBeforeTheFirstTestIsBackgroundBesideALateOne(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.instr.mockErrorsFor = misses(beforeFirst, nil, afterLast)

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s, want FAILED", status)
	}
	rep := h.report.report
	if len(rep.UnclaimedCalls) != 1 || rep.UnclaimedCalls[0].ActualSummary != "POST /publish" {
		t.Fatalf("unclaimed calls %+v; want the publish alone", rep.UnclaimedCalls)
	}
	if len(rep.BackgroundCalls) != 1 || rep.BackgroundCalls[0].ActualSummary != "insert on events" {
		t.Fatalf("background calls %+v; want the insert", rep.BackgroundCalls)
	}
	if strings.Contains(rep.FailureReason, "insert on events") || !strings.Contains(rep.FailureReason, "POST /publish") {
		t.Fatalf("failure reason %q; want the publish named and the insert not", rep.FailureReason)
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
	rest := b.take(context.Background(), append(calls, own), "test-1", nil)
	if len(rest) != 1 || rest[0].ActualSummary != "GET /own" {
		t.Fatalf("the test keeps %+v; want only its own call", rest)
	}
	if got := b.backgroundCalls(); len(got) != 1 || got[0].BackgroundWhy != madeBeforeFirst {
		t.Fatalf("the set lists %+v; want the insert", got)
	}
	b.reset(nil)
	if got := b.backgroundCalls(); len(got) != 0 {
		t.Fatalf("after the reset the set lists %+v; want nothing", got)
	}
}

// A set run again (as a run's next set is) starts with none of the previous
// run's calls made before its first test.
func TestRunTestSet_ACallBeforeTheFirstTestIsNotTheNextSetsToo(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.instr.mockErrorsFor = misses(beforeFirst)
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED", status)
	}
	h.instr.mockErrorsFor = misses()
	if status := h.runServing(t); status != models.TestSetStatusPassed {
		t.Fatalf("the next run of the set is %s (reason %q); the previous run's call before its first test is not its", status, h.report.report.FailureReason)
	}
	if got := h.report.report.BackgroundCalls; len(got) != 0 {
		t.Fatalf("the next run lists %+v; the previous run's call before its first test is not its", got)
	}
}
