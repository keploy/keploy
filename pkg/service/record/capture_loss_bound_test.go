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
