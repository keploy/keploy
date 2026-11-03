package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
	// ended, when set, is called once the held request's context has ended,
	// before the request fails with that end.
	ended func()
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
		if h.ended != nil {
			h.ended()
		}
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
// ends APP_HALTED, the run exits 1, and no test is sent after the exit. The
// watch of an application run per set cancels the set's context as it
// exits, and the exit of an application kept across the sets cancels the
// run's with that exit: neither is the run's context ended by its user
// (stoppedByItsUser). Kept alive, at GOMAXPROCS=1, the next test was sent to
// the application that had exited and the set ended FAILED: the set's watch
// records the exit on a goroutine of its own, which nothing ordered before
// the next test. Each case runs at GOMAXPROCS=1 and at the default, and with
// the exit in the set's last test too.
func TestAnAppExitThatCutsARequestShortIsNoUserStop(t *testing.T) {
	for _, c := range []struct {
		name string
		kept bool
		cut  string
	}{
		{name: "run per set", cut: "test-2"},
		{name: "kept alive", kept: true, cut: "test-2"},
		{name: "run per set, the set's last test", cut: "test-3"},
		{name: "kept alive, the set's last test", kept: true, cut: "test-3"},
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
				setSwitch(t, true)
				h := newPartialRunHarness(t, 3, 0)
				h.replayer.config.Command = "./app"
				h.replayer.config.CommandType = string(utils.Native)
				h.replayer.config.Test.MaxFlakyChecks = 1
				h.replayer.config.Test.KeepAppAlive = c.kept
				sent := &sendLog{stopAt: -1}
				var once sync.Once
				h.replayer.hookImpl = cutShortHooks{name: c.cut, sent: sent, during: func() {
					once.Do(func() { sent.stop(); close(h.instr.appStopped) })
				}}
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
				if after := sent.afterStop(); len(after) > 0 {
					t.Errorf("%v sent after the application exited in %s", after, c.cut)
				}
			})
		}
	}
}

// An application's exit that came before its user's stop decides the set:
// APP_HALTED (APP_FAULT for a command error, INTERNAL_ERR for keploy's error
// running it), and the test it cut short is filed as it is with no stop, in
// both modes, run per set and kept alive, wherever keploy sees the stop.
// The stop comes once the exit has ended the held request. Before, run per
// set, the set ended USER_ABORT where a test loop saw the stop before its
// next test, and under the switch as the cut request failed (no verdict for
// it, exit 130), but APP_HALTED where the switch was off and the cut test was
// the set's last: where keploy saw the stop decided the set.
func TestAnAppExitBeforeItsUserStopKeepsTheSetHalted(t *testing.T) {
	const sigint = 2
	for _, c := range []struct {
		name     string
		on, kept bool
		// cut is the test the application's exit cuts short.
		cut string
		// exit is how the application's run ends, and want the set's status
		// for it: an exit (ErrAppStopped, APP_HALTED) where empty.
		exit models.AppErrorType
		want models.TestSetStatus
	}{
		{name: "switch off, run per set, a command error", cut: "test-2", exit: models.ErrCommandError, want: models.TestSetStatusFaultUserApp},
		{name: "switch on, run per set, a command error", on: true, cut: "test-2", exit: models.ErrCommandError, want: models.TestSetStatusFaultUserApp},
		{name: "switch off, run per set, keploy's error", cut: "test-2", exit: models.ErrInternal, want: models.TestSetStatusInternalErr},
		{name: "switch on, run per set, keploy's error", on: true, cut: "test-2", exit: models.ErrInternal, want: models.TestSetStatusInternalErr},
		{name: "switch off, run per set", cut: "test-2"},
		{name: "switch on, run per set", on: true, cut: "test-2"},
		{name: "switch off, run per set, the set's last test", cut: "test-3"},
		{name: "switch on, run per set, the set's last test", on: true, cut: "test-3"},
		{name: "switch off, kept alive", kept: true, cut: "test-2"},
		{name: "switch on, kept alive", on: true, kept: true, cut: "test-2"},
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
				h := newPartialRunHarness(t, 3, 0)
				h.replayer.config.Test.KeepAppAlive = c.kept
				h.instr.stopErr = c.exit
				want := c.want
				if want == "" {
					want = models.TestSetStatusAppHalted
				}
				sent := &sendLog{stopAt: -1}
				h.replayer.config.Command = "./app"
				h.replayer.config.CommandType = string(utils.Native)
				h.replayer.config.Test.MaxFlakyChecks = 1
				saved := utils.ErrCode
				t.Cleanup(func() { utils.ErrCode = saved })
				utils.ErrCode = 0
				t.Cleanup(utils.SetInterruptSignalForTest(0))
				ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
				defer cancel()
				ctx, stop := context.WithCancel(ctx)
				defer stop()
				var exits, stops sync.Once
				h.replayer.hookImpl = cutShortHooks{name: c.cut, sent: sent,
					during: func() { exits.Do(func() { close(h.instr.appStopped) }) },
					ended: func() {
						stops.Do(func() {
							sent.stop()
							utils.SetInterruptSignalForTest(sigint)
							stop()
						})
					}}
				// Start returns the kept application's exit, and the stop's
				// cancellation run per set.
				_ = h.replayer.Start(ctx)
				h.report.mu.Lock()
				rep := h.report.report
				h.report.mu.Unlock()
				if rep == nil {
					t.Fatal("the set wrote no report")
				}
				code, status := utils.ErrCode, models.TestSetStatus(rep.Status)
				if status != want {
					t.Errorf("the set ended %s, want %s", status, want)
				}
				if code != 1 {
					t.Errorf("the run exits %d, want 1", code)
				}
				if got := failedTests(h); len(got) != 1 || got[0] != c.cut {
					t.Errorf("the tests filed FAILED are %v, want [%s]", got, c.cut)
				}
				if after := sent.afterStop(); len(after) > 0 {
					t.Errorf("%v sent after the user's stop in %s", after, c.cut)
				}
			})
		}
	}
}

// keploy's error writing a set's results, before its streamed tests, is an
// end of the set's own: the set ends INTERNAL_ERR and names that error when
// its application's exit, the user's stop or the exit of the application kept
// across the sets comes after it, before the first streamed test, and no
// streamed test is sent. Each later end comes in the failed write, which
// returns once the set's context has ended and the set's watch has had a
// moment to record it (the moment matters only to what the case shows
// before the fix: the fixed set's end does not depend on it). Before, the
// watch's status replaced the set's at the streamed loop's head whenever the
// watch had sent on exitLoopChan by then: run per set, the application's exit
// ended the set APP_HALTED in every run (that watch sends before it ends the
// set's context), and the stop and the kept application's exit ended it
// USER_ABORT or APP_HALTED by scheduling.
func TestAnErrorWritingResultsBeforeTheStreamedTestsKeepsTheSetInternalErr(t *testing.T) {
	const sigint = 2
	const unwritable = "failed to insert final test case result: report store is unwritable"
	for _, c := range []struct {
		name     string
		on, kept bool
		// stop: the later end is the user's stop; else the application's
		// exit.
		stop bool
	}{
		{name: "switch off, run per set, the application's exit"},
		{name: "switch on, run per set, the application's exit", on: true},
		{name: "switch off, run per set, the user's stop", stop: true},
		{name: "switch on, run per set, the user's stop", on: true, stop: true},
		{name: "switch off, kept alive, the application's exit", kept: true},
		{name: "switch on, kept alive, the application's exit", on: true, kept: true},
		{name: "switch off, kept alive, the user's stop", kept: true, stop: true},
		{name: "switch on, kept alive, the user's stop", on: true, kept: true, stop: true},
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
				h := newPartialRunHarness(t, 3, 0)
				h.streaming("test-3")
				h.replayer.config.Test.KeepAppAlive = c.kept
				h.replayer.config.Command = "./app"
				h.replayer.config.CommandType = string(utils.Native)
				h.replayer.config.Test.MaxFlakyChecks = 1
				saved := utils.ErrCode
				t.Cleanup(func() { utils.ErrCode = saved })
				utils.ErrCode = 0
				t.Cleanup(utils.SetInterruptSignalForTest(0))
				ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
				defer cancel()
				ctx, stop := context.WithCancel(ctx)
				defer stop()
				sent := &sendLog{stopAt: -1}
				h.replayer.hookImpl = cutShortHooks{sent: sent}
				var ends sync.Once
				h.report.failInsertOf = map[string]error{"test-1": errors.New("report store is unwritable")}
				h.report.onFailInsert = func(setCtx context.Context) {
					ends.Do(func() {
						sent.stop()
						if c.stop {
							utils.SetInterruptSignalForTest(sigint)
							stop()
						} else {
							close(h.instr.appStopped)
						}
						<-setCtx.Done()
						time.Sleep(20 * time.Millisecond)
					})
				}
				_ = h.replayer.Start(ctx)
				h.report.mu.Lock()
				rep := h.report.report
				h.report.mu.Unlock()
				if rep == nil {
					t.Fatal("the set wrote no report")
				}
				if status := models.TestSetStatus(rep.Status); status != models.TestSetStatusInternalErr {
					t.Errorf("the set ended %s, want %s", status, models.TestSetStatusInternalErr)
				}
				if got := h.replayer.setInternalErr; got == nil || !strings.Contains(got.Error(), unwritable) {
					t.Errorf("the set's end hands on %v, want an error saying %q", got, unwritable)
				}
				if utils.ErrCode != 1 {
					t.Errorf("the run exits %d, want 1", utils.ErrCode)
				}
				if after := sent.afterStop(); len(after) > 0 {
					t.Errorf("%v sent after the set's results could not be written", after)
				}
			})
		}
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
