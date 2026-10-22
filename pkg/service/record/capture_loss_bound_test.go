package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	agenthttp "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Under the proven switch the base, test cases' and final reads of the
// agent's count (captureWatch) had no bound and no escape: an agent that
// never answered one held keploy record's capture start, its test-case
// consumer, or its stop, forever. Each is bounded now as the switch-off
// watch's are (countReads): given up once keploy had stall.HangBudget of
// its own runnable time to run without an answer, said at Info with that
// bound while waited for, and what it would have checked is then not
// verified, and fails the recording, saying so. Nothing it saved is lost.

// manualCounts has every read of the agent's count measure its bound on a
// clock the test drives, and its waits look every millisecond.
// keploysTicks is countReadTicks as keploy has it.
var keploysTicks = countReadTicks

func manualCounts(t *testing.T) *stall.ManualClock {
	t.Helper()
	clk := &stall.ManualClock{}
	countReadClock, countReadEvery = clk, time.Millisecond
	t.Cleanup(func() { countReadClock, countReadEvery = nil, drainProgressEvery })
	return clk
}

// saidTheBound waits for the watch to say, at Info, that it waits for the
// agent's count, that Ctrl+C does not cut the wait short, and each read's
// bound, in a line logged after the first from (logs.Len() as the wait
// began: a line of an earlier wait, as the base read's, is not this one's).
func saidTheBound(t *testing.T, logs *observer.ObservedLogs, from int) {
	t.Helper()
	const want = "Still waiting for the keploy agent to say whether it captured every call while this recording ran; Ctrl+C does not cut this short: each read is given up once keploy has had 1m0s of its own time to run without an answer"
	deadline := time.Now().Add(30 * time.Second) // bounds only a wait that never says so
	for {
		for _, e := range logs.All()[from:] {
			if e.Message != want {
				continue
			}
			if e.Level != zapcore.InfoLevel {
				t.Fatalf("said at %v, want Info", e.Level)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the wait never said it waits, with its bound: %v", logs.All()[from:])
		}
		time.Sleep(time.Millisecond)
	}
}

// notVerified is the one ERROR that names test cases as not verified, with
// why.
func notVerified(t *testing.T, logs *observer.ObservedLogs) (testCases, why string) {
	t.Helper()
	errs := logs.FilterMessageSnippet("could not read whether its agent captured every call while these test cases were recorded").All()
	if len(errs) != 1 || errs[0].Level != zapcore.ErrorLevel {
		t.Fatalf("%d ERRORs naming test cases not verified, want 1: %v", len(errs), logs.All())
	}
	f := errs[0].ContextMap()
	return fmt.Sprint(f["testCases"]), fmt.Sprint(f["error"])
}

// A base read the agent never answers holds the capture's start only until
// keploy had its bound to run: it says it waits, and its bound; the stop
// does not end it, and a press finds no offer; wall time keploy could not
// run gives nothing up. Every test case is then not verified, saying why,
// and the recording fails.
func TestTheSwitchsBaseReadIsGivenUpOnItsBound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stoppedRun(t)
	clk := manualCounts(t)
	r := newWedgedReader(1, nil)
	core, logs := observer.New(zapcore.InfoLevel)
	ctx, stop := context.WithCancel(context.Background())
	got := make(chan *captureWatch, 1)
	go func() { got <- watchCaptureLosses(ctx, zap.New(core), r) }()
	within(t, r.asked, "the base read")
	saidTheBound(t, logs, 0)
	stop() // the stop: record's context ends
	if utils.MarkInterruptedAgain() {
		t.Fatal("the base read stood an offer of the stop's")
	}
	for i := 0; i < 10; i++ {
		clk.Pass() // wall time passes; keploy cannot run
	}
	select {
	case <-got:
		t.Fatal("the base read ended on the stop, a press, or wall time keploy could not run")
	default:
	}
	giveUp(t, clk)
	var w *captureWatch
	select {
	case w = <-got:
	case <-time.After(30 * time.Second): // bounds only a read never given up
		t.Fatal("the base read was never given up")
	}
	within(t, r.ended, "the end of the base read")
	w.afterTestCase(ctx, "test-1")
	w.afterTestCases(ctx)
	if !w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[test-1]" {
		t.Fatalf("%s; want test-1 not verified and the recording failed", w)
	}
	if names, why := notVerified(t, logs); names != "[test-1]" || why != errCaptureCountSilent.Error() {
		t.Fatalf("named %s for %q; want [test-1] for %q", names, why, errCaptureCountSilent)
	}
}

// afterTheLast is the one ERROR that says what the agent may have lost after
// the last test case was not read, and why.
func afterTheLast(t *testing.T, logs *observer.ObservedLogs) (why string) {
	t.Helper()
	errs := logs.FilterMessageSnippet("could not read whether its agent captured every call after the last test case was recorded").All()
	if len(errs) != 1 || errs[0].Level != zapcore.ErrorLevel {
		t.Fatalf("%d ERRORs saying the final read was not read, want 1: %v", len(errs), logs.All())
	}
	return fmt.Sprint(errs[0].ContextMap()["error"])
}

// A base read given up on its bound leaves the recording's end not verified
// too, not only its test cases: the final read has no count from the start
// to be judged against, and is not made. A recording that saved no test case
// (its calls, if any, all after the last) fails all the same, saying so,
// rather than passing with nothing checked.
func TestTheSwitchsBaseReadGivenUpLeavesTheRecordingsEndNotVerified(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clk := manualCounts(t)
	r := newWedgedReader(1, nil)
	core, logs := observer.New(zapcore.InfoLevel)
	got := make(chan *captureWatch, 1)
	go func() { got <- watchCaptureLosses(context.Background(), zap.New(core), r) }()
	within(t, r.asked, "the base read")
	giveUp(t, clk)
	var w *captureWatch
	select {
	case w = <-got:
	case <-time.After(30 * time.Second): // bounds only a read never given up
		t.Fatal("the base read was never given up")
	}
	w.afterTestCases(context.Background())
	w.waitForReads()
	if !w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[]" {
		t.Fatalf("%s; want the recording failed for its end, with no test case named", w)
	}
	if why := afterTheLast(t, logs); why != errCaptureCountSilent.Error() {
		t.Fatalf("the final read not read for %q; want %q", why, errCaptureCountSilent)
	}
	if n := r.n.Load(); n != 1 {
		t.Fatalf("%d reads after the base read was given up; want none", n-1)
	}
}

// The interrupt that gave the stop's waits up (utils.StopNow) ends a base
// read the agent never answers at once, and says it was a Ctrl+C that left
// the recording not verified, as for any other read it ends.
func TestTheSwitchsBaseReadEndedByTheUserSaysSo(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stoppedRun(t)
	manualCounts(t)
	r := newWedgedReader(1, nil)
	core, logs := observer.New(zapcore.ErrorLevel)
	got := make(chan *captureWatch, 1)
	go func() { got <- watchCaptureLosses(context.Background(), zap.New(core), r) }()
	within(t, r.asked, "the base read")
	_, withdraw := utils.OfferStopNow() // a drain said another interrupt stops it now
	defer withdraw()
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the press did not answer the drain's offer")
	}
	var w *captureWatch
	select {
	case w = <-got:
	case <-time.After(30 * time.Second): // bounds only a read never ended
		t.Fatal("the base read did not end at the press that gave the stop's waits up")
	}
	w.afterTestCase(context.Background(), "test-1")
	w.afterTestCases(context.Background())
	if !w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[test-1]" {
		t.Fatalf("%s; want test-1 not verified and the recording failed", w)
	}
	if names, why := notVerified(t, logs); names != "[test-1]" || why != errCaptureCountStopped.Error() {
		t.Fatalf("named %s for %q; want [test-1] for %q", names, why, errCaptureCountStopped)
	}
	if why := afterTheLast(t, logs); why != errCaptureCountStopped.Error() {
		t.Fatalf("the final read not read for %q; want %q", why, errCaptureCountStopped)
	}
}

// A test case's check is asked as it is saved, and the consumer goes on: a
// check the agent never answers holds no test case up. The stop's wait for
// it, the frame drains over, says it waits and its bound, stands no offer,
// and is given up on that bound alone; the test case is then not verified,
// and the next check (test-2's, and the final read, made after it, in
// order) is counted as usual.
func TestTheSwitchsTestCaseCheckIsAskedAndGivenUpOnItsBound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stoppedRun(t)
	clk := manualCounts(t)
	r := newWedgedReader(2, nil) // the base; test-1's check never answered; then nothing lost
	core, logs := observer.New(zapcore.InfoLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	asked := make(chan struct{})
	go func() { w.afterTestCase(context.Background(), "test-1"); close(asked) }()
	within(t, asked, "test-1's ask, without waiting for its check")
	within(t, r.asked, "test-1's check")
	w.afterTestCase(context.Background(), "test-2") // the consumer goes on
	w.afterTestCases(context.Background())          // the stop's final read
	from := logs.Len()
	waited := make(chan struct{})
	go func() { w.waitForReads(); close(waited) }()
	saidTheBound(t, logs, from)
	if utils.MarkInterruptedAgain() {
		t.Fatal("the stop's wait for the checks stood an offer")
	}
	select {
	case <-utils.StopNow():
		t.Fatal("a press at the stop's wait for the checks gave the stop's waits up")
	default:
	}
	for i := 0; i < 10; i++ {
		clk.Pass()
	}
	select {
	case <-waited:
		t.Fatal("a check was given up for wall time keploy could not run")
	default:
	}
	giveUp(t, clk)
	within(t, r.ended, "the end of test-1's check")
	within(t, waited, "the stop's wait, the check given up on its bound")
	if !w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[test-1]" {
		t.Fatalf("%s; want test-1 alone not verified and the recording failed", w)
	}
	if names, why := notVerified(t, logs); names != "[test-1]" || why != errCaptureCountSilent.Error() {
		t.Fatalf("named %s for %q; want [test-1] for %q", names, why, errCaptureCountSilent)
	}
	if n := logs.FilterMessageSnippet("after the last test case").Len(); n != 0 {
		t.Fatalf("%d lines of the final read, which answered", n)
	}
}

// A final read the agent never answers is given up on its bound: what was
// lost after the last test case is not verified, saying why, and the
// recording fails.
func TestTheSwitchsFinalReadIsGivenUpOnItsBound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clk := manualCounts(t)
	r := newWedgedReader(3, nil) // the base, test-1's check; the final read never answered
	core, logs := observer.New(zapcore.InfoLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	asked := make(chan struct{})
	go func() { w.afterTestCases(context.Background()); close(asked) }()
	within(t, asked, "the final read's ask, without waiting for it")
	within(t, r.asked, "the final read")
	from := logs.Len()
	waited := make(chan struct{})
	go func() { w.waitForReads(); close(waited) }()
	saidTheBound(t, logs, from)
	giveUp(t, clk)
	within(t, waited, "the stop's wait, the final read given up on its bound")
	if !w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[]" {
		t.Fatalf("%s; want the recording failed for its final read alone", w)
	}
	errs := logs.FilterMessageSnippet("could not read whether its agent captured every call after the last test case was recorded").All()
	if len(errs) != 1 || fmt.Sprint(errs[0].ContextMap()["error"]) != errCaptureCountSilent.Error() {
		t.Fatalf("ERRORs %v; want one saying the final read was not read, for %q", errs, errCaptureCountSilent)
	}
}

// Where a later pass judges each recorded test case (JudgeLossesLater), a
// read given up on its bound fails nothing here: the test case its check
// was for is handed to that pass as not verified, with why, and so is the
// recording's end when the final read is given up. Neither is an ERROR: the
// one WARN says the auto-replay judges the set.
func TestAReadGivenUpIsHandedToTheLaterPassThatJudges(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	got := judgeLater(t)
	for _, c := range []struct {
		name       string
		wedge      int32 // the read the agent never answers: 2 is test-1's check, 3 the final read
		unverified string
		final      bool
	}{
		{"test-1's check", 2, "[test-1]", false},
		{"the final read", 3, "[]", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk := manualCounts(t)
			r := newWedgedReader(c.wedge, nil)
			core, logs := observer.New(zapcore.WarnLevel)
			w := watchCaptureLosses(context.Background(), zap.New(core), r)
			w.afterTestCase(context.Background(), "test-1")
			if !c.final {
				within(t, r.asked, "test-1's check")
			} else {
				settle(w)
			}
			w.afterTestCases(context.Background())
			if c.final {
				within(t, r.asked, "the final read")
			}
			waited := make(chan struct{})
			go func() { w.waitForReads(); close(waited) }()
			giveUp(t, clk)
			within(t, waited, "the stop's wait, the read given up on its bound")
			before := len(*got)
			if w.finish("test-set-0", false) {
				t.Fatal("a read given up failed a recording a later pass judges")
			}
			if len(*got) != before+1 {
				t.Fatalf("handed %d accounts, want 1", len(*got)-before)
			}
			l := (*got)[len(*got)-1]
			if fmt.Sprint(l.Unverified) != c.unverified || !l.Failing() {
				t.Fatalf("handed %+v, want %s not verified, the set failing", l, c.unverified)
			}
			if !c.final && l.UnverifiedWhy != errCaptureCountSilent.Error() {
				t.Fatalf("test-1 handed as not verified for %q, want %q", l.UnverifiedWhy, errCaptureCountSilent)
			}
			if c.final != strings.Contains(l.NotVerified, errCaptureCountSilent.Error()) {
				t.Fatalf("the recording's end handed as not verified for %q; want the bound's reason only when the final read was given up", l.NotVerified)
			}
			if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
				t.Fatalf("%d ERRORs where a later pass judges: %v", n, logs.All())
			}
			if n := logs.FilterMessageSnippet("the auto-replay that follows judges each of its test cases").Len(); n != 1 {
				t.Fatalf("%d WARNs that the auto-replay judges the set, want 1: %v", n, logs.All())
			}
		})
	}
}

// finish, after every drain, offers one more Ctrl+C while a check is out: a
// press ends the wait and the check, the test case it was for is not
// verified (never dropped, nor taken for clean), and the offer goes with
// finish.
func TestAPressAtTheSwitchsFinishNamesTheTestCaseItsCheckWasFor(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stoppedRun(t)
	manualCounts(t)
	r := newWedgedReader(4, nil) // the base, test-1's, the final read; test-2's never answered
	core, logs := observer.New(zapcore.InfoLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	w.afterTestCases(context.Background()) // the native order: before the drain
	settle(w)
	w.afterTestCase(context.Background(), "test-2") // saved by the drain
	within(t, r.asked, "test-2's check")
	failed := make(chan bool, 1)
	go func() { failed <- w.finish("test-set-0", false) }()
	deadline := time.Now().Add(30 * time.Second) // bounds only a finish that never offers
	for logs.FilterMessage("Still waiting for the keploy agent to say whether it captured every call while this recording ran; press Ctrl+C again to stop now: the test cases it would check are then not verified, and the recording fails").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("finish never offered: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the press did not answer finish's offer")
	}
	select {
	case f := <-failed:
		if !f {
			t.Fatal("the recording passed with test-2 not checked")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("finish did not end at the press")
	}
	within(t, r.ended, "the end of test-2's check")
	if w.String() != "tainted=[] unverified=[test-2]" {
		t.Fatalf("%s; want test-2 not verified", w)
	}
	if names, why := notVerified(t, logs); names != "[test-2]" || why != errCaptureCountStopped.Error() {
		t.Fatalf("named %s for %q; want [test-2] for %q", names, why, errCaptureCountStopped)
	}
	if utils.MarkInterruptedAgain() {
		t.Fatal("finish's offer still stood after it ended")
	}
}

// A press at finish's offer while the final read is out ends it: what was
// lost after the last test case is not verified, because a Ctrl+C stopped
// the wait, and the recording fails. finish takes the read as the press left
// it before it says anything, whichever of the two runs first.
func TestAPressAtTheSwitchsFinishLeavesTheFinalReadNotVerified(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stoppedRun(t)
	manualCounts(t)
	r := newWedgedReader(3, nil) // the base, test-1's; the final read never answered
	core, logs := observer.New(zapcore.InfoLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	w.afterTestCases(context.Background())
	within(t, r.asked, "the final read")
	failed := make(chan bool, 1)
	go func() { failed <- w.finish("test-set-0", false) }()
	deadline := time.Now().Add(30 * time.Second) // bounds only a finish that never offers
	for logs.FilterMessageSnippet("press Ctrl+C again to stop now").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("finish never offered: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the press did not answer finish's offer")
	}
	select {
	case f := <-failed:
		if !f {
			t.Fatal("the recording passed with its final read not read")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("finish did not end at the press")
	}
	errs := logs.FilterMessageSnippet("could not read whether its agent captured every call after the last test case was recorded").All()
	if len(errs) != 1 || fmt.Sprint(errs[0].ContextMap()["error"]) != errCaptureCountStopped.Error() {
		t.Fatalf("ERRORs %v; want one saying the final read was not read, for %q", errs, errCaptureCountStopped)
	}
	if w.String() != "tainted=[] unverified=[]" {
		t.Fatalf("%s; want test-1, which was checked, neither tainted nor unverified", w)
	}
}

// A test case asked for as finish began (a consumer of a record drain that
// gave up saves one after finish's wait), and a final read asked for that no
// read answered, are not verified: unknown is not none. No order a test can
// set from outside puts an ask between finish's wait and its lock, so this
// leaves the watch as such asks do (their read not yet made) and runs finish.
func TestWhatWasAskedAsTheSwitchsFinishBeganIsNotVerified(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	instr := &countingInstr{answers: []models.CaptureLosses{nil}}
	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	w.mu.Lock()
	w.reads.pending = append(w.reads.pending, "test-2")
	w.reads.finalAsked = true
	w.mu.Unlock()
	if !w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[test-2]" {
		t.Fatalf("%s; want test-2 not verified and the recording failed", w)
	}
	if names, why := notVerified(t, logs); names != "[test-2]" || why != errCaptureCountNotRead.Error() {
		t.Fatalf("named %s for %q; want [test-2] for %q", names, why, errCaptureCountNotRead)
	}
	errs := logs.FilterMessageSnippet("could not read whether its agent captured every call after the last test case was recorded").All()
	if len(errs) != 1 || fmt.Sprint(errs[0].ContextMap()["error"]) != errCaptureCountNotRead.Error() {
		t.Fatalf("ERRORs %v; want one saying the final read was not read, for %q", errs, errCaptureCountNotRead)
	}
	if instr.reads != 2 {
		t.Fatalf("%d reads; want the base and test-1's: finish made none", instr.reads)
	}
}

// In a native or docker stop the final read is asked before the drain
// saves its last test cases, so their checks come after it: one of those
// that fails leaves its test case not verified, and the final read, which
// answered, stands. The recording fails for that test case alone, and says
// nothing of a final read it did read.
func TestACheckThatFailsAfterTheSwitchsFinalReadLeavesThatReadStanding(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	// The base, test-1's, the final read; test-2's check (the drain's) fails.
	instr := &countingInstr{answers: []models.CaptureLosses{nil}, errAt: map[int]error{4: errors.New("the agent's connection reset")}}
	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	w.afterTestCases(context.Background())
	settle(w)
	w.afterTestCase(context.Background(), "test-2") // saved by the drain
	if !w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[test-2]" {
		t.Fatalf("%s; want test-2 alone not verified and the recording failed", w)
	}
	if names, _ := notVerified(t, logs); names != "[test-2]" {
		t.Fatalf("named %s; want [test-2]", names)
	}
	if n := logs.FilterMessageSnippet("after the last test case was recorded").Len(); n != 0 {
		t.Fatalf("%d lines of the final read, which answered: %v", n, logs.All())
	}
	if instr.reads != 4 {
		t.Fatalf("%d reads; want the base, test-1's, the final read and test-2's", instr.reads)
	}
}

// The interrupt that gave the stop's waits up (a drain's offer answered:
// utils.StopNow) ends a check the agent never answers at once; its test case
// is not verified, saying so.
func TestTheSwitchsCheckEndsWhenTheUserGaveTheStopsWaitsUp(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stoppedRun(t)
	manualCounts(t)
	r := newWedgedReader(2, nil)
	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	w.afterTestCase(context.Background(), "test-1")
	within(t, r.asked, "test-1's check")
	_, withdraw := utils.OfferStopNow() // a drain said another interrupt stops it now
	defer withdraw()
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the press did not answer the drain's offer")
	}
	within(t, r.ended, "the end of the check")
	if !w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[test-1]" {
		t.Fatalf("%s; want test-1 not verified and the recording failed", w)
	}
	if names, why := notVerified(t, logs); names != "[test-1]" || why != errCaptureCountStopped.Error() {
		t.Fatalf("named %s for %q; want [test-1] for %q", names, why, errCaptureCountStopped)
	}
}

// Test cases saved while a check is out are counted by the next one,
// together, and each is charged with what it saw: one failed handshake
// between test-2's and test-3's saves charges both, never neither.
func TestTheSwitchsCheckThatLagsChargesEveryTestCaseItCounts(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	o := &orderedReader{perTest: make(chan struct{}), release: make(chan struct{})}
	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), o)
	w.afterTestCase(context.Background(), "test-1")
	<-o.perTest
	w.afterTestCase(context.Background(), "test-2")
	w.afterTestCase(context.Background(), "test-3")
	close(o.release) // test-1's check: 1; the next, for test-2 and test-3: 2
	if !w.finish("test-set-0", false) || w.String() != "tainted=[test-1,test-2,test-3] unverified=[]" {
		t.Fatalf("%s; want all three tainted", w)
	}
	if o.n.Load() != 3 {
		t.Fatalf("%d reads; want the base, test-1's, and one for test-2 and test-3", o.n.Load())
	}
	if logs.FilterMessageSnippet("the recording is incomplete").Len() != 1 {
		t.Fatalf("logged %v; want one 'incomplete' ERROR", logs.All())
	}
}

// heldReader answers the base read at once, test-1's check (the second read)
// with nothing lost only once released, and every later read with then.
type heldReader struct {
	Instrumentation
	n       atomic.Int32
	perTest chan struct{} // closed as test-1's check is asked
	release chan struct{}
	then    models.CaptureLosses
}

func (h *heldReader) GetCaptureLosses(context.Context) (models.CaptureLosses, error) {
	switch h.n.Add(1) {
	case 1:
		return nil, nil
	case 2:
		close(h.perTest)
		<-h.release
		return nil, nil
	default:
		return h.then, nil
	}
}

// A check that lags counts what leaves a test case without a verdict for
// every test case it counts, as it does a loss: a handshake failed and one
// abandoned while test-2 and test-3 were saved charge both, one judged, the
// other said.
func TestTheSwitchsCheckThatLagsSaysWhatItCannotJudgeForEveryTestCaseItCounts(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &heldReader{perTest: make(chan struct{}), release: make(chan struct{}), then: models.CaptureLosses{
		{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to a:443"},
		{Kind: models.CaptureLossTLSAbandoned, Count: 1, What: "to b:443"},
	}}
	w := watchCaptureLosses(context.Background(), zap.NewNop(), h)
	w.afterTestCase(context.Background(), "test-1")
	<-h.perTest
	w.afterTestCase(context.Background(), "test-2")
	w.afterTestCase(context.Background(), "test-3")
	close(h.release)
	if !w.finish("test-set-0", false) || w.String() != "tainted=[test-2,test-3] unverified=[]" {
		t.Fatalf("%s; want test-2 and test-3 tainted", w)
	}
	if got := fmt.Sprint(w.uncapturable); got != "[test-2 test-3]" {
		t.Fatalf("said %s for the abandoned handshake; want [test-2 test-3]", got)
	}
}

// Once finish has said what it had to, a late ask (a consumer of a record
// drain that gave up) reads nothing, and changes nothing it said.
func TestAnAskAfterTheSwitchsFinishReadsNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	instr := &countingInstr{answers: []models.CaptureLosses{nil, pid(1)}}
	w := watchCaptureLosses(context.Background(), zap.NewNop(), instr)
	if w.finish("test-set-0", false) {
		t.Fatal("a recording with no test case failed")
	}
	w.afterTestCase(context.Background(), "test-1")
	w.afterTestCases(context.Background())
	settle(w)
	if instr.reads != 1 || w.String() != "tainted=[] unverified=[]" {
		t.Fatalf("%d reads, %s; an ask after finish read again", instr.reads, w)
	}
}

// No goroutine of the switch-on watch's outlives its reads.
func TestNoGoroutineOfTheSwitchsWatchOutlivesItsReads(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		record3(t, &countingInstr{answers: []models.CaptureLosses{nil, pid(1)}})
	}
	deadline := time.Now().Add(30 * time.Second) // bounds only goroutines that never end
	for runtime.NumGoroutine() > before+5 {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines after 20 recordings, %d before", runtime.NumGoroutine(), before)
		}
		time.Sleep(time.Millisecond)
	}
}

// silentAgent is a keploy agent's control API, served over HTTP, whose
// GET /capturelosses answers nothing lost but request number wedge, which it
// never answers: it ends only as the request's context does (or the test).
type silentAgent struct {
	srv     *httptest.Server
	wedge   int32
	n       atomic.Int32
	asked   chan struct{} // closed as the wedged request comes
	ended   chan struct{} // closed as it ends
	release chan struct{}
}

func newSilentAgent(t *testing.T, wedge int32) *silentAgent {
	t.Helper()
	a := &silentAgent{wedge: wedge, asked: make(chan struct{}), ended: make(chan struct{}), release: make(chan struct{})}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/agent/capturelosses" {
			http.NotFound(w, req)
			return
		}
		if a.n.Add(1) == a.wedge {
			close(a.asked)
			defer close(a.ended)
			select {
			case <-req.Context().Done():
			case <-a.release:
			}
			return
		}
		_ = json.NewEncoder(w).Encode(models.CaptureLosses{})
	}))
	t.Cleanup(func() { close(a.release); a.srv.Close() })
	return a
}

// wedgedCountInstr is an agent whose count of what it could not capture is
// read through keploy's own agent client (AgentClient.GetCaptureLosses, which
// has no deadline of its own) from a silentAgent, and whose app ends once
// appEnd is closed; its frame streams end when keploy says it is shutting
// down, as an agent's do once it is stopped.
type wedgedCountInstr struct {
	*fakeInstr
	count  *agenthttp.AgentClient
	appEnd chan struct{}
	once   sync.Once
}

func (i *wedgedCountInstr) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	return i.count.GetCaptureLosses(ctx)
}

func (i *wedgedCountInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	select {
	case <-i.appEnd:
	case <-ctx.Done():
	}
	return models.AppError{AppErrorType: models.ErrAppStopped}
}

func (i *wedgedCountInstr) NotifyGracefulShutdown(context.Context) error {
	i.once.Do(func() {
		close(i.mappings)
		close(i.incoming)
		close(i.outgoing)
	})
	return nil
}

// keploy record under the switch, with an agent that never answers the check
// of its first test case: the recording saves every test case the agent
// handed over (the consumer is not held by the check), its stop ends once the
// check is given up on its bound, and it fails, naming the test case it could
// not verify. Before, the consumer waited on the check forever: the test
// cases after it were never saved, and the stop's drain gave them up.
func TestStart_ACheckTheAgentNeverAnswersHoldsNoTestCaseUp(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clk := manualCounts(t)
	agent := newSilentAgent(t, 2) // the base read answers; test-1's check never does
	instr := &wedgedCountInstr{
		fakeInstr: newFakeInstr(),
		count:     agenthttp.New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{SetupOptions: models.SetupOptions{AgentURI: agent.srv.URL + "/agent"}}}),
		appEnd:    make(chan struct{}),
	}
	r := exitCodeRecorder(instr)
	core, logs := observer.New(zapcore.InfoLevel)
	r.logger = zap.New(core)
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	at := time.Now()
	for i := 1; i <= 3; i++ {
		select {
		case instr.incoming <- tcAt(fmt.Sprintf("test-%d", i), at):
		case err := <-done:
			t.Fatalf("Start returned (%v) before test-%d was handed over", err, i)
		case <-time.After(30 * time.Second): // only turns a hang into a failure
			t.Fatalf("test-%d was never taken", i)
		}
		if i == 1 {
			// test-1's check is out, for test-1 alone, before the agent
			// hands over the next.
			within(t, agent.asked, "test-1's check")
		}
	}
	db := r.testDB.(*recTestDB)
	deadline := time.Now().Add(30 * time.Second) // bounds only a consumer that is held
	for {
		db.mu.Lock()
		n := len(db.inserted)
		db.mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("saved %d of 3 test cases while test-1's check was out: the consumer waits on it", n)
		}
		time.Sleep(time.Millisecond)
	}
	from := logs.Len()  // the base read's lines, if it said any, are before
	close(instr.appEnd) // the app ends: the stop
	saidTheBound(t, logs, from)
	giveUp(t, clk)
	within(t, agent.ended, "test-1's check, given up on its bound")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return once the check was given up on its bound")
	}
	if utils.ErrCode != utils.ExitKeployError {
		t.Fatalf("exit code %d, want %d: a test case keploy could not verify fails the recording", utils.ErrCode, utils.ExitKeployError)
	}
	if names, why := notVerified(t, logs); names != "[test-1]" || why != errCaptureCountSilent.Error() {
		t.Fatalf("named %s for %q; want [test-1] for %q", names, why, errCaptureCountSilent)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if got := strings.Join(db.inserted, ","); got != "test-1,test-2,test-3" || len(db.deleted) != 0 {
		t.Fatalf("saved [%s], deleted %v; want every test case kept", got, db.deleted)
	}
}

// stopHeldAgent is a keploy agent the stop reached too, as CI's
// `sudo pkill -INT -f keploy`, or a Ctrl+C in a terminal, reaches it: it
// holds its own stop only while this recording holds its frame streams open
// (routes.HoldStop). Asked to end them (EndRecordingStreams), it ends each at
// its marker, and goes: a read of its count asked from then on is refused (its
// HTTP server shuts down), and the one it was answering is cut (its process
// exits: Shutdown waits for no read past its own short grace). It says it
// still holds something from before the stop, its capture moving (with stuck,
// not moving), until clear is set, and holds read number slow until released.
// refused counts the reads it refused or cut.
type stopHeldAgent struct {
	*endingAgent
	appEnd   chan struct{}
	stuck    atomic.Bool
	slow     int32
	n        atomic.Int32
	answered atomic.Int32
	asked    chan struct{} // closed as read slow comes
	release  chan struct{}
	looks    atomic.Uint64
	gone     atomic.Bool
	refused  atomic.Int32
	endOnce  sync.Once
	ended    chan struct{} // closed once it has ended its streams
}

func newStopHeldAgent(slow int32) *stopHeldAgent {
	return &stopHeldAgent{endingAgent: newEndingAgent(), appEnd: make(chan struct{}), slow: slow,
		asked: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{})}
}

func (a *stopHeldAgent) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	if a.gone.Load() {
		a.refused.Add(1)
		return nil, errors.New("failed to ask the agent for its capture losses: dial tcp 127.0.0.1:43900: connect: connection refused")
	}
	if a.n.Add(1) == a.slow {
		close(a.asked)
		select {
		case <-a.release:
		case <-a.ended: // it went with this read out
			a.refused.Add(1)
			return nil, errors.New("failed to ask the agent for its capture losses: read tcp 127.0.0.1:52114->127.0.0.1:43900: read: connection reset by peer")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	a.answered.Add(1)
	return nil, nil
}

func (a *stopHeldAgent) BacklogBefore(ctx context.Context, before time.Time) (models.AgentBacklog, error) {
	if !a.clear.Load() {
		// Still parsing what it captured before the stop: its capture
		// moves, so the drain waits for it however long the test takes,
		// unless it is stuck.
		progress := a.looks.Add(1)
		if a.stuck.Load() {
			progress = 0
		}
		return models.AgentBacklog{Verdict: models.BacklogPending, EndMarkers: true, Progress: progress}, nil
	}
	return a.endingAgent.BacklogBefore(ctx, before)
}

func (a *stopHeldAgent) EndRecordingStreams(ctx context.Context, token string) error {
	a.gone.Store(true) // its stop held for these streams alone
	err := a.endingAgent.EndRecordingStreams(ctx, token)
	a.endOnce.Do(func() {
		a.end(models.TestCaseStream, func() { close(a.incoming) })
		a.end(models.MockStream, func() { close(a.outgoing) })
		a.end(models.MappingStream, func() { close(a.mappings) })
		close(a.ended)
	})
	return err
}

func (a *stopHeldAgent) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	select {
	case <-a.appEnd:
	case <-ctx.Done():
	}
	return models.AppError{AppErrorType: models.ErrAppStopped}
}

// heldForReads is what the stop's frame drains say, once, when they keep the
// agent's streams open for the switch's reads of its count.
const heldForReads = "Still waiting for the keploy agent to say whether it captured every call while this recording ran, before its streams end: an agent the stop reached too stops once they do; each read is given up once keploy has had 1m0s of its own time to run without an answer"

// releaseOnceHeldOrGone releases a's held read once the stop's drains say
// they keep its streams open for it, or once the agent has gone (the
// recording ended its streams with the read out): reports which.
func releaseOnceHeldOrGone(t *testing.T, a *stopHeldAgent, logs *observer.ObservedLogs) (held bool) {
	t.Helper()
	defer close(a.release)
	deadline := time.Now().Add(30 * time.Second) // bounds only a stop that does neither
	for {
		if n := logs.FilterMessage(heldForReads).Len(); n > 0 {
			if e := logs.FilterMessage(heldForReads).All()[0]; e.Level != zapcore.InfoLevel {
				t.Fatalf("said at %v, want Info", e.Level)
			}
			return true
		}
		select {
		case <-a.ended:
			return false
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stop neither held the agent's streams for its read nor ended them: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
}

// startHeld starts a recording under the switch against a, logging at Info,
// once setup (when set) has set its recorder up.
func startHeld(t *testing.T, a Instrumentation, cfg *config.Config, setup func(*Recorder)) (*Recorder, *observer.ObservedLogs, <-chan error) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	r := exitCodeRecorder(a)
	if cfg != nil {
		r.config = cfg
	}
	if setup != nil {
		setup(r)
	}
	core, logs := observer.New(zapcore.InfoLevel)
	r.logger = zap.New(core)
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	return r, logs, done
}

// passedWhole waits for Start, and fails the test unless the recording
// passed, with nothing said to be not verified, and no read of the agent's
// count was refused.
func passedWhole(t *testing.T, done <-chan error, logs *observer.ObservedLogs, refused *atomic.Int32) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(60 * time.Second): // only turns a hang into a failure
		t.Fatal("Start never returned")
	}
	if n := logs.FilterMessageSnippet("is not verified").Len(); n != 0 || utils.ErrCode != 0 || refused.Load() != 0 {
		t.Fatalf("exit code %d, %d reads refused, %d 'not verified' ERRORs: a recording that lost nothing failed, a read of it made once the agent had gone: %v",
			utils.ErrCode, refused.Load(), n, logs.FilterLevelExact(zapcore.ErrorLevel).All())
	}
}

// keploy record under the switch, natively, with an agent the stop reached
// too and a check of test-1 slow to come back as the stop begins: the final
// read, asked behind it, is made while the agent is up, because the stop's
// drains hold the agent's streams open (and so its stop) until every read
// asked is answered, saying so, and the recording, which lost nothing,
// passes. Before, the drains ended the streams with the check out; the agent
// went, the final read made after it was refused, and the recording failed
// as not verified after its last test case (exit 1).
func TestStart_TheSwitchsFinalReadIsMadeWhileTheAgentIsHeld(t *testing.T) {
	a := newStopHeldAgent(2) // the base read; test-1's check is slow
	a.clear.Store(true)      // nothing held from before the stop
	_, logs, done := startHeld(t, a, nil, nil)
	select {
	case a.incoming <- tcAt("test-1", time.Now()):
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("test-1 was never taken")
	}
	within(t, a.asked, "test-1's check")
	close(a.appEnd) // the stop, while test-1's check is out
	if !releaseOnceHeldOrGone(t, a, logs) {
		t.Error("the recording ended the agent's streams, and the agent went, with a read of its count out")
	}
	passedWhole(t, done, logs, &a.refused)
	if n := a.answered.Load(); n != 3 {
		t.Fatalf("%d reads answered; want the base, test-1's check and the final read", n)
	}
}

// The stop's drains hold the agent's streams for a read out with none asked
// behind it, as for one with another queued: at a loaded stop the final read
// is most often the only one slow to come back. test-1's check is answered
// before the stop; the final read, slow and out alone, is made while the agent
// is up, and the recording, which lost nothing, passes. Were the drains to
// hold only while a read waits behind the one out, they would end the streams
// with the final read out; the agent would go and cut it, and the recording
// fail as not verified after its last test case (exit 1).
func TestStart_TheSwitchsFinalReadOutAloneIsMadeWhileTheAgentIsHeld(t *testing.T) {
	a := newStopHeldAgent(3) // the base read, test-1's check; the final read, out alone, is slow
	a.clear.Store(true)      // nothing held from before the stop
	_, logs, done := startHeld(t, a, nil, nil)
	select {
	case a.incoming <- tcAt("test-1", time.Now()):
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("test-1 was never taken")
	}
	deadline := time.Now().Add(30 * time.Second) // bounds only a check that never comes back
	for a.answered.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("test-1's check never came back")
		}
		time.Sleep(time.Millisecond)
	}
	close(a.appEnd) // the stop, with no read out
	within(t, a.asked, "the final read")
	if !releaseOnceHeldOrGone(t, a, logs) {
		t.Error("the recording ended the agent's streams, and the agent went, with the final read out alone")
	}
	passedWhole(t, done, logs, &a.refused)
	if n := a.answered.Load(); n != 3 {
		t.Fatalf("%d reads answered; want the base, test-1's check and the final read", n)
	}
}

// A read held out longer than the stop's drain may go without progress
// (teardownDrainStall, of keploy's runnable time) does not get the drain
// given up: each look that holds the agent's streams for it is progress, and
// the read's own bound ends it. The drain says it holds them once. Without
// that the drain was given up while the read was out ("teardown drain made
// no progress"), the agent's streams left to it, and the recording's end
// said to be cut.
func TestStart_AReadOutPastTheDrainsStallDoesNotGiveTheDrainUp(t *testing.T) {
	a := newStopHeldAgent(2) // the base read; test-1's check is slow
	a.clear.Store(true)
	clk := &stall.ManualClock{}
	r, logs, done := startHeld(t, a, nil, func(r *Recorder) { r.drainClock = clk })
	select {
	case a.incoming <- tcAt("test-1", time.Now()):
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("test-1 was never taken")
	}
	within(t, a.asked, "test-1's check")
	close(a.appEnd)                              // the stop, while test-1's check is out
	deadline := time.Now().Add(30 * time.Second) // bounds only a stop that never holds
	for logs.FilterMessage(heldForReads).Len() == 0 {
		select {
		case <-a.ended:
			t.Fatal("the recording ended the agent's streams with a read of its count out")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stop never held the agent's streams for its read: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		// The record-req drain looks at its stall; a look holds the
		// streams since; keploy then runs for the drain's whole bound.
		for clk.Pending() == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the stop's drain never looked at its stall")
			}
			time.Sleep(time.Millisecond)
		}
		for held := r.readHolds.Load(); r.readHolds.Load() == held; {
			if time.Now().After(deadline) {
				t.Fatal("the drain stopped holding the agent's streams with the read out")
			}
			time.Sleep(time.Millisecond)
		}
		clk.Advance(teardownDrainStall)
	}
	if gaveUp(logs, "record-req")() {
		t.Fatal("the drain was given up while it held the agent's streams for a read")
	}
	if n := logs.FilterMessage(heldForReads).Len(); n != 1 {
		t.Fatalf("said %d times that it holds the agent's streams, want once", n)
	}
	close(a.release)
	passedWhole(t, done, logs, &a.refused)
}

// While a read of the agent's count is out, the stop's frame drains hold the
// agent's streams for it before they ask the agent what it still holds from
// before the stop: a look that holds for the read is not one of the looks a
// drain gives the agent's backlog to move (drainStallLooks). An agent slow to
// answer the read is one slow to move its capture too (a starved agent);
// were its backlog looked at meanwhile, the drain would give the backlog up
// while the read was out ("it made no progress"), and a recording that lost
// nothing fail. Once the read is answered, the drain asks, the agent says it
// holds nothing more, and the recording passes.
func TestStart_ADrainHeldForAReadDoesNotCountTheAgentsBacklogAgainstIt(t *testing.T) {
	a := newStopHeldAgent(2) // the base read; test-1's check is slow
	a.stuck.Store(true)      // its capture does not move while the read is out
	r, logs, done := startHeld(t, a, nil, nil)
	select {
	case a.incoming <- tcAt("test-1", time.Now()):
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("test-1 was never taken")
	}
	within(t, a.asked, "test-1's check")
	close(a.appEnd) // the stop, while test-1's check is out
	// Held for far more looks than a drain gives a backlog that does not move:
	// at least one of the two frame drains looked more than drainStallLooks
	// times.
	deadline := time.Now().Add(30 * time.Second) // bounds only a stop that never holds
	for r.readHolds.Load() < 4*drainStallLooks {
		select {
		case <-a.ended:
			t.Fatal("the recording ended the agent's streams with a read of its count out")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stop held the agent's streams for its read only %d times: %v", r.readHolds.Load(), logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	if n := logs.FilterMessageSnippet("it made no progress").Len(); n != 0 {
		t.Fatalf("a drain gave the agent's backlog up while it held the agent's streams for a read: %v", logs.FilterLevelExact(zapcore.ErrorLevel).All())
	}
	a.clear.Store(true) // its capture is through
	close(a.release)
	passedWhole(t, done, logs, &a.refused)
}

// keptAgent is a keploy agent the stop did not reach (keploy's record timer
// ended the recording, or a Ctrl+C that only keploy got): it stays up once
// its streams end, until keploy stops it (the context its Setup got ends, as
// its process, or its container, then does), and from then on refuses every
// read of its count, and fails the one it was answering. It holds nothing
// from before the stop but test-2, captured before it, which it puts on the
// test-case stream when asked to end its streams, before that stream's
// marker; the mapping stream's marker comes only once test-2's check is asked
// (read number slow, held until released), so the stop's drains end with
// that read out.
type keptAgent struct {
	*endingAgent
	appEnd     chan struct{}
	captured   time.Time
	setup      atomic.Pointer[context.Context]
	slow       int32
	n          atomic.Int32
	answered   atomic.Int32
	checkAsked chan struct{} // closed as read slow comes
	release    chan struct{}
	refused    atomic.Int32
	endOnce    sync.Once
}

func newKeptAgent(slow int32) *keptAgent {
	a := &keptAgent{endingAgent: newEndingAgent(), appEnd: make(chan struct{}), captured: time.Now(), slow: slow,
		checkAsked: make(chan struct{}), release: make(chan struct{})}
	a.clear.Store(true)
	return a
}

func (a *keptAgent) Setup(ctx context.Context, _ string, _ models.SetupOptions) error {
	a.setup.Store(&ctx)
	return nil
}

// stopping is what ends once keploy stops the agent.
func (a *keptAgent) stopping() <-chan struct{} {
	return (*a.setup.Load()).Done()
}

// stopped reports whether keploy has stopped the agent.
func (a *keptAgent) stopped() bool {
	select {
	case <-a.stopping():
		return true
	default:
		return false
	}
}

func (a *keptAgent) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	if a.stopped() {
		a.refused.Add(1)
		return nil, errors.New("failed to ask the agent for its capture losses: dial tcp 127.0.0.1:43900: connect: connection refused")
	}
	if a.n.Add(1) == a.slow {
		close(a.checkAsked)
		select {
		case <-a.release:
		case <-a.stopping():
			a.refused.Add(1)
			return nil, errors.New("failed to ask the agent for its capture losses: read tcp 127.0.0.1:43900: connection reset by peer")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	a.answered.Add(1)
	return nil, nil
}

func (a *keptAgent) EndRecordingStreams(ctx context.Context, token string) error {
	err := a.endingAgent.EndRecordingStreams(ctx, token)
	a.endOnce.Do(func() {
		go func() {
			a.incoming <- tcAt("test-2", a.captured)
			a.end(models.TestCaseStream, func() { close(a.incoming) })
			a.end(models.MockStream, func() { close(a.outgoing) })
			<-a.checkAsked
			a.end(models.MappingStream, func() { close(a.mappings) })
		}()
	})
	return err
}

func (a *keptAgent) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	select {
	case <-a.appEnd:
	case <-ctx.Done():
	}
	return models.AppError{AppErrorType: models.ErrAppStopped}
}

// A read still out once the stop's frame drains are over (the check of a
// test case the agent put on its stream as it ended it) is waited for before
// keploy stops the agent, saying so with its bound, and is answered: the
// recording, which lost nothing, passes. Stopped with the read out, the agent
// fails it, and the test case it checks is named not verified (exit 1).
func TestStart_AReadOutAsTheDrainsEndIsAnsweredBeforeKeployStopsTheAgent(t *testing.T) {
	manualCounts(t)      // no read is given up on its bound here
	a := newKeptAgent(4) // the base read, test-1's check, the final read; test-2's check is slow
	_, logs, done := startHeld(t, a, nil, nil)
	select {
	case a.incoming <- tcAt("test-1", time.Now()):
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("test-1 was never taken")
	}
	deadline := time.Now().Add(30 * time.Second) // bounds only reads that never come
	for a.answered.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("test-1's check never came back")
		}
		time.Sleep(time.Millisecond)
	}
	from := logs.Len() // what the stop says from here
	close(a.appEnd)    // the stop
	within(t, a.checkAsked, "test-2's check")
	const waits = "Still waiting for the keploy agent to say whether it captured every call while this recording ran; Ctrl+C does not cut this short: each read is given up once keploy has had 1m0s of its own time to run without an answer"
	for said := false; !said; {
		if a.stopped() {
			t.Fatal("keploy stopped the agent with test-2's check out")
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stop never said it waits for test-2's check: %v", logs.All()[from:])
		}
		for _, e := range logs.All()[from:] {
			said = said || (e.Message == waits && e.Level == zapcore.InfoLevel)
		}
		time.Sleep(time.Millisecond)
	}
	if a.stopped() {
		t.Fatal("keploy stopped the agent with test-2's check out")
	}
	close(a.release)
	passedWhole(t, done, logs, &a.refused)
	if n := a.answered.Load(); n != 4 {
		t.Fatalf("%d reads answered; want the base, test-1's check, the final read and test-2's check", n)
	}
}

// askedHooks notes each test case once its check is asked (the consumer asks
// before AfterTestCaseInsert).
type askedHooks struct {
	BaseRecordHooks
	asked chan string
}

func (h askedHooks) AfterTestCaseInsert(_ context.Context, info *TestCaseContext) error {
	h.asked <- info.TestCase.Name
	return nil
}

// The test cases the stop's drain saves are checked while the agent is up
// too: test-3's check, asked while test-2's (slow to come back) is out, is
// made before the drains let the agent's streams end, and the recording
// passes. Before, the drain ended them once the agent said it held nothing
// more; the agent went, and test-3's check, made after test-2's came back,
// was refused: test-3 was named not verified, and the recording failed.
func TestStart_TheSwitchsChecksOfTheDrainsTestCasesAreMadeWhileTheAgentIsHeld(t *testing.T) {
	a := newStopHeldAgent(4) // the base read, test-1's check, the final read; test-2's check is slow
	hooks := askedHooks{asked: make(chan string, 8)}
	_, logs, done := startHeld(t, a, nil, func(r *Recorder) { r.hooks = hooks })
	captured := time.Now()
	send := func(name string) {
		t.Helper()
		select {
		case a.incoming <- tcAt(name, captured):
		case <-time.After(30 * time.Second): // only turns a hang into a failure
			t.Fatalf("%s was never taken", name)
		}
		select {
		case got := <-hooks.asked:
			if got != name {
				t.Fatalf("asked for %s's check, want %s's", got, name)
			}
		case <-time.After(30 * time.Second): // only turns a hang into a failure
			t.Fatalf("%s's check was never asked", name)
		}
	}
	send("test-1")
	deadline := time.Now().Add(30 * time.Second) // bounds only reads that never come
	for a.answered.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("test-1's check never came back")
		}
		time.Sleep(time.Millisecond)
	}
	close(a.appEnd) // the stop
	for a.answered.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the final read never came back")
		}
		time.Sleep(time.Millisecond)
	}
	// What the agent still held from before the stop, saved by the drain.
	send("test-2")
	within(t, a.asked, "test-2's check")
	send("test-3") // its check waits for test-2's
	a.clear.Store(true)
	if !releaseOnceHeldOrGone(t, a, logs) {
		t.Error("the recording ended the agent's streams, and the agent went, with a read of its count out")
	}
	passedWhole(t, done, logs, &a.refused)
	if n := a.answered.Load(); n != 5 {
		t.Fatalf("%d reads answered; want the base, the final read and test-1's, test-2's and test-3's checks", n)
	}
}

// goingComposeAgent is a stopHeldAgent docker compose stops with the app's
// project (the app exited on its own): it says so (WatchAgentStop), waits for
// this recording's drain, and goes once its streams end.
type goingComposeAgent struct {
	*stopHeldAgent
	compose *stoppingComposeAgent
}

func (g *goingComposeAgent) EndComposeAgent(release bool) { g.compose.EndComposeAgent(release) }
func (g *goingComposeAgent) WatchAgentStop(ctx context.Context) <-chan struct{} {
	return g.compose.WatchAgentStop(ctx)
}
func (g *goingComposeAgent) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	return g.compose.Run(ctx, opts)
}

// Under docker compose an agent compose stops with the project waits for
// this recording's drain and goes once its streams end: the final read,
// asked behind a check slow to come back, is made before the drain lets them
// end, and the recording, which lost nothing, passes. Before, it was made
// once the drain had ended them, the agent gone, and was refused (exit 1).
func TestStart_TheSwitchsFinalReadIsMadeBeforeAComposeAgentGoes(t *testing.T) {
	a := newStopHeldAgent(2) // the base read; test-1's check is slow
	compose := &stoppingComposeAgent{endingAgent: a.endingAgent, stopping: make(chan struct{}), stackDown: make(chan struct{}), ending: make(chan struct{})}
	g := &goingComposeAgent{stopHeldAgent: a, compose: compose}
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // the compose agent's health check
	}))
	defer health.Close()
	cfg := &config.Config{}
	cfg.CommandType = string(utils.DockerCompose)
	cfg.Agent.AgentURI = health.URL
	_, logs, done := startHeld(t, g, cfg, nil)
	select {
	case a.incoming <- tcAt("test-1", time.Now()):
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("test-1 was never taken")
	}
	within(t, a.asked, "test-1's check")
	a.clear.Store(true)
	close(compose.stopping) // compose stops the agent, which waits for the drain
	if !releaseOnceHeldOrGone(t, a, logs) {
		t.Error("the recording ended the agent's streams, and the agent went, with a read of its count out")
	}
	within(t, compose.ending, "the drained agent seen to its end")
	close(compose.stackDown)
	passedWhole(t, done, logs, &a.refused)
}

// lateWordComposeAgent is a stopHeldAgent docker compose runs, which the stop
// signal reached too (CI's `sudo pkill -INT -f keploy` matches the agent in
// its container), but whose word that it did (WatchAgentStop) comes only
// after keploy's own stop has begun its drain (a starved agent): keploy stops
// the app itself (StopComposeApp, under the switch) and drains the agent from
// its own stop. The agent, its stop held only for those streams, goes once
// they end.
type lateWordComposeAgent struct {
	*stopHeldAgent
	appStopped atomic.Bool
	// seenToEnd: the recording saw the agent to its end (EndComposeAgent),
	// which only a drain the agent's word began does.
	seenToEnd atomic.Bool
}

func (l *lateWordComposeAgent) StopComposeApp(context.Context) (string, error) {
	l.appStopped.Store(true)
	return "", nil
}

func (l *lateWordComposeAgent) WatchAgentStop(context.Context) <-chan struct{} {
	return make(chan struct{}) // too late: keploy's own stop drains first
}

func (l *lateWordComposeAgent) EndComposeAgent(bool) { l.seenToEnd.Store(true) }

// Under docker compose, when keploy's own stop drains an agent the stop
// reached too, the final read, asked behind a check slow to come back, is
// made before the drain lets the agent's streams end, and the recording,
// which lost nothing, passes. Before, the reads of that stop were asked only
// once the drain had ended the streams: the agent had gone, the final read
// was refused, and the recording failed as not verified after its last test
// case (exit 1).
func TestStart_TheSwitchsFinalReadIsMadeBeforeKeploysOwnStopLetsAComposeAgentGo(t *testing.T) {
	a := newStopHeldAgent(2) // the base read; test-1's check is slow
	a.clear.Store(true)      // nothing held from before the stop
	l := &lateWordComposeAgent{stopHeldAgent: a}
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // the compose agent's health check
	}))
	defer health.Close()
	t.Setenv(provenswitch.Env, "1")
	r := exitCodeRecorder(l)
	r.config.CommandType = string(utils.DockerCompose)
	r.config.Agent.AgentURI = health.URL
	core, logs := observer.New(zapcore.InfoLevel)
	r.logger = zap.New(core)
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	select {
	case a.incoming <- tcAt("test-1", time.Now()):
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("test-1 was never taken")
	}
	within(t, a.asked, "test-1's check")
	stop() // keploy's stop (its SIGINT), while test-1's check is out
	if !releaseOnceHeldOrGone(t, a, logs) {
		t.Error("the recording ended the agent's streams, and the agent went, with a read of its count out")
	}
	passedWhole(t, done, logs, &a.refused)
	if !l.appStopped.Load() {
		t.Fatal("keploy's stop did not stop the app itself before its drain")
	}
	if l.seenToEnd.Load() {
		t.Fatal("keploy's own stop saw the agent to its end; it takes the stack down, the agent with it")
	}
	if n := a.answered.Load(); n != 3 {
		t.Fatalf("%d reads answered; want the base, test-1's check and the final read", n)
	}
}

// A read made after a final read that failed answers for it: in a native or
// docker stop the drain saves its last test cases after the final read was
// asked, and the check of one of them counts everything the agent lost
// before the stop. The recording's end is then verified, and the recording,
// which lost nothing, passes.
func TestACheckAfterTheSwitchsFailedFinalReadAnswersForIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	// The base, test-1's check; the final read fails; test-2's check (the
	// drain's) answers.
	instr := &countingInstr{answers: []models.CaptureLosses{nil}, errAt: map[int]error{3: errors.New("the agent's connection reset")}}
	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	w.afterTestCases(context.Background())
	settle(w)
	w.afterTestCase(context.Background(), "test-2") // saved by the drain
	if w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[]" {
		t.Fatalf("%s; want both test cases checked, and the recording passed: %v", w, logs.All())
	}
	if logs.Len() != 0 {
		t.Fatalf("logged %v; want nothing: test-2's check answered for the final read", logs.All())
	}
	if instr.reads != 4 {
		t.Fatalf("%d reads; want the base, test-1's, the final read and test-2's", instr.reads)
	}
}

// slowSaveDB is a test store whose insert of test case slow, once entered,
// waits until release: a consumer slow to save what the stop's drain handed
// it (a disk stalled under load). With fail set, that insert then fails.
type slowSaveDB struct {
	*recTestDB
	slow    string
	fail    bool
	entered chan struct{}
	release chan struct{}
}

func newSlowSaveDB(slow string) *slowSaveDB {
	return &slowSaveDB{recTestDB: &recTestDB{}, slow: slow, entered: make(chan struct{}), release: make(chan struct{})}
}

func (d *slowSaveDB) InsertTestCase(ctx context.Context, tc *models.TestCase, testSetID string, enableLog bool) error {
	if tc.Name == d.slow {
		close(d.entered)
		<-d.release
		if d.fail {
			return errors.New("no space left on device")
		}
	}
	return d.recTestDB.InsertTestCase(ctx, tc, testSetID, enableLog)
}

// stopWithSlowSave records test-1, stops, and once the final read has come
// back hands the drain test-2, which the agent held from before the stop and
// whose save is slow (db), and has the agent say it holds nothing more.
func stopWithSlowSave(t *testing.T, a *stopHeldAgent, db *slowSaveDB) {
	t.Helper()
	captured := time.Now()
	send := func(name string) {
		t.Helper()
		select {
		case a.incoming <- tcAt(name, captured):
		case <-time.After(30 * time.Second): // only turns a hang into a failure
			t.Fatalf("%s was never taken", name)
		}
	}
	waitAnswered := func(n int32, what string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second) // bounds only reads that never come
		for a.answered.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%s never came back", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	send("test-1")
	waitAnswered(2, "test-1's check")
	close(a.appEnd) // the stop
	waitAnswered(3, "the final read")
	send("test-2") // saved by the drain
	within(t, db.entered, "test-2's save")
	a.clear.Store(true)
}

// The check of a test case the stop's drain saves is made while the agent is
// up however long its save takes: the drain hands it to the consumer, and,
// its stream quiet and the agent holding nothing more, holds the agent's
// streams open until the consumer has asked its check. The recording, which
// lost nothing, passes. Before, the drain held them only for a read already
// asked: with test-2 still being saved it ended them, the agent went, and
// test-2's check, asked once its save was through, was refused; test-2 was
// named not verified, and the recording failed (exit 1).
func TestStart_TheSwitchsCheckOfATestCaseSavedSlowlyIsMadeWhileTheAgentIsHeld(t *testing.T) {
	a := newStopHeldAgent(0)
	db := newSlowSaveDB("test-2")
	r, logs, done := startHeld(t, a, nil, func(r *Recorder) { r.testDB = db })
	stopWithSlowSave(t, a, db)
	deadline := time.Now().Add(30 * time.Second) // bounds only a drain that never looks
	heldFor := func(n uint64) {
		t.Helper()
		for r.checkHolds.Load() < n {
			select {
			case <-a.ended:
				t.Fatal("the recording ended the agent's streams, and the agent went, with test-2's check yet to be asked")
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("the stop's drain never held the agent's streams for test-2's check: %v", logs.All())
			}
			time.Sleep(time.Millisecond)
		}
	}
	heldFor(r.checkHolds.Load() + 1)
	// A look held for the save alone: it found every read through, and none
	// is asked again until test-2's check, once its save is (a look before
	// may have held for the final read, slow to come back, and said so).
	from := logs.Len()
	heldFor(r.checkHolds.Load() + 2)
	for _, e := range logs.All()[from:] {
		if e.Message == heldForReads {
			t.Fatalf("said, at a look that held only for test-2's save, that it waits for the agent's reads: %v", logs.All()[from:])
		}
	}
	close(db.release)
	passedWhole(t, done, logs, &a.refused)
	if n := a.answered.Load(); n != 4 {
		t.Fatalf("%d reads answered; want the base, test-1's check, the final read and test-2's check", n)
	}
}

// A saved test case stays counted as one still to be checked
// (uncheckedCount) until its read is out: the consumer takes it off once
// the watch has asked it (countReads.askTest), so a drain that looks while
// the check is being asked holds the agent's streams for it, and the check
// is made while the agent is up: the recording, which lost nothing, passes.
// Were the consumer to take it off before the ask, a drain looking in
// between would find nothing unchecked and no read out, and end the
// streams; the agent would go, the check be refused, and test-2 be named not
// verified (exit 1).
func TestStart_ADrainThatLooksAsACheckIsAskedHoldsTheAgentsStreamsForIt(t *testing.T) {
	a := newStopHeldAgent(0)
	asking, release := make(chan struct{}), make(chan struct{})
	askingCheck = func(name string) {
		if name == "test-2" {
			close(asking)
			<-release
		}
	}
	t.Cleanup(func() { askingCheck = nil })
	var releaseOnce sync.Once
	letAsk := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letAsk) // a test that failed with the ask held lets it go
	r, logs, done := startHeld(t, a, nil, nil)
	captured := time.Now()
	send := func(name string) {
		t.Helper()
		select {
		case a.incoming <- tcAt(name, captured):
		case <-time.After(30 * time.Second): // only turns a hang into a failure
			t.Fatalf("%s was never taken", name)
		}
	}
	deadline := time.Now().Add(30 * time.Second) // bounds only what never comes
	waitAnswered := func(n int32, what string) {
		t.Helper()
		for a.answered.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%s never came back", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	send("test-1")
	waitAnswered(2, "test-1's check")
	close(a.appEnd) // the stop
	waitAnswered(3, "the final read")
	send("test-2") // saved by the drain; its check is being asked
	within(t, asking, "test-2's check, being asked")
	a.clear.Store(true) // the agent holds nothing more
	// Two looks that held the streams for test-2 alone: every read through,
	// and test-2's check not yet asked.
	for held := r.checkHolds.Load(); r.checkHolds.Load() < held+2; {
		select {
		case <-a.ended:
			t.Fatal("the recording ended the agent's streams, and the agent went, with test-2's check being asked")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stop's drain never held the agent's streams for test-2's check: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	letAsk()
	passedWhole(t, done, logs, &a.refused)
	if n := a.answered.Load(); n != 4 {
		t.Fatalf("%d reads answered; want the base, test-1's check, the final read and test-2's check", n)
	}
}

// tokenReader answers each read of the agent's count, nothing lost, once it
// is handed a token on answer, or as its context ends.
type tokenReader struct{ answer chan struct{} }

func (r *tokenReader) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	select {
	case <-r.answer:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// heldChecks is the reads of a watch whose reader answers a read only once
// handed a token, holding the test cases handed over whose check they are
// yet to ask (unchecked).
func heldChecks(t *testing.T) (*countReads, *tokenReader, *uncheckedCount) {
	t.Helper()
	reader := &tokenReader{answer: make(chan struct{})}
	reads := newCountReads(context.Background(), zap.NewNop(), reader, &sync.Mutex{}, errCaptureCountSilent, "", "", func(countRead) {})
	unchecked := new(uncheckedCount)
	reads.holdFor(unchecked)
	t.Cleanup(func() { close(reader.answer) }) // a read a failed test left out ends
	return reads, reader, unchecked
}

// What the stop's frame drains hold the agent's streams for, as one look
// sees it (countReads.holds): a test case handed over is unchecked until its
// save fails or its check's read is out, when the consumer takes it off;
// the read out holds them until it is answered.
func TestACheckIsTakenOffUncheckedAsItsReadIsAsked(t *testing.T) {
	reads, reader, unchecked := heldChecks(t)
	look := func(wantOut, wantUnchecked bool, when string) {
		t.Helper()
		if out, u := reads.holds(); out != wantOut || u != wantUnchecked {
			t.Fatalf("%s: a look found a read out %v, a test case unchecked %v; want %v, %v", when, out, u, wantOut, wantUnchecked)
		}
	}
	look(false, false, "nothing handed over")
	unchecked.handed()
	unchecked.handed()
	look(false, true, "test-1 and test-2 handed over")
	unchecked.settled() // test-2's save failed
	look(false, true, "test-1 still being saved")
	reads.askTest("test-1")
	look(true, true, "test-1's check asked, the consumer yet to take it off")
	unchecked.settled() // as the consumer does once the ask returns
	look(true, false, "test-1's check asked")
	reader.answer <- struct{}{}
	if !reads.wait(false) {
		t.Fatal("the wait for test-1's check did not see it answered")
	}
	look(false, false, "test-1's check answered")
}

// No look of the stop's drains finds a test case's check taken off unchecked
// with its read not yet out: askTest puts the read out under the lock a look
// is made under, and the consumer takes the test case off after it returns.
// A drain that looks without pause sees each test case handed over
// unchecked, or its check's read out, until the read is answered. Were the
// test case taken off first, a look in between would find neither, and end
// the agent's streams with the check yet to be made.
func TestNoLookFindsACheckTakenOffBeforeItsReadIsOut(t *testing.T) {
	reads, reader, unchecked := heldChecks(t)
	const checks = 500
	for i := 1; i <= checks; i++ {
		unchecked.handed()
		var looks atomic.Int64
		var stop, found atomic.Bool
		looked := make(chan struct{})
		go func() {
			defer close(looked)
			for !stop.Load() {
				if out, u := reads.holds(); !out && !u {
					found.Store(true)
				}
				looks.Add(1)
			}
		}()
		for looks.Load() == 0 { // the drain is looking as the check is asked
			runtime.Gosched()
		}
		reads.askTest(fmt.Sprintf("test-%d", i))
		// As the consumer does, once the ask has returned.
		unchecked.settled()
		for n := looks.Load(); looks.Load() == n; { // and looks once it is
			runtime.Gosched()
		}
		stop.Store(true)
		<-looked
		if found.Load() {
			t.Fatalf("test-%d: a look found it off unchecked with its check's read not out", i)
		}
		reader.answer <- struct{}{}
		if !reads.wait(false) {
			t.Fatalf("the wait for test-%d's check did not see it answered", i)
		}
	}
}

// A test case the drain handed whose save fails has no check to wait for:
// the drain ends the agent's streams once the save has failed, rather than
// holding them for a check that never comes until the stop's drain is given
// up on its stall (which this test's drain clock never lets happen).
func TestStart_ATestCaseTheDrainCouldNotSaveHoldsNoStream(t *testing.T) {
	a := newStopHeldAgent(0)
	db := newSlowSaveDB("test-2")
	db.fail = true
	r, logs, done := startHeld(t, a, nil, func(r *Recorder) {
		r.testDB = db
		r.drainClock = &stall.ManualClock{}
	})
	stopWithSlowSave(t, a, db)
	held := r.checkHolds.Load()
	deadline := time.Now().Add(30 * time.Second) // bounds only a drain that never looks
	for r.checkHolds.Load() < held+1 {
		if time.Now().After(deadline) {
			t.Fatalf("the stop's drain never held the agent's streams for test-2's save: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	close(db.release)
	within(t, a.ended, "the agent's streams ended once test-2's save failed")
	select {
	case <-done:
	case <-time.After(60 * time.Second): // only turns a hang into a failure
		t.Fatal("Start never returned")
	}
	if gaveUp(logs, "record-req")() {
		t.Fatal("the stop's drain was given up")
	}
	if n := a.refused.Load(); n != 0 {
		t.Fatalf("%d reads refused; none was asked once the agent had gone", n)
	}
	if n := a.answered.Load(); n != 3 {
		t.Fatalf("%d reads answered; want the base, test-1's check and the final read: test-2 was not saved", n)
	}
}

// baseReadAgent is a stopHeldAgent whose first read of its count, the
// switch's base read, fails with base.
type baseReadAgent struct {
	*stopHeldAgent
	base  error
	first atomic.Bool
}

func (b *baseReadAgent) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	if b.first.CompareAndSwap(false, true) {
		return nil, b.base
	}
	return b.stopHeldAgent.GetCaptureLosses(ctx)
}

// Under the switch, a recording whose watch asks no check, because its base
// read failed or its agent cannot count (models.ErrCaptureLossesUnsupported),
// holds the agent's streams for none: the consumer takes every test case it
// saves off unchecked, whatever the watch did with its check, so the stop's
// drains end the streams once they are through, and no test case is left
// counted. A base read refused fails the recording, test-1 and its end not
// verified, for the agent's reason; an agent that cannot count passes it,
// unchecked. Were a saved test case left counted, a drain that looked at the
// count would hold the agent's streams for a check that never comes, until
// the stop gave it up on its stall (this test's drain clock never does).
func TestStart_AWatchThatAsksNoCheckHoldsNoStream(t *testing.T) {
	refused := errors.New("failed to ask the agent for its capture losses: unexpected status 500")
	for _, c := range []struct {
		name string
		base error
	}{
		{"its base read refused", refused},
		{"an agent that cannot count", models.ErrCaptureLossesUnsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := &baseReadAgent{stopHeldAgent: newStopHeldAgent(0), base: c.base}
			r, logs, done := startHeld(t, a, nil, func(r *Recorder) { r.drainClock = &stall.ManualClock{} })
			select {
			case a.incoming <- tcAt("test-1", time.Now()):
			case <-time.After(30 * time.Second): // only turns a hang into a failure
				t.Fatal("test-1 was never taken")
			}
			close(a.appEnd) // the stop
			a.clear.Store(true)
			within(t, a.ended, "the agent's streams ended, with no check to make")
			select {
			case <-done:
			case <-time.After(60 * time.Second): // only turns a hang into a failure
				t.Fatal("Start never returned")
			}
			if gaveUp(logs, "record-req")() {
				t.Fatal("the stop's drain was given up")
			}
			if r.unchecked.Load().any() {
				t.Fatal("test-1, saved, is still counted as a test case whose check is yet to be asked; none is asked")
			}
			if n := a.answered.Load() + a.refused.Load(); n != 0 {
				t.Fatalf("%d reads after the base read; none is made once it failed", n)
			}
			db := r.testDB.(*recTestDB)
			db.mu.Lock()
			saved := strings.Join(db.inserted, ",")
			db.mu.Unlock()
			if saved != "test-1" {
				t.Fatalf("saved [%s]; want test-1", saved)
			}
			if c.base == refused {
				if utils.ErrCode != utils.ExitKeployError {
					t.Fatalf("exit code %d, want %d: a test case keploy could not verify fails the recording", utils.ErrCode, utils.ExitKeployError)
				}
				if names, why := notVerified(t, logs); names != "[test-1]" || why != refused.Error() {
					t.Fatalf("named %s for %q; want [test-1] for %q", names, why, refused)
				}
				return
			}
			if utils.ErrCode != 0 || logs.FilterMessageSnippet("is not verified").Len() != 0 {
				t.Fatalf("exit code %d, %v; an agent that cannot count leaves the recording unchecked, not failed", utils.ErrCode, logs.FilterLevelExact(zapcore.ErrorLevel).All())
			}
		})
	}
}

// With the switch off nothing of this holds the agent's streams: the stop's
// drain ends them with a test case's save still out, as it always has.
func TestStart_WithTheSwitchOffTheDrainDoesNotWaitForATestCasesSave(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	a := newStopHeldAgent(0)
	db := newSlowSaveDB("test-2")
	r := exitCodeRecorder(a)
	r.testDB = db
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	done := make(chan error, 1)
	go func() { done <- r.Start(context.Background()) }()
	captured := time.Now()
	select {
	case a.incoming <- tcAt("test-1", captured):
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("test-1 was never taken")
	}
	close(a.appEnd)
	select {
	case a.incoming <- tcAt("test-2", captured):
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("test-2 was never taken")
	}
	within(t, db.entered, "test-2's save")
	a.clear.Store(true)
	within(t, a.ended, "the agent's streams ended with test-2's save out")
	close(db.release)
	select {
	case <-done:
	case <-time.After(60 * time.Second): // only turns a hang into a failure
		t.Fatal("Start never returned")
	}
	// The consumer takes each test case off unchecked as it is through with
	// it, the switch off too: none is left counted.
	if r.unchecked.Load().any() {
		t.Fatal("a test case the consumer saved is still counted as one whose check is yet to be asked")
	}
}

// A save wedged past the stop drain's stall still gets the drain given up:
// a look that holds the agent's streams only for a test case still being
// saved is not progress (the consumer's hand-overs are), so a store that
// never comes back cannot hold keploy record's stop forever. The recording's
// end is said to be cut, as for any drain given up; once the save does come
// back, its check is still made while the agent is up.
func TestStart_ASaveWedgedPastTheDrainsStallGetsTheDrainGivenUp(t *testing.T) {
	a := newStopHeldAgent(0)
	db := newSlowSaveDB("test-2")
	clk := &stall.ManualClock{}
	r, logs, done := startHeld(t, a, nil, func(r *Recorder) {
		r.testDB = db
		r.drainClock = clk
	})
	stopWithSlowSave(t, a, db)
	deadline := time.Now().Add(30 * time.Second) // bounds only a drain never given up
	for advances := 0; !gaveUp(logs, "record-req")(); {
		if time.Now().After(deadline) {
			t.Fatalf("the stop's drain was not given up after %d of its stall bounds held only for a save", advances)
		}
		// The record-req drain has looked at its progress and waits again
		// (Advance fires its timer, and it asks for the next once it has
		// looked). A look of the frame drain that holds the agent's streams
		// for test-2's save comes after that, and keploy then runs for the
		// drain's whole bound: were such a look progress, the drain would
		// see it, and never be given up.
		if clk.Pending() == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		for since := r.checkHolds.Load(); r.checkHolds.Load() == since; {
			if time.Now().After(deadline) {
				t.Fatalf("the stop's drain was not given up after %d of its stall bounds held only for a save", advances)
			}
			time.Sleep(time.Millisecond)
		}
		clk.Advance(teardownDrainStall)
		advances++
	}
	select {
	case <-a.ended:
		t.Fatal("the agent's streams ended with test-2's check yet to be asked")
	default:
	}
	close(db.release)
	select {
	case <-done:
	case <-time.After(60 * time.Second): // only turns a hang into a failure
		t.Fatal("Start never returned")
	}
	if why := r.endCut.Load(); why == nil || !strings.Contains(*why, "made no progress") {
		t.Fatalf("the recording's end was not said to be cut (%v); its drain was given up", why)
	}
	if n := a.refused.Load(); n != 0 {
		t.Fatalf("%d reads refused; test-2's check was to be made while the agent was up", n)
	}
}

// A recorder can be started again (Start), and what an earlier recording
// left of its stop's holds is not this one's: a test case its consumer was
// still saving when it ended (its drain given up on a stall), and its word
// that the drains held the agent's streams for a read. This recording's
// drains hold the agent's streams for its own read alone, say so, and end
// them once it is answered. Were the earlier test case counted, they would
// hold the streams for a check that never comes until the stop gave them up
// (this test's drain clock never does); were the earlier word kept, this
// stop's hold would go unsaid.
func TestStart_ARecorderStartedAgainHoldsTheAgentsStreamsForItsOwnReadsAlone(t *testing.T) {
	a := newStopHeldAgent(2) // the base read; test-1's check is slow
	a.clear.Store(true)      // nothing held from before the stop
	_, logs, done := startHeld(t, a, nil, func(r *Recorder) {
		earlier := new(uncheckedCount)
		earlier.handed() // the earlier recording's consumer, still saving
		r.unchecked.Store(earlier)
		r.readHoldSaid.Store(true) // the earlier recording's stop said it held
		r.drainClock = &stall.ManualClock{}
	})
	select {
	case a.incoming <- tcAt("test-1", time.Now()):
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("test-1 was never taken")
	}
	within(t, a.asked, "test-1's check")
	close(a.appEnd) // the stop, while test-1's check is out
	if !releaseOnceHeldOrGone(t, a, logs) {
		t.Fatal("the recording ended the agent's streams, and the agent went, with a read of its count out")
	}
	within(t, a.ended, "the agent's streams ended once this recording's reads were answered")
	passedWhole(t, done, logs, &a.refused)
}
