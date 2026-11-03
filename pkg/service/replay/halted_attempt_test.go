package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// deleteRecDB is the harness's test DB, recording the test cases --must-pass
// deletes from a set.
type deleteRecDB struct {
	*prTestDB
	mu      sync.Mutex
	deleted []string
}

func (f *deleteRecDB) DeleteTests(_ context.Context, _ string, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, ids...)
	return nil
}

// crashOnceHooks answers as the harness's hooks, but the first time the
// named test is sent its application exits (crash) while it waits on the
// request, which then fails as a reset connection does once the set's
// context ends.
type crashOnceHooks struct {
	prHooks
	name  string
	crash func()
	done  *atomic.Bool
}

func (h crashOnceHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	if tc.Name != h.name || h.done.Swap(true) {
		return h.prHooks.SimulateRequest(ctx, tc, set)
	}
	h.crash()
	<-ctx.Done()
	return nil, fmt.Errorf("Get %q: read tcp: connection reset by peer", tc.HTTPReq.URL)
}

// haltOnInstr is the harness's instrumentation whose application stays up
// through each run but its on-th (every run, for 0), through which it exits
// part-way, after afterUpdates more of the agent's updates.
type haltOnInstr struct {
	*prInstr
	on           int32
	afterUpdates int
	runs         atomic.Int32
}

func (f *haltOnInstr) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	if n := f.runs.Add(1); f.on != 0 && n != f.on {
		<-ctx.Done()
		return models.AppError{AppErrorType: models.ErrCtxCanceled, ExitCode: -1}
	}
	f.mu.Lock()
	// A run's exit of its own: an earlier run's has closed the last one.
	f.appStopped = make(chan struct{})
	f.stopAppAfterNUpdates = f.updateCalls + f.afterUpdates
	f.mu.Unlock()
	return f.prInstr.Run(ctx, opts)
}

// composeRetried is a harness of n tests whose set docker compose runs per
// set (a stack each attempt), twice (the flaky check), with an agent that
// answers healthy, logging to logs.
func composeRetried(t *testing.T, n int) (*prRun, *observer.ObservedLogs) {
	t.Helper()
	h := newPartialRunHarness(t, n, 0)
	h.replayer.config.Command = "docker compose up"
	h.replayer.config.CommandType = string(utils.DockerCompose)
	h.replayer.config.Test.MaxFlakyChecks = 2
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(agent.Close)
	h.replayer.config.Agent.AgentURI = agent.URL
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core)
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	return h, logs
}

func startRetried(t *testing.T, h *prRun) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	if err := h.replayer.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Logf("Start: %v", err)
	}
}

// A set its application halted on one attempt and that ran to a verdict on
// another is flaky, whatever its failed tests: the halt may file none FAILED.
// Its report is the latest attempt's, PASSED where that one ran clean, and
// the halt fails the run (exit 1). The run said no flaky line for it, so its
// report and its summary said PASSED, alone, for a run that exited 1. Now a
// flaky line names the attempt that halted and how, and the set is in the
// run's list of flaky sets: either order. A set that halts on both attempts,
// or on neither, is not flaky for a halt.
func TestASetItsAppHaltedOnOneAttemptOnlyIsFlaky(t *testing.T) {
	const haltLine = "the testset is flaky: its application halted on one attempt"
	for _, c := range []struct {
		name string
		on   bool
		// halts is which runs of the application exit part-way: "first",
		// "second", "third", "both" (of two), or none.
		halts string
		// attempts is how many the flaky check makes: 2 where 0.
		attempts int
		// wrong answers test-2 wrongly on every attempt.
		wrong                 bool
		haltedAt, verdictAt   int
		wantReport, verdictIs models.TestSetStatus
		wantCode              int
	}{
		{name: "halts, then runs clean", halts: "first", haltedAt: 1, verdictAt: 2,
			wantReport: models.TestSetStatusPassed, verdictIs: models.TestSetStatusPassed, wantCode: 1},
		{name: "halts, then runs clean, switch on", on: true, halts: "first", haltedAt: 1, verdictAt: 2,
			wantReport: models.TestSetStatusPassed, verdictIs: models.TestSetStatusPassed, wantCode: 1},
		{name: "halts, then fails a test", halts: "first", wrong: true, haltedAt: 1, verdictAt: 2,
			wantReport: models.TestSetStatusFailed, verdictIs: models.TestSetStatusFailed, wantCode: 1},
		{name: "runs clean, then halts", halts: "second", haltedAt: 2, verdictAt: 1,
			wantReport: models.TestSetStatusAppHalted, verdictIs: models.TestSetStatusPassed, wantCode: 1},
		{name: "runs clean twice, then halts", halts: "third", attempts: 3, haltedAt: 3, verdictAt: 1,
			wantReport: models.TestSetStatusAppHalted, verdictIs: models.TestSetStatusPassed, wantCode: 1},
		{name: "halts on both attempts", halts: "both", wantReport: models.TestSetStatusAppHalted, wantCode: 1},
		{name: "runs clean on both attempts", wantReport: models.TestSetStatusPassed, wantCode: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			setSwitch(t, c.on)
			h, logs := composeRetried(t, 2)
			switch c.halts {
			case "first":
				h.instr.stopAppAfterNUpdates = 2
				h.replayer.instrumentation = &haltOnceInstr{prInstr: h.instr, again: func() {}}
			case "second":
				h.replayer.instrumentation = &haltOnInstr{prInstr: h.instr, on: 2, afterUpdates: 2}
			case "third":
				h.replayer.instrumentation = &haltOnInstr{prInstr: h.instr, on: 3, afterUpdates: 2}
			case "both":
				h.replayer.instrumentation = &haltOnInstr{prInstr: h.instr, afterUpdates: 2}
			}
			if c.wrong {
				h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-2": true}}
			}
			attempts := 2
			if c.attempts != 0 {
				attempts = c.attempts
			}
			h.replayer.config.Test.MaxFlakyChecks = uint32(attempts)
			startRetried(t, h)
			if ran := setsRan(logs); len(ran) != attempts {
				t.Fatalf("the run ran %v, the case needs test-set-0 run %d times", ran, attempts)
			}
			h.report.mu.Lock()
			rep := h.report.report
			h.report.mu.Unlock()
			if rep == nil || rep.Status != string(c.wantReport) {
				t.Fatalf("the set's report is %+v, the case needs its latest attempt %s", rep, c.wantReport)
			}
			if utils.ErrCode != c.wantCode {
				t.Errorf("exit code %d, want %d", utils.ErrCode, c.wantCode)
			}
			flaky := logs.FilterMessageSnippet(haltLine).All()
			if c.haltedAt == 0 {
				if len(flaky) != 0 {
					t.Errorf("%d lines say the set is flaky for a halt, want none", len(flaky))
				}
				return
			}
			if len(flaky) != 1 {
				t.Fatalf("%d lines say the set is flaky for its halt, want 1", len(flaky))
			}
			f := flaky[0].ContextMap()
			if f["haltedAttempt"] != int64(c.haltedAt) || f["verdictAttempt"] != int64(c.verdictAt) || f["verdictStatus"] != string(c.verdictIs) || f["haltedStatus"] != string(models.TestSetStatusAppHalted) {
				t.Errorf("the flaky line says %v, want attempt %d halted (%s) and attempt %d %s", f, c.haltedAt, models.TestSetStatusAppHalted, c.verdictAt, c.verdictIs)
			}
			if want := "the application exited during test-set test-set-0 with exit code 1: app stopped"; f["halt"] != want {
				t.Errorf("the flaky line names the halt %q, want %q", f["halt"], want)
			}
			listed := logs.FilterMessageSnippet("flaky testsets detected").All()
			if len(listed) != 1 || fmt.Sprint(listed[0].ContextMap()["testSets"]) != "[test-set-0]" {
				t.Errorf("the run's list of flaky sets is %v, want test-set-0 in it", listed)
			}
		})
	}
}

// --must-pass removes no test case for an attempt the application halted: a
// test the exit cut short failed for the exit, not for what it checks. It was
// deleted from the set on disk as a failing test (docker compose, a stack per
// set, where the run goes on past a halted set). The next attempt runs the
// set again, here clean; the halt still fails the run, and a line names the
// attempt that halted.
func TestMustPassRemovesNoTestForAnAttemptTheAppHalted(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch %v", on), func(t *testing.T) {
			setSwitch(t, on)
			h, logs := composeRetried(t, 3)
			h.replayer.config.Test.MustPass = true
			h.replayer.config.Test.MaxFailAttempts = 5
			db := &deleteRecDB{prTestDB: h.replayer.testDB.(*prTestDB)}
			h.replayer.testDB = db
			var once sync.Once
			h.replayer.instrumentation = &haltOnceInstr{prInstr: h.instr, again: func() {}}
			h.replayer.hookImpl = crashOnceHooks{name: "test-2", done: new(atomic.Bool),
				crash: func() { once.Do(func() { close(h.instr.appStopped) }) }}
			startRetried(t, h)
			if ran := setsRan(logs); len(ran) < 2 {
				t.Fatalf("the run ran %v, the case needs test-set-0 run again", ran)
			}
			stops := logs.FilterMessage("stopping Keploy").All()
			if len(stops) != 1 || !strings.Contains(fmt.Sprint(stops[0].ContextMap()["reason"]), "the application exited during test-set test-set-0") {
				t.Fatalf("the run stops with %v, the case needs an attempt halted by the application's exit", stops)
			}
			db.mu.Lock()
			deleted := db.deleted
			db.mu.Unlock()
			if len(deleted) != 0 {
				t.Errorf("--must-pass deleted %v from the set for an attempt its application halted, want none", deleted)
			}
			kept := logs.FilterMessageSnippet("--must-pass removes no test case for an attempt the application halted").All()
			if len(kept) != 1 || kept[0].ContextMap()["attempt"] != int64(1) {
				t.Errorf("%d lines say --must-pass removed nothing for the halted attempt (%v), want 1 naming attempt 1", len(kept), kept)
			}
			if utils.ErrCode != 1 {
				t.Errorf("exit code %d, want 1", utils.ErrCode)
			}
		})
	}
}
