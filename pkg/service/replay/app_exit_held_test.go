package replay

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The reason an APP_HALTED set gives names the application's exit code and
// the test it had not answered, where keploy knows them: the reader goes to
// the app's logs for that request. A startup failure names the code too.
func TestAnAppHaltedSetsReasonNamesTheExitAndTheTestItHeld(t *testing.T) {
	results := []models.TestResult{{Name: "get-price-r0a-1"}, {Name: "get-price-r0b-1"}}
	exit := models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3}

	got := describeTestSetFailure(models.TestSetStatusAppHalted, results,
		runShape{appExit: appExitWhy(exit, "get-price-r0b-1"), heldTest: "get-price-r0b-1"})
	want := "application stopped during replay: it exited with code 3 before it answered test get-price-r0b-1 - check application logs"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("reason %q; want it to begin %q", got, want)
	}

	// The app took the first test's request and exited before any result
	// was written: it had started, so this is no startup failure.
	got = describeTestSetFailure(models.TestSetStatusAppHalted, nil,
		runShape{appExit: appExitWhy(exit, "get-price-r0a-1"), heldTest: "get-price-r0a-1"})
	if !strings.Contains(got, "stopped during replay: it exited with code 3 before it answered test get-price-r0a-1") {
		t.Fatalf("an app that exited holding the set's first test reads %q", got)
	}

	got = describeTestSetFailure(models.TestSetStatusAppHalted, nil, runShape{appExit: appExitWhy(exit, "")})
	if !strings.HasPrefix(got, "application startup failed: it exited with code 3 - ") {
		t.Fatalf("a startup failure reads %q; want its exit code", got)
	}
}

func TestAppExitWhySaysOnlyWhatTheRunKnows(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    models.AppError
		held string
		want string
	}{
		{"keploy's own stop is no exit of the app's", models.AppError{AppErrorType: models.ErrCtxCanceled, ExitCode: -1}, "t-1", ""},
		{"no app error", models.AppError{}, "t-1", ""},
		{"a code above 0", models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3}, "", "it exited with code 3"},
		{"a clean exit", models.AppError{AppErrorType: models.ErrAppStopped}, "t-1", "it exited with code 0 before it answered test t-1"},
		// No code is known: none is made up.
		{"an unknown code", models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: -1}, "t-1", "it exited before it answered test t-1"},
		{"an internal error with no code", models.AppError{AppErrorType: models.ErrInternal}, "", "it exited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := appExitWhy(tc.e, tc.held); got != tc.want {
				t.Fatalf("appExitWhy = %q, want %q", got, tc.want)
			}
		})
	}
}

// A run that reached its end and failed does not say it completed
// "successfully" in its stop's log.
func TestAFailedReplaySaysSoInItsStopsLog(t *testing.T) {
	if got := replayEndStopReason(1); strings.Contains(got, "successfully") {
		t.Fatalf("a failed run's stop reason is %q", got)
	}
	if got := replayEndStopReason(0); got != "replay completed successfully" {
		t.Fatalf("a passing run's stop reason is %q", got)
	}
}

// heldRequests is a stand-in application that exits while it holds the
// request of the test named: it exits (exit), and gives the request no answer
// once keploy has ended the set's run, or after a bound, so a replayer that
// never learns of the exit fails the row rather than hanging it. It counts
// every request it takes.
type heldRequests struct {
	mu       sync.Mutex
	exitAt   string
	exit     func()
	requests map[string]int
}

func (h *heldRequests) onRequest(ctx context.Context, tc *models.TestCase) error {
	h.mu.Lock()
	h.requests[tc.Name]++
	first := h.requests[tc.Name] == 1
	h.mu.Unlock()
	if tc.Name != h.exitAt {
		return nil
	}
	if first {
		h.exit()
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
	}
	return io.EOF
}

func (h *heldRequests) count(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requests[name]
}

// An application that exits while it holds a test's request halts the set,
// and the set's reason names the exit's code and that test: the native path,
// where the app's run returns at its exit.
func TestAnAppsExitMidTestNamesTheTestItHeld(t *testing.T) {
	provenOn(t, false)
	h := newPartialRunHarness(t, 3, 0)
	h.instr.stopErr = models.ErrUnExpected
	app := &heldRequests{exitAt: "test-2", exit: func() { close(h.instr.appStopped) }, requests: map[string]int{}}
	h.replayer.hookImpl = prHooks{onRequest: app.onRequest}

	if status := h.run(t); status != models.TestSetStatusAppHalted {
		t.Fatalf("status %s, want APP_HALTED", status)
	}
	reason := h.report.report.FailureReason
	if !strings.Contains(reason, "it exited with code 1 before it answered test test-2") {
		t.Fatalf("reason %q; want it to name the exit's code and the test the app held", reason)
	}
	if n := app.count("test-3"); n != 0 {
		t.Fatalf("test-3 was sent %d times after the app had exited", n)
	}
}

// toldExitInstr runs a compose app the way `docker compose up` does with its
// abort on the app's exit: the runner tells the exit at the app's container's
// die (models.WithAppExit), and the run returns only once keploy's end of the
// set cancels it, compose's teardown of the rest of the stack being slower.
type toldExitInstr struct {
	*composeInstr
	died chan models.AppError
}

func (c *toldExitInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	select {
	case e := <-c.died:
		if told := models.AppExitOf(ctx); told != nil {
			told(e)
		}
	case <-ctx.Done():
	}
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled, ExitCode: -1}
}

// A compose app that exits while it holds a test's request halts the set at
// its die, with or without the proven switch, and whether the set brings its
// stack up (run) or the run keeps one stack for every set (runServing, the
// default with mocking: keploy runs the app once, in Start): the set is
// APP_HALTED, its reason names the exit's code and the test held, the held
// test is not sent again, and no test begins after it. The repro: the app
// exits 3 holding the set's second test while compose's up waits out the
// rest of the stack.
func TestAComposeAppsExitMidTestHaltsTheSetNamingTheTestItHeld(t *testing.T) {
	for _, tc := range []struct {
		name        string
		on, serving bool
	}{
		{"a stack per set, switch off", false, false},
		{"a stack per set, switch on", true, false},
		{"a stack kept across sets, switch off", false, true},
		{"a stack kept across sets, switch on", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, tc.on)
			c := &composeInstr{}
			h := composeHarnessOf(t, c, 3)
			exit := models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3}
			var tell func()
			if tc.serving {
				// Start runs the app under listenedTo's context: its
				// runner tells the exit through it.
				ctx, kept := listenedTo(context.Background())
				h.replayer.keptExit = kept
				tell = func() { models.AppExitOf(ctx)(exit) }
			} else {
				told := &toldExitInstr{composeInstr: c, died: make(chan models.AppError, 1)}
				h.replayer.instrumentation = told
				tell = func() { told.died <- exit }
			}
			app := &heldRequests{exitAt: "test-2", exit: tell, requests: map[string]int{}}
			h.replayer.hookImpl = prHooks{onRequest: app.onRequest}

			var status models.TestSetStatus
			if tc.serving {
				status = h.runServing(t)
			} else {
				status = h.run(t)
			}
			if status != models.TestSetStatusAppHalted {
				t.Fatalf("status %s, want APP_HALTED", status)
			}
			reason := h.report.report.FailureReason
			if !strings.Contains(reason, "it exited with code 3 before it answered test test-2") {
				t.Fatalf("reason %q; want it to name the exit's code and the test the app held", reason)
			}
			if n := app.count("test-2"); n != 1 {
				t.Fatalf("test-2 was sent %d times; the app had exited holding it, so it is sent once", n)
			}
			if n := app.count("test-3"); n != 0 {
				t.Fatalf("test-3 was sent %d times after the app had exited", n)
			}
		})
	}
}

// The exit the run's application tells is kept once, the first word: a later
// tell (the run's return after the die) does not replace it.
func TestTheKeptAppExitIsTheFirstWord(t *testing.T) {
	ctx, kept := listenedTo(context.Background())
	told := models.AppExitOf(ctx)
	if told == nil {
		t.Fatal("the run's context carries no listener for the app's exit")
	}
	select {
	case <-kept.told:
		t.Fatal("an exit was told before the app exited")
	default:
	}
	told(models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3})
	told(models.AppError{AppErrorType: models.ErrAppStopped})
	select {
	case <-kept.told:
	default:
		t.Fatal("the told exit was not kept")
	}
	if kept.exit.ExitCode != 3 {
		t.Fatalf("kept exit %+v; want the first word, code 3", kept.exit)
	}
}
