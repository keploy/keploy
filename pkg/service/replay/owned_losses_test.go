package replay

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

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
		if !reflect.DeepEqual(set[i], want[i]) {
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
// post-script failed) reports, as it returns, the losses its cut-short ends
// owned: those taken at its test cases' ends, and those its return takes
// once more. Each is said in its log, naming the set and the test case, as
// what it was: bytes lost, a call keploy could not capture; a close keploy
// could not tell the bytes of, one it cannot tell carried a call. None
// reaches a later set, whose test cases repeat its names, so the next set's
// end applies nothing and its test-2 stays as it was.
func TestOwnedLossesASetLeftUnappliedReachNoLaterSet(t *testing.T) {
	for _, c := range []struct {
		name string
		at   int
		kind models.CaptureLossKind
		says string
	}{
		{"taken at its last test case's end", 3, models.CaptureLossAppBytesLost, "keploy could not capture a call of this test-set's"},
		{"taken as it returns", 4, models.CaptureLossAppBytesLost, "keploy could not capture a call of this test-set's"},
		{"an untold close, taken at its last test case's end", 3, models.CaptureLossAppBytesUntold, "keploy could not tell whether a connection of this test-set's"},
		{"an untold close, taken as it returns", 4, models.CaptureLossAppBytesUntold, "keploy could not tell whether a connection of this test-set's"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			h := newPartialRunHarness(t, 3, 0)
			h.replayer.config.Test.Mocking = true
			h.replayer.testSetConf = postScriptFails{}
			core, logs := observer.New(zap.DebugLevel)
			h.replayer.logger = zap.New(core)
			loss := models.CaptureLoss{Kind: c.kind, Count: 1,
				What: "from 127.0.0.1:50912, written in test-2, where keploy's wait for them was cut short"}
			instr := &prOwnedInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}},
				at: c.at, owned: []models.OwnedCaptureLoss{{Test: "test-2", Loss: loss}}}
			h.replayer.instrumentation = instr
			h.replayer.markPIDMaps(context.Background())
			status, err := h.replayer.RunTestSet(context.Background(), "test-set-0", "test-run-0", false)
			if status != models.TestSetStatusFaultScript || err == nil {
				t.Fatalf("set %q, %v; want %q and the post-script's error", status, err, models.TestSetStatusFaultScript)
			}
			if instr.takes != 4 {
				t.Fatalf("%d takes, want 4: one at each test case's end, and one as the set returns", instr.takes)
			}
			var said []map[string]any
			for _, e := range logs.All() {
				if m := e.ContextMap(); m["test-case"] != nil && e.Level == zap.ErrorLevel {
					m["_msg"] = e.Message
					said = append(said, m)
				}
			}
			if len(said) != 1 || said[0]["test-set"] != "test-set-0" || said[0]["test-case"] != "test-2" ||
				said[0]["kind"] != string(c.kind) || !strings.Contains(said[0]["what"].(string), "50912") {
				t.Fatalf("the set's log said %v; want one error naming test-set-0's test-2 and the loss", said)
			}
			if msg := said[0]["_msg"].(string); !strings.HasPrefix(msg, c.says) {
				t.Fatalf("the set's log said %q; want it to begin %q", msg, c.says)
			}
			// The next set's own test-2 failed; its test-1 passed.
			next := []models.TestResult{{TestCaseID: "test-1", Status: models.TestStatusPassed}, {TestCaseID: "test-2", Status: models.TestStatusFailed}}
			set, err := h.replayer.applyOwnedLosses(next)
			if err != nil || len(set) != 0 {
				t.Fatalf("the next set's end: %v, %v; want nothing: an earlier set's loss is no later set's", set, err)
			}
			if got := next[1].FailureInfo.CaptureIncomplete; len(got) != 0 {
				t.Fatalf("the next set's test-2 names %v: an earlier set's loss, by its name", got)
			}
		})
	}
}

// composeOwnedInstr is a docker compose stack's agent (composeInstr) whose
// takes are o's (prOwnedInstr).
type composeOwnedInstr struct {
	*composeInstr
	o *prOwnedInstr
}

func (c *composeOwnedInstr) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	return c.o.GetCaptureLosses(ctx)
}

func (c *composeOwnedInstr) TakeOwnedCaptureLosses(ctx context.Context) ([]models.OwnedCaptureLoss, error) {
	return c.o.TakeOwnedCaptureLosses(ctx)
}

// TestOwnedLossesASetLeftUnappliedReachNoLaterSet under docker compose, a
// stack per set and one kept across sets: the set's session is begun on the
// agent in the stack (beginSession), so a set whose post-script failed takes
// once more as it returns, and says the loss its cut-short end owned, lost or
// untold, naming its test case. None reaches the next set's end.
func TestOwnedLossesAComposeSetLeftUnappliedReachNoLaterSet(t *testing.T) {
	for _, kept := range []bool{false, true} {
		for _, c := range []struct {
			name string
			at   int
			kind models.CaptureLossKind
		}{
			{"taken at its last test case's end", 2, models.CaptureLossAppBytesLost},
			{"taken as it returns", 3, models.CaptureLossAppBytesLost},
			{"an untold close, taken at its last test case's end", 2, models.CaptureLossAppBytesUntold},
			{"an untold close, taken as it returns", 3, models.CaptureLossAppBytesUntold},
		} {
			name := "a stack per set/" + c.name
			if kept {
				name = "a reused stack/" + c.name
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv(provenswitch.Env, "1")
				ci := &composeInstr{}
				h := composeHarness(t, ci) // two test cases
				// As Start runs them: keploy mocking the dependencies reuses
				// the stack across sets, and a stack per set mocks nothing.
				h.replayer.config.Test.Mocking = kept
				h.replayer.testSetConf = postScriptFails{}
				core, logs := observer.New(zap.DebugLevel)
				h.replayer.logger = zap.New(core)
				loss := models.CaptureLoss{Kind: c.kind, Count: 1,
					What: "from 127.0.0.1:50912, written in test-2, where keploy's wait for them was cut short"}
				o := &prOwnedInstr{prLossInstr: &prLossInstr{prInstr: h.instr, answers: []models.CaptureLosses{nil}},
					at: c.at, owned: []models.OwnedCaptureLoss{{Test: "test-2", Loss: loss}}}
				h.replayer.instrumentation = &composeOwnedInstr{composeInstr: ci, o: o}
				h.replayer.markPIDMaps(context.Background())
				status, err := h.replayer.RunTestSet(context.Background(), "test-set-0", "test-run-0", kept)
				if status != models.TestSetStatusFaultScript || err == nil {
					t.Fatalf("set %q, %v; want %q and the post-script's error", status, err, models.TestSetStatusFaultScript)
				}
				o.lmu.Lock()
				takes := o.takes
				o.lmu.Unlock()
				if takes != 3 {
					t.Fatalf("%d takes, want 3: one at each test case's end, and one as the set returns", takes)
				}
				var said []map[string]any
				for _, e := range logs.All() {
					if m := e.ContextMap(); m["test-case"] != nil && e.Level == zap.ErrorLevel {
						said = append(said, m)
					}
				}
				if len(said) != 1 || said[0]["test-set"] != "test-set-0" || said[0]["test-case"] != "test-2" ||
					said[0]["kind"] != string(c.kind) || !strings.Contains(said[0]["what"].(string), "50912") {
					t.Fatalf("the set's log said %v; want one error naming test-set-0's test-2 and the loss", said)
				}
				next := []models.TestResult{{TestCaseID: "test-1", Status: models.TestStatusPassed}, {TestCaseID: "test-2", Status: models.TestStatusFailed}}
				set, err := h.replayer.applyOwnedLosses(next)
				if err != nil || len(set) != 0 || len(next[1].FailureInfo.CaptureIncomplete) != 0 {
					t.Fatalf("the next set's end: %v, %v, test-2 names %v; want nothing: an earlier set's loss is no later set's", set, err, next[1].FailureInfo.CaptureIncomplete)
				}
			})
		}
	}
}
