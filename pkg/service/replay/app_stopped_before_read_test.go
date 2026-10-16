package replay

import (
	"context"
	"strings"
	"sync"
	"testing"

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
