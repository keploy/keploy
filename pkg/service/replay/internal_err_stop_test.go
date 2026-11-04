package replay

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// startTwoSets runs Start over h's tests as two sets, test-set-0 and
// test-set-1, as keploy test does with a native command, and returns the
// sets it ran, the reason of its one "stopping Keploy" line and the code
// the process exits with.
func startTwoSets(t *testing.T, h *prRun) ([]string, string, int) {
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
	ran := setsRan(logs)
	stops := logs.FilterMessage("stopping Keploy").All()
	if len(stops) != 1 {
		t.Fatalf("%d \"stopping Keploy\" lines, want 1", len(stops))
	}
	reason, _ := stops[0].ContextMap()["reason"].(string)
	return ran, reason, utils.ErrCode
}

// sgrCode is one colour or style code of a terminal.
var sgrCode = regexp.MustCompile("\x1b\\[[0-9;]*m")

// setsRan is the sets the run's "running" lines name, in order, with their
// colour codes removed. Whether the set's name is coloured is the process's
// global colour switch (fatih/color's NoColor), which any JSON diff keploy
// prints turns on, so an earlier row in the package can turn it on.
func setsRan(logs *observer.ObservedLogs) []string {
	var ran []string
	for _, e := range logs.FilterMessage("running").All() {
		if set, ok := e.ContextMap()["test-set"].(string); ok {
			ran = append(ran, sgrCode.ReplaceAllString(set, ""))
		}
	}
	return ran
}

// A set that ends INTERNAL_ERR and so stops the run is what the run's stop
// names: the set and keploy's error that ended it, never "replay completed
// successfully". The run exits 1, as it did.
func TestAnInternalErrorThatStopsTheRunIsItsStopReason(t *testing.T) {
	for _, c := range []struct {
		name string
		arm  func(*prRun)
		why  string
	}{
		{"the set's results could not be read", func(h *prRun) {
			h.report.readErr = errors.New("report store is unreadable")
		}, "failed to get test case results: report store is unreadable"},
		{"the application's runner failed inside keploy", func(h *prRun) {
			h.instr.stopAppAfterNUpdates, h.instr.stopErr = 5, models.ErrInternal
		}, string(models.ErrInternal)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 4, 0)
			c.arm(h)
			ran, reason, code := startTwoSets(t, h)
			if len(ran) != 1 || ran[0] != "test-set-0" {
				t.Errorf("the run ran the sets %v, want it to stop after test-set-0", ran)
			}
			if want := "test-set test-set-0 ended INTERNAL_ERR: " + c.why; reason != want {
				t.Errorf("the run stops with the reason %q, want %q", reason, want)
			}
			if code != 1 {
				t.Errorf("exit code %d, want 1", code)
			}
		})
	}
}

// A set only not judged (keploy could not read all of its evidence) does
// not stop the run, and is no reason it stopped: the run goes on to its next
// set, its report says why the set is not judged, and it exits 1.
func TestANotJudgedSetIsNoReasonTheRunStopped(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 3, 0)
	h.instr.mockErrorsFor = misses(nil, failedRead)
	ran, reason, code := startTwoSets(t, h)
	if len(ran) != 2 {
		t.Fatalf("the run ran the sets %v, want it to go on past the set not judged", ran)
	}
	if !h.replayer.notJudged("test-set-0") {
		t.Fatal("precondition: test-set-0 is judged; the case needs it not judged")
	}
	if reason != runCompletedReason {
		t.Errorf("the run stops with the reason %q, want %q", reason, runCompletedReason)
	}
	if code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
}

// internalErrRunReason names the set and keploy's error that ended it, else
// the internal error the application's runner reported; any other exit of
// the application is not the set's internal error.
func TestInternalErrRunReasonNamesTheSetAndTheError(t *testing.T) {
	internal := models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("no replacement container was created")}
	stopped := models.AppError{AppErrorType: models.ErrAppStopped, ExitCode: 1}
	readFailed := fmt.Errorf("failed to get test case results: %w", errors.New("store unreadable"))
	for _, c := range []struct {
		name   string
		setErr error
		exit   models.AppError
		want   string
	}{
		{"keploy's error", readFailed, models.AppError{}, "test-set s1 ended INTERNAL_ERR: failed to get test case results: store unreadable"},
		{"the runner's internal error", nil, internal, "test-set s1 ended INTERNAL_ERR: an internal error occurred: no replacement container was created"},
		{"keploy's error before the runner's", readFailed, internal, "test-set s1 ended INTERNAL_ERR: failed to get test case results: store unreadable"},
		{"an exit that is no internal error", nil, stopped, "test-set s1 ended INTERNAL_ERR"},
		{"nothing noted", nil, models.AppError{}, "test-set s1 ended INTERNAL_ERR"},
	} {
		got := internalErrRunReason("s1", c.setErr, c.exit)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if strings.Contains(got, runCompletedReason) {
			t.Errorf("%s: %q says the replay completed", c.name, got)
		}
	}
}

// Each failure of keploy's own that ends a set INTERNAL_ERR and so stops the
// run is the error the set's end hands on to name it (setInternalErr); a set
// that ends any other way hands on none, so a later set never names an
// earlier one's.
func TestEachInternalErrorThatEndsASetIsTheOneItNames(t *testing.T) {
	for _, c := range []struct {
		name string
		on   bool
		arm  func(*prRun)
		// stop, when set, ends the set's context with that cause as test-2
		// is sent.
		stop error
		want string
	}{
		{name: "its results could not be read", arm: func(h *prRun) {
			h.report.readErr = errors.New("report store is unreadable")
		}, want: "failed to get test case results: report store is unreadable"},
		{name: "a judged test's result could not be written", arm: func(h *prRun) {
			h.report.failInsertOf = map[string]error{"test-1": errors.New("report store is unwritable")}
		}, want: "failed to insert final test case result: report store is unwritable"},
		// With no agent: the watch of an application keploy started would
		// race the set's end to the cancel that follows the failed write.
		{name: "an ignored test's result could not be written", arm: func(h *prRun) {
			h.replayer.noAgent = true
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-3"}}
			h.report.failInsertOf = map[string]error{"test-3": errors.New("report store is unwritable")}
		}, want: "report store is unwritable"},
		{name: "what the learn step learned could not be kept", on: true, arm: func(h *prRun) {
			h.replayer.config.Test.Learn = true
		}, want: "what the learn step learned was not kept: keploy's mock store cannot write what the learn step learned"},
		{name: "a run with no agent ended under its tests", arm: func(h *prRun) {
			h.replayer.noAgent = true
		}, stop: errors.New("a goroutine of the run failed"), want: "the run ended before the set's tests did: a goroutine of the run failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			setSwitch(t, c.on)
			h := newPartialRunHarness(t, 3, 0)
			c.arm(h)
			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			if c.stop != nil {
				var stop context.CancelCauseFunc
				ctx, stop = context.WithCancelCause(ctx)
				h.replayer.hookImpl = stopHooks{TestHooks: h.replayer.hookImpl, send: "test-2", stop: func() { stop(c.stop) }}
			}
			status, err := h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
			if err != nil || status != models.TestSetStatusInternalErr || h.replayer.notJudged("test-set-0") {
				t.Fatalf("precondition: the set ends %s (error %v, not judged %v); the case needs INTERNAL_ERR, judged, no error",
					status, err, h.replayer.notJudged("test-set-0"))
			}
			if got := h.replayer.setInternalErr; got == nil || !strings.Contains(got.Error(), c.want) {
				t.Errorf("the set's end hands on %v, want an error saying %q", got, c.want)
			}
			// The next set, which ends PASSED, hands on none.
			h.report.readErr, h.report.failInsertOf = nil, nil
			h.replayer.hookImpl, h.replayer.noAgent = prHooks{}, false
			h.replayer.config.Test.Learn, h.replayer.config.Test.IgnoredTests = false, nil
			ctx2, cancel2 := context.WithTimeout(context.Background(), prTimeout)
			defer cancel2()
			if status, err := h.replayer.RunTestSet(ctx2, "test-set-0", "test-run-1", false); err != nil || status != models.TestSetStatusPassed {
				t.Fatalf("precondition: the next set ends %s (error %v); the case needs PASSED", status, err)
			}
			if got := h.replayer.setInternalErr; got != nil {
				t.Errorf("a set that ended PASSED hands on %v, want none", got)
			}
		})
	}
}
