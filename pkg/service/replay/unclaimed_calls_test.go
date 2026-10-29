package replay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// P101: a call the application makes after its set's last test that no mock
// answered is a pool miss no later test takes. Under the proven switch it
// fails the set, named; with the switch off the run is what it was.

// counted counts the agent's GetMockErrors reads, and answers each as reads
// does.
func counted(n *atomic.Int32, reads func() ([]models.UnmatchedCall, error)) func() ([]models.UnmatchedCall, error) {
	return func() ([]models.UnmatchedCall, error) {
		n.Add(1)
		return reads()
	}
}

// afterLast is the read after the set's last test: the publish the app made
// once it had answered, which no mock answered.
func afterLast() ([]models.UnmatchedCall, error) {
	return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /publish", Destination: "broker:9099", Diff: "body: after 1-changed"}}, nil
}

func provenOn(t *testing.T, on bool) {
	t.Helper()
	if on {
		t.Setenv(provenswitch.Env, "1")
	} else {
		t.Setenv(provenswitch.Env, "")
	}
}

func TestRunTestSet_ACallAfterTheLastTestThatNoMockAnsweredFailsTheSet(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch=%v", on), func(t *testing.T) {
			provenOn(t, on)
			h := newPartialRunHarness(t, 2, 0)
			var reads atomic.Int32
			h.instr.mockErrorsFor = counted(&reads, misses(nil, nil, afterLast))

			status := h.run(t)

			rep := h.report.report
			for _, r := range rep.Tests {
				if r.Status != models.TestStatusPassed {
					t.Fatalf("test %s is %s; the call after the last test is no test's", r.TestCaseID, r.Status)
				}
			}
			if !on {
				// Switch off: as before, no read after the last test, the set
				// PASSED.
				if status != models.TestSetStatusPassed || len(rep.UnclaimedCalls) != 0 || reads.Load() != 2 {
					t.Fatalf("switch off: status %s, unclaimed %v, %d reads; want PASSED, none, 2 (one per test) as before", status, rep.UnclaimedCalls, reads.Load())
				}
				return
			}
			if reads.Load() != 3 {
				t.Fatalf("%d reads of the agent's misses; want one per test and one after the last", reads.Load())
			}
			if status != models.TestSetStatusFailed || rep.Status != string(models.TestSetStatusFailed) {
				t.Fatalf("status %s (report %s); a call after the last test that no mock answered must fail the set", status, rep.Status)
			}
			if len(rep.UnclaimedCalls) != 1 || rep.UnclaimedCalls[0].ActualSummary != "POST /publish" {
				t.Fatalf("unclaimed calls %+v; want the publish", rep.UnclaimedCalls)
			}
			if reason := rep.FailureReason; !strings.Contains(reason, "in the set's last test") || !strings.Contains(reason, "read only at the set's end") || !strings.Contains(reason, "POST /publish to broker:9099") {
				t.Fatalf("failure reason %q does not name the call", reason)
			}
			if h.replayer.notJudged("test-set-0") {
				t.Fatal("the set is marked not judged; it was judged, FAILED")
			}
		})
	}
}

// A test's own miss still fails it, and the call after the last test is
// named beside it.
func TestRunTestSet_ACallAfterTheLastTestIsNamedOnAFailedSet(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.instr.mockErrorsFor = misses(func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /own"}}, nil
	}, nil, afterLast)

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s, want FAILED", status)
	}
	rep := h.report.report
	if len(rep.UnclaimedCalls) != 1 || !strings.Contains(rep.FailureReason, "POST /publish") {
		t.Fatalf("unclaimed %+v, reason %q; the call after the last test must be named on a set that failed already", rep.UnclaimedCalls, rep.FailureReason)
	}
	for _, r := range rep.Tests {
		for _, c := range r.FailureInfo.UnmatchedCalls {
			if c.ActualSummary == "POST /publish" {
				t.Fatalf("the call after the last test was charged to %s", r.TestCaseID)
			}
		}
	}
}

// A DNS miss, or a connection that sent no request, never counts against a
// test, nor against a set.
func TestRunTestSet_AControlPlaneMissAfterTheLastTestLeavesTheSetGreen(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.instr.mockErrorsFor = misses(nil, nil, func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{
			{Protocol: "DNS", ActualSummary: "A broker."},
			{Protocol: "TCP", MatchPhase: models.MatchPhaseNoRequest},
		}, nil
	})
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED", status)
	}
	if calls := h.report.report.UnclaimedCalls; len(calls) != 0 {
		t.Fatalf("unclaimed calls %+v, want none", calls)
	}
}

// A read after the last test that cannot vouch for every call leaves the set
// with no verdict; what it did read is still reported.
func TestRunTestSet_AnUnreadableReadAfterTheLastTestLeavesTheSetUnjudged(t *testing.T) {
	for _, read := range []struct {
		name string
		fn   func() ([]models.UnmatchedCall, error)
		kept int
	}{
		{"failed read", func() ([]models.UnmatchedCall, error) { return nil, errors.New("get mock errors returned status 500") }, 0},
		{"incomplete read", func() ([]models.UnmatchedCall, error) {
			calls, _ := afterLast()
			return calls, fmt.Errorf("%w (agent: keploy's error drain stopped)", models.ErrUnmatchedCallsUnread)
		}, 1},
	} {
		t.Run(read.name, func(t *testing.T) {
			provenOn(t, true)
			h := newPartialRunHarness(t, 2, 0)
			h.instr.mockErrorsFor = misses(nil, nil, read.fn)
			if status := h.run(t); status != models.TestSetStatusInternalErr {
				t.Fatalf("status %s; a set whose calls after its last test could not all be read has no verdict", status)
			}
			rep := h.report.report
			if !strings.Contains(rep.FailureReason, "after the set's last test") {
				t.Fatalf("failure reason %q does not say what could not be read", rep.FailureReason)
			}
			if len(rep.UnclaimedCalls) != read.kept {
				t.Fatalf("unclaimed calls %+v; want the %d the read returned", rep.UnclaimedCalls, read.kept)
			}
			if !h.replayer.notJudged("test-set-0") {
				t.Fatal("the set is not recorded as not judged")
			}
		})
	}
}

// unclaimedWhy names every call, with its protocol and where it went.
func TestUnclaimedWhy(t *testing.T) {
	if got := unclaimedWhy(nil); got != "" {
		t.Fatalf("no calls: %q, want empty", got)
	}
	got := unclaimedWhy([]models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "POST /publish", Destination: "broker:9099"},
		{Protocol: "TCP", Destination: "127.0.0.1:9098"},
	})
	for _, want := range []string{"2 call(s)", "HTTP POST /publish to broker:9099", "TCP to 127.0.0.1:9098"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q does not contain %q", got, want)
		}
	}
}

// A hook that quarantines every test of the set does not clear the call the
// app made after its last test: that is evidence against the set, not a test,
// as the evidence floor keeps it against a test.
func TestRunTestSet_ACallAfterTheLastTestFailsASetWhoseTestsWereAllQuarantined(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.hookImpl = verdictHooks{set: func(rs []models.TestResult) {
		for i := range rs {
			rs[i].Status = models.TestStatusIgnored
		}
	}}
	h.instr.mockErrorsFor = misses(nil, nil, afterLast)
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s, want FAILED: the call after the last test is no test's to quarantine", status)
	}
	if calls := h.report.report.UnclaimedCalls; len(calls) != 1 {
		t.Fatalf("unclaimed calls %+v, want the publish", calls)
	}
}

// haltsAfterLastTest is the stand-in agent and application, whose
// application exits on its own once the last test's calls are read, before
// the set's end stops it: the read is answered, and the app watcher has the
// set's status by the time it returns.
type haltsAfterLastTest struct {
	*prInstr
	reads atomic.Int32
	at    int32
}

func (f *haltsAfterLastTest) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	if f.reads.Add(1) != f.at {
		return f.prInstr.GetMockErrors(ctx)
	}
	close(f.appStopped)
	<-ctx.Done() // the app watcher's stop: the set's status is recorded first
	return nil, nil
}

// An application that exits on its own after the set's last test ends the
// set APP_HALTED: the set's end, which under the switch stops the app and
// then reads what it did after its last test, reads nothing of an app that
// stopped itself, and does not take it for a set whose calls could not be
// read (INTERNAL_ERR, not judged), which would also keep the run going past
// an app that exited.
func TestRunTestSet_AnAppThatExitsAfterTheLastTestIsHalted(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	instr := &haltsAfterLastTest{prInstr: h.instr, at: 2} // the last test's read
	h.replayer.instrumentation = instr
	status := h.run(t)
	if instr.reads.Load() != 2 {
		t.Fatalf("%d reads of the agent's misses, want 2: one per test, and none at the end of a set whose app exited", instr.reads.Load())
	}
	if status != models.TestSetStatusAppHalted {
		t.Fatalf("status %s, want APP_HALTED: the app exited after the last test", status)
	}
	if h.replayer.notJudged("test-set-0") {
		t.Fatal("a set whose app exited is marked not judged, and the run would go on past it")
	}
}

// stopsAtSetEnd is the stand-in agent and application, whose user stops the
// run (Ctrl+C) as the set's end reads the calls made after its last test:
// the read is cut short by the stop (its ctx is cancelled), as a real agent
// client's is.
type stopsAtSetEnd struct {
	*prInstr
	reads atomic.Int32
	at    int32
	stop  func()
}

func (f *stopsAtSetEnd) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	if f.reads.Add(1) != f.at {
		return f.prInstr.GetMockErrors(ctx)
	}
	f.stop()
	<-ctx.Done()
	return nil, ctx.Err()
}

// A read of the set's end that the user's stop cuts short is the stop's: the
// set is USER_ABORT, not a set whose calls could not be read (not judged).
func TestRunTestSet_AStopThatCutsTheSetEndReadShortIsTheStops(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	instr := &stopsAtSetEnd{prInstr: h.instr, at: 3} // a read per test, then the set's
	instr.stop = func() { h.cancel() }
	h.replayer.instrumentation = instr
	status := h.run(t)
	if instr.reads.Load() != 3 {
		t.Fatalf("fixture: %d reads of the agent's misses, want 3", instr.reads.Load())
	}
	if status != models.TestSetStatusUserAbort {
		t.Fatalf("status %s, want USER_ABORT: the user stopped the run as the set's end read", status)
	}
	if h.replayer.notJudged("test-set-0") {
		t.Fatal("a set the user stopped is marked not judged")
	}
}
