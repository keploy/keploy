package replay

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// viewInstr is the partial-run harness's agent under exact serving, telling
// in order what the replayer asks of it: each view it installs ("stage",
// "pool", "test:<name>"; a call it fails is "<what>-failed"), each test's
// capture window opened ("begin", "continue": the agent's learn tracker
// begins the test there) and each one read ("end": GetMockErrors, where the
// tracker ends it). failPool fails the pool calls it answers true for, by
// their number (1 for the set's first). failOpen fails the capture windows
// it answers true for ("begin-failed", "continue-failed"), by their number
// (1 for the set's first test), with openErr when it is set. loaded is the
// mocks it says it holds (GetMockStats).
type viewInstr struct {
	*prInstr
	failPool func(n int) bool
	failOpen func(n int) bool
	openErr  error
	loaded   int

	mu     sync.Mutex
	pools  int
	opens  int
	events []string
}

func (v *viewInstr) note(e string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.events = append(v.events, e)
}

func (v *viewInstr) BeginTestErrorCapture(context.Context) error {
	return v.open("begin")
}

func (v *viewInstr) ContinueTestErrorCapture(context.Context) error {
	return v.open("continue")
}

func (v *viewInstr) open(what string) error {
	v.mu.Lock()
	v.opens++
	n := v.opens
	v.mu.Unlock()
	if v.failOpen != nil && v.failOpen(n) {
		v.note(what + "-failed")
		if v.openErr != nil {
			return v.openErr
		}
		return errors.New("the agent did not open the capture window")
	}
	v.note(what)
	return nil
}

func (v *viewInstr) GetMockStats(context.Context) (models.MockStats, error) {
	return models.MockStats{Loaded: v.loaded}, nil
}

func (v *viewInstr) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	v.note("end")
	return v.prInstr.GetMockErrors(ctx)
}

func (v *viewInstr) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	what := "other"
	switch ex := params.Exact; {
	case ex == nil:
	case ex.Stage:
		what = "stage"
	case ex.Test != "":
		what = "test:" + ex.Test
	default:
		what = "pool"
		v.mu.Lock()
		v.pools++
		n := v.pools
		v.mu.Unlock()
		if v.failPool != nil && v.failPool(n) {
			v.note("pool-failed")
			return errors.New("the agent did not take the pool call")
		}
	}
	v.note(what)
	return v.prInstr.UpdateMockParams(ctx, params)
}

func (v *viewInstr) trace() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.events...)
}

// overATestView is the first test the trace begins (its capture window
// opened, the agent's tracker beginning it) while the agent may still serve
// a test's own view: one asked for since the latest pool call it took. ""
// when every test begins over the pool.
func overATestView(events []string) string {
	view := ""
	for i, e := range events {
		switch {
		case strings.HasPrefix(e, "test:"):
			view = strings.TrimPrefix(e, "test:")
		case e == "pool" || e == "stage":
			view = ""
		case e == "begin" || e == "continue":
			if view != "" {
				return "event " + strconv.Itoa(i) + " (" + e + ") over " + view + "'s view"
			}
		}
	}
	return ""
}

// unendedTest is the first test whose capture window is read ("end", the
// agent's tracker ending it) with no pool call made since its view was
// asked for: "" when every test asks for the pool first, as the main loop
// does once its test has its answer.
func unendedTest(events []string) string {
	open := ""
	for _, e := range events {
		switch {
		case strings.HasPrefix(e, "test:"):
			open = strings.TrimPrefix(e, "test:")
		case e == "pool" || e == "pool-failed":
			open = ""
		case e == "end":
			if open != "" {
				return open
			}
		}
	}
	return ""
}

// exactViewRun is a three-test set under the switch, test-1 owning mock-a,
// its agent viewInstr.
func exactViewRun(t *testing.T, failPool func(int) bool, streamed ...string) (*prRun, *viewInstr) {
	t.Helper()
	setSwitch(t, true)
	h := newPartialRunHarness(t, 3, 0)
	pinWindows(h)
	h.streaming(streamed...)
	h.mocks.filtered = []*models.Mock{{Name: "mock-a", Kind: models.HTTP}, {Name: "mock-boot", Kind: models.HTTP}}
	mappingFile(t, h, map[string][]models.MockEntry{"test-1": mongoEntries("mock-a")})
	vi := &viewInstr{prInstr: h.instr, failPool: failPool}
	h.replayer.instrumentation = vi
	return h, vi
}

// startDiffers answers test case name with a stream whose start (its status)
// differs from its recording's, and then nothing of its body: the streaming
// pass judges the start alone, a judged failure.
type startDiffers struct {
	prHooks
	name string
}

func (h startDiffers) SimulateRequest(ctx context.Context, tc *models.TestCase, id string) (interface{}, error) {
	if tc.Name != h.name {
		return h.prHooks.SimulateRequest(ctx, tc, id)
	}
	return &pkg.StreamingHTTPResponse{StatusCode: 503, Header: tc.HTTPResp.Header,
		Reader: io.NopCloser(iotest.ErrReader(&pkg.AnswerStallError{Target: "app", Answered: true})), StreamConfig: pkg.DetectHTTPStreamConfig(tc, nil)}, nil
}

// A streamed test case that fails ends with the pool served, on every exit
// the streaming pass has, before its capture window is read: the agent's
// learn tracker ends it and begins the next test case only over the pool.
// Before, these exits served no pool: the next test case's tracker began
// over the failed one's view, and the store answered the next test's calls
// from that view while the tracker filed what the failed one served as
// served before the next. A value one of its recordings held was learned as
// noise onto a recording of the failed test (integrations' Postgres and
// Mongo learn steps).
func TestAFailedStreamedTestEndsWithThePoolServed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hooks func() TestHooks
	}{
		{name: "the request fails", hooks: func() TestHooks {
			return prHooks{simErr: map[string]error{"test-1": errors.New("connection refused")}}
		}},
		{name: "the stream fails to read", hooks: func() TestHooks {
			return prHooks{streamErr: map[string]error{"test-1": errors.New("stream cut")}}
		}},
		{name: "the stream's start is judged a failure", hooks: func() TestHooks {
			return startDiffers{name: "test-1"}
		}},
		{name: "the answer is of another type", hooks: func() TestHooks {
			return prHooks{wrongType: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, vi := exactViewRun(t, nil, "test-1", "test-2", "test-3")
			h.replayer.hookImpl = tc.hooks()
			if status := h.run(t); status != models.TestSetStatusFailed {
				t.Fatalf("precondition: the set reported %q; want FAILED (test-1 fails)", status)
			}
			events := vi.trace()
			if !hasEvent(events, "test:test-2") {
				t.Fatalf("precondition: test-2 never opened: %v", events)
			}
			if bad := unendedTest(events); bad != "" {
				t.Errorf("%s's capture window was read with its own view still served: %v", bad, events)
			}
			if bad := overATestView(events); bad != "" {
				t.Errorf("a test began over a test's view, %s: %v", bad, events)
			}
			if got := poolCallsBetweenTests(events); len(got) != 2 || got[0] != 1 || got[1] != 1 {
				t.Errorf("pool calls between the test cases %v; want [1 1], one on the failed exit: %v", got, events)
			}
		})
	}
}

// A pool call the agent does not take, as a test case ends, is made again
// before the next one begins: the next test case's tracker begins only over
// the pool, on the main loop and on the streaming pass. Before, the next test
// began over the last one's view.
func TestAPoolCallTheAgentDidNotTakeIsMadeAgainBeforeTheNextTestBegins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		streamed []string
	}{
		{name: "the main loop"},
		{name: "the streaming pass", streamed: []string{"test-1", "test-2", "test-3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// test-1's end is the set's first pool call.
			h, vi := exactViewRun(t, func(n int) bool { return n == 1 }, tc.streamed...)
			h.run(t)
			events := vi.trace()
			if !hasEvent(events, "pool-failed") || !hasEvent(events, "test:test-3") {
				t.Fatalf("precondition: the pool call did not fail once, or the set did not reach test-3: %v", events)
			}
			if bad := overATestView(events); bad != "" {
				t.Errorf("a test began over a test's view, %s: %v", bad, events)
			}
		})
	}
}

// When the agent takes no pool call at all after a test case, the set stops
// failing closed, before the next test case begins: not judged, saying why,
// never a pass, and no later test case is asked of the agent.
func TestASetWhosePoolTheAgentNeverTakesStopsBeforeTheNextTestBegins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		streamed []string
	}{
		{name: "the main loop"},
		{name: "the streaming pass", streamed: []string{"test-1", "test-2", "test-3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, vi := exactViewRun(t, func(int) bool { return true }, tc.streamed...)
			logs := observeLog(h)
			status := h.run(t)
			events := vi.trace()
			if status != models.TestSetStatusInternalErr {
				t.Errorf("the set reported %q; want %q: %v", status, models.TestSetStatusInternalErr, events)
			}
			if hasEvent(events, "test:test-2") {
				t.Errorf("test-2 was asked of the agent with test-1's view never ended: %v", events)
			}
			if bad := overATestView(events); bad != "" {
				t.Errorf("a test began over a test's view, %s: %v", bad, events)
			}
			if h.report.report == nil || !strings.Contains(h.report.report.FailureReason, "not judged: keploy could not serve the test set's pool after test case test-1") {
				t.Errorf("the report does not say the set is not judged because test-1's pool was not served: %+v", h.report.report)
			}
			stoppedSaid(t, logs, "keploy could not serve the test set's pool after test case test-1", events)
			notBegunSaid(t, logs, "keploy could not serve the test set's pool after test case test-1", events)
			if !h.replayer.notJudged("test-set-0") {
				t.Errorf("the set is not marked not judged, so the run would stop after it: %v", events)
			}
		})
	}
}

func hasEvent(events []string, e string) bool {
	for _, x := range events {
		if x == e {
			return true
		}
	}
	return false
}

// poolCallsBetweenTests is, for each test asked of the agent after the first,
// the pool calls made since the test before it was asked for, taken or not.
func poolCallsBetweenTests(events []string) []int {
	var out []int
	n, seen := 0, false
	for _, e := range events {
		switch {
		case strings.HasPrefix(e, "test:"):
			if seen {
				out = append(out, n)
			}
			n, seen = 0, true
		case e == "pool" || e == "pool-failed":
			n++
		}
	}
	return out
}

// A pool call the agent took as a test case ended is not made again before
// the next one begins: one pool call between two test cases, on the main loop
// and on the streaming pass. Serving the pool again would install a new pool
// view between them, and what the first answered of the next test's calls in
// flight (its ahead) would be lost from what the next test opens with.
func TestAPoolTheAgentTookIsNotServedAgainBeforeTheNextTestBegins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		streamed []string
	}{
		{name: "the main loop"},
		{name: "the streaming pass", streamed: []string{"test-1", "test-2", "test-3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, vi := exactViewRun(t, nil, tc.streamed...)
			h.run(t)
			events := vi.trace()
			got := poolCallsBetweenTests(events)
			if len(got) != 2 || got[0] != 1 || got[1] != 1 {
				t.Fatalf("pool calls between the test cases %v; want [1 1]: %v", got, events)
			}
		})
	}
}

// beginOnly is v as an agent that predates Continue: every test case opens
// its capture window with Begin.
type beginOnly struct {
	*prInstr
	v *viewInstr
}

func (b beginOnly) BeginTestErrorCapture(ctx context.Context) error {
	return b.v.BeginTestErrorCapture(ctx)
}

func (b beginOnly) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	return b.v.GetMockErrors(ctx)
}

func (b beginOnly) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	return b.v.UpdateMockParams(ctx, params)
}

// observeLog gives h's replayer a logger whose every entry logs holds.
func observeLog(h *prRun) *observer.ObservedLogs {
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core)
	return logs
}

// notBegunMsg starts the line that says a set is not judged because keploy
// did not begin one of its test cases on its agent.
const notBegunMsg = "test-set is not judged: keploy did not begin one of its test cases on its agent"

// notBegunLogs is the entries of logs that say a set is not judged because a
// test case was not begun.
func notBegunLogs(logs *observer.ObservedLogs) []observer.LoggedEntry {
	return logs.FilterMessageSnippet(notBegunMsg).All()
}

// stoppedMsg starts the line that says, where it happens, that keploy
// stopped a set because it did not begin a test case on its agent.
const stoppedMsg = "keploy did not begin a test case on its agent, so it stopped the test-set before that test case"

// stoppedSaid fails t unless the log says once, at the warn level, that
// keploy stopped test-set-0 before a test case it did not begin, with a
// reason that holds why.
func stoppedSaid(t *testing.T, logs *observer.ObservedLogs, why string, events []string) {
	t.Helper()
	said := logs.FilterMessageSnippet(stoppedMsg).All()
	if len(said) != 1 {
		t.Errorf("the log said keploy stopped the set before a test case it did not begin %d times; want once: %v", len(said), events)
		return
	}
	e, f := said[0], said[0].ContextMap()
	if e.Level != zapcore.WarnLevel {
		t.Errorf("the stop line is at %s; want warn", e.Level)
	}
	if f["test-set"] != "test-set-0" {
		t.Errorf("the stop line names test-set %v; want test-set-0", f["test-set"])
	}
	if r, _ := f["reason"].(string); !strings.Contains(r, why) {
		t.Errorf("the stop line's reason %q does not say %q", r, why)
	}
}

// notBegunSaid fails t unless the log says once, at the error level, that
// test-set-0 is not judged because a test case was not begun, with a reason
// that holds why and a next step.
func notBegunSaid(t *testing.T, logs *observer.ObservedLogs, why string, events []string) {
	t.Helper()
	said := notBegunLogs(logs)
	if len(said) != 1 {
		t.Errorf("the log said the set is not judged for a test case not begun %d times; want once: %v", len(said), events)
		return
	}
	e, f := said[0], said[0].ContextMap()
	if e.Level != zapcore.ErrorLevel {
		t.Errorf("the not-begun line is at %s; want error", e.Level)
	}
	if f["test-set"] != "test-set-0" {
		t.Errorf("the not-begun line names test-set %v; want test-set-0", f["test-set"])
	}
	if r, _ := f["reason"].(string); !strings.Contains(r, why) {
		t.Errorf("the not-begun line's reason %q does not say %q", r, why)
	}
	if n, _ := f["next_step"].(string); !strings.Contains(n, "check the agent's log") {
		t.Errorf("the not-begun line's next_step %q does not say to check the agent's log", n)
	}
}

// notBegunReported fails t unless the set's report says it is not judged
// because test was not begun, the run marks the set not judged (so the run
// goes on to its next set), and the log says so where the set stopped
// (stoppedSaid) and at its end (notBegunSaid).
func notBegunReported(t *testing.T, h *prRun, logs *observer.ObservedLogs, test string, events []string) {
	t.Helper()
	stoppedSaid(t, logs, "keploy's agent did not begin test case "+test, events)
	notBegunSaid(t, logs, "keploy's agent did not begin test case "+test, events)
	if h.report.report == nil {
		t.Fatalf("no report was persisted: %v", events)
	}
	if want := "not judged: keploy's agent did not begin test case " + test; !strings.Contains(h.report.report.FailureReason, want) {
		t.Errorf("the report's reason %q does not say %q: %v", h.report.report.FailureReason, want, events)
	}
	if !h.replayer.notJudged("test-set-0") {
		t.Errorf("the set is not marked not judged, so the run would stop after it: %v", events)
	}
}

// Under exact serving, a test case whose capture window the agent does not
// open (where the agent's learn tracker begins it) stops the set before its
// view is asked for, on the main loop and on the streaming pass, for a set's
// first test (begin) and a later one (continue, or begin on an agent that
// predates continue). The set is not judged, its report says why, and the
// run goes on to the next set. Before, the failure was logged at debug and
// the test case's view was served anyway: the tracker stayed where it was
// and filed the serves of the test case's view as the test before's.
func TestATestCaseTheAgentCannotBeginStopsTheSetBeforeItsViewIsServed(t *testing.T) {
	for _, loop := range []struct {
		name     string
		streamed []string
	}{
		{name: "the main loop"},
		{name: "the streaming pass", streamed: []string{"test-1", "test-2", "test-3"}},
	} {
		for _, tc := range []struct {
			name, failed, test string
			n                  int
			beginOnly          bool
		}{
			{name: "the set's first test", failed: "begin-failed", test: "test-1", n: 1},
			{name: "a later test", failed: "continue-failed", test: "test-2", n: 2},
			{name: "a later test on an agent that predates continue", failed: "begin-failed", test: "test-2", n: 2, beginOnly: true},
		} {
			t.Run(loop.name+"/"+tc.name, func(t *testing.T) {
				h, vi := exactViewRun(t, nil, loop.streamed...)
				logs := observeLog(h)
				vi.failOpen = func(n int) bool { return n == tc.n }
				if tc.beginOnly {
					h.replayer.instrumentation = beginOnly{prInstr: h.instr, v: vi}
				}
				status := h.run(t)
				events := vi.trace()
				if !hasEvent(events, tc.failed) {
					t.Fatalf("precondition: %s's capture window did not fail to open (%s): %v", tc.test, tc.failed, events)
				}
				if status != models.TestSetStatusInternalErr {
					t.Errorf("the set reported %q; want %q: %v", status, models.TestSetStatusInternalErr, events)
				}
				for _, e := range events {
					if e == "test:"+tc.test || e == "test:test-3" {
						t.Errorf("%s was asked of the agent after %s's capture window failed to open: %v", e, tc.test, events)
					}
				}
				notBegunReported(t, h, logs, tc.test, events)
			})
		}
	}
}

// A test case the main loop did not begin stops the set's streaming pass
// too: no streamed test case is begun or served after it.
func TestATestCaseTheMainLoopDidNotBeginStopsTheStreamingPass(t *testing.T) {
	h, vi := exactViewRun(t, nil, "test-3")
	vi.failOpen = func(n int) bool { return n == 1 }
	status := h.run(t)
	events := vi.trace()
	if !hasEvent(events, "begin-failed") {
		t.Fatalf("precondition: test-1's capture window did not fail to open: %v", events)
	}
	if status != models.TestSetStatusInternalErr {
		t.Errorf("the set reported %q; want %q: %v", status, models.TestSetStatusInternalErr, events)
	}
	for _, e := range events {
		if strings.HasPrefix(e, "test:") || e == "continue" {
			t.Errorf("%s came after test-1 was not begun: %v", e, events)
		}
	}
}

// A capture call that fails because the application's docker compose stack
// went down under it (keploy's agent among it) is the app's halt: the set is
// APP_HALTED, as any other call the stack's halt fails, never a set keploy
// did not judge. The log says where keploy stopped the set and why, and
// nothing of a set not judged.
func TestACaptureCallTheComposeStacksHaltFailsIsTheAppsHalt(t *testing.T) {
	for _, loop := range []struct {
		name     string
		streamed []string
	}{
		{name: "the main loop"},
		{name: "the streaming pass", streamed: []string{"test-1", "test-2", "test-3"}},
	} {
		t.Run(loop.name, func(t *testing.T) {
			h, vi := exactViewRun(t, nil, loop.streamed...)
			h.replayer.config.CommandType = string(utils.DockerCompose)
			// The stack's agent holds the set's mocks, as compose's bring-up
			// left them: the set's tests fire.
			vi.loaded = len(h.mocks.filtered)
			logs := observeLog(h)
			vi.failOpen = func(n int) bool { return n == 2 }
			vi.openErr = errors.New("dial tcp 172.18.0.3:8086: connect: connection refused")
			// A compose stack is kept across sets: the set is served
			// (runServing), as keploy runs a compose app's sets.
			status := h.runServing(t)
			events := vi.trace()
			if !hasEvent(events, "continue-failed") {
				t.Fatalf("precondition: test-2's capture window did not fail to open: %v", events)
			}
			if status != models.TestSetStatusAppHalted {
				t.Errorf("the set reported %q; want %q: %v", status, models.TestSetStatusAppHalted, events)
			}
			if hasEvent(events, "test:test-2") || hasEvent(events, "test:test-3") {
				t.Errorf("a test case was asked of the agent after test-2's capture window failed to open: %v", events)
			}
			if h.replayer.notJudged("test-set-0") {
				t.Errorf("the set is marked not judged; want the app's halt: %v", events)
			}
			stoppedSaid(t, logs, "connection refused", events)
			if said := notBegunLogs(logs); len(said) != 0 {
				t.Errorf("the log said the set is not judged for a test case not begun: %v", said[0].ContextMap())
			}
		})
	}
}

// A capture call cut short by the run's user stopping it is the user's stop,
// not keploy's failure: the set is USER_ABORT, never INTERNAL_ERR.
func TestACaptureCallTheUsersStopCutsShortIsTheStop(t *testing.T) {
	for _, loop := range []struct {
		name     string
		streamed []string
	}{
		{name: "the main loop"},
		{name: "the streaming pass", streamed: []string{"test-1", "test-2", "test-3"}},
	} {
		t.Run(loop.name, func(t *testing.T) {
			h, vi := exactViewRun(t, nil, loop.streamed...)
			logs := observeLog(h)
			vi.failOpen = func(n int) bool {
				if n != 2 {
					return false
				}
				h.cancel()
				return true
			}
			status := h.run(t)
			events := vi.trace()
			if !hasEvent(events, "continue-failed") {
				t.Fatalf("precondition: test-2's capture window did not fail to open: %v", events)
			}
			if status != models.TestSetStatusUserAbort {
				t.Errorf("the set reported %q; want %q: %v", status, models.TestSetStatusUserAbort, events)
			}
			if hasEvent(events, "test:test-2") {
				t.Errorf("test-2 was asked of the agent after its capture window failed to open: %v", events)
			}
			if n := len(logs.FilterMessageSnippet(stoppedMsg).All()) + len(notBegunLogs(logs)); n != 0 {
				t.Errorf("the log said %d times that keploy did not begin a test case; want the user's stop alone: %v", n, events)
			}
		})
	}
}

// Without the switch the capture window stays best-effort: one the agent
// does not open degrades to the old behaviour, every test case still opens
// its own (begin, then continue), and the set runs on.
func TestACaptureWindowTheAgentDoesNotOpenIsBestEffortWithoutTheSwitch(t *testing.T) {
	setSwitch(t, false)
	h := newPartialRunHarness(t, 3, 0)
	vi := &viewInstr{prInstr: h.instr, failOpen: func(int) bool { return true }}
	h.replayer.instrumentation = vi
	status := h.run(t)
	events := vi.trace()
	if status != models.TestSetStatusPassed {
		t.Errorf("the set reported %q; want %q: %v", status, models.TestSetStatusPassed, events)
	}
	if got := len(h.report.results); got != 3 {
		t.Errorf("recorded %d test results; want 3: %v", got, events)
	}
	var opens []string
	for _, e := range events {
		if strings.HasSuffix(e, "-failed") {
			opens = append(opens, e)
		}
	}
	if want := []string{"begin-failed", "continue-failed", "continue-failed"}; strings.Join(opens, " ") != strings.Join(want, " ") {
		t.Errorf("the capture calls were %v; want %v: %v", opens, want, events)
	}
}
