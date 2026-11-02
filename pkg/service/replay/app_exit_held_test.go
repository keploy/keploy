package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
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
		runShape{appExit: appExitWhy(exit, testHeld{name: "get-price-r0b-1"}), heldTest: "get-price-r0b-1"})
	want := "application stopped during replay: it exited with code 3 before it answered test get-price-r0b-1 - check application logs"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("reason %q; want it to begin %q", got, want)
	}

	// The app took the first test's request and exited before any result
	// was written: it had started, so this is no startup failure.
	got = describeTestSetFailure(models.TestSetStatusAppHalted, nil,
		runShape{appExit: appExitWhy(exit, testHeld{name: "get-price-r0a-1"}), heldTest: "get-price-r0a-1"})
	if !strings.Contains(got, "stopped during replay: it exited with code 3 before it answered test get-price-r0a-1") {
		t.Fatalf("an app that exited holding the set's first test reads %q", got)
	}

	got = describeTestSetFailure(models.TestSetStatusAppHalted, nil, runShape{appExit: appExitWhy(exit, testHeld{})})
	if !strings.HasPrefix(got, "application startup failed: it exited with code 3 - ") {
		t.Fatalf("a startup failure reads %q; want its exit code", got)
	}
}

func TestAppExitWhySaysOnlyWhatTheRunKnows(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    models.AppError
		held testHeld
		want string
	}{
		{"keploy's own stop is no exit of the app's", models.AppError{AppErrorType: models.ErrCtxCanceled, ExitCode: -1}, testHeld{name: "t-1"}, ""},
		{"no app error", models.AppError{}, testHeld{name: "t-1"}, ""},
		{"a code above 0", models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3}, testHeld{}, "it exited with code 3"},
		{"a clean exit", models.AppError{AppErrorType: models.ErrAppStopped}, testHeld{name: "t-1"}, "it exited with code 0 before it answered test t-1"},
		// No code is known: none is made up.
		{"an unknown code", models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: -1}, testHeld{name: "t-1"}, "it exited before it answered test t-1"},
		{"an internal error with no code", models.AppError{AppErrorType: models.ErrInternal}, testHeld{}, "it exited"},
		// A streaming test's head came, its body still being read.
		{"an answer begun", models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3}, testHeld{name: "t-1", answering: true}, "it exited with code 3 while it was answering test t-1"},
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

// An app's exit while a set runs its streaming tests names, as the one the
// app held, only a test whose answer it had not given whole, with the proven
// switch off and on, and the set is APP_HALTED with the exit's code:
//   - told as keploy readies test-3 on its agent, test-2 answered whole
//     (read by its send, or as a stream read to its end): test-3 is not sent,
//     and no test is named;
//   - told while test-3's stream is read, its head come: "while it was
//     answering test test-3";
//   - told after test-3's stream broke part-way, as keploy readies test-4:
//     test-3's answer was never whole, so it is named as above;
//   - told while test-3's send waits for its head: "before it answered test
//     test-3", and so for test-4's after test-3's stream broke.
//
// The streaming tests' loop looked for the exit only at its head, and sent
// the test to an app keploy knew had exited; and it took a stream's head for
// its whole answer.
func TestAnAppsExitWhileKeployReadiesAStreamingTestSendsItNot(t *testing.T) {
	whole := func(_ context.Context, tc *models.TestCase, _ func()) *pkg.StreamingHTTPResponse {
		return &pkg.StreamingHTTPResponse{StatusCode: tc.HTTPResp.StatusCode, Header: tc.HTTPResp.Header,
			Reader: io.NopCloser(strings.NewReader(tc.HTTPResp.Body)), StreamConfig: pkg.DetectHTTPStreamConfig(tc, nil)}
	}
	exitMidBody := func(ctx context.Context, tc *models.TestCase, exit func()) *pkg.StreamingHTTPResponse {
		return &pkg.StreamingHTTPResponse{StatusCode: tc.HTTPResp.StatusCode, Header: tc.HTTPResp.Header,
			Reader: io.NopCloser(&exitingBody{ctx: ctx, exit: exit}), StreamConfig: pkg.DetectHTTPStreamConfig(tc, nil)}
	}
	for _, tc := range []struct {
		name      string
		n         int
		streaming []string
		// readying is the test whose readying on the agent the exit lands
		// in; answers, the tests answered by a stream, given the exit.
		readying string
		answers  map[string]func(context.Context, *models.TestCase, func()) *pkg.StreamingHTTPResponse
		// exitAt is the test whose send the exit lands in, before its head
		// comes; broken, the tests whose stream breaks part-way.
		exitAt string
		broken []string
		// held is the phrase the reason names the held test in, "" for
		// none; sent, how many times each test is sent.
		held string
		sent map[string]int
	}{
		{"not sent, test-2 answered by its send", 3, []string{"test-2", "test-3"}, "test-3", nil, "", nil,
			"", map[string]int{"test-1": 1, "test-2": 1, "test-3": 0}},
		{"not sent, test-2's stream read whole", 3, []string{"test-2", "test-3"}, "test-3",
			map[string]func(context.Context, *models.TestCase, func()) *pkg.StreamingHTTPResponse{"test-2": whole}, "", nil,
			"", map[string]int{"test-1": 1, "test-2": 1, "test-3": 0}},
		{"its stream being read", 3, []string{"test-2", "test-3"}, "",
			map[string]func(context.Context, *models.TestCase, func()) *pkg.StreamingHTTPResponse{"test-2": whole, "test-3": exitMidBody}, "", nil,
			"while it was answering test test-3", map[string]int{"test-1": 1, "test-2": 1, "test-3": 1}},
		{"its stream broken, then the exit", 4, []string{"test-3", "test-4"}, "test-4", nil, "", []string{"test-3"},
			"while it was answering test test-3", map[string]int{"test-1": 1, "test-2": 1, "test-3": 1, "test-4": 0}},
		{"its head not come", 3, []string{"test-2", "test-3"}, "", nil, "test-3", nil,
			"before it answered test test-3", map[string]int{"test-1": 1, "test-2": 1, "test-3": 1}},
		{"its head not come, the stream before it broken", 4, []string{"test-3", "test-4"}, "", nil, "test-4", []string{"test-3"},
			"before it answered test test-4", map[string]int{"test-1": 1, "test-2": 1, "test-3": 1, "test-4": 1}},
	} {
		for _, on := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, switch on %v", tc.name, on), func(t *testing.T) {
				provenOn(t, on)
				h := newPartialRunHarness(t, tc.n, 0)
				h.streaming(tc.streaming...)
				h.instr.stopErr = models.ErrUnExpected
				h.instr.stopAppReadying = tc.readying
				exit := func() { close(h.instr.appStopped) }
				app := &heldRequests{exitAt: tc.exitAt, exit: exit, requests: map[string]int{}}
				hooks := prHooks{onRequest: app.onRequest, streamErr: map[string]error{}}
				for _, name := range tc.broken {
					hooks.streamErr[name] = io.ErrUnexpectedEOF
				}
				h.replayer.hookImpl = answerHooks{prHooks: hooks, answers: tc.answers, exit: exit}

				if status := h.run(t); status != models.TestSetStatusAppHalted {
					t.Fatalf("status %s, reason %q; want APP_HALTED", status, h.report.report.FailureReason)
				}
				reason := h.report.report.FailureReason
				named := strings.Contains(reason, "answered test") || strings.Contains(reason, "answering test")
				if !strings.Contains(reason, "it exited with code 1") || (tc.held == "") == named || (tc.held != "" && !strings.Contains(reason, tc.held)) {
					t.Fatalf("reason %q; want it to name the exit's code and %q", reason, tc.held)
				}
				for name, want := range tc.sent {
					if n := app.count(name); n != want {
						t.Fatalf("%s was sent %d times, want %d", name, n, want)
					}
				}
			})
		}
	}
}

// answerHooks answers the tests named in answers with the stream each
// gives, under the send's context, and exit as the app's exit; the rest as
// prHooks does. Every send is taken by prHooks.onRequest first.
type answerHooks struct {
	prHooks
	answers map[string]func(context.Context, *models.TestCase, func()) *pkg.StreamingHTTPResponse
	exit    func()
}

func (h answerHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	answer := h.answers[tc.Name]
	if answer == nil {
		return h.prHooks.SimulateRequest(ctx, tc, testSetID)
	}
	if err := h.onRequest(ctx, tc); err != nil {
		return nil, err
	}
	return answer(ctx, tc, h.exit), nil
}

// exitingBody is a stream whose app exits at its first read: the read waits
// until keploy has ended the set's run (ctx), or a bound, so a replayer that
// never learns of the exit fails the row rather than hanging it, and then
// fails as a stream cut part-way does.
type exitingBody struct {
	ctx  context.Context
	exit func()
	once sync.Once
}

func (b *exitingBody) Read([]byte) (int, error) {
	b.once.Do(b.exit)
	select {
	case <-b.ctx.Done():
	case <-time.After(10 * time.Second):
	}
	return 0, io.ErrUnexpectedEOF
}

// toldExitInstr runs a compose app the way `docker compose up` does with its
// abort on the app's exit: the runner tells the exit at the app's container's
// die (models.WithAppExit), and the run returns only once keploy's end of the
// set cancels it, compose's teardown of the rest of the stack being slower.
type toldExitInstr struct {
	*composeInstr
	died chan models.AppError
	// toldBack is closed once the exit is told: the tell is the runner's
	// call to its caller's listener, which returns before the die's tell
	// does.
	toldBack chan struct{}
}

func (c *toldExitInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	select {
	case e := <-c.died:
		if told := models.AppExitOf(ctx); told != nil {
			told(e)
		}
		close(c.toldBack)
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
		told := &toldExitInstr{composeInstr: c, died: make(chan models.AppError, 1), toldBack: make(chan struct{})}
		h.replayer.instrumentation = told
		tell = func() { told.died <- exit; <-told.toldBack }
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

// An app that exits holding a test fails that test's request (its
// connection reset) before keploy learns of its exit: under compose the
// app's die is told after the request failed. The test was never answered,
// so the set halts naming it: here the die is told once keploy has filed
// the test's failure, the set's last, for a stack the set brings up (whose
// runner tells the die to the set as it comes).
func TestAnExitToldAfterTheHeldTestsRequestFailedNamesThatTest(t *testing.T) {
	for _, tc := range composeExitShapes {
		if tc.serving {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			r := runComposeExitMidTest(t, tc.on, tc.serving, func(h *prRun, app *heldRequests) {
				tell := app.exit
				app.exitAt = ""
				h.replayer.hookImpl = prHooks{onRequest: app.onRequest,
					simErr: map[string]error{"test-3": errors.New("read tcp 172.18.0.3:8080: connection reset by peer")}}
				h.report.afterInsert = func(_ context.Context, name string) {
					if name == "test-3" {
						tell()
					}
				}
			})
			if r.status != models.TestSetStatusAppHalted {
				t.Fatalf("status %s, want APP_HALTED", r.status)
			}
			reason := r.h.report.report.FailureReason
			if !strings.Contains(reason, "it exited with code 3 before it answered test test-3") {
				t.Fatalf("reason %q; want it to name the exit's code and test-3, whose request failed on the exit", reason)
			}
		})
	}
}

// A run with no agent whose context ends after a cycle's last test, a test
// of the cycle having failed, begins no mock-consistency retry: the set
// ends on its user's stop, and the passing tests are not run again.
func TestARunWithNoAgentEndedInAFailingCycleBeginsNoRetry(t *testing.T) {
	const retrying = "Retrying passing test cases to validate mock consistency"
	provenOn(t, false)
	core, logs := observer.New(zapcore.InfoLevel)
	h := newPartialRunHarness(t, 3, 0)
	h.replayer.noAgent = true
	h.replayer.config.RetryPassing = true
	h.replayer.logger = zap.New(core)
	app := &heldRequests{requests: map[string]int{}}
	h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-2": true},
		onRequest: func(ctx context.Context, tc *models.TestCase) error {
			err := app.onRequest(ctx, tc)
			if tc.Name == "test-3" {
				h.cancel()
			}
			return err
		}}

	if status := h.run(t); status != models.TestSetStatusUserAbort {
		t.Fatalf("status %s, want USER_ABORT", status)
	}
	if n := logs.FilterMessage(retrying).Len(); n != 0 {
		t.Fatalf("%d retry cycles began after the run's context had ended; want none", n)
	}
	if n := app.count("test-1"); n != 1 {
		t.Fatalf("test-1 was sent %d times; the set ended before any retry, so it is sent once", n)
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
// as a set that ran does, in its report and in the run's totals, and its
// report carries the app's last logs: the told exit carries none (its runner
// reads none at the die), so they are read then, as at the end of a set that
// ran. A run whose every set halted so prints its summary, with those tests
// in it: one with no test counted prints none.
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
			if total, passed, failed := h.replayer.GetTestRunTotals(); total != 2 || passed+failed != 0 {
				t.Fatalf("the run's totals: %d, %d passed, %d failed; want the set's 2 tests, none run", total, passed, failed)
			}
			// Its plain form: the coloured one's printer keeps the stdout
			// it began with, which captureStdout does not swap.
			h.replayer.config.DisableANSI = true
			out := captureStdout(t, func() { h.replayer.printSummary(t.Context(), false) })
			if !strings.Contains(out, "COMPLETE TESTRUN SUMMARY") || !strings.Contains(out, "Total tests: 2\n") {
				t.Fatalf("the run's summary, every set halted before its tests:\n%s", out)
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

// The status of a set keploy knows its app exited in is the exit's, whether
// the set's watchers have set theirs yet or not: the app's listener keeps the
// exit as it is told, and the reader that ends the set on it sets the status
// only after. The user's stop is no exit: its status stands.
func TestStatusOnExitIsTheExitsWhicheverWatcherRanFirst(t *testing.T) {
	for _, set := range []models.TestSetStatus{models.TestSetStatusRunning, models.TestSetStatusAppHalted, models.TestSetStatusUserAbort} {
		if got := statusOnExit(set, models.TestSetStatusAppHalted, true); got != models.TestSetStatusAppHalted {
			t.Fatalf("watchers' status %s with the exit known: %s, want APP_HALTED", set, got)
		}
	}
	if got := statusOnExit(models.TestSetStatusUserAbort, "", false); got != models.TestSetStatusUserAbort {
		t.Fatalf("the user's stop with no exit known: %s, want USER_ABORT", got)
	}
}

// A set whose app exits holding its last test, whose request failed on the
// exit, ends the same whichever comes first once the exit is told: the reader
// of the app's errors setting the set's status and ending it, or the set's
// end reading its status after its last test. In every compose shape it is
// APP_HALTED and names the exit's code and that test. The set's end read the
// status only as the reader set it, so the listener and the kept app's watch
// set it as well, before the reader: two writers of one status.
func TestASetsOutcomeIsTheSameWhicheverOfItsExitsReadersRunsFirst(t *testing.T) {
	for _, readerFirst := range []bool{true, false} {
		for _, tc := range composeExitShapes {
			t.Run(fmt.Sprintf("reader first %v, %s", readerFirst, tc.name), func(t *testing.T) {
				r := runComposeExitMidTest(t, tc.on, tc.serving, func(h *prRun, app *heldRequests) {
					tell := app.exit
					app.exitAt = ""
					h.replayer.hookImpl = prHooks{onRequest: app.onRequest,
						simErr: map[string]error{"test-3": errors.New("read tcp 172.18.0.3:8080: connection reset by peer")}}
					h.report.afterInsert = func(ctx context.Context, name string) {
						if name != "test-3" {
							return
						}
						tell()
						if readerFirst {
							// The reader ends the set's context once it has
							// set the set's status.
							<-ctx.Done()
						}
					}
				})
				if r.status != models.TestSetStatusAppHalted {
					t.Fatalf("status %s, want APP_HALTED", r.status)
				}
				reason := r.h.report.report.FailureReason
				if !strings.Contains(reason, "it exited with code 3 before it answered test test-3") {
					t.Fatalf("reason %q; want it to name the exit's code and test-3, whose request failed on the exit", reason)
				}
			})
		}
	}
}

// An app's exit told while keploy readies a test case of the set's main loop
// on its agent ends the set before that test's request is sent, with the
// proven switch off and on: the set is APP_HALTED, its reason names the
// exit's code and no test as held, and the test is not sent.
func TestAnAppsExitWhileKeployReadiesATestSendsItNot(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch on %v", on), func(t *testing.T) {
			provenOn(t, on)
			h := newPartialRunHarness(t, 3, 0)
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

// exitLogHold holds the reader of a set's app errors in its log of the app's
// exit (applicationFailedToRunLogMessage), which it writes before it ends the
// set (exitLoopChan, the set's cancel), until release: keploy knows of the
// exit meanwhile (the app's listener, the kept app's tell), and the reader
// has not ended the set. A bound keeps a miswired row from hanging; the row
// fails if the bound, not release, let the reader go (boundHit).
type exitLogHold struct {
	once     sync.Once
	released chan struct{}
	boundHit atomic.Bool
}

func (h *exitLogHold) release() { h.once.Do(func() { close(h.released) }) }

type exitLogHoldCore struct {
	zapcore.Core
	h *exitLogHold
}

func (c exitLogHoldCore) Enabled(l zapcore.Level) bool {
	return l >= zapcore.ErrorLevel || c.Core.Enabled(l)
}

func (c exitLogHoldCore) With(f []zapcore.Field) zapcore.Core {
	return exitLogHoldCore{c.Core.With(f), c.h}
}

func (c exitLogHoldCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if e.Message == applicationFailedToRunLogMessage {
		select {
		case <-c.h.released:
		case <-time.After(10 * time.Second):
			c.h.boundHit.Store(true)
		}
	}
	return c.Core.Check(e, ce)
}

// holdExitLog makes r's logger hold the reader of its sets' app errors
// (exitLogHold).
func holdExitLog(t *testing.T, r *Replayer) *exitLogHold {
	t.Helper()
	h := &exitLogHold{released: make(chan struct{})}
	r.logger = zap.New(exitLogHoldCore{r.logger.Core(), h})
	t.Cleanup(h.release)
	return h
}

// An app's exit keploy knows of ends the set before its next test, though
// the reader of the app's errors has not ended it yet: the app's listener
// keeps the exit as it is told (the kept app's runner, its tell), and the
// reader logs it before it ends the set. Here the exit is told as keploy
// readies test-3 on its agent, and the reader is held in that log until the
// set reads its results: in every compose shape test-3 is not sent, and the
// set halts naming the exit's code and no test held.
func TestAnExitKeployKnowsOfEndsTheSetBeforeItsReaderDoes(t *testing.T) {
	for _, tc := range composeExitShapes {
		t.Run(tc.name, func(t *testing.T) {
			var hold *exitLogHold
			r := runComposeExitMidTest(t, tc.on, tc.serving, func(h *prRun, app *heldRequests) {
				hold = holdExitLog(t, h.replayer)
				tell := app.exit
				app.exitAt = ""
				h.replayer.hookImpl = prHooks{onRequest: func(ctx context.Context, c *models.TestCase) error {
					if c.Name == "test-3" {
						hold.release()
					}
					return app.onRequest(ctx, c)
				}}
				var once sync.Once
				h.instr.onUpdate = func(p models.MockFilterParams) {
					if h.instr.readies(p, "test-3") {
						once.Do(tell)
					}
				}
				h.report.onRead = hold.release
			})
			if hold.boundHit.Load() {
				t.Fatal("the reader was let go by the row's bound: the set never read its results")
			}
			if n := r.app.count("test-3"); n != 0 {
				t.Fatalf("test-3 was sent %d times; keploy knew the app had exited", n)
			}
			if r.status != models.TestSetStatusAppHalted {
				t.Fatalf("status %s, want APP_HALTED", r.status)
			}
			reason := r.h.report.report.FailureReason
			if !strings.Contains(reason, "it exited with code 3") || strings.Contains(reason, "before it answered test") {
				t.Fatalf("reason %q; want it to name the exit's code and no test held", reason)
			}
		})
	}
}

// goneReal is keploy's agent with mock.real once it went down with the app's
// stack: the set's end cannot read what the run reached.
type goneReal struct{}

func (goneReal) PushRealDeps(context.Context, []string) error { return nil }
func (goneReal) KeepRealDeps(context.Context, []string) error { return nil }
func (goneReal) RealTest(context.Context) (models.RealTest, error) {
	return models.RealTest{}, nil
}
func (goneReal) RealRun(context.Context) (models.RealRun, error) {
	return models.RealRun{}, errors.New("dial tcp 172.18.0.2:16789: connect: connection refused")
}

type goneRealTold struct {
	*toldExitInstr
	goneReal
}

type goneRealCompose struct {
	*composeInstr
	goneReal
}

// A set its app's exit halted keeps the exit's status through its end's
// reads: with the proven switch and mock.real, the agent that went down with
// the app cannot say what the run reached, and the set is APP_HALTED naming
// the exit, not "not judged" (INTERNAL_ERR) for the read. Those reads judge
// only a set with a verdict to give, and a halted set has its status as the
// loop ends on the exit.
func TestAHaltedSetIsNotJudgedOnReadsItsGoneAgentFails(t *testing.T) {
	for _, tc := range composeExitShapes {
		if !tc.on {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			r := runComposeExitMidTest(t, tc.on, tc.serving, func(h *prRun, _ *heldRequests) {
				h.replayer.config.Mock.Real = []string{"db:5432"}
				switch in := h.replayer.instrumentation.(type) {
				case *toldExitInstr:
					h.replayer.instrumentation = &goneRealTold{toldExitInstr: in}
				case *composeInstr:
					h.replayer.instrumentation = &goneRealCompose{composeInstr: in}
				default:
					t.Fatalf("instrumentation %T", in)
				}
			})
			if r.status != models.TestSetStatusAppHalted {
				t.Fatalf("status %s (reason %q), want APP_HALTED", r.status, r.h.report.report.FailureReason)
			}
			reason := r.h.report.report.FailureReason
			if !strings.Contains(reason, "it exited with code 3 before it answered test test-2") || strings.Contains(reason, "judged") {
				t.Fatalf("reason %q; want it to name the exit and the test held, and nothing of the gone agent's read", reason)
			}
		})
	}
}

// A streaming test whose request failed on the app's exit (its connection
// reset), the exit told once keploy filed it, was never answered: the set
// halts naming it, as a test of the main loop is named
// (TestAnExitToldAfterTheHeldTestsRequestFailedNamesThatTest).
func TestAnExitToldAfterAHeldStreamingTestsRequestFailedNamesThatTest(t *testing.T) {
	for _, tc := range composeExitShapes {
		if tc.serving {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			r := runComposeExitMidTest(t, tc.on, tc.serving, func(h *prRun, app *heldRequests) {
				h.streaming("test-3")
				tell := app.exit
				app.exitAt = ""
				h.replayer.hookImpl = prHooks{onRequest: app.onRequest,
					simErr: map[string]error{"test-3": errors.New("read tcp 172.18.0.3:8080: connection reset by peer")}}
				h.report.afterInsert = func(_ context.Context, name string) {
					if name == "test-3" {
						tell()
					}
				}
			})
			if r.status != models.TestSetStatusAppHalted {
				t.Fatalf("status %s, want APP_HALTED", r.status)
			}
			reason := r.h.report.report.FailureReason
			if !strings.Contains(reason, "it exited with code 3 before it answered test test-3") {
				t.Fatalf("reason %q; want it to name the exit's code and test-3, whose request failed on the exit", reason)
			}
		})
	}
}
