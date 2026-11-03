package replay

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// cutShortHooks answers as the harness's hooks, but holds the named test's
// request, or its stream's read (read), until the request's context ends,
// the way an application still working on it does, and then fails it with
// that end, as the HTTP client does. As it holds it, it calls during: the
// user's stop, or the application's exit.
type cutShortHooks struct {
	prHooks
	name   string
	read   bool
	during func()
}

func (h cutShortHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	if tc.Name != h.name {
		return h.prHooks.SimulateRequest(ctx, tc, set)
	}
	held := func() error {
		h.during()
		<-ctx.Done()
		return fmt.Errorf("Get %q: %w", tc.HTTPReq.URL, ctx.Err())
	}
	if h.read {
		return &pkg.StreamingHTTPResponse{StatusCode: 200, Header: tc.HTTPResp.Header,
			Reader: io.NopCloser(heldReader(held)), StreamConfig: pkg.DetectHTTPStreamConfig(tc, nil)}, nil
	}
	return nil, held()
}

// heldReader is a stream whose read waits for held and fails with its error.
type heldReader func() error

func (r heldReader) Read([]byte) (int, error) { return 0, r() }

// A test whose request the user's stop cut short has no verdict for the
// stop's sake (proven switch): the set ends USER_ABORT with that test not
// failed, and the run exits 128+N for the signal, not 1. A run with an agent
// filed the test FAILED, "failed to simulate request", and exited 1, in
// either mode, kept or per set; a streamed test likewise, as its request or
// its stream's read was cut. With the switch off the test still fails and
// the run exits 1, as before.
func TestAStopThatCutsATestShortGivesItNoVerdict(t *testing.T) {
	const sigint = 2
	for _, c := range []struct {
		name            string
		on, kept        bool
		stream, read    bool
		want            int
		wantStatus      []models.TestSetStatus
		wantTest2Failed bool
	}{
		{name: "switch on, run per set", on: true, want: 130, wantStatus: []models.TestSetStatus{models.TestSetStatusUserAbort}},
		{name: "switch on, kept alive", on: true, kept: true, want: 130, wantStatus: []models.TestSetStatus{models.TestSetStatusUserAbort}},
		{name: "switch on, a streamed test's request", on: true, stream: true, want: 130, wantStatus: []models.TestSetStatus{models.TestSetStatusUserAbort}},
		{name: "switch on, a streamed test's read", on: true, stream: true, read: true, want: 130, wantStatus: []models.TestSetStatus{models.TestSetStatusUserAbort}},
		// With the switch off the set's status is not pinned: USER_ABORT or
		// FAILED. The app watcher records the user's stop (USER_ABORT) on a
		// goroutine of its own, and with the switch off nothing orders that
		// before the set's status is resolved. Only under the switch does a
		// set run with an agent read the stop itself: as the cut request
		// returns (endedPartWay) and at the set's end (stoppedByUser). At
		// GOMAXPROCS=1 the set ended FAILED in each of 140 runs, with test-3
		// run after the stop; beside busy-loop load it did so now and then at
		// other GOMAXPROCS too. The exit code (1, as test-2 is filed FAILED)
		// holds in every order.
		{name: "switch off, run per set", want: 1, wantTest2Failed: true,
			wantStatus: []models.TestSetStatus{models.TestSetStatusUserAbort, models.TestSetStatusFailed}},
		{name: "switch off, kept alive", kept: true, want: 1, wantTest2Failed: true,
			wantStatus: []models.TestSetStatus{models.TestSetStatusUserAbort, models.TestSetStatusFailed}},
	} {
		t.Run(c.name, func(t *testing.T) {
			setSwitch(t, c.on)
			h := newPartialRunHarness(t, 3, 0)
			if c.stream {
				h.streaming("test-2")
			}
			h.replayer.config.Test.KeepAppAlive = c.kept
			core, logs := observer.New(zapcore.DebugLevel)
			h.replayer.logger = zap.New(core)
			code, status := startStopped(t, h, sigint, func(stop func()) {
				h.replayer.hookImpl = cutShortHooks{name: "test-2", read: c.read, during: stop}
			})
			if !containsStatus(c.wantStatus, status) {
				t.Errorf("the set ended %s, want one of %v", status, c.wantStatus)
			}
			if code != c.want {
				t.Errorf("the run exits %d, want %d", code, c.want)
			}
			if got := failedTests(h); (len(got) == 1 && got[0] == "test-2") != c.wantTest2Failed || len(got) > 1 {
				t.Errorf("the tests filed FAILED are %v; want test-2 filed FAILED: %v", got, c.wantTest2Failed)
			}
			said := logs.FilterMessageSnippet("the run was stopped before keploy judged it").Len()
			if (said == 1) != c.on || said > 1 {
				t.Errorf("keploy said %d times that the run was stopped before it judged it; want it said: %v", said, c.on)
			}
		})
	}
}

// The application's exit while a test's request waits on it is the
// application's, not its user's stop, under the proven switch too: the set
// ends APP_HALTED, the run exits 1. The watch of an application run per set
// cancels the set's context as it exits, and the exit of an application kept
// across the sets cancels the run's with that exit: neither is the run's
// context ended by its user (stoppedByItsUser).
func TestAnAppExitThatCutsARequestShortIsNoUserStop(t *testing.T) {
	for _, c := range []struct {
		name string
		kept bool
	}{{name: "run per set"}, {name: "kept alive", kept: true}} {
		t.Run(c.name, func(t *testing.T) {
			setSwitch(t, true)
			h := newPartialRunHarness(t, 3, 0)
			h.replayer.config.Command = "./app"
			h.replayer.config.CommandType = string(utils.Native)
			h.replayer.config.Test.MaxFlakyChecks = 1
			h.replayer.config.Test.KeepAppAlive = c.kept
			var once sync.Once
			h.replayer.hookImpl = cutShortHooks{name: "test-2", during: func() { once.Do(func() { close(h.instr.appStopped) }) }}
			saved := utils.ErrCode
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = saved })
			t.Cleanup(utils.SetInterruptSignalForTest(0))
			ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
			defer cancel()
			_ = h.replayer.Start(ctx)
			h.report.mu.Lock()
			rep := h.report.report
			h.report.mu.Unlock()
			if rep == nil {
				t.Fatal("the set wrote no report")
			}
			if rep.Status != string(models.TestSetStatusAppHalted) {
				t.Errorf("the set ended %s, want %s", rep.Status, models.TestSetStatusAppHalted)
			}
			if utils.ErrCode != 1 {
				t.Errorf("the run exits %d, want 1", utils.ErrCode)
			}
		})
	}
}

func containsStatus(in []models.TestSetStatus, s models.TestSetStatus) bool {
	for _, x := range in {
		if x == s {
			return true
		}
	}
	return false
}

// failedTests names the tests h's set filed FAILED, in their order.
func failedTests(h *prRun) []string {
	h.report.mu.Lock()
	defer h.report.mu.Unlock()
	var out []string
	for _, r := range h.report.results {
		if r.Status == models.TestStatusFailed {
			out = append(out, r.TestCaseID)
		}
	}
	return out
}
