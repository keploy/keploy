package replay

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// An app's exit told while keploy readies a streaming test case on its agent
// ends the set before that test's request is sent, as in the set's main loop,
// with the proven switch off and on: the set is APP_HALTED, its reason names
// the exit's code and no test as held, and the streaming test is not sent.
// The streaming tests' loop looked for the exit only at its head, and sent
// the test to an app keploy knew had exited.
func TestAnAppsExitWhileKeployReadiesAStreamingTestSendsItNot(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch on %v", on), func(t *testing.T) {
			provenOn(t, on)
			h := newPartialRunHarness(t, 3, 0)
			h.streaming("test-3")
			h.instr.stopErr = models.ErrUnExpected
			h.instr.stopAppReadying = "test-3"
			app := &heldRequests{requests: map[string]int{}}
			h.replayer.hookImpl = prHooks{onRequest: app.onRequest}

			if status := h.run(t); status != models.TestSetStatusAppHalted {
				t.Fatalf("status %s, want APP_HALTED", status)
			}
			reason := h.report.report.FailureReason
			if !strings.Contains(reason, "it exited with code 1") || strings.Contains(reason, "before it answered test") {
				t.Fatalf("reason %q; want it to name the exit's code and no test held", reason)
			}
			for name, want := range map[string]int{"test-1": 1, "test-2": 1, "test-3": 0} {
				if n := app.count(name); n != want {
					t.Fatalf("%s was sent %d times, want %d: the app exited as keploy readied test-3", name, n, want)
				}
			}
		})
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

// composeExitShapes are the compose runs an app's exit mid-set halts: with
// or without the proven switch, and whether the set brings its stack up
// (run) or the run keeps one stack for every set (runServing, the default
// with mocking: keploy runs the app once, in Start).
var composeExitShapes = []struct {
	name        string
	on, serving bool
}{
	{"a stack per set, switch off", false, false},
	{"a stack per set, switch on", true, false},
	{"a stack kept across sets, switch off", false, true},
	{"a stack kept across sets, switch on", true, true},
}

// composeExitRun is a compose set of three tests whose app exits with code
// 3 while it holds test-2, the repro's shape: compose's up waits out the rest
// of the stack meanwhile. adjust, set, readies the harness and the app before
// the run.
type composeExitRun struct {
	h      *prRun
	app    *heldRequests
	status models.TestSetStatus
}

func runComposeExitMidTest(t *testing.T, on, serving bool, adjust func(*prRun, *heldRequests)) composeExitRun {
	t.Helper()
	provenOn(t, on)
	c := &composeInstr{}
	h := composeHarnessOf(t, c, 3)
	exit := models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3}
	var tell func()
	if serving {
		// Start runs the app under listenedTo's context: its runner tells
		// the exit through it.
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
	if adjust != nil {
		adjust(h, app)
	}
	r := composeExitRun{h: h, app: app}
	if serving {
		r.status = h.runServing(t)
	} else {
		r.status = h.run(t)
	}
	return r
}

// A compose app that exits while it holds a test's request halts the set at
// its die, in every compose shape (composeExitShapes): the set is
// APP_HALTED, its reason names the exit's code and the test held, the held
// test is not sent again, and no test begins after it.
func TestAComposeAppsExitMidTestHaltsTheSetNamingTheTestItHeld(t *testing.T) {
	for _, tc := range composeExitShapes {
		t.Run(tc.name, func(t *testing.T) {
			r := runComposeExitMidTest(t, tc.on, tc.serving, nil)
			if r.status != models.TestSetStatusAppHalted {
				t.Fatalf("status %s, want APP_HALTED", r.status)
			}
			reason := r.h.report.report.FailureReason
			if !strings.Contains(reason, "it exited with code 3 before it answered test test-2") {
				t.Fatalf("reason %q; want it to name the exit's code and the test the app held", reason)
			}
			if n := r.app.count("test-2"); n != 1 {
				t.Fatalf("test-2 was sent %d times; the app had exited holding it, so it is sent once", n)
			}
			if n := r.app.count("test-3"); n != 0 {
				t.Fatalf("test-3 was sent %d times after the app had exited", n)
			}
		})
	}
}

// A set its app's exit halted begins no mock-consistency retry (retryPassing):
// the passing tests are not run again against an app that is gone, and no
// retry cycle begins, its log and its rewind of the consumed mocks with it.
// The repro: the retry cycle began 0.6 ms after the told exit, the held
// test, the set's last, having failed on the set's end; a held test with
// another after it ends the cycle at that one.
func TestAHaltedSetBeginsNoRetryCycle(t *testing.T) {
	for _, held := range []string{"test-3", "test-2"} {
		for _, tc := range composeExitShapes {
			t.Run(held+" held, "+tc.name, func(t *testing.T) {
				haltedSetBeginsNoRetryCycle(t, held, tc.on, tc.serving)
			})
		}
	}
}

func haltedSetBeginsNoRetryCycle(t *testing.T, held string, on, serving bool) {
	const retrying = "Retrying passing test cases to validate mock consistency"
	core, logs := observer.New(zapcore.InfoLevel)
	r := runComposeExitMidTest(t, on, serving, func(h *prRun, app *heldRequests) {
		h.replayer.config.RetryPassing = true
		h.replayer.logger = zap.New(core)
		app.exitAt = held
	})
	if r.status != models.TestSetStatusAppHalted {
		t.Fatalf("status %s, want APP_HALTED", r.status)
	}
	if n := logs.FilterMessage(retrying).Len(); n != 0 {
		t.Fatalf("%d retry cycles began after the app had exited; want none", n)
	}
	if n := r.app.count("test-1"); n != 1 {
		t.Fatalf("test-1 was sent %d times; the set halted before any retry, so it is sent once", n)
	}
}

// agentCalls is a compose stack's instrumentation that notes each call that
// stages a set on keploy's agent or starts or stops the app's container: a
// set that begins after the app has gone makes none.
type agentCalls struct {
	*composeInstr
	mu    sync.Mutex
	calls []string
}

func (a *agentCalls) note(call string) {
	a.mu.Lock()
	a.calls = append(a.calls, call)
	a.mu.Unlock()
}

func (a *agentCalls) made() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func (a *agentCalls) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	a.note("MockOutgoing")
	return a.composeInstr.MockOutgoing(ctx, opts)
}

func (a *agentCalls) StoreMocks(ctx context.Context, filtered, unfiltered []*models.Mock) error {
	a.note("StoreMocks")
	return a.composeInstr.StoreMocks(ctx, filtered, unfiltered)
}

func (a *agentCalls) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	a.note("UpdateMockParams")
	return a.composeInstr.UpdateMockParams(ctx, params)
}

func (a *agentCalls) MakeAgentReadyForDockerCompose(ctx context.Context) error {
	a.note("MakeAgentReadyForDockerCompose")
	return a.composeInstr.MakeAgentReadyForDockerCompose(ctx)
}

func (a *agentCalls) BeforeTestSetCompose(ctx context.Context, testRunID, firstRunTestSet string, serve bool) error {
	a.note("BeforeTestSetCompose")
	return a.composeInstr.BeforeTestSetCompose(ctx, testRunID, firstRunTestSet, serve)
}

func (a *agentCalls) StartComposeApp(ctx context.Context) (models.ComposeAppStart, error) {
	a.note("StartComposeApp")
	return a.composeInstr.StartComposeApp(ctx)
}

func (a *agentCalls) StopComposeApp(ctx context.Context) (string, error) {
	a.note("StopComposeApp")
	return a.composeInstr.StopComposeApp(ctx)
}

// A set that begins after the app the run keeps for every set has exited
// says so, naming the set it exited in, and begins nothing: no test is sent
// and nothing is staged on keploy's agent, whose stack went down with the
// app. Its app did not fail to start, it was gone. The repro's second set
// staged its mocks on the gone agent, failed "context canceled", and the run
// stopped on that, the set unreported.
func TestASetAfterTheKeptAppsExitSaysTheAppWasGone(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch on %v", on), func(t *testing.T) {
			r := runComposeExitMidTest(t, on, true, nil)
			if r.status != models.TestSetStatusAppHalted {
				t.Fatalf("first set: status %s, want APP_HALTED", r.status)
			}
			calls := &agentCalls{composeInstr: r.h.replayer.instrumentation.(*composeInstr)}
			r.h.replayer.instrumentation = calls
			r.h.set = "test-set-1"
			if status := r.h.runServing(t); status != models.TestSetStatusAppHalted {
				t.Fatalf("second set: status %s, want APP_HALTED", status)
			}
			rep := r.h.report.report
			const want = "application exited before this test set began: it exited with code 3 while test set test-set-0 ran - check application logs"
			if rep.TestSet != "test-set-1" || rep.Status != string(models.TestSetStatusAppHalted) || !strings.Contains(rep.FailureReason, want) {
				t.Fatalf("second set's report: set %s, status %s, reason %q; want test-set-1 APP_HALTED with %q", rep.TestSet, rep.Status, rep.FailureReason, want)
			}
			if rep.Total != 3 || rep.Success+rep.Failure != 0 {
				t.Fatalf("second set's report: total %d, %d passed, %d failed; want its 3 tests, none run", rep.Total, rep.Success, rep.Failure)
			}
			if made := calls.made(); len(made) != 0 {
				t.Fatalf("the second set called %v; the app and its stack were gone, so it calls nothing", made)
			}
			if r.h.replayer.saysResultsUnread(models.TestSetStatusAppHalted, "test-set-1") {
				t.Fatal("the second set's missing results would be said as an error; it said why it ran none")
			}
			for name, was := range map[string]int{"test-1": 1, "test-2": 1, "test-3": 0} {
				if n := r.app.count(name); n != was {
					t.Fatalf("%s was sent %d times, %d of them by the second set; the app was gone, so it sends none", name, n, n-was)
				}
			}
		})
	}
}

// An exit told before any set began is the app's startup failing: the first
// set says that, with the exit's code, and begins nothing.
func TestAKeptAppGoneBeforeTheFirstSetFailedToStart(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch on %v", on), func(t *testing.T) {
			provenOn(t, on)
			c := &composeInstr{}
			h := composeHarnessOf(t, c, 3)
			ctx, kept := listenedTo(context.Background())
			h.replayer.keptExit = kept
			models.AppExitOf(ctx)(models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3})
			app := &heldRequests{requests: map[string]int{}}
			h.replayer.hookImpl = prHooks{onRequest: app.onRequest}
			calls := &agentCalls{composeInstr: c}
			h.replayer.instrumentation = calls
			if status := h.runServing(t); status != models.TestSetStatusAppHalted {
				t.Fatalf("status %s, want APP_HALTED", status)
			}
			const want = "application startup failed: it exited with code 3 - check application logs"
			if reason := h.report.report.FailureReason; !strings.Contains(reason, want) {
				t.Fatalf("reason %q; want %q", reason, want)
			}
			if made := calls.made(); len(made) != 0 {
				t.Fatalf("the set called %v; the app was gone, so it calls nothing", made)
			}
			if h.replayer.saysResultsUnread(models.TestSetStatusAppHalted, "test-set-0") {
				t.Fatal("the set's missing results would be said as an error; it said why it ran none")
			}
			for _, name := range []string{"test-1", "test-2", "test-3"} {
				if n := app.count(name); n != 0 {
					t.Fatalf("%s was sent %d times; the app was gone, so the set sends none", name, n)
				}
			}
		})
	}
}

// A set halted before its first test counts the tests the run selected of it,
// as a set that ran does, and its report carries the app's last logs: the
// told exit carries none (its runner reads none at the die), so they are read
// then, as at the end of a set that ran.
func TestASetHaltedBeforeItsTestsCountsTheSelectedOnesAndCarriesTheAppsLogs(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch on %v", on), func(t *testing.T) {
			provenOn(t, on)
			c := &composeInstr{}
			h := composeHarnessOf(t, c, 3)
			h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {"test-1", "test-3"}}
			h.instr.recentAppLogs = "app: exiting with code 3\n"
			ctx, kept := listenedTo(context.Background())
			h.replayer.keptExit = kept
			models.AppExitOf(ctx)(models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3})
			if status := h.runServing(t); status != models.TestSetStatusAppHalted {
				t.Fatalf("status %s, want APP_HALTED", status)
			}
			rep := h.report.report
			if rep.Total != 2 || rep.Success+rep.Failure != 0 {
				t.Fatalf("total %d, %d passed, %d failed; want the 2 tests selected of the set, none run", rep.Total, rep.Success, rep.Failure)
			}
			if rep.AppLogs != h.instr.recentAppLogs {
				t.Fatalf("app_logs %q; want the app's last logs, %q", rep.AppLogs, h.instr.recentAppLogs)
			}
		})
	}
}

// When an exit was told is said from the set running then, or the one that
// had ended last: an exit before any set ran is the app's startup failing.
func TestTheKeptExitSaysWhenItWasTold(t *testing.T) {
	exit := models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3}
	for _, tc := range []struct {
		name string
		// before runs the sets, by name, that begin and end before the
		// exit, and during the one running at it ("" for none).
		before        []string
		during, begin string
		want          string
	}{
		{"before any set", nil, "", "s0", ""},
		{"while a set ran", nil, "s0", "s1", "while test set s0 ran"},
		{"between sets", []string{"s0"}, "", "s1", "after test set s0 ended"},
		{"while an earlier attempt ran", nil, "s0", "s0", "while an earlier attempt of this test set ran"},
		{"after an earlier attempt", []string{"s0"}, "", "s0", "after an earlier attempt of this test set ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, kept := listenedTo(context.Background())
			for _, s := range tc.before {
				gone, _, end := kept.begin(s)
				if gone {
					t.Fatalf("set %s began with the app gone before its exit", s)
				}
				end()
			}
			if tc.during != "" {
				_, _, end := kept.begin(tc.during)
				models.AppExitOf(ctx)(exit)
				end()
			} else {
				models.AppExitOf(ctx)(exit)
			}
			gone, earlier, end := kept.begin(tc.begin)
			defer end()
			if !gone || earlier != tc.want {
				t.Fatalf("begin: gone %v, earlier %q; want gone, %q", gone, earlier, tc.want)
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
