package utils

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// drainStopNowTest readies a test of a teardown drain's offer to end at the
// user's next Ctrl+C: the run interrupted (the stop's first signal), a short
// notice period, and the interrupts and the period put back at the end.
func drainStopNowTest(t *testing.T) {
	t.Helper()
	drainStopNowTestOnItsOwn(t)
	MarkInterrupted()
}

// drainStopNowTestOnItsOwn is drainStopNowTest for a stop that began on its
// own (the app exited, a record timer): the run not interrupted yet.
func drainStopNowTestOnItsOwn(t *testing.T) {
	t.Helper()
	awaitDrainsEnded(t)
	notice := drainNotice
	drainNotice = 5 * time.Millisecond
	ClearInterrupted()
	ResetInterruptedAgain()
	t.Cleanup(func() { awaitDrainsEnded(t); drainNotice = notice; ClearInterrupted(); ResetInterruptedAgain() })
}

// drainsRunning counts the drains rows have started (startCarriedDrain)
// that have not returned yet.
var drainsRunning sync.WaitGroup

// drainOutlivedItsRow is set once a drain was still running a minute after
// its row and its group ended.
var drainOutlivedItsRow atomic.Bool

// awaitDrainsEnded waits until every drain a row started has returned, its
// offer withdrawn, before the interrupts are reset. A row's group ends at
// the latest as the row ends (holdGroup, groupEndsAtThePress), so its drain
// may still be running then. A withdraw that came after the reset took the
// next row's offer out of the count, and that row's press after its drain's
// offer then ended no stop. A drain that did not end within a minute of its
// group's end fails every later row at once, rather than holding each of
// them a minute more.
func awaitDrainsEnded(t *testing.T) {
	t.Helper()
	if drainOutlivedItsRow.Load() {
		t.Error("a drain an earlier row started never ended after its group did")
		return
	}
	ended := make(chan struct{})
	go func() { drainsRunning.Wait(); close(ended) }()
	select {
	case <-ended:
	case <-time.After(time.Minute): // only turns a hang into a failure
		drainOutlivedItsRow.Store(true)
		t.Error("a drain a row started was still running a minute after its row ended")
	}
}

// holdGroup adds to g a goroutine that runs until release is called or the
// row ends, whichever comes first. A row that fails before it ends its group
// thus leaves no drain running past its end.
func holdGroup(t *testing.T, g *errgroup.Group) (release func()) {
	t.Helper()
	block := make(chan struct{})
	release = sync.OnceFunc(func() { close(block) })
	t.Cleanup(release)
	g.Go(func() error { <-block; return nil })
	return release
}

// groupEndsAtThePress adds to g a goroutine that ends at the press that
// ends a stop (StopNow), as record's frame forwarders do, or as the row
// ends, should the row fail before that press.
func groupEndsAtThePress(t *testing.T, g *errgroup.Group) {
	t.Helper()
	rowEnded := make(chan struct{})
	t.Cleanup(func() { close(rowEnded) })
	g.Go(func() error {
		select {
		case <-StopNow():
		case <-rowEnded:
		}
		return nil
	})
}

// drainResult is what a teardown drain returned.
type drainResult struct {
	err error
	end DrainEnd
}

// startDrain runs the teardown drain of g on clk, at bound, logging to an
// observer.
func startDrain(clk stall.Clock, g *errgroup.Group, bound time.Duration, progress func() uint64) (<-chan drainResult, *observer.ObservedLogs) {
	return startCarriedDrain(clk, g, bound, progress, nil)
}

// startCarriedDrain is startDrain for a drain whose offer another wait makes
// until carrier is closed (DrainErrGroupProgressCarried).
func startCarriedDrain(clk stall.Clock, g *errgroup.Group, bound time.Duration, progress func() uint64, carrier <-chan struct{}) (<-chan drainResult, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	res := make(chan drainResult, 1)
	drainsRunning.Add(1)
	go func() {
		defer drainsRunning.Done()
		err, end := drainErrGroupProgress(clk, zap.New(core), "t", g, bound, progress, carrier)
		res <- drainResult{err, end}
	}()
	return res, logs
}

// offersStanding is how many stops offer to end at the next interrupt.
func offersStanding() int {
	repeatInterrupts.mu.Lock()
	defer repeatInterrupts.mu.Unlock()
	return repeatInterrupts.offers
}

// awaitLog waits for a line of logs whose message holds part, while the
// drain has not returned, and returns it.
func awaitLog(t *testing.T, logs *observer.ObservedLogs, part string, res <-chan drainResult) observer.LoggedEntry {
	t.Helper()
	e, _ := awaitLogAfter(t, logs, part, -1, res)
	return e
}

// awaitLogAfter is awaitLog for a line after the after'th (counted from 0),
// and returns its place too.
func awaitLogAfter(t *testing.T, logs *observer.ObservedLogs, part string, after int, res <-chan drainResult) (observer.LoggedEntry, int) {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for {
		for i, e := range logs.All() {
			if i > after && strings.Contains(e.Message, part) {
				return e, i
			}
		}
		select {
		case r := <-res:
			t.Fatalf("the drain returned %+v before it said %q; it said %v", r, part, logs.All())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the drain never said %q; it said %v", part, logs.All())
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitEnd waits for the drain's end, and fails the test if it does not
// come (a minute only turns a hang into a failure).
func awaitEnd(t *testing.T, res <-chan drainResult, what string) drainResult {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(time.Minute):
		t.Fatalf("the drain did not end: %s", what)
	}
	return drainResult{}
}

// notReturned fails the test if the drain has returned.
func notReturned(t *testing.T, res <-chan drainResult, what string) {
	t.Helper()
	select {
	case r := <-res:
		t.Fatalf("the drain returned %+v: %s", r, what)
	default:
	}
}

// awaitLooks waits until progress has been looked at n more times than
// at, the drain's notices each looking once.
func awaitLooks(t *testing.T, looks *atomic.Uint64, at, n uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for looks.Load() < at+n {
		if time.Now().After(deadline) {
			t.Fatal("the drain stopped looking at its progress")
		}
		time.Sleep(time.Millisecond)
	}
}

const drainOffer = "press Ctrl+C again to stop now"

// A teardown drain whose stall verdict cannot come still ends at an event:
// the user's interrupt after its offer. Here keploy's runnable clock is
// frozen (on an overloaded host it moved at a fraction of a percent of wall
// time, and a wedged group's 30 s bound held the stop for hours), and the
// group never ends. The drain says it made no progress, offers, and at the
// press gives the group up, saying the verdict was still pending and how
// much of the bound had passed: none.
func TestATeardownDrainWhoseVerdictCannotComeEndsAtTheUsersInterrupt(t *testing.T) {
	drainStopNowTest(t)
	clk := &stall.ManualClock{}
	var g errgroup.Group
	holdGroup(t, &g)
	var p atomic.Uint64
	const bound = 30 * time.Second
	res, logs := startDrain(clk, &g, bound, p.Load)
	offer, at := awaitLogAfter(t, logs, drainOffer, -1, res)
	if f := offer.ContextMap(); f["group"] != "t" || f["stalledFor"] != time.Duration(0) || f["bound"] != bound {
		t.Fatalf("the offer said %v: want the group, no stall yet (a frozen clock), and the bound", f)
	}
	// Its own offer standing, it says so again at its next notice with no
	// progress.
	awaitLogAfter(t, logs, drainOffer, at, res)
	if !MarkInterruptedAgain() {
		t.Fatal("the user's interrupt after the drain's offer ended no stop")
	}
	var r drainResult
	select {
	case r = <-res:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drain did not end at the user's interrupt after its offer, its verdict pending")
	}
	if r.err != nil || r.end != DrainStopped {
		t.Fatalf("the drain returned %+v: want it given up at the user's interrupt (DrainStopped), with no error", r)
	}
	if MarkInterruptedAgain() {
		t.Fatal("the drain's offer outlived it: a later interrupt ended a stop that never offered")
	}
	gaveUp := logs.FilterMessageSnippet("given up at the user's interrupt").All()
	if len(gaveUp) != 1 {
		t.Fatalf("the drain did not say the user gave it up; it said %v", logs.All())
	}
	if f := gaveUp[0].ContextMap(); f["stalledFor"] != time.Duration(0) || f["bound"] != bound {
		t.Fatalf("the give-up said %v: want no stall yet and the bound", f)
	}
	if n := logs.FilterMessageSnippet("made no progress; forcing shutdown").Len(); n != 0 {
		t.Fatal("the drain named the stall verdict as the cause; it never came")
	}
}

// A teardown drain whose group still makes progress says nothing of a stall
// and offers nothing, however long it takes: the user's interrupt then ends
// nothing, and the drain waits for the group's end. Its progress here moves
// at every look, its clock frozen.
func TestATeardownDrainThatProgressesOffersNoStopNow(t *testing.T) {
	drainStopNowTest(t)
	clk := &stall.ManualClock{}
	var g errgroup.Group
	release := holdGroup(t, &g)
	var p atomic.Uint64
	progress := func() uint64 { return p.Add(1) }
	res, logs := startDrain(clk, &g, time.Second, progress)
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for p.Load() < 20 {                     // the first look, then 19 notices or more
		if time.Now().After(deadline) {
			t.Fatal("the drain stopped looking at its progress")
		}
		time.Sleep(time.Millisecond)
	}
	if MarkInterruptedAgain() {
		t.Fatal("a drain making progress offered to stop now, and the interrupt ended it")
	}
	if n := logs.FilterMessageSnippet("made no progress").Len(); n != 0 {
		t.Fatalf("a drain making progress said it made none: %v", logs.All())
	}
	release()
	select {
	case r := <-res:
		if r.err != nil || r.end != DrainJoined {
			t.Fatalf("the drain returned %+v: want the group's end", r)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drain did not end at its group's end")
	}
}

// The user's interrupt during an agent's stop does not give the drain up
// while the agent's stop holds it (BeginAgentStop): the agent's stop hears
// the interrupt itself. The group's end, meanwhile, is the drain's end.
func TestATeardownDrainHeldByAnAgentStopIsNotGivenUpAtTheInterrupt(t *testing.T) {
	drainStopNowTest(t)
	end := BeginAgentStop()
	t.Cleanup(end)
	clk := &stall.ManualClock{}
	var g errgroup.Group
	release := holdGroup(t, &g)
	var p atomic.Uint64
	res, logs := startDrain(clk, &g, time.Second, p.Load)
	awaitLog(t, logs, drainOffer, res)
	MarkInterruptedAgain()
	awaitLog(t, logs, stopWaitedFor(), res)
	if MarkInterruptedAgain() {
		t.Fatal("the drain's offer stood while the agent's stop held it: the agent's stop makes its own")
	}
	release()
	if r := awaitEnd(t, res, "at its group's end"); r.err != nil || r.end != DrainJoined {
		t.Fatalf("the drain returned %+v: its group ended, want no give-up", r)
	}
}

// A group that ends as the user's interrupt comes is the drain's end, not a
// give-up: a wait that finds both its group's end and the press after its
// offer ends joined, whichever it reads first.
func TestATeardownDrainWhoseGroupEndsAsTheUserInterruptsEndsAsDone(t *testing.T) {
	drainStopNowTest(t)
	done := make(chan struct{})
	close(done)
	for i := 0; i < 100; i++ {
		notice := NewStopNowNotice(0)
		if !notice.Next() {
			t.Fatal("the notice made no offer")
		}
		MarkInterruptedAgain()
		end, _ := waitWhileProgressing(&stall.ManualClock{}, done, time.Second, func() uint64 { return 0 }, notice, nil, func(time.Duration, bool) {})
		notice.Withdraw()
		if end != DrainJoined {
			t.Fatalf("run %d: the wait ended %v: its group had ended as the user interrupted, want it joined", i, end)
		}
	}
}

// A stop that began on its own (the app exited) offers nothing before the
// run's interrupt, and says so only once the offer stands: its notices say
// it made no progress, with no word of Ctrl+C, until the offer is made, a
// notice period after the drain first saw the interrupt; the press then ends
// it.
func TestATeardownDrainOffersOnlyOnceTheOfferStands(t *testing.T) {
	drainStopNowTestOnItsOwn(t)
	clk := &stall.ManualClock{}
	var g errgroup.Group
	holdGroup(t, &g)
	var p atomic.Uint64
	res, logs := startDrain(clk, &g, time.Second, p.Load)
	first := awaitLog(t, logs, "made no progress", res)
	if strings.Contains(first.Message, "Ctrl+C") {
		t.Fatalf("a drain whose stop began on its own offered before any interrupt: %q", first.Message)
	}
	MarkInterrupted()
	awaitLog(t, logs, drainOffer, res)
	if !MarkInterruptedAgain() {
		t.Fatal("the drain said to press Ctrl+C before its offer stood: the press ended nothing")
	}
	select {
	case r := <-res:
		if r.end != DrainStopped {
			t.Fatalf("the drain returned %+v: want it given up at the press", r)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drain did not end at the press after its offer")
	}
}

// A drain inside another wait's offer (record's frame drain, inside its
// notices' offer) says nothing and offers nothing while that offer stands,
// and its group, which ends at the press that answers it (StopNow, as
// record's frame forwarders do), is joined, never given up or named a stall.
func TestATeardownDrainInsideAnotherWaitsOfferJoinsAGroupThatEndsAtThePress(t *testing.T) {
	for i := 0; i < 50; i++ {
		drainStopNowTest(t)
		_, withdraw := OfferStopNow() // the inner wait's own offer
		var g errgroup.Group
		groupEndsAtThePress(t, &g)
		var looks atomic.Uint64
		progress := func() uint64 { looks.Add(1); return 0 }
		res, logs := startDrain(&stall.ManualClock{}, &g, 30*time.Second, progress)
		awaitLooks(t, &looks, 0, 10)
		if n := logs.FilterMessageSnippet("made no progress").Len(); n != 0 {
			t.Fatalf("run %d: the drain spoke, and offered, while another wait's offer stood: %v", i, logs.All())
		}
		MarkInterruptedAgain()
		r := awaitEnd(t, res, "at its group's end, at the press")
		withdraw()
		if r.err != nil || r.end != DrainJoined {
			t.Fatalf("run %d: the drain returned %+v: its group ended at the press, want it joined", i, r)
		}
	}
}

// A press that answered the drain's offer and another wait's together (the
// two made at once) is not the drain's alone: the group, which ends at it,
// is joined; one that does not end is waited for, the drain then saying so,
// until the drain's own later offer is answered.
func TestAPressThatAnsweredTheDrainsOfferAndAnothersIsNotTheDrainsAlone(t *testing.T) {
	for i := 0; i < 50; i++ {
		drainStopNowTest(t)
		var g errgroup.Group
		groupEndsAtThePress(t, &g)
		var p atomic.Uint64
		res, logs := startDrain(&stall.ManualClock{}, &g, 30*time.Second, p.Load)
		awaitLog(t, logs, drainOffer, res)
		_, withdraw := OfferStopNow() // another wait's, made as the drain's stood
		MarkInterruptedAgain()
		r := awaitEnd(t, res, "at its group's end, at the press")
		withdraw()
		if r.err != nil || r.end != DrainJoined {
			t.Fatalf("run %d: the drain returned %+v: its group ended at the press, want it joined", i, r)
		}
	}

	drainStopNowTest(t)
	var g errgroup.Group
	holdGroup(t, &g)
	var p atomic.Uint64
	res, logs := startDrain(&stall.ManualClock{}, &g, 30*time.Second, p.Load)
	awaitLog(t, logs, drainOffer, res)
	_, withdraw := OfferStopNow()
	MarkInterruptedAgain()
	_, at := awaitLogAfter(t, logs, "answered another wait's offer as well", -1, res)
	withdraw() // that wait ends at the press
	awaitLogAfter(t, logs, drainOffer, at, res)
	MarkInterruptedAgain()
	if r := awaitEnd(t, res, "at the press after its own later offer"); r.end != DrainStopped {
		t.Fatalf("the drain returned %+v: want it given up at the press after its own offer", r)
	}
}

// A press that answers only another wait's offer (StopNow) does not give the
// drain up: the drain had told the user nothing. Once that wait ends, the
// drain makes its own offer, and the press after it ends the drain.
func TestAPressThatAnsweredOnlyAnotherWaitsOfferDoesNotGiveTheDrainUp(t *testing.T) {
	drainStopNowTest(t)
	_, withdraw := OfferStopNow()
	var g errgroup.Group
	holdGroup(t, &g)
	var looks atomic.Uint64
	progress := func() uint64 { looks.Add(1); return 0 }
	res, logs := startDrain(&stall.ManualClock{}, &g, 30*time.Second, progress)
	awaitLooks(t, &looks, 0, 2)
	if !MarkInterruptedAgain() {
		t.Fatal("the other wait's offer did not stand")
	}
	awaitLooks(t, &looks, looks.Load(), 20)
	notReturned(t, res, "a press that answered another wait's offer gave the drain up")
	withdraw()
	awaitLog(t, logs, drainOffer, res)
	MarkInterruptedAgain()
	if r := awaitEnd(t, res, "at the press after its own offer"); r.end != DrainStopped {
		t.Fatalf("the drain returned %+v: want it given up at the press after its own offer", r)
	}
}

// A press while an agent's stop holds the drain answered the offer that
// stood then: once the hold ends, the drain does not take it for an answer
// to a later one. It makes its own offer again, and the press after that
// gives it up.
func TestAPressBeforeAnAgentStopsHoldEndsIsNotAnAnswerToTheDrainsLaterOffer(t *testing.T) {
	drainStopNowTest(t)
	endStop := BeginAgentStop()
	t.Cleanup(endStop)
	var g errgroup.Group
	holdGroup(t, &g)
	var p atomic.Uint64
	res, logs := startDrain(&stall.ManualClock{}, &g, 30*time.Second, p.Load)
	awaitLog(t, logs, drainOffer, res)
	MarkInterruptedAgain()
	_, held := awaitLogAfter(t, logs, stopWaitedFor(), -1, res)
	endStop()
	awaitLogAfter(t, logs, drainOffer, held, res)
	MarkInterruptedAgain()
	if r := awaitEnd(t, res, "at the press after its later offer"); r.end != DrainStopped {
		t.Fatalf("the drain returned %+v: want it given up at the press after its later offer", r)
	}
}

// A drain gave its group up, so a goroutine may still run, at the stall
// verdict and at the user's press alike; only one that joined did not.
func TestADrainGaveUpUnlessItJoined(t *testing.T) {
	for end, want := range map[DrainEnd]bool{DrainJoined: false, DrainStalled: true, DrainStopped: true} {
		if end.GaveUp() != want {
			t.Fatalf("%v.GaveUp() = %v, want %v", end, !want, want)
		}
	}
}

// DrainErrGroupProgress, on the process's own clock, says that a drain the
// user gave up at the press timed out, as it says of one the stall verdict
// ended: a goroutine of its group may still run (DrainEnd.GaveUp). The bound
// is an hour of runnable time, so only the press ends this drain.
func TestDrainErrGroupProgressSaysAGroupGivenUpAtThePressTimedOut(t *testing.T) {
	drainStopNowTest(t)
	var g errgroup.Group
	holdGroup(t, &g)
	var p atomic.Uint64
	core, logs := observer.New(zap.DebugLevel)
	type result struct {
		err      error
		timedOut bool
	}
	res := make(chan result, 1)
	go func() {
		err, timedOut := DrainErrGroupProgress(zap.New(core), "t", &g, time.Hour, p.Load)
		res <- result{err, timedOut}
	}()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for logs.FilterMessageSnippet(drainOffer).Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the drain never offered to stop now; it said %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	if !MarkInterruptedAgain() {
		t.Fatal("the user's interrupt after the drain's offer ended no stop")
	}
	select {
	case r := <-res:
		if r.err != nil || !r.timedOut {
			t.Fatalf("the drain given up at the press returned (%v, timedOut %v): want no error, timed out", r.err, r.timedOut)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drain did not end at the user's interrupt after its offer")
	}
}
