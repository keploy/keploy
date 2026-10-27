// Package teardowntest drives a drain of a run's teardown in a test: a
// stand-in for a stop the drain waits on, and the two checks every drain
// bounded by its run's teardown (utils.TeardownScope) must pass, on a clock
// the test moves (stall.ManualClock).
package teardowntest

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
)

// Stop stands in for a stop keploy waits on at a run's end (the agent's, the
// app's): once the context it runs under ends, it goes on until released,
// and at each step the test gives it, moves the teardown scope that context
// carries, as keploy's stops do when they see the stop make progress.
type Stop struct {
	step      chan struct{}
	stepped   chan struct{}
	release   chan struct{}
	cancelled chan struct{}
	finished  chan struct{}
	release1  sync.Once
}

// NewStop returns a Stop not yet run.
func NewStop() *Stop {
	return &Stop{step: make(chan struct{}), stepped: make(chan struct{}), release: make(chan struct{}),
		cancelled: make(chan struct{}), finished: make(chan struct{})}
}

// Run is the stop, under ctx: it returns once released, after ctx has ended.
func (s *Stop) Run(ctx context.Context) {
	defer close(s.finished)
	scope := utils.TeardownScopeOf(ctx)
	<-ctx.Done()
	close(s.cancelled)
	for {
		select {
		case <-s.step:
			scope.Note()
			s.stepped <- struct{}{}
		case <-s.release:
			return
		}
	}
}

// Release lets the stop end. Idempotent.
func (s *Stop) Release() { s.release1.Do(func() { close(s.release) }) }

// take gives the stop its next step, and returns once it has moved its
// scope; the stop takes steps only once its context has ended.
func (s *Stop) take(t *testing.T) {
	t.Helper()
	select {
	case s.step <- struct{}{}:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the stop took no step: its context never ended")
	}
	<-s.stepped
}

// Finished is closed once the stop has returned.
func (s *Stop) Finished() <-chan struct{} { return s.finished }

// AwaitArmed waits for a drain on clk to arm its next look; false once ended
// is closed first.
func AwaitArmed(t *testing.T, clk *stall.ManualClock, ended <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for clk.Pending() == 0 {
		select {
		case <-ended:
			return false
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the drain never armed its next look")
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// CheckWaitsWhileItMoves checks that the drain of a run's teardown waits for
// stop while it moves, however long: four times its bound of runnable time
// on clk, stop taking a step every half bound, without the drain saying it
// gave up (gaveUp) or the run ending (ended). It then releases stop, and
// checks the run ends, the drain never having given up.
func CheckWaitsWhileItMoves(t *testing.T, clk *stall.ManualClock, bound time.Duration, stop *Stop, gaveUp func() bool, ended <-chan struct{}) {
	t.Helper()
	for i := 0; i < 8; i++ {
		if !AwaitArmed(t, clk, ended) {
			t.Fatalf("the run ended %v into its drain while the stop it waited on still made progress", time.Duration(i)*bound/2)
		}
		if gaveUp() {
			t.Fatalf("the drain gave the stop up %v into it, while the stop still made progress", time.Duration(i)*bound/2)
		}
		stop.take(t)
		clk.Advance(bound / 2)
	}
	if !AwaitArmed(t, clk, ended) || gaveUp() {
		t.Fatal("the drain gave up, or the run ended, while the stop its drain waited on still made progress")
	}
	stop.Release()
	select {
	case <-ended:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the run did not end once the stop its drain waited on had")
	}
	if gaveUp() {
		t.Fatal("the drain gave up a stop that made progress until it ended")
	}
}

// CheckGivesUpOnTheBound checks that the drain of a run's teardown gives a
// stop that makes no progress up on its bound of runnable time on clk, and
// not before: from the stop's cancellation (the drain begins at it), clk
// moves a quarter bound at each look the drain arms, until it says it gave
// up (gaveUp), which must come at the first look a whole bound in. The run
// (ended) then ends, the stop still running.
func CheckGivesUpOnTheBound(t *testing.T, clk *stall.ManualClock, bound time.Duration, stop *Stop, gaveUp func() bool, ended <-chan struct{}) {
	t.Helper()
	select {
	case <-stop.cancelled:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the run's teardown never cancelled the stop")
	}
	base := clk.Runnable()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for !gaveUp() {
		if time.Now().After(deadline) {
			endedYet := false
			select {
			case <-ended:
				endedYet = true
			default:
			}
			t.Fatalf("the drain did not give up a stop that made no progress for %v of runnable time (looks armed: %d; the run ended: %v)",
				clk.Runnable()-base, clk.Pending(), endedYet)
		}
		if clk.Pending() == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		if clk.Runnable()-base >= 2*bound {
			t.Fatalf("the drain did not give up a stop that made no progress for %v of runnable time", clk.Runnable()-base)
		}
		// The look this fires is answered by the drain arming its next one,
		// or by the give-up: the loop waits for either. (Waiting here for
		// the armed look to be fired instead hung whenever the drain armed
		// its next before the wait began.)
		clk.Advance(bound / 4)
	}
	if took := clk.Runnable() - base; took < bound || took > bound+bound/4 {
		t.Fatalf("the drain gave the stop up after %v of runnable time without progress; want at its bound, %v", took, bound)
	}
	select {
	case <-ended:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the run did not end once its drain gave the stop up")
	}
	select {
	case <-stop.Finished():
		t.Fatal("the stop ended on its own: the drain was not given up")
	default:
	}
}
