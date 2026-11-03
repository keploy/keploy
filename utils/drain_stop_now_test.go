package utils

import (
	"strings"
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
	notice := drainNotice
	drainNotice = 5 * time.Millisecond
	ClearInterrupted()
	ResetInterruptedAgain()
	t.Cleanup(func() { drainNotice = notice; ClearInterrupted(); ResetInterruptedAgain() })
}

// drainResult is what a teardown drain returned.
type drainResult struct {
	err      error
	timedOut bool
}

// startDrain runs the teardown drain of g on clk, at bound, logging to an
// observer.
func startDrain(clk stall.Clock, g *errgroup.Group, bound time.Duration, progress func() uint64) (<-chan drainResult, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	res := make(chan drainResult, 1)
	go func() {
		err, timedOut := drainErrGroupProgress(clk, zap.New(core), "t", g, bound, progress)
		res <- drainResult{err, timedOut}
	}()
	return res, logs
}

// awaitLog waits for a line of logs whose message holds part, while the
// drain has not returned, and returns it.
func awaitLog(t *testing.T, logs *observer.ObservedLogs, part string, res <-chan drainResult) observer.LoggedEntry {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for {
		for _, e := range logs.All() {
			if strings.Contains(e.Message, part) {
				return e
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
	block := make(chan struct{})
	defer close(block)
	g.Go(func() error { <-block; return nil })
	var p atomic.Uint64
	const bound = 30 * time.Second
	res, logs := startDrain(clk, &g, bound, p.Load)
	offer := awaitLog(t, logs, drainOffer, res)
	if f := offer.ContextMap(); f["group"] != "t" || f["stalledFor"] != time.Duration(0) || f["bound"] != bound {
		t.Fatalf("the offer said %v: want the group, no stall yet (a frozen clock), and the bound", f)
	}
	if !MarkInterruptedAgain() {
		t.Fatal("the user's interrupt after the drain's offer ended no stop")
	}
	var r drainResult
	select {
	case r = <-res:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drain did not end at the user's interrupt after its offer, its verdict pending")
	}
	if r.err != nil || !r.timedOut {
		t.Fatalf("the drain returned %+v: want it given up (timedOut), with no error", r)
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
	block := make(chan struct{})
	g.Go(func() error { <-block; return nil })
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
	close(block)
	select {
	case r := <-res:
		if r.err != nil || r.timedOut {
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
	block := make(chan struct{})
	g.Go(func() error { <-block; return nil })
	var p atomic.Uint64
	res, logs := startDrain(clk, &g, time.Second, p.Load)
	awaitLog(t, logs, drainOffer, res)
	MarkInterruptedAgain()
	awaitLog(t, logs, stopWaitedFor(), res)
	close(block)
	select {
	case r := <-res:
		if r.err != nil || r.timedOut {
			t.Fatalf("the drain returned %+v: its group ended, want no give-up", r)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drain did not end at its group's end")
	}
}

// A group that ends as the user's interrupt comes is the drain's end, not a
// give-up: the drain finds both at its next look, its group's end first. Its
// progress look, at a notice, ends the group and gives the interrupt after
// the drain's offer, each run.
func TestATeardownDrainWhoseGroupEndsAsTheUserInterruptsEndsAsDone(t *testing.T) {
	for i := 0; i < 20; i++ {
		drainStopNowTest(t)
		clk := &stall.ManualClock{}
		var g errgroup.Group
		block := make(chan struct{})
		g.Go(func() error { <-block; return nil })
		seen := make(chan struct{})
		f := func(ended *errgroup.Group) {
			if ended == &g { // not a group of an earlier row, ending late
				close(seen)
			}
		}
		drainGroupEnded.Store(&f)
		t.Cleanup(func() { drainGroupEnded.Store(nil) })
		var both atomic.Bool
		progress := func() uint64 {
			if both.CompareAndSwap(true, false) {
				close(block)
				<-seen // the drain has seen its group end
				MarkInterruptedAgain()
			}
			return 0
		}
		res, logs := startDrain(clk, &g, time.Second, progress)
		awaitLog(t, logs, drainOffer, res)
		both.Store(true)
		select {
		case r := <-res:
			if r.err != nil || r.timedOut {
				t.Fatalf("run %d: the drain returned %+v: its group ended as the user interrupted, want no give-up", i, r)
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("the drain did not end at its group's end")
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
	block := make(chan struct{})
	defer close(block)
	g.Go(func() error { <-block; return nil })
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
		if !r.timedOut {
			t.Fatalf("the drain returned %+v: want it given up at the press", r)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drain did not end at the press after its offer")
	}
}
