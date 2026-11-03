package replay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// ownedTaker is an instrumentation that hands keploy test, in turn, each of
// takes's answers (OwnedLossTaker).
type ownedTaker struct {
	Instrumentation
	takes []ownedTake
}

type ownedTake struct {
	owned []models.OwnedCaptureLoss
	err   error
}

func (o *ownedTaker) TakeOwnedCaptureLosses(context.Context) ([]models.OwnedCaptureLoss, error) {
	if len(o.takes) == 0 {
		return nil, nil
	}
	t := o.takes[0]
	o.takes = o.takes[1:]
	return t.owned, t.err
}

// A loss a cut-short end owned reaches a verdict in keploy test, never none
// and never the test case open when it was counted: the gap's (a pool call
// between test cases cut short), the run's, an earlier set's and a passing
// test case's fail the set, naming it; a test case already failed, or with
// no verdict, has it named in its report. A loss that names no test case
// (the run's, after a session reset) is the set's even beside a failed test
// case whose file names it nothing. A take that failed is the set's error;
// an agent that has none to tell apart adds nothing.
func TestKeployTestReadsTheLossesACutShortEndOwned(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	loss := func(test string) models.OwnedCaptureLoss {
		return models.OwnedCaptureLoss{Test: test, Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: "from 127.0.0.1:50912, written " + test}}
	}
	results := []models.TestResult{
		{TestCaseID: "test-1", Status: models.TestStatusPassed},
		{TestCaseID: "test-2", Status: models.TestStatusFailed},
		{TestCaseID: "test-3", Status: models.TestStatusPassed, FailureInfo: models.FailureInfo{TestMocksUnread: true}},
		{TestCaseID: "test-4", Status: models.TestStatusPassed},
		{TestCaseID: "", Status: models.TestStatusFailed},
	}
	instr := &ownedTaker{takes: []ownedTake{
		{owned: []models.OwnedCaptureLoss{loss("between test cases"), loss("test-2")}},
		{err: models.ErrCaptureLossesUnsupported},
		{owned: []models.OwnedCaptureLoss{loss("test-1"), loss(""), loss("test-3"), loss("test-9")}},
	}}
	r := &Replayer{instrumentation: instr}
	for range 3 {
		r.takeOwnedLosses(context.Background())
	}
	set, err := r.applyOwnedLosses(results)
	if err != nil {
		t.Fatal(err)
	}
	want := models.CaptureLosses{loss("between test cases").Loss, loss("test-1").Loss, loss("").Loss, loss("test-9").Loss}
	if len(set) != len(want) {
		t.Fatalf("the set's: %v, want %v", set, want)
	}
	for i := range want {
		if set[i] != want[i] {
			t.Fatalf("the set's: %v, want %v", set, want)
		}
	}
	for i, n := range []int{0, 1, 1, 0, 0} {
		if got := len(results[i].FailureInfo.CaptureIncomplete); got != n {
			t.Fatalf("%s's report names %d owned losses, want %d", results[i].TestCaseID, got, n)
		}
	}
	if set, err := r.applyOwnedLosses(results); len(set) != 0 || err != nil {
		t.Fatalf("applied again: %v, %v; want each once", set, err)
	}

	failed := &Replayer{instrumentation: &ownedTaker{takes: []ownedTake{{owned: []models.OwnedCaptureLoss{loss("between test cases")}, err: errors.New("connection reset")}}}}
	failed.takeOwnedLosses(context.Background())
	if set, err := failed.applyOwnedLosses(nil); err == nil || len(set) != 1 {
		t.Fatalf("a take that failed: %v, %v; want its loss and the error", set, err)
	}
	// Its error is the set's it was applied at, and no later set's: a
	// later set whose takes answered is judged.
	if set, err := failed.applyOwnedLosses(nil); len(set) != 0 || err != nil {
		t.Fatalf("a take that failed, applied again: %v, %v; want each once", set, err)
	}

	t.Setenv(provenswitch.Env, "")
	off := &Replayer{instrumentation: &ownedTaker{takes: []ownedTake{{owned: []models.OwnedCaptureLoss{loss("test-1")}}}}}
	off.takeOwnedLosses(context.Background())
	if set, _ := off.applyOwnedLosses(results); len(set) != 0 {
		t.Fatalf("with the switch off: %v, want none taken", set)
	}
}

// prOwnedInstr is a run's instrumentation whose agent hands keploy test, at
// its nth take, owned (OwnedLossTaker), and counts the takes.
type prOwnedInstr struct {
	*prLossInstr
	at    int
	owned []models.OwnedCaptureLoss
	err   error
	takes int
}

func (o *prOwnedInstr) TakeOwnedCaptureLosses(context.Context) ([]models.OwnedCaptureLoss, error) {
	o.lmu.Lock()
	defer o.lmu.Unlock()
	o.takes++
	if o.takes == o.at {
		return o.owned, o.err
	}
	return nil, nil
}

// A pool call between test cases whose bytes wait was cut short, with those
// bytes lost later: the agent puts the loss aside as the gap's, in no count
// keploy test's per-test or set-end reads see. keploy test takes it at the
// next test case's end, or at the set's end, and fails the set naming it.
// The test case open when it was counted passes: it is not its loss.
func TestKeployTestFailsTheSetOnTheGapsLostBytes(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	gap := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1,
		What: "from [::ffff:127.0.0.1]:50912, written between test cases, where keploy's wait for them was cut short"}
	// Taken at test-2's end (the second take), and at the set's end (the
	// fourth: three test cases' ends, then the set's).
	for _, at := range []int{2, 4} {
		h := newPartialRunHarness(t, 3, 0) // every test case passes
		h.replayer.config.Test.Mocking = true
		instr := &prOwnedInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}},
			at: at, owned: []models.OwnedCaptureLoss{{Test: "between test cases", Loss: gap}}}
		h.replayer.instrumentation = instr
		h.replayer.markPIDMaps(context.Background())
		if status := h.run(t); status != models.TestSetStatusFailed {
			t.Fatalf("taken at take %d: set %q, want FAILED: the gap's bytes were lost", at, status)
		}
		if instr.takes != 4 {
			t.Fatalf("%d takes, want 4: one at each test case's end and one at the set's", instr.takes)
		}
		for _, r := range h.report.report.Tests {
			if r.Status != models.TestStatusPassed || len(r.FailureInfo.CaptureIncomplete) != 0 {
				t.Fatalf("taken at take %d: %s %s %+v; want PASSED, uncharged: the loss is the gap's", at, r.TestCaseID, r.Status, r.FailureInfo.CaptureIncomplete)
			}
		}
		cv := h.report.report.CaptureVerification
		if cv == nil || cv.Status != models.CaptureIncompleteStatus || !strings.Contains(cv.Reason, gap.What) {
			t.Fatalf("taken at take %d: capture_verification %+v, want incomplete naming %q", at, cv, gap.What)
		}
	}

	// The set end's take fails: what a cut-short end owned cannot be told,
	// so the set is neither passed nor failed.
	h := newPartialRunHarness(t, 3, 0)
	h.replayer.config.Test.Mocking = true
	h.replayer.instrumentation = &prOwnedInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}},
		at: 4, err: errors.New("connection reset")}
	h.replayer.markPIDMaps(context.Background())
	if status := h.run(t); status == models.TestSetStatusPassed || status == models.TestSetStatusFailed {
		t.Fatalf("a failed take: set %q, want neither PASSED nor FAILED", status)
	}
	if cv := h.report.report.CaptureVerification; cv == nil || cv.Status != models.CaptureUnverifiedStatus {
		t.Fatalf("a failed take: capture_verification %+v, want unverified", cv)
	}
}

// postScriptFails is a set's configuration whose post-script fails.
type postScriptFails struct{ prTestSetConf }

func (postScriptFails) Read(context.Context, string) (*models.TestSet, error) {
	return &models.TestSet{PostScript: "exit 3"}, nil
}

// A set that returns before its end applies what keploy took (its
// post-script failed) leaves losses that name its test cases, and the next
// set's test cases repeat those names. A later set's end never names one of
// its own test cases with them: they are the run's, and fail that set, in
// words that still name the test case they were written in.
func TestOwnedLossesASetLeftUnappliedNameNoLaterSetsTestCase(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 3, 0)
	h.replayer.config.Test.Mocking = true
	h.replayer.testSetConf = postScriptFails{}
	loss := models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1,
		What: "from 127.0.0.1:50912, written in test-2, where keploy's wait for them was cut short"}
	// Taken at test-3's end, the set's last before its post-script.
	instr := &prOwnedInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}},
		at: 3, owned: []models.OwnedCaptureLoss{{Test: "test-2", Loss: loss}}}
	h.replayer.instrumentation = instr
	h.replayer.markPIDMaps(context.Background())
	status, err := h.replayer.RunTestSet(context.Background(), "test-set-0", "test-run-0", false)
	if status != models.TestSetStatusFaultScript || err == nil {
		t.Fatalf("set %q, %v; want %q and the post-script's error", status, err, models.TestSetStatusFaultScript)
	}
	if instr.takes != 3 {
		t.Fatalf("%d takes, want 3: one at each test case's end, none at a set's end it never reached", instr.takes)
	}
	// The next set's own test-2 failed.
	next := []models.TestResult{{TestCaseID: "test-1", Status: models.TestStatusPassed}, {TestCaseID: "test-2", Status: models.TestStatusFailed}}
	set, err := h.replayer.applyOwnedLosses(next)
	if err != nil || len(set) != 1 || set[0] != loss {
		t.Fatalf("the next set's end: %v, %v; want the set's, %v", set, err, loss)
	}
	if got := next[1].FailureInfo.CaptureIncomplete; len(got) != 0 {
		t.Fatalf("the next set's test-2 names %v: an earlier set's loss, by its name", got)
	}
}
