package replay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// P101-b: a call the application makes after its set's last answer, but only
// once keploy stops it (a publish its shutdown flushes, a batch it writes on
// its way out), lands after a set-end read made while the app still runs, and
// is judged by no one. Under the proven switch keploy stops the app keploy
// started for the set, waits for its exit, and only then reads the set's end.

// stoppingApp is the harness's agent and application, whose application
// makes, as it is stopped, a call no mock answers; the agent holds it until
// keploy reads the misses.
type stoppingApp struct {
	*prInstr
	mu      sync.Mutex
	held    []models.UnmatchedCall
	reads   int
	stopped bool
	// readAfterStop is whether the agent's misses were read after the app
	// had exited.
	readAfterStop bool
}

func (s *stoppingApp) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Its shutdown flushes a publish, which no mock answers.
	s.held = append(s.held, models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /publish", Destination: "broker:9099", Diff: "body: shutdown-changed"})
	s.stopped = true
	return models.AppError{AppErrorType: models.ErrCtxCanceled, ExitCode: -1}
}

func (s *stoppingApp) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.stopped {
		s.readAfterStop = true
	}
	calls := s.held
	s.held = nil
	return calls, nil
}

func TestRunTestSet_ACallTheAppMakesAsItIsStoppedIsJudged(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	app := &stoppingApp{prInstr: h.instr}
	h.replayer.instrumentation = app

	status := h.run(t)

	if !app.readAfterStop {
		t.Fatalf("the set's end read the agent's misses (%d reads) only while the app still ran: a call it makes as it is stopped is judged by no one", app.reads)
	}
	if status != models.TestSetStatusFailed {
		t.Fatalf("status %s; the publish the app made as it was stopped, which no mock answered, must fail the set", status)
	}
	rep := h.report.report
	if len(rep.UnclaimedCalls) != 1 || !strings.Contains(rep.FailureReason, "POST /publish to broker:9099") {
		t.Fatalf("unclaimed %+v, reason %q; want the shutdown publish named", rep.UnclaimedCalls, rep.FailureReason)
	}
	for _, r := range rep.Tests {
		if r.Status != models.TestStatusPassed {
			t.Fatalf("test %s is %s; the publish after the last answer is no test's", r.TestCaseID, r.Status)
		}
	}
	if h.replayer.notJudged("test-set-0") {
		t.Fatal("the set is marked not judged; it was judged, FAILED")
	}
}

// With the switch off the set ends as it did: the app is stopped after the
// set is reported, and nothing more is read.
func TestRunTestSet_TheAppIsStoppedAfterTheSetWithTheSwitchOff(t *testing.T) {
	provenOn(t, false)
	h := newPartialRunHarness(t, 2, 0)
	app := &stoppingApp{prInstr: h.instr}
	h.replayer.instrumentation = app

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED as before", status)
	}
	if app.readAfterStop {
		t.Fatal("switch off: the agent's misses were read after the app was stopped; the set's end is as it was")
	}
}

// stoppedAppLogs is the harness's agent and application whose application's
// logs are gone once it has stopped, as a container docker removes on exit
// (`docker run --rm`) has none to give.
type stoppedAppLogs struct {
	*prInstr
	mu      sync.Mutex
	stopped bool
}

func (s *stoppedAppLogs) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	defer func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
	}()
	return s.prInstr.Run(ctx, opts)
}

func (s *stoppedAppLogs) GetRecentAppLogs(ctx context.Context) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return ""
	}
	return s.prInstr.GetRecentAppLogs(ctx)
}

// The set's end stops the app before its report is written, so the report's
// app logs are taken before the stop: a set that stopped part-way (the agent
// refused a test's mocks) still carries what the app logged.
func TestRunTestSet_AReportCarriesTheLogsOfTheAppTheSetEndStopped(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 4, 3)
	h.instr.recentAppLogs = "app: still serving\n"
	app := &stoppedAppLogs{prInstr: h.instr}
	h.replayer.instrumentation = app
	_ = h.run(t)
	if h.report.report == nil {
		t.Fatal("no report was persisted")
	}
	if got := h.report.report.AppLogs; got != "app: still serving\n" {
		t.Fatalf("the report's app_logs is %q; what the app logged before the set's end stopped it is lost", got)
	}
}

// consumingAtStop is the harness's agent and application, whose application
// consumes, as it is stopped, a pool mock the agent answers it with: the
// agent holds the consumption until keploy drains it.
type consumingAtStop struct {
	*prInstr
	mu      sync.Mutex
	stopped bool
	drained bool
}

func (c *consumingAtStop) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	err := c.prInstr.Run(ctx, opts)
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	return err
}

// consumed answers the agent's consumption read (GetConsumedMocks).
func (c *consumingAtStop) consumed() []models.MockState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped || c.drained {
		return nil
	}
	c.drained = true
	return []models.MockState{{Name: "pool-shutdown", Kind: models.HTTP}}
}

// learnedStore is the harness's mock store, which keeps what the learn step
// learned: it records the consumed mocks it was handed.
type learnedStore struct {
	*prMockDB
	persisted map[string]models.MockState
}

func (l *learnedStore) PersistLearned(_ context.Context, _ string, states map[string]models.MockState) (map[string][]string, error) {
	l.persisted = states
	return nil, nil
}

// What the app consumes as the set's end stops it is drained into the set's
// own: what record's full round learned on it is kept with the set's, and the
// agent no longer holds it for the next set's first read to take, by its
// name, for one of that set's.
func TestRunTestSet_WhatTheAppConsumesAsItIsStoppedIsTheSets(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Test.Learn = true
	store := &learnedStore{prMockDB: h.mocks}
	h.replayer.mockDB = store
	app := &consumingAtStop{prInstr: h.instr}
	h.replayer.instrumentation = app
	h.replayer.hookImpl = prHooks{consumedFor: app.consumed}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED", status)
	}
	if !app.drained {
		t.Fatal("what the app consumed as it was stopped was not drained in its own set; the agent still holds it for the next set's first read")
	}
	if _, kept := store.persisted["pool-shutdown"]; !kept {
		t.Fatalf("the learn step's persisted set %v lacks the mock the app consumed as it stopped: what was learned on it is lost", store.persisted)
	}
}

// killedAtStop is the harness's agent and application, whose application's
// runner had to kill it at the set's end: it neither exited nor made progress
// toward exiting (models.AppError.StopKilled).
type killedAtStop struct{ *prInstr }

func (k *killedAtStop) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	if errors.Is(context.Cause(ctx), models.ErrAppStoppedForSetEnd) {
		return models.AppError{AppErrorType: models.ErrCtxCanceled, StopKilled: "keploy interrupted the application at the set's end and a process of it had not exited 3s later (the grace keploy's stop gives each of its process groups), so keploy killed it"}
	}
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

// An app the set's end had to kill leaves the set without a verdict: what it
// would have done as it stopped is not known.
func TestRunTestSet_AnAppTheSetEndHadToKillLeavesTheSetUnjudged(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.instrumentation = &killedAtStop{prInstr: h.instr}
	if status := h.run(t); status != models.TestSetStatusInternalErr {
		t.Fatalf("status %s; a set whose app had to be killed at its end has no verdict", status)
	}
	if !h.replayer.notJudged("test-set-0") {
		t.Fatal("the set is not recorded as not judged")
	}
	if reason := h.report.report.FailureReason; !strings.Contains(reason, "so keploy killed it") || !strings.Contains(reason, "would have done as it stopped is not known") {
		t.Fatalf("failure reason %q does not say the app was killed at the set's end", reason)
	}
}

// What the app consumed as it stopped that cannot be read leaves the set
// without a verdict.
func TestRunTestSet_AnUnreadConsumptionAtTheStopLeavesTheSetUnjudged(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	app := &consumingAtStop{prInstr: h.instr}
	h.replayer.instrumentation = app
	h.replayer.hookImpl = failingConsumedAtStop{prHooks: prHooks{}, app: app}
	if status := h.run(t); status != models.TestSetStatusInternalErr {
		t.Fatalf("status %s; a set whose app's consumption as it stopped could not be read has no verdict", status)
	}
	if reason := h.report.report.FailureReason; !strings.Contains(reason, "consumed as the set's end stopped it") {
		t.Fatalf("failure reason %q does not say what could not be read", reason)
	}
}

// failingConsumedAtStop fails the consumption read made once the app has
// stopped, as an agent that cannot answer it does.
type failingConsumedAtStop struct {
	prHooks
	app *consumingAtStop
}

func (f failingConsumedAtStop) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	f.app.mu.Lock()
	stopped := f.app.stopped
	f.app.mu.Unlock()
	if stopped {
		return nil, errors.New("get consumed mocks returned status 500")
	}
	return f.prHooks.GetConsumedMocks(ctx)
}

// An app that exits on its own after the last test ends the set APP_HALTED,
// even when what it consumed cannot be drained: the drain is not cut short
// by the set's stop (it reads past the cancel), and a drain that fails for
// a set its app stopped leaves the stop as the set's status, never "not
// judged", which would keep the run going past an app that exited.
func TestRunTestSet_AnAppThatExitsAfterTheLastTestIsHaltedWhateverItsDrain(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(map[bool]string{false: "a read on the set's cancelled context fails", true: "the drain fails"}[failing], func(t *testing.T) {
			provenOn(t, true)
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.instrumentation = &haltsAfterLastTest{prInstr: h.instr, at: 2}
			var drained atomic.Bool
			if failing {
				h.replayer.hookImpl = failingDrain{app: h.instr}
			} else {
				h.replayer.hookImpl = prHooks{consumedFailsOnStop: true, consumedFor: func() []models.MockState {
					select {
					case <-h.instr.appStopped:
						drained.Store(true)
					default:
					}
					return nil
				}}
			}
			if status := h.run(t); status != models.TestSetStatusAppHalted {
				t.Fatalf("status %s, want APP_HALTED: the app exited after the last test", status)
			}
			if h.replayer.notJudged("test-set-0") {
				t.Fatal("a set whose app exited is marked not judged, and the run would go on past it")
			}
			if !failing && !drained.Load() {
				t.Fatal("what the app consumed before it exited was not drained in its own set: the read was cut short by the set's stop")
			}
		})
	}
}

// failingDrain fails every consumption read once the set's app has exited.
type failingDrain struct {
	prHooks
	app *prInstr
}

func (f failingDrain) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	select {
	case <-f.app.appStopped:
		return nil, errors.New("get consumed mocks returned status 500")
	default:
		return nil, nil
	}
}

// blockingDrain is the agent's consumption read after the app stopped, as an
// agent that does not answer it: it ends only with its context, after
// calling during, if set.
type blockingDrain struct {
	prHooks
	app    *consumingAtStop
	during func()
}

func (b blockingDrain) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	b.app.mu.Lock()
	stopped := b.app.stopped
	b.app.mu.Unlock()
	if !stopped {
		return nil, nil
	}
	if b.during != nil {
		b.during()
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// The read of what the app consumed as it stopped is bounded, and the
// user's stop ends it: an agent that does not answer leaves the set not
// judged, never a run that does not end; a user who stops the run meanwhile
// has it USER_ABORT.
func TestRunTestSet_TheDrainAfterTheStopEnds(t *testing.T) {
	old := setEndDrainBudget
	defer func() { setEndDrainBudget = old }()
	for _, tc := range []struct {
		name string
		user bool
		want models.TestSetStatus
	}{
		{"the agent does not answer", false, models.TestSetStatusInternalErr},
		{"the user stops the run", true, models.TestSetStatusUserAbort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, true)
			if tc.user {
				setEndDrainBudget = time.Hour
			} else {
				setEndDrainBudget = 50 * time.Millisecond
			}
			h := newPartialRunHarness(t, 2, 0)
			app := &consumingAtStop{prInstr: h.instr}
			h.replayer.instrumentation = app
			drain := blockingDrain{app: app}
			if tc.user {
				drain.during = func() { h.cancel() }
			}
			h.replayer.hookImpl = drain
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.cancel = cancel
			done := make(chan models.TestSetStatus, 1)
			go func() {
				status, _ := h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
				done <- status
			}()
			select {
			case status := <-done:
				if status != tc.want {
					t.Fatalf("status %s, want %s", status, tc.want)
				}
			case <-time.After(time.Minute):
				t.Fatal("the drain after the stop did not end: it is not bounded, or the user's stop does not end it")
			}
		})
	}
}
