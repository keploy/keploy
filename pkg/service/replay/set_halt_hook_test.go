package replay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// setEnd is one AfterTestSetRun call: the set, its status, and what its
// context said of a halt (SetHalt).
type setEnd struct {
	set    string
	status bool
	halted bool
	reason string
}

// setEndHooks records each AfterTestSetRun call, and returns err from it;
// the rest is prHooks'.
type setEndHooks struct {
	prHooks
	err  error
	mu   sync.Mutex
	ends []setEnd
}

func (h *setEndHooks) AfterTestSetRun(ctx context.Context, testSetID string, status bool) error {
	reason, halted := SetHalt(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ends = append(h.ends, setEnd{set: testSetID, status: status, halted: halted, reason: reason})
	return h.err
}

// startTwoSetsLogged is startTwoSets, with the run's log lines too.
func startTwoSetsLogged(t *testing.T, h *prRun) ([]string, string, *observer.ObservedLogs) {
	t.Helper()
	h.replayer.testDB = twoPRSetsDB{h.replayer.testDB.(*prTestDB)}
	h.replayer.config.Command = "./app"
	h.replayer.config.CommandType = string(utils.Native)
	h.replayer.config.Test.MaxFlakyChecks = 1
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core)
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	_ = h.replayer.Start(ctx)
	stops := logs.FilterMessage("stopping Keploy").All()
	if len(stops) != 1 {
		t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
	}
	reason, _ := stops[0].ContextMap()["reason"].(string)
	return setsRan(logs), reason, logs
}

const hookFailed = "failed to execute after test set run hook"

func (h *setEndHooks) got() []setEnd {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]setEnd(nil), h.ends...)
}

// A set that ends its run early, because its application exited or an
// internal error of keploy's stopped it, still has its hooks told it ended:
// once, failed, and halted for the very reason the run's stop names it with.
// Before, the run stopped before its AfterTestSetRun, and a hook that
// reports each set (k8s-proxy's metric of an auto-replay round) reported the
// set as nothing. The set after it never runs, so its hooks hear nothing. A
// hook that fails on that end is logged, and the run still stops as it did.
func TestASetThatEndsItsRunTellsItsHooksItHaltedAndWhy(t *testing.T) {
	for _, c := range []struct {
		name    string
		exit    models.AppErrorType
		hookErr error
	}{
		{"the application exits", models.ErrAppStopped, nil},
		{"the application's runner fails inside keploy", models.ErrInternal, nil},
		{"the application exits and the hook fails", models.ErrAppStopped, errors.New("metric store is down")},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 4, 0)
			h.instr.stopAppAfterNUpdates, h.instr.stopErr = 2, c.exit
			hooks := &setEndHooks{err: c.hookErr}
			h.replayer.hookImpl = hooks
			ran, reason, logs := startTwoSetsLogged(t, h)
			if len(ran) != 1 || ran[0] != "test-set-0" {
				t.Fatalf("the run ran the sets %v, want it to stop after test-set-0", ran)
			}
			if reason == runCompletedReason {
				t.Fatalf("the run stops with the reason %q, want the set's halt", reason)
			}
			want := setEnd{set: "test-set-0", status: false, halted: true, reason: reason}
			if got := hooks.got(); len(got) != 1 || got[0] != want {
				t.Fatalf("AfterTestSetRun calls %+v, want [%+v] (the run stopped with %q)", got, want, reason)
			}
			failed := logs.FilterMessage(hookFailed).All()
			switch {
			case c.hookErr == nil && len(failed) != 0:
				t.Errorf("%d %q lines for a hook that did not fail, want none", len(failed), hookFailed)
			case c.hookErr != nil && len(failed) != 1:
				t.Errorf("%d %q lines for a hook that failed, want 1", len(failed), hookFailed)
			case c.hookErr != nil && failed[0].ContextMap()["testSet"] != "test-set-0":
				t.Errorf("the %q line names the set %v, want test-set-0", hookFailed, failed[0].ContextMap()["testSet"])
			}
		})
	}
}

// A set that does not halt is not told it halted: its hooks hear its end as
// before, with no halt in its context.
func TestASetThatDoesNotHaltIsNotToldItHalted(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	hooks := &setEndHooks{}
	h.replayer.hookImpl = hooks
	ran, _, logs := startTwoSetsLogged(t, h)
	if len(ran) != 2 {
		t.Fatalf("the run ran the sets %v, want both", ran)
	}
	got := hooks.got()
	if len(got) != 2 {
		t.Fatalf("AfterTestSetRun calls %+v, want one per set", got)
	}
	for _, e := range got {
		if e.halted || e.reason != "" {
			t.Errorf("AfterTestSetRun of %s was told it halted (%q); the set did not halt", e.set, e.reason)
		}
	}
	if n := logs.FilterMessage(hookFailed).Len(); n != 0 {
		t.Errorf("%d %q lines for hooks that did not fail, want none", n, hookFailed)
	}
}

// SetHalt reads only what WithSetHalt put in a context.
func TestSetHaltReadsOnlyWhatWithSetHaltPut(t *testing.T) {
	if reason, halted := SetHalt(context.Background()); halted || reason != "" {
		t.Errorf("SetHalt of a bare context = %q, %v; want no halt", reason, halted)
	}
	ctx := WithSetHalt(context.Background(), "the application exited during test-set s with exit code 4: boom")
	if reason, halted := SetHalt(ctx); !halted || reason != "the application exited during test-set s with exit code 4: boom" {
		t.Errorf("SetHalt = %q, %v; want the reason WithSetHalt put, halted", reason, halted)
	}
}

// stopParts is the run's one stop reason, split into what it names each
// halted set with (runStopReason joins them with "; ").
func stopParts(t *testing.T, logs *observer.ObservedLogs) []string {
	t.Helper()
	stops := logs.FilterMessage("stopping Keploy").All()
	if len(stops) != 1 {
		t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
	}
	reason, _ := stops[0].ContextMap()["reason"].(string)
	return strings.Split(reason, "; ")
}

// Under docker compose with keploy's mocks off, a stack runs per set, and the
// run goes on past a set whose application exits, to the next set on a stack
// of its own. The halted set's hooks are told it halted, and why, as a set
// that ends the run is: before, they heard a plain failed set, and a hook
// that reports each set (k8s-proxy's metric of an auto-replay round) filed
// its tests the exit cut short as tests that got no answer. Its reason is
// the one the run's stop names it with. A set after it that runs clean is
// not told it halted. Here the application exits part-way through the first
// set (the set ends APP_HALTED), or also at once on the second set's stack,
// its command failing (the set's run returns that exit as its error).
func TestAComposeSetThatHaltsWhileTheRunGoesOnTellsItsHooksItHalted(t *testing.T) {
	for _, c := range []struct {
		name     string
		both     bool
		wantEnds []setEnd // reason: index into the run's stop parts, or -1
	}{
		{name: "the first set halts, the second runs clean",
			wantEnds: []setEnd{{set: "test-set-0", halted: true}, {set: "test-set-1", status: true}}},
		{name: "each set halts", both: true,
			wantEnds: []setEnd{{set: "test-set-0", halted: true}, {set: "test-set-1", halted: true}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, logs := composeRetried(t, 2)
			h.replayer.testDB = twoPRSetsDB{h.replayer.testDB.(*prTestDB)}
			h.replayer.config.Test.MaxFlakyChecks = 1
			if c.both {
				h.instr.stopAppAfterNUpdates = 2
				h.instr.stopErrs = []models.AppErrorType{models.ErrAppStopped, models.ErrCommandError}
			} else {
				h.replayer.instrumentation = &haltOnInstr{prInstr: h.instr, on: 1, afterUpdates: 2}
			}
			hooks := &setEndHooks{}
			h.replayer.hookImpl = hooks
			startRetried(t, h)
			if ran := setsRan(logs); len(ran) != 2 {
				t.Fatalf("the run ran the sets %v, want it to go on past the halted set", ran)
			}
			parts := stopParts(t, logs)
			got := hooks.got()
			if len(got) != len(c.wantEnds) {
				t.Fatalf("AfterTestSetRun calls %+v, want one per set", got)
			}
			halts := 0
			for i, want := range c.wantEnds {
				if want.halted {
					if halts >= len(parts) {
						t.Fatalf("the run stops with %q, want it to name each halted set", parts)
					}
					want.reason = parts[halts]
					halts++
					if !strings.Contains(want.reason, "exited during test-set "+want.set) {
						t.Fatalf("the run's stop names %s with %q, want its exit", want.set, want.reason)
					}
				}
				if got[i] != want {
					t.Errorf("AfterTestSetRun call %d = %+v, want %+v (the run stopped with %q)", i, got[i], want, parts)
				}
			}
		})
	}
}

// A set whose application halts on its first attempt and runs clean on the
// next (a flaky check's; docker compose, a stack per attempt) has its hooks
// told it halted, and why, with the status of its last attempt: the halt
// fails the run whatever that verdict, and the run's stop names it.
func TestASetThatHaltsThenRunsCleanIsToldItHaltedWithItsLastStatus(t *testing.T) {
	h, logs := composeRetried(t, 2)
	h.instr.stopAppAfterNUpdates = 2
	h.replayer.instrumentation = &haltOnceInstr{prInstr: h.instr, again: func() {}}
	hooks := &setEndHooks{}
	h.replayer.hookImpl = hooks
	startRetried(t, h)
	if ran := setsRan(logs); len(ran) != 2 {
		t.Fatalf("the run ran %v, the case needs test-set-0 run twice", ran)
	}
	parts := stopParts(t, logs)
	if len(parts) != 1 || !strings.Contains(parts[0], "exited during test-set test-set-0") {
		t.Fatalf("the run stops with %q, the case needs it to name the first attempt's halt", parts)
	}
	want := setEnd{set: "test-set-0", status: true, halted: true, reason: parts[0]}
	if got := hooks.got(); len(got) != 1 || got[0] != want {
		t.Fatalf("AfterTestSetRun calls %+v, want [%+v]", got, want)
	}
	if utils.ErrCode != 1 {
		t.Errorf("exit code %d, want 1: the halt fails the run", utils.ErrCode)
	}
}
