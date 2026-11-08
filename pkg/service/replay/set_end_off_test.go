package replay

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// offRun is the harness with the proven switch off and an agent serving the
// set's mocks, where keploy test reads the set's end (readsSetEndOff), its
// reads of the misses noted as marked or not, and its WARN lines observed.
func offRun(t *testing.T, n int, reads ...func() ([]models.UnmatchedCall, error)) (*prRun, *readsMarked, *observer.ObservedLogs) {
	t.Helper()
	provenOn(t, false)
	h := newPartialRunHarness(t, n, 0)
	h.replayer.config.Test.Mocking = true
	h.instr.mockErrorsFor = misses(reads...)
	instr := &readsMarked{prInstr: h.instr}
	h.replayer.instrumentation = instr
	core, logs := observer.New(zapcore.WarnLevel)
	h.replayer.logger = zap.New(core)
	return h, instr, logs
}

// heldRows is the run's mismatch summary rows for a held call.
func heldRows(r *Replayer) []TestFailure {
	var out []TestFailure
	for _, f := range r.mockMismatchFailures.GetFailures() {
		if f.MismatchReport != nil && f.MismatchReport.ActualSummary == "Fetch orders" {
			out = append(out, f)
		}
	}
	return out
}

func heldLines(logs *observer.ObservedLogs) int {
	return logs.FilterMessageSnippet("keploy held a call of the application for a later test's recording").Len()
}

// With the switch off, the set's end read of the misses comes last, after
// each test's, and only it is marked as the set's end: the agent's parsers
// file there the calls they hold for a later test of the set.
func TestRunTestSet_SwitchOffReadsTheSetsEndOfTheMissesLast(t *testing.T) {
	h, instr, _ := offRun(t, 2)
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED", status)
	}
	instr.mu.Lock()
	defer instr.mu.Unlock()
	if n := len(instr.marked); n != 3 {
		t.Fatalf("%d reads of the misses (%v), want each test's and the set's end", n, instr.marked)
	}
	for i, m := range instr.marked {
		if m != (i == len(instr.marked)-1) {
			t.Fatalf("read %d of %d marked %v: only the set's end, the last, is marked: %v", i+1, len(instr.marked), m, instr.marked)
		}
	}
}

// With the switch off and no mocks served, no agent (a base path), or an
// application keploy test does not run (no command), keploy test reads no
// set's end of the misses, as before.
func TestRunTestSet_SwitchOffWithoutMocksOrAgentReadsNoSetsEnd(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		mocking, noAgent, uninstrumented bool
	}{
		{name: "no mocking"},
		{name: "no agent", mocking: true, noAgent: true},
		{name: "not instrumented", mocking: true, uninstrumented: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, instr, _ := offRun(t, 2)
			h.replayer.config.Test.Mocking = tc.mocking
			h.replayer.noAgent = tc.noAgent
			h.replayer.instrument = !tc.uninstrumented
			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %s, want PASSED", status)
			}
			instr.mu.Lock()
			defer instr.mu.Unlock()
			for i, m := range instr.marked {
				if m {
					t.Fatalf("read %d of %d is marked as the set's end with no set's end read to make: %v", i+1, len(instr.marked), instr.marked)
				}
			}
		})
	}
}

// A held call the set's end read files (its consumer's connection outlives
// the set) is reported with the switch off: in the run's mismatch summary as
// the set's, naming no test, and at WARN. It changes no verdict.
func TestRunTestSet_SwitchOffReportsAHeldCallTheSetsEndFiles(t *testing.T) {
	h, _, logs := offRun(t, 2, nil, nil, heldRead)
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED: with the switch off a miss fails nothing", status)
	}
	rows := heldRows(h.replayer)
	if len(rows) != 1 || rows[0].TestSetID != "test-set-0" || rows[0].TestID != "" {
		t.Fatalf("mismatch summary rows %+v; want the held poll once, the set's, no test's", rows)
	}
	if n := heldLines(logs); n != 1 {
		t.Fatalf("%d WARN lines name the held call, want 1", n)
	}
	for _, r := range h.report.report.Tests {
		if len(r.FailureInfo.UnmatchedCalls) != 0 {
			t.Fatalf("the held call was charged to %s: %+v", r.TestCaseID, r.FailureInfo.UnmatchedCalls)
		}
	}
}

// A held call a test's read took (its connection ended in that test) is the
// set's: with the switch off it is reported at the set's end, not dropped,
// and charged to no test.
func TestRunTestSet_SwitchOffReportsAHeldCallATestsReadTookAtTheSetsEnd(t *testing.T) {
	h, _, logs := offRun(t, 2, nil, heldRead, nil)
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED", status)
	}
	rows := heldRows(h.replayer)
	if len(rows) != 1 || rows[0].TestID != "" {
		t.Fatalf("mismatch summary rows %+v; want the held poll once, no test's", rows)
	}
	if n := heldLines(logs); n != 1 {
		t.Fatalf("%d WARN lines name the held call, want 1", n)
	}
	for _, r := range h.report.report.Tests {
		if r.Status != models.TestStatusPassed || len(r.FailureInfo.UnmatchedCalls) != 0 {
			t.Fatalf("test %s is %s with %+v; the held call is no test's", r.TestCaseID, r.Status, r.FailureInfo.UnmatchedCalls)
		}
	}
}

// A call made after the set's last test that no mock answered, no held one,
// is said at WARN with the switch off, and neither reported as held nor
// charged to a test: as before, it changes nothing.
func TestRunTestSet_SwitchOffSaysACallAfterTheLastTestAndReportsNoHeldOne(t *testing.T) {
	h, _, logs := offRun(t, 2, nil, nil, afterLast)
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED", status)
	}
	if rows := h.replayer.mockMismatchFailures.GetFailures(); len(rows) != 0 {
		t.Fatalf("mismatch summary rows %+v; a call after the last test is not reported there", rows)
	}
	if n := heldLines(logs); n != 0 {
		t.Fatalf("%d WARN lines call a call after the last test held", n)
	}
	after := logs.FilterMessageSnippet("after the test set's last test that no mock answered; they are no test's").All()
	if len(after) != 1 || !strings.Contains(fmt.Sprint(after[0].ContextMap()["calls"]), "POST /publish") {
		t.Fatalf("WARN lines %+v; want one naming the call after the last test", after)
	}
}

// A set's end read that could not take every call marks the set's misses
// unattributed, with the switch off, and still reports what it read.
func TestRunTestSet_SwitchOffAnIncompleteSetsEndReadMarksTheSetUnattributed(t *testing.T) {
	incompleteHeld := func() ([]models.UnmatchedCall, error) {
		held, _ := heldRead()
		_, err := incompleteRead()
		return held, err
	}
	h, _, logs := offRun(t, 2, nil, nil, incompleteHeld)
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED", status)
	}
	h.replayer.unattributedMu.Lock()
	marked := h.replayer.unattributedSets["test-set-0"]
	h.replayer.unattributedMu.Unlock()
	if !marked {
		t.Fatal("the set is not marked unattributed after a set's end read that could not take every call")
	}
	if rows := heldRows(h.replayer); len(rows) != 1 {
		t.Fatalf("mismatch summary rows %+v; what the incomplete read took is still reported", rows)
	}
	if n := logs.FilterMessageSnippet("a held call may be missing from the set's mismatch summary").Len(); n != 1 {
		t.Fatalf("%d WARN lines say the read was incomplete, want 1", n)
	}
}

// Under the proven switch the set's end of the misses is read once, by
// unclaimedCalls, an agent serving the set's mocks or not: the switch-off
// read (setEndOff) is not made too.
func TestRunTestSet_SwitchOnReadsTheSetsEndOfTheMissesOnce(t *testing.T) {
	for _, mocking := range []bool{false, true} {
		provenOn(t, true)
		h := newPartialRunHarness(t, 2, 0)
		h.replayer.config.Test.Mocking = mocking
		instr := &readsMarked{prInstr: h.instr}
		h.replayer.instrumentation = instr
		if status := h.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("mocking %v: status %s, want PASSED", mocking, status)
		}
		instr.mu.Lock()
		marked := 0
		for _, m := range instr.marked {
			if m {
				marked++
			}
		}
		n := len(instr.marked)
		instr.mu.Unlock()
		if n != 3 || marked != 1 {
			t.Fatalf("mocking %v: %d reads of the misses, %d marked as the set's end; want each test's and one set's end", mocking, n, marked)
		}
	}
}

// orderedReads notes, in order, the set's end reads keploy test makes: of
// what was consumed after the last test, and of the misses.
type orderedReads struct {
	*prInstr
	mu    *sync.Mutex
	order *[]string
}

func (o *orderedReads) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	if models.SetEndRead(ctx) {
		o.mu.Lock()
		*o.order = append(*o.order, "misses")
		o.mu.Unlock()
	}
	return o.prInstr.GetMockErrors(ctx)
}

// With the switch off the set's end of the misses is read after what was
// consumed after the last test: that read makes every answer held for the
// set's end due, and the misses read takes the misses of the calls those
// answers make.
func TestRunTestSet_SwitchOffReadsTheMissesAfterWhatWasConsumed(t *testing.T) {
	h, _, _ := offRun(t, 2)
	var mu sync.Mutex
	var order []string
	h.replayer.instrumentation = &orderedReads{prInstr: h.instr, mu: &mu, order: &order}
	h.replayer.hookImpl = prHooks{consumedFor: func() []models.MockState {
		mu.Lock()
		order = append(order, "consumed")
		mu.Unlock()
		return nil
	}}
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := len(order); n < 2 || order[n-1] != "misses" || order[n-2] != "consumed" {
		t.Fatalf("the set's end reads were %v; want what was consumed, then the misses, last", order)
	}
}

// With the switch off, the set's report holds each of its held calls in
// SetHeldCalls, the set's, whichever read took it: a user who reads only the
// report file sees it. The list is absent when the set holds none, and the
// verdict is the same either way: the set passes, no test is charged with the
// call, and nothing lands in UnclaimedCalls. Under the switch the call fails
// the set in UnclaimedCalls, and SetHeldCalls stays empty.
func TestRunTestSet_SwitchOffWritesTheSetsHeldCallsInItsReport(t *testing.T) {
	for _, c := range []struct {
		name  string
		reads []func() ([]models.UnmatchedCall, error)
		held  int
	}{
		{"filed by the set's end read", []func() ([]models.UnmatchedCall, error){nil, nil, heldRead}, 1},
		{"taken by a test's read", []func() ([]models.UnmatchedCall, error){nil, heldRead, nil}, 1},
		{"none held, a call after the last test", []func() ([]models.UnmatchedCall, error){nil, nil, afterLast}, 0},
		{"none at all", nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, _ := offRun(t, 2, c.reads...)
			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %s, want PASSED: a held call fails nothing with the switch off", status)
			}
			rep := h.report.report
			if rep.Status != string(models.TestSetStatusPassed) || rep.FailureReason != "" {
				t.Fatalf("the report says %s (%q); want PASSED, no reason", rep.Status, rep.FailureReason)
			}
			if len(rep.SetHeldCalls) != c.held {
				t.Fatalf("the report's set_held_calls %+v; want %d", rep.SetHeldCalls, c.held)
			}
			if c.held == 0 && rep.SetHeldCalls != nil {
				t.Fatalf("the report's set_held_calls is %#v, not absent", rep.SetHeldCalls)
			}
			for _, call := range rep.SetHeldCalls {
				if !call.Held || call.ActualSummary != "Fetch orders" {
					t.Fatalf("set_held_calls holds %+v; want the held poll", call)
				}
			}
			if len(rep.UnclaimedCalls) != 0 {
				t.Fatalf("the report's unclaimed_calls %+v; with the switch off a held call is not there", rep.UnclaimedCalls)
			}
			for _, r := range rep.Tests {
				if r.Status != models.TestStatusPassed || len(r.FailureInfo.UnmatchedCalls) != 0 {
					t.Fatalf("test %s is %s with %+v; the held call is no test's", r.TestCaseID, r.Status, r.FailureInfo.UnmatchedCalls)
				}
			}
		})
	}
	t.Run("switch on", func(t *testing.T) {
		provenOn(t, true)
		h := newPartialRunHarness(t, 3, 0)
		h.instr.mockErrorsFor = misses(nil, heldRead)
		if status := h.run(t); status != models.TestSetStatusFailed {
			t.Fatalf("status %s, want FAILED: under the switch a held call no test answered fails the set", status)
		}
		rep := h.report.report
		if rep.SetHeldCalls != nil {
			t.Fatalf("the report's set_held_calls %+v under the switch; want it absent", rep.SetHeldCalls)
		}
		if len(rep.UnclaimedCalls) != 1 || !rep.UnclaimedCalls[0].Held {
			t.Fatalf("the report's unclaimed_calls %+v; want the held poll", rep.UnclaimedCalls)
		}
	})
}
