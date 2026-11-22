package replay

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// setEnd is one AfterTestSetRun call: the set, its status, and what its
// context said of a halt (SetHalt, SetHaltAppLogs).
type setEnd struct {
	set     string
	status  bool
	halted  bool
	reason  string
	appLogs string
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
	h.ends = append(h.ends, setEnd{set: testSetID, status: status, halted: halted, reason: reason, appLogs: SetHaltAppLogs(ctx)})
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
// An application's exit comes with the last lines it logged (SetHaltAppLogs);
// an internal error of keploy's, which no line of the app's explains, without.
func TestASetThatEndsItsRunTellsItsHooksItHaltedAndWhy(t *testing.T) {
	for _, c := range []struct {
		name    string
		exit    models.AppErrorType
		hookErr error
		appLogs string
	}{
		{"the application exits", models.ErrAppStopped, nil, prExitLogs},
		{"the application's runner fails inside keploy", models.ErrInternal, nil, ""},
		{"the application exits and the hook fails", models.ErrAppStopped, errors.New("metric store is down"), prExitLogs},
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
			want := setEnd{set: "test-set-0", status: false, halted: true, reason: reason, appLogs: c.appLogs}
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
	if appLogs := SetHaltAppLogs(ctx); appLogs != "" {
		t.Errorf("SetHaltAppLogs of a halt with no lines = %q, want none", appLogs)
	}
	if appLogs := SetHaltAppLogs(WithSetHaltAppLogs(ctx, prExitLogs)); appLogs != prExitLogs {
		t.Errorf("SetHaltAppLogs = %q, want the lines WithSetHaltAppLogs put", appLogs)
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
					want.appLogs = prExitLogs
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
	want := setEnd{set: "test-set-0", status: true, halted: true, reason: parts[0], appLogs: prExitLogs}
	if got := hooks.got(); len(got) != 1 || got[0] != want {
		t.Fatalf("AfterTestSetRun calls %+v, want [%+v]", got, want)
	}
	if utils.ErrCode != 1 {
		t.Errorf("exit code %d, want 1: the halt fails the run", utils.ErrCode)
	}
}

// A set whose application halts on its first attempt and whose next attempt
// ends in an internal error of keploy's, which stops the run, is told it
// halted for that error, and with none of the lines the first exit came
// with: no line of the app's explains keploy's own error.
func TestASetThatHaltsThenStopsTheRunOnAnInternalErrorHasNoAppLines(t *testing.T) {
	h, logs := composeRetried(t, 2)
	// The first attempt's application exits part-way through the set. As
	// the next attempt's starts, test-1's result can no longer be written:
	// that attempt ends INTERNAL_ERR.
	h.instr.stopAppAfterNUpdates = 2
	h.replayer.instrumentation = &haltOnceInstr{prInstr: h.instr, again: func() {
		h.report.mu.Lock()
		h.report.failInsertOf = map[string]error{"test-1": errors.New("report store is unwritable")}
		h.report.mu.Unlock()
	}}
	hooks := &setEndHooks{}
	h.replayer.hookImpl = hooks
	startRetried(t, h)
	if ran := setsRan(logs); len(ran) != 2 {
		t.Fatalf("the run ran %v, the case needs test-set-0 run twice", ran)
	}
	parts := stopParts(t, logs)
	if len(parts) != 1 || !strings.Contains(parts[0], string(models.TestSetStatusInternalErr)) {
		t.Fatalf("the run stops with %q, the case needs it to name the second attempt's internal error", parts)
	}
	want := setEnd{set: "test-set-0", halted: true, reason: parts[0]}
	if got := hooks.got(); len(got) != 1 || got[0] != want {
		t.Fatalf("AfterTestSetRun calls %+v, want [%+v]", got, want)
	}
}

// An application --keep-app-alive keeps across the run's sets (the stack
// docker compose keeps for an in-Pod replay) that exits before it accepts a
// connection ends the run as the first set waits for it: that set halted,
// and its hooks are told so, once, failed, for the very reason the run's
// stop names, with the last lines the application logged. Before, the run
// returned before its AfterTestSetRun, and a hook that reports each set
// (k8s-proxy's metric of an auto-replay round) reported the set as nothing
// and ran it again in its verifying pass. A user's stop as the set waits is
// not a halt: its hooks are told none. A hook that fails on that end is
// logged, and the run still stops as it did.
func TestAKeptAppThatExitsBeforeItAcceptsTellsTheSetsHooksItHalted(t *testing.T) {
	for _, c := range []struct {
		name    string
		exits   bool
		halted  bool
		hookErr error
	}{
		{"the application exits", true, true, nil},
		{"the application exits and the hook fails", true, true, errors.New("metric store is down")},
		{"the user stops keploy", false, false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusing := net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))
			h := newPartialRunHarness(t, 2, 0)
			for _, tc := range h.cases {
				tc.HTTPReq.URL = "http://" + refusing + "/x"
			}
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.KeepAppAlive = true
			h.replayer.config.Test.MaxFlakyChecks = 1
			hooks := &setEndHooks{err: c.hookErr}
			h.replayer.hookImpl = hooks
			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			// The application exits, or its user stops keploy, once the
			// first set waits for it to accept a connection.
			var once sync.Once
			core, logs := observer.New(zapcore.DebugLevel)
			h.replayer.logger = zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
				if strings.Contains(e.Message, "waiting for the application to accept a connection on "+refusing) {
					once.Do(func() {
						if c.exits {
							close(h.instr.appStopped)
							return
						}
						cancel()
					})
				}
				return nil
			}))
			saved := utils.ErrCode
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = saved })
			if err := h.replayer.Start(ctx); err == nil {
				t.Fatal("Start returned nil, the case needs the run stopped as its first set waits")
			}
			if n := len(h.report.results); n != 0 {
				t.Fatalf("%d tests were sent, the case needs none", n)
			}
			got := hooks.got()
			if !c.halted {
				for _, end := range got {
					if end.halted {
						t.Fatalf("AfterTestSetRun calls %+v, want none told it halted on a user's stop", got)
					}
				}
				return
			}
			stops := logs.FilterMessage("stopping Keploy").All()
			if len(stops) != 1 {
				t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
			}
			reason, _ := stops[0].ContextMap()["reason"].(string)
			if !strings.Contains(reason, "exited before it accepted a connection") {
				t.Fatalf("the run stops with %q, the case needs it to name the application's exit", reason)
			}
			want := setEnd{set: "test-set-0", status: false, halted: true, reason: reason, appLogs: prExitLogs}
			if len(got) != 1 || got[0] != want {
				t.Fatalf("AfterTestSetRun calls %+v, want [%+v]", got, want)
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
