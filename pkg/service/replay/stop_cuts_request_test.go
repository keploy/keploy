package replay

import (
	"context"
	"fmt"
	"io"
	"runtime"
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
	// sent, when set, logs each test sent, in order.
	sent *sendLog
	// answered: the named test is answered as the harness's hooks answer
	// it, and during is called after, not while its request is held.
	answered bool
}

// sendLog is the tests a run sent, in order, and how many it had sent when
// its user stopped it (stopAt, -1 before).
type sendLog struct {
	mu     sync.Mutex
	names  []string
	stopAt int
}

func (l *sendLog) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
}

func (l *sendLog) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopAt = len(l.names)
}

// afterStop is the tests sent after the stop.
func (l *sendLog) afterStop() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopAt < 0 {
		return nil
	}
	return append([]string(nil), l.names[l.stopAt:]...)
}

func (h cutShortHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	if h.sent != nil {
		h.sent.add(tc.Name)
	}
	if tc.Name != h.name {
		return h.prHooks.SimulateRequest(ctx, tc, set)
	}
	if h.answered {
		defer h.during()
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
//
// Whatever the switch, the set ends USER_ABORT and no test is sent after the
// stop. With the switch off the set ended FAILED or USER_ABORT by
// scheduling, and the next test could be sent after the stop: the app
// watcher records the stop on a goroutine of its own, and nothing ordered it
// before the next test. At GOMAXPROCS=1 the set ended FAILED in each of 140
// runs, with test-3 sent and filed PASSED after the stop. Each case runs at
// GOMAXPROCS=1 and at the default, and with the stop in the set's last test
// too, which leaves no next test for the loop to stop before.
func TestAStopThatCutsATestShortGivesItNoVerdict(t *testing.T) {
	const sigint = 2
	for _, c := range []struct {
		name         string
		on, kept     bool
		stream, read bool
		// streamNext streams test-3 too, so a streamed test follows the
		// cut one.
		streamNext bool
		// cut is the test whose request the stop cuts short: test-2 of
		// three where empty. With answered, the stop comes as its answer
		// returns, and cuts nothing short.
		cut      string
		answered bool
		want     int
	}{
		{name: "switch on, run per set", on: true, want: 130},
		{name: "switch on, kept alive", on: true, kept: true, want: 130},
		{name: "switch on, a streamed test's request", on: true, stream: true, want: 130},
		{name: "switch on, a streamed test's read", on: true, stream: true, read: true, want: 130},
		{name: "switch on, the set's last test", on: true, cut: "test-3", want: 130},
		{name: "switch off, run per set", want: 1},
		{name: "switch off, kept alive", kept: true, want: 1},
		{name: "switch off, a streamed test's request", stream: true, want: 1},
		{name: "switch on, a streamed test's request, another streamed after it", on: true, stream: true, streamNext: true, want: 130},
		{name: "switch off, a streamed test's request, another streamed after it", stream: true, streamNext: true, want: 1},
		{name: "switch off, the set's last test", cut: "test-3", want: 1},
		{name: "switch off, kept alive, the set's last test", kept: true, cut: "test-3", want: 1},
		// Every test answered, then the stop: the set has no verdict under
		// the switch, and with the switch off a stop is no failure.
		{name: "switch on, after the last test's answer", on: true, cut: "test-3", answered: true, want: 130},
		{name: "switch off, after the last test's answer", cut: "test-3", answered: true, want: 0},
	} {
		for _, procs := range []int{1, 0} {
			name := c.name + ", GOMAXPROCS default"
			if procs != 0 {
				name = fmt.Sprintf("%s, GOMAXPROCS=%d", c.name, procs)
			}
			t.Run(name, func(t *testing.T) {
				if procs != 0 {
					defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(procs))
				}
				setSwitch(t, c.on)
				cut := c.cut
				if cut == "" {
					cut = "test-2"
				}
				h := newPartialRunHarness(t, 3, 0)
				if c.stream {
					h.streaming(cut)
				}
				if c.streamNext {
					h.streaming("test-3")
				}
				h.replayer.config.Test.KeepAppAlive = c.kept
				core, logs := observer.New(zapcore.DebugLevel)
				h.replayer.logger = zap.New(core)
				sent := &sendLog{stopAt: -1}
				code, status := startStopped(t, h, sigint, func(stop func()) {
					h.replayer.hookImpl = cutShortHooks{name: cut, read: c.read, sent: sent, answered: c.answered,
						during: func() { sent.stop(); stop() }}
				})
				if status != models.TestSetStatusUserAbort {
					t.Errorf("the set ended %s, want %s", status, models.TestSetStatusUserAbort)
				}
				if code != c.want {
					t.Errorf("the run exits %d, want %d", code, c.want)
				}
				// With the switch off the cut test is filed FAILED; under it,
				// none is.
				if got := failedTests(h); (len(got) == 1 && got[0] == cut) != (!c.on && !c.answered) || len(got) > 1 {
					t.Errorf("the tests filed FAILED are %v; want %s filed FAILED: %v", got, cut, !c.on && !c.answered)
				}
				if after := sent.afterStop(); len(after) > 0 {
					t.Errorf("%v sent after the user's stop in %s", after, cut)
				}
				said := logs.FilterMessageSnippet("the run was stopped before keploy judged it").Len()
				if (said == 1) != c.on || said > 1 {
					t.Errorf("keploy said %d times that the run was stopped before it judged it; want it said: %v", said, c.on)
				}
			})
		}
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
