package replay

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/localfile"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

// withSetDir makes the harness's set's folder, as a recorded set has one:
// keploy test writes its misses file only into it.
func withSetDir(t *testing.T, h *prRun) string {
	t.Helper()
	dir := filepath.Join(h.replayer.config.Path, "test-set-0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// readTestMisses is keploy test's misses file of the harness's set, as the
// run left it.
func readTestMisses(t *testing.T, h *prRun) localfile.MissesReport {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.replayer.config.Path, "test-set-0", localfile.TestMissesFile))
	if err != nil {
		t.Fatalf("keploy test's misses file was not written: %v", err)
	}
	var got localfile.MissesReport
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.By != "keploy test" || got.Set != "test-set-0" {
		t.Fatalf("the misses file holds %+v, want keploy test's of test-set-0", got)
	}
	return got
}

// calledWhole is each call of a misses file, whole.
func calledWhole(f localfile.MissesReport) string {
	out := ""
	for _, c := range f.Misses {
		out += c.Protocol + " " + c.Call + " " + c.Destination + ";"
	}
	return out
}

// calledShaped is each outside-test miss of a report, by its shape, with when
// it was made.
func calledShaped(misses []models.OutsideTestMiss) string {
	out := ""
	for _, m := range misses {
		out += m.Protocol + " " + m.Call + " @" + m.When + ";"
	}
	return out
}

// readsAsAnAgentDoes is the harness's stand-in agent, read as keploy's agent
// client reads it: a read whose context is done reads nothing and returns the
// context's error, as the client's request to the agent does, so a read made
// with no time left (a set's end read given no budget) fails its row. atEnd,
// when set, answers the set's end read (models.SetEndRead) in place of the
// harness's answers, however many tests were read before it.
type readsAsAnAgentDoes struct {
	*prInstr
	atEnd func() ([]models.UnmatchedCall, error)
	ends  atomic.Int32
}

func (f *readsAsAnAgentDoes) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if models.SetEndRead(ctx) {
		f.ends.Add(1)
		if f.atEnd != nil {
			return f.atEnd()
		}
	}
	return f.prInstr.GetMockErrors(ctx)
}

// readAsAnAgentIs has the harness's replayer read its stand-in agent as
// keploy's agent client reads it (readsAsAnAgentDoes).
func readAsAnAgentIs(h *prRun) *readsAsAnAgentDoes {
	a := &readsAsAnAgentDoes{prInstr: h.instr}
	h.replayer.instrumentation = a
	return a
}

// observeWarnings has the harness's replayer log to an observer, at WARN.
func observeWarnings(h *prRun) *observer.ObservedLogs {
	core, logs := observer.New(zapcore.WarnLevel)
	h.replayer.logger = zap.New(core)
	return logs
}

const afterLastReadFailed = "could not read every call the application made after the test-set's last test"

// A set whose application looked a name up and polled in the background
// after its last test, neither answered by a mock, passes in either switch
// mode (neither counts against a test), and names both, as made after its
// last test: by their shape in its report, and whole in keploy test's misses
// file. With the switch off its end is read for that alone: a plain call
// made then fails nothing either, and is named the same way (under the
// switch it fails the set: TestRunTestSet_ACallAfterTheLastTestThatNoMock
// AnsweredFailsTheSet).
func TestRunTestSet_CallsAfterTheLastTestAreNamedInEitherMode(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			provenOn(t, proven)
			h := newPartialRunHarness(t, 2, 0)
			withSetDir(t, h)
			logs := observeWarnings(h)
			setEnd := []models.UnmatchedCall{
				{Protocol: "DNS", ActualSummary: "A metrics.staging.internal.", HostTag: "#cd34"},
				{Protocol: "HTTP", ActualSummary: "GET /v1/heartbeat", Destination: "10.20.30.41:9090", Background: true},
			}
			wantShaped := "DNS A {}.{}.{}. @after_last_test;HTTP GET /{}/{} @after_last_test;"
			wantWhole := "DNS A metrics.staging.internal. ;HTTP GET /v1/heartbeat 10.20.30.41:9090;"
			if !proven {
				setEnd = append(setEnd, models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /v1/flush", Destination: "10.20.30.42:9099"})
				wantShaped += "HTTP POST /{}/{} @after_last_test;"
				wantWhole += "HTTP POST /v1/flush 10.20.30.42:9099;"
			}
			var reads atomic.Int32
			h.instr.mockErrorsFor = counted(&reads, misses(nil, nil, func() ([]models.UnmatchedCall, error) { return setEnd, nil }))
			agent := readAsAnAgentIs(h)

			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %s (reason %q); a call after the last test that counts against no test fails nothing", status, h.report.report.FailureReason)
			}
			if n, ends := reads.Load(), agent.ends.Load(); n != 3 || ends != 1 {
				t.Fatalf("%d reads of the agent's misses, %d of them the set's end; want one per test and the set's end", n, ends)
			}
			rep := h.report.report
			if len(rep.UnclaimedCalls) != 0 {
				t.Fatalf("unclaimed calls %+v; none fails the set", rep.UnclaimedCalls)
			}
			if got := calledShaped(rep.OutsideTestMisses); got != wantShaped {
				t.Fatalf("the report names %s outside every test, want %s", got, wantShaped)
			}
			for _, m := range rep.OutsideTestMisses {
				if strings.Contains(m.Call+m.Destination, "metrics") || strings.Contains(m.Call, "heartbeat") || strings.Contains(m.Call, "flush") {
					t.Errorf("the report names %+v whole, want its shape", m)
				}
			}
			if got := calledWhole(readTestMisses(t, h)); got != wantWhole {
				t.Fatalf("the misses file names %s, want %s", got, wantWhole)
			}
			if said := logs.FilterMessageSnippet(afterLastReadFailed).Len(); said != 0 {
				t.Fatalf("a set's end read that vouched for every call is said to have failed %d times", said)
			}
		})
	}
}

// With the switch off a set's end read that cannot vouch for every call made
// after the set's last test leaves the set's verdict as it is: the set
// passes, the report names what the read returned, and a warning says the
// report may not name each.
func TestRunTestSet_ASetEndReadThatFailsWithTheSwitchOffLeavesTheVerdict(t *testing.T) {
	provenOn(t, false)
	h := newPartialRunHarness(t, 2, 0)
	logs := observeWarnings(h)
	h.instr.mockErrorsFor = misses(nil, nil, func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "DNS", ActualSummary: "A metrics.staging.internal."}},
			fmt.Errorf("%w (agent: keploy's error drain stopped)", models.ErrUnmatchedCallsUnread)
	})

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s (reason %q); with the switch off a set's end read fails nothing", status, h.report.report.FailureReason)
	}
	if h.replayer.notJudged("test-set-0") {
		t.Fatal("the set is marked not judged; with the switch off its end read judges nothing")
	}
	if got, want := calledShaped(h.report.report.OutsideTestMisses), "DNS A {}.{}.{}. @after_last_test;"; got != want {
		t.Fatalf("the report names %s outside every test, want %s", got, want)
	}
	if said := logs.FilterMessageSnippet(afterLastReadFailed).All(); len(said) != 1 || said[0].ContextMap()["test-set"] != "test-set-0" {
		t.Fatalf("the failed read is said as %+v, want once, naming the set", said)
	}
}

// With the switch off a set one of whose tests fails has its end read as a
// set that passes has: it fails for that test alone, and the call its
// application made after its last test that no mock answered is named as
// made then, by its shape in its report and whole in keploy test's misses
// file. It is no unclaimed call: with the switch off none fails a set.
func TestRunTestSet_ACallAfterTheLastTestIsNamedOnASetThatFailedWithTheSwitchOff(t *testing.T) {
	provenOn(t, false)
	h := newPartialRunHarness(t, 2, 0)
	withSetDir(t, h)
	h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-1": true}} // test-1's answer differs
	agent := readAsAnAgentIs(h)
	agent.atEnd = func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /v1/flush", Destination: "10.20.30.42:9099"}}, nil
	}

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s (reason %q), want FAILED for test-1", status, h.report.report.FailureReason)
	}
	rep := h.report.report
	for _, r := range rep.Tests {
		if want := models.TestStatusPassed; r.TestCaseID != "test-1" && r.Status != want {
			t.Fatalf("test %s is %s, want %s: only test-1's answer differs", r.TestCaseID, r.Status, want)
		} else if r.TestCaseID == "test-1" && r.Status != models.TestStatusFailed {
			t.Fatalf("test-1 is %s, want FAILED: its answer differs", r.Status)
		}
	}
	if len(rep.UnclaimedCalls) != 0 || strings.Contains(rep.FailureReason, "flush") {
		t.Fatalf("unclaimed calls %+v, reason %q; with the switch off the call fails nothing", rep.UnclaimedCalls, rep.FailureReason)
	}
	if n := agent.ends.Load(); n != 1 {
		t.Fatalf("%d reads of the set's end, want 1", n)
	}
	if got, want := calledShaped(rep.OutsideTestMisses), "HTTP POST /{}/{} @after_last_test;"; got != want {
		t.Fatalf("the report names %s outside every test, want %s", got, want)
	}
	if got, want := calledWhole(readTestMisses(t, h)), "HTTP POST /v1/flush 10.20.30.42:9099;"; got != want {
		t.Fatalf("the misses file names %s, want %s", got, want)
	}
}

// With the switch off a set whose application exited on its own before the
// set's end (APP_HALTED) has its end read too, as one that ran to its end
// has: the agent still holds what the application missed before it exited,
// and the set's report names it as made after its last test (it fails
// nothing, and the set's status is its halt's).
func TestRunTestSet_ASetWhoseAppHaltedHasItsEndReadWithTheSwitchOff(t *testing.T) {
	provenOn(t, false)
	h := newPartialRunHarness(t, 2, 0)
	withSetDir(t, h)
	h.instr.stopAppAfterNUpdates = 2
	agent := readAsAnAgentIs(h)
	agent.atEnd = func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "DNS", ActualSummary: "A metrics.staging.internal.", HostTag: "#cd34"}}, nil
	}

	if status := h.run(t); status != models.TestSetStatusAppHalted {
		t.Fatalf("status %s, want APP_HALTED: the case needs a set whose application exited before its end", status)
	}
	if n := agent.ends.Load(); n != 1 {
		t.Fatalf("%d reads of the set's end, want 1: the agent holds what the application missed before it exited", n)
	}
	if got, want := calledShaped(h.report.report.OutsideTestMisses), "DNS A {}.{}.{}. @after_last_test;"; got != want {
		t.Fatalf("the report names %s outside every test, want %s", got, want)
	}
	if got, want := calledWhole(readTestMisses(t, h)), "DNS A metrics.staging.internal. ;"; got != want {
		t.Fatalf("the misses file names %s, want %s", got, want)
	}
}

// With the switch off a set's end read that the run's user cut short, by
// stopping the run as it was made, is the stop's, as under the switch
// (TestRunTestSet_AStopThatCutsTheSetEndReadShortIsTheStops): the set is
// USER_ABORT, whether its tests passed or one failed, it is not marked not
// judged, and the cut read is said at Debug, never at WARN, since the stop is
// the set's status. The read is made after the set's status is final, with
// its application stopped (stopForSetEndRead), so it is the read that makes
// the set the stop's (cutReadStatus).
func TestRunTestSet_AStopThatCutsTheSwitchOffSetEndReadIsTheStops(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprintf("test-1-fails=%v", failing), func(t *testing.T) {
			provenOn(t, false)
			h := newPartialRunHarness(t, 2, 0)
			if failing {
				h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-1": true}} // test-1's answer differs
			}
			core, logs := observer.New(zapcore.DebugLevel)
			h.replayer.logger = zap.New(core)
			instr := &stopsAtSetEnd{prInstr: h.instr, at: 3} // a read per test, then the set's
			instr.stop = func() { h.cancel() }
			h.replayer.instrumentation = instr

			status := h.run(t)
			if n := instr.reads.Load(); n != 3 {
				t.Fatalf("fixture: %d reads of the agent's misses, want 3", n)
			}
			if status != models.TestSetStatusUserAbort {
				t.Fatalf("status %s, want USER_ABORT: the user stopped the run as the set's end was read", status)
			}
			if got := h.report.report.Status; got != string(models.TestSetStatusUserAbort) {
				t.Fatalf("the set's report says %s, want USER_ABORT", got)
			}
			if h.replayer.notJudged("test-set-0") {
				t.Fatal("a set the user stopped is marked not judged")
			}
			if said := logs.FilterMessageSnippet(afterLastReadFailed).FilterLevelExact(zapcore.WarnLevel).Len(); said != 0 {
				t.Fatalf("the read the user's stop cut short is said at WARN %d times, want none", said)
			}
			if said := logs.FilterMessageSnippet("cut short by the run's stop").FilterLevelExact(zapcore.DebugLevel).All(); len(said) != 1 || said[0].ContextMap()["test-set"] != "test-set-0" {
				t.Fatalf("the read the user's stop cut short is said at Debug as %+v, want once, naming the set", said)
			}
		})
	}
}

// A set-end read the run's user cut short makes a judged set (PASSED or
// FAILED) the stop's, USER_ABORT; any other status stands: a set not judged,
// one an end of its own ended first, one whose every test was ignored.
func TestACutSetEndReadMakesOnlyAJudgedSetTheStops(t *testing.T) {
	for _, c := range []struct{ had, want models.TestSetStatus }{
		{models.TestSetStatusPassed, models.TestSetStatusUserAbort},
		{models.TestSetStatusFailed, models.TestSetStatusUserAbort},
		{models.TestSetStatusAppHalted, models.TestSetStatusAppHalted},
		{models.TestSetStatusFaultUserApp, models.TestSetStatusFaultUserApp},
		{models.TestSetStatusInternalErr, models.TestSetStatusInternalErr},
		{models.TestSetStatusFaultScript, models.TestSetStatusFaultScript},
		{models.TestSetStatusIgnored, models.TestSetStatusIgnored},
		{models.TestSetStatusUserAbort, models.TestSetStatusUserAbort},
	} {
		if got := cutReadStatus(c.had); got != c.want {
			t.Errorf("a %s set whose end read the user's stop cut is %s, want %s", c.had, got, c.want)
		}
	}
}

// blocksAtSetEnd is the stand-in agent whose read of the set's end answers
// only once that read's context is done, with its error: an agent that does
// not answer within the read's budget.
type blocksAtSetEnd struct{ *prInstr }

func (f *blocksAtSetEnd) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	if models.SetEndRead(ctx) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.prInstr.GetMockErrors(ctx)
}

// With the switch off a set-end read whose budget runs out is no user's stop:
// it is said at WARN, naming the set, and the set's verdict stands.
func TestRunTestSet_ASwitchOffSetEndReadWhoseBudgetRunsOutIsSaidAtWarn(t *testing.T) {
	provenOn(t, false)
	was := setEndDrainBudget
	setEndDrainBudget = 100 * time.Millisecond
	t.Cleanup(func() { setEndDrainBudget = was })
	h := newPartialRunHarness(t, 2, 0)
	core, logs := observer.New(zapcore.DebugLevel)
	h.replayer.logger = zap.New(core)
	h.replayer.instrumentation = &blocksAtSetEnd{h.instr}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s (reason %q); with the switch off a set's end read fails nothing", status, h.report.report.FailureReason)
	}
	if said := logs.FilterMessageSnippet(afterLastReadFailed).FilterLevelExact(zapcore.WarnLevel).All(); len(said) != 1 || said[0].ContextMap()["test-set"] != "test-set-0" {
		t.Fatalf("the read whose budget ran out is said at WARN as %+v, want once, naming the set", said)
	}
	if said := logs.FilterMessageSnippet("cut short by the run's stop").Len(); said != 0 {
		t.Fatalf("the read whose budget ran out is said %d times as cut short by the run's stop; no one stopped the run", said)
	}
}

// A set whose application looked a name up and fetched its configuration as
// it started, neither answered by a mock: with the switch off neither fails
// anything, so the set passes and both are named outside every test, in its
// report and its misses file; under the switch the fetch counts against a
// test and fails the set (UnclaimedCalls), and only the lookup is named
// outside every test. A call is in one list, never both.
func TestRunTestSet_AStartupFetchIsNamedOutsideEveryTestOnlyWhereItFailsNothing(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			provenOn(t, proven)
			h := newPartialRunHarness(t, 2, 0)
			withSetDir(t, h)
			h.instr.mockErrorsFor = misses(func() ([]models.UnmatchedCall, error) {
				return []models.UnmatchedCall{
					{Protocol: "DNS", ActualSummary: "A orders-db.staging.internal.", HostTag: "#ab12", BeforeSet: true, Kind: models.MissOutsideTest},
					{Protocol: "HTTP", ActualSummary: "GET /v1/config", Destination: "10.20.30.40:8080", BeforeSet: true, Kind: models.MissOutsideTest},
				}, nil
			})

			status := h.run(t)
			rep := h.report.report
			for _, r := range rep.Tests {
				if r.Status != models.TestStatusPassed || len(r.FailureInfo.UnmatchedCalls) != 0 {
					t.Fatalf("test %s is %s with %+v; a start-up call is no test's", r.TestCaseID, r.Status, r.FailureInfo.UnmatchedCalls)
				}
			}
			wantShaped := "DNS A {}.{}.{}. @before_first_test;HTTP GET /{}/{} @before_first_test;"
			wantWhole := "DNS A orders-db.staging.internal. ;HTTP GET /v1/config 10.20.30.40:8080;"
			if proven {
				if status != models.TestSetStatusFailed || len(rep.UnclaimedCalls) != 1 || rep.UnclaimedCalls[0].ActualSummary != "GET /v1/config" {
					t.Fatalf("status %s, unclaimed %+v; under the switch the fetch fails the set", status, rep.UnclaimedCalls)
				}
				wantShaped = "DNS A {}.{}.{}. @before_first_test;"
				wantWhole = "DNS A orders-db.staging.internal. ;"
			} else if status != models.TestSetStatusPassed || len(rep.UnclaimedCalls) != 0 {
				t.Fatalf("status %s, unclaimed %+v; with the switch off a start-up call fails nothing", status, rep.UnclaimedCalls)
			}
			if got := calledShaped(rep.OutsideTestMisses); got != wantShaped {
				t.Fatalf("the report names %s outside every test, want %s", got, wantShaped)
			}
			if got := calledWhole(readTestMisses(t, h)); got != wantWhole {
				t.Fatalf("the misses file names %s, want %s", got, wantWhole)
			}
		})
	}
}

// A set none of whose tests opened its capture window (both are ignored) has
// its end read in neither mode: what the agent holds then is the
// application's start-up, which no test's read handed over as the set's, and
// would be named as made after its last test. So for an application keploy
// stops before the set's report (one it started for the set) and for one it
// reads running (one --keep-app-alive keeps for a later set).
func TestRunTestSet_ASetNoTestOfWhichOpenedItsWindowReadsNoSetEnd(t *testing.T) {
	for _, proven := range []bool{false, true} {
		for _, kept := range []bool{false, true} {
			t.Run(fmt.Sprintf("proven=%v,kept=%v", proven, kept), func(t *testing.T) {
				provenOn(t, proven)
				h := newPartialRunHarness(t, 2, 0)
				h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-1", "test-2"}}
				var reads atomic.Int32
				h.instr.mockErrorsFor = counted(&reads, func() ([]models.UnmatchedCall, error) {
					return []models.UnmatchedCall{{Protocol: "DNS", ActualSummary: "A orders-db.staging.internal."}}, nil
				})

				if kept {
					// Kept for a later set: read running, not stopped.
					h.replayer.keepAliveStop = func() string { return "" }
					h.replayer.isLastTestSet, h.replayer.finalAttempt = false, true
					h.runServing(t)
				} else {
					h.run(t)
				}
				if rep := h.report.report; rep == nil || rep.Ignored != 2 {
					t.Fatalf("the report %+v; the case needs both tests ignored", rep)
				}
				if n := reads.Load(); n != 0 {
					t.Fatalf("%d reads of the agent's misses, want none: no test opened the set's window", n)
				}
				if got := h.report.report.OutsideTestMisses; len(got) != 0 {
					t.Fatalf("the report names %+v outside every test, want none", got)
				}
			})
		}
	}
}

// A run of a set that ends before its report is written (its post-script
// fails) leaves no misses file naming an earlier run's calls as its own: the
// set's start removed keploy test's, and keploy mock replay's is byte for
// byte as it was.
func TestRunTestSet_ASetThatEndsBeforeItsReportLeavesNoEarlierRunsMissesFile(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			provenOn(t, proven)
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.testSetConf = postScriptFails{}
			dir := withSetDir(t, h)
			mockFile := filepath.Join(dir, localfile.MissesFile)
			mockWas := []byte("set: test-set-0\nby: keploy mock replay\nmisses:\n    - protocol: HTTP\n      call: GET /v1/reset\n      count: 1\n")
			if err := os.WriteFile(mockFile, mockWas, 0o600); err != nil {
				t.Fatal(err)
			}
			testFile := filepath.Join(dir, localfile.TestMissesFile)
			if err := os.WriteFile(testFile, []byte("set: test-set-0\nby: keploy test\nmisses:\n    - protocol: DNS\n      call: A an-earlier-run.example.\n      count: 1\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			status, err := h.replayer.RunTestSet(context.Background(), "test-set-0", "test-run-0", false)
			if status != models.TestSetStatusFaultScript || err == nil {
				t.Fatalf("set %q, %v; want %q and the post-script's error", status, err, models.TestSetStatusFaultScript)
			}
			if rep := h.report.report; rep == nil || rep.Status != string(models.TestStatusRunning) {
				t.Fatalf("the set's report is %+v; the case needs a set that ends before its report, which says it is still running", rep)
			}
			if _, err := os.Lstat(testFile); !os.IsNotExist(err) {
				t.Errorf("an earlier run's keploy test misses file stands after a run that wrote none: %v", err)
			}
			if b, err := os.ReadFile(mockFile); err != nil || !bytes.Equal(b, mockWas) {
				t.Errorf("keploy mock replay's misses file holds %q (%v), want it byte for byte", b, err)
			}
		})
	}
}

// postScriptTouches is a set's config whose post-script makes a file, path.
type postScriptTouches struct {
	prTestSetConf
	path string
}

func (c postScriptTouches) Read(context.Context, string) (*models.TestSet, error) {
	return &models.TestSet{PostScript: "touch '" + c.path + "'"}, nil
}

// stopsAfterPost is a stand-in agent whose application, as it is stopped,
// says whether the set's post-script had run (its file was there).
type stopsAfterPost struct {
	*readsAsAnAgentDoes
	post     string
	postRan  atomic.Bool
	stoppedN atomic.Int32
	// exited: the application has exited, a moment after its stop began.
	exited atomic.Bool
	// told is what the agent was told of and what the application saw,
	// in order: "notify" for each notice of the shutdown, "stop" for the
	// application's stop.
	toldMu sync.Mutex
	told   []string
}

func (f *stopsAfterPost) note(what string) {
	f.toldMu.Lock()
	defer f.toldMu.Unlock()
	f.told = append(f.told, what)
}

func (f *stopsAfterPost) NotifyGracefulShutdown(context.Context) error {
	f.note("notify")
	return nil
}

func (f *stopsAfterPost) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	err := f.readsAsAnAgentDoes.Run(ctx, opts)
	if ctx.Err() != nil {
		f.note("stop")
		_, statErr := os.Stat(f.post)
		f.postRan.Store(statErr == nil)
		f.stoppedN.Add(1)
		// Its shutdown takes a moment: a read that does not wait for its
		// exit is made before it.
		time.Sleep(100 * time.Millisecond)
		f.exited.Store(true)
	}
	return err
}

// stopLookup is the call the stand-in application makes only as it stops.
var stopLookup = models.UnmatchedCall{Protocol: "DNS", ActualSummary: "A shutdown-audit.staging.internal.", HostTag: "#ab12"}

// With the switch off the set's end stops the application keploy started for
// it after the set's post-script, which runs with it up, then reads the calls
// it made after its last test, as it stopped among them, and then writes the
// set's report: the report names the lookup the application made only as it
// stopped as made after the last test, by its shape, the misses file names it
// whole, and the set passes (it fails nothing). The agent is told of the
// shutdown before the stop, as the set's teardown tells it, so it says the
// application's shutdown errors as such.
func TestRunTestSet_ACallTheAppMakesAsItStopsIsNamedWithTheSwitchOff(t *testing.T) {
	provenOn(t, false)
	h := newPartialRunHarness(t, 2, 0)
	withSetDir(t, h)
	agent := readAsAnAgentIs(h)
	app := &stopsAfterPost{readsAsAnAgentDoes: agent, post: filepath.Join(t.TempDir(), "post-ran")}
	h.replayer.instrumentation = app
	h.replayer.testSetConf = postScriptTouches{path: app.post}
	agent.atEnd = func() ([]models.UnmatchedCall, error) {
		if !app.exited.Load() {
			// Still running: its stop's lookup is made only as it stops.
			return nil, nil
		}
		return []models.UnmatchedCall{stopLookup}, nil
	}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s (reason %q); a lookup the application makes as it stops fails nothing", status, h.report.report.FailureReason)
	}
	if n := app.stoppedN.Load(); n != 1 {
		t.Fatalf("the application was stopped %d times, want once", n)
	}
	if !app.postRan.Load() {
		t.Fatal("the application was stopped before the set's post-script ran; the post-script runs with it up")
	}
	app.toldMu.Lock()
	told := strings.Join(app.told, ",")
	app.toldMu.Unlock()
	if !strings.HasPrefix(told, "notify,stop") {
		t.Fatalf("the agent was told and the application saw %q; want the shutdown's notice before the stop", told)
	}
	if ends := agent.ends.Load(); ends != 1 {
		t.Fatalf("%d set-end reads, want one, after the application's stop", ends)
	}
	if got, want := calledShaped(h.report.report.OutsideTestMisses), "DNS A {}.{}.{}. @after_last_test;"; got != want {
		t.Fatalf("the report names %s outside every test, want %s (the lookup made as the application stopped)", got, want)
	}
	if got, want := calledWhole(readTestMisses(t, h)), "DNS A shutdown-audit.staging.internal. ;"; got != want {
		t.Fatalf("the misses file names %s, want %s", got, want)
	}
}

// With the switch off the application --keep-app-alive keeps across the sets
// is stopped before a set's end read only on the run's last set, on an
// attempt no other follows: that read, after the set's post-script, then
// takes the calls it made as it stopped. On any other set it is read
// running, before the set's post-script, as before, and not stopped; so is
// an application keploy does not stop at all (a docker compose stack's).
func TestRunTestSet_TheKeptAppIsStoppedBeforeTheReadOnlyAtTheRunsEnd(t *testing.T) {
	for _, c := range []struct {
		name        string
		last, final bool
		noStop      bool
		wantStop    bool
	}{
		{"the run's last set, final attempt", true, true, false, true},
		{"an earlier set", false, true, false, false},
		{"the last set, an attempt that others follow", true, false, false, false},
		{"the last set, final attempt, an app keploy does not stop", true, true, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			provenOn(t, false)
			h := newPartialRunHarness(t, 2, 0)
			withSetDir(t, h)
			agent := readAsAnAgentIs(h)
			var stops atomic.Int32
			if !c.noStop {
				h.replayer.keepAliveStop = func() string { stops.Add(1); return "" }
			}
			h.replayer.isLastTestSet, h.replayer.finalAttempt = c.last, c.final
			post := filepath.Join(t.TempDir(), "post-ran")
			h.replayer.testSetConf = postScriptTouches{path: post}
			var postAtRead atomic.Bool
			agent.atEnd = func() ([]models.UnmatchedCall, error) {
				_, err := os.Stat(post)
				postAtRead.Store(err == nil)
				if stops.Load() == 0 {
					return nil, nil
				}
				return []models.UnmatchedCall{stopLookup}, nil
			}

			if status := h.runServing(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %s (reason %q)", status, h.report.report.FailureReason)
			}
			want, wantShaped := int32(0), ""
			if c.wantStop {
				want, wantShaped = 1, "DNS A {}.{}.{}. @after_last_test;"
			}
			if n := stops.Load(); n != want {
				t.Fatalf("the kept application was stopped %d times, want %d", n, want)
			}
			if ends := agent.ends.Load(); ends != 1 {
				t.Fatalf("%d set-end reads, want one", ends)
			}
			if postAtRead.Load() != c.wantStop {
				t.Fatalf("the set-end read came after the post-script: %v, want %v (after it only when the read waits for the application's stop)", postAtRead.Load(), c.wantStop)
			}
			if _, err := os.Stat(post); err != nil {
				t.Fatalf("the set's post-script did not run: %v", err)
			}
			if got := calledShaped(h.report.report.OutsideTestMisses); got != wantShaped {
				t.Fatalf("the report names %q outside every test, want %q", got, wantShaped)
			}
		})
	}
}
