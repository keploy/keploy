package utils

import (
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// waitArmed waits for the drain to arm its next look on clk.
func waitArmed(t *testing.T, clk *stall.ManualClock) {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for clk.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the drain never armed its next look")
		}
		time.Sleep(time.Millisecond)
	}
}

// A drain's stall bound is time the process could run: a CLI starved of CPU
// (a loaded host, a frozen laptop) does not give a group up for the wall time
// it could not run. Before, it was wall time, and a recording's stop forced
// its shutdown, its sinks still writing what the agent held at the stop, once
// the CLI had waited for the CPU for the bound. Once the process could run for
// the bound with no progress, it gives up.
func TestADrainStarvedOfCPUDoesNotGiveUp(t *testing.T) {
	const bound = 50 * time.Millisecond
	clk := &stall.ManualClock{}
	var g errgroup.Group
	block := make(chan struct{})
	defer close(block)
	g.Go(func() error { <-block; return nil })
	var p atomic.Uint64
	type result struct{ timedOut bool }
	res := make(chan result, 1)
	go func() {
		_, timedOut := drainErrGroupProgress(clk, zap.NewNop(), "t", &g, bound, p.Load, nil)
		res <- result{timedOut}
	}()
	// Four bounds of real wall time, no CPU: neither the clock's wall time
	// (its timers) nor the host's may count.
	for end := time.Now().Add(4 * bound); time.Now().Before(end); {
		waitArmed(t, clk)
		clk.Pass()
	}
	waitArmed(t, clk)
	select {
	case r := <-res:
		t.Fatalf("a drain starved of CPU gave up (timedOut=%v)", r.timedOut)
	default:
	}
	clk.Advance(bound)
	select {
	case r := <-res:
		if !r.timedOut {
			t.Fatal("a drain with no progress for the bound of run time did not give up")
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("a drain with no progress for the bound of run time did not give up")
	}
}

// Progress starts the stall again, however long the drain runs in all.
func TestADrainThatProgressesIsWaitedForPastItsBound(t *testing.T) {
	clk := &stall.ManualClock{}
	var g errgroup.Group
	release := make(chan struct{})
	g.Go(func() error { <-release; return nil })
	var p atomic.Uint64
	res := make(chan bool, 1)
	go func() {
		_, timedOut := drainErrGroupProgress(clk, zap.NewNop(), "t", &g, time.Second, p.Load, nil)
		res <- timedOut
	}()
	for i := 0; i < 20; i++ { // 10 bounds of run time, progressing
		waitArmed(t, clk)
		p.Add(1)
		clk.Advance(500 * time.Millisecond)
	}
	waitArmed(t, clk)
	select {
	case <-res:
		t.Fatal("a drain still progressing was given up")
	default:
	}
	close(release)
	select {
	case timedOut := <-res:
		if timedOut {
			t.Fatal("a drain that finished was said to time out")
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drain did not end with its group")
	}
}

// waitArmedNotReturned waits for the drain to arm its next look on clk, and
// fails at once if it returned instead: it gave up (or ended) at the look
// before.
func waitArmedNotReturned(t *testing.T, clk *stall.ManualClock, res <-chan bool, gaveUp string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for clk.Pending() == 0 {
		select {
		case timedOut := <-res:
			t.Fatalf("%s (timedOut=%v)", gaveUp, timedOut)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the drain never armed its next look")
		}
		time.Sleep(time.Millisecond)
	}
}

// A stop's end gives the rest of the group the bound again, from the stop's
// end, also when the drain was still measuring its stall as the stop ended:
// the stall run while the stop was in progress was the stop's time. Before, a
// verdict that came just after the stop ended, on a stall begun before it,
// gave up at once on a group whose last goroutine was on its way out (seen on
// a loaded macOS runner, where the process clock crawled while the stop held
// the drain).
func TestAStopsEndGivesTheRestOfTheGroupTheBoundAgain(t *testing.T) {
	for _, status := range []bool{false, true} {
		const bound = 100 * time.Millisecond
		clk := &stall.ManualClock{}
		end := BeginAgentStop()
		t.Cleanup(end) // a failed row leaves no stop in progress to hold later ones
		var g errgroup.Group
		block := make(chan struct{})
		t.Cleanup(func() { close(block) })
		g.Go(func() error { <-block; return nil }) // the rest of the group, wedged
		progress := func() uint64 { return 7 }     // a drain whose group moves no more
		if status {
			progress = noProgress // DrainErrGroupStatus's
		}
		res := make(chan bool, 1)
		go func() {
			_, timedOut := drainErrGroupProgress(clk, zap.NewNop(), "t", &g, bound, progress, nil)
			res <- timedOut
		}()
		waitArmed(t, clk)
		clk.Advance(bound - time.Millisecond) // most of a stall, the stop in progress
		waitArmedNotReturned(t, clk, res, "gave up before the bound")
		end()
		clk.Advance(time.Millisecond) // the stall begun before the stop's end is due now
		waitArmedNotReturned(t, clk, res, "charged the stall run while the stop was in progress to the rest of the group")
		clk.Advance(bound - time.Millisecond)
		waitArmedNotReturned(t, clk, res, "gave the rest of the group less than the bound from the stop's end")
		clk.Advance(time.Millisecond)
		select {
		case timedOut := <-res:
			if !timedOut {
				t.Fatalf("status=%v: a wedged group was said to have joined", status)
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("status=%v: the bound from the stop's end passed with no progress and the drain did not give up", status)
		}
	}
}

// hookClock is a ManualClock that calls hook, once armed, at the next read of
// its runnable time: a row acts between a drain's look at its progress and
// its verdict on the stall.
type hookClock struct {
	*stall.ManualClock
	hook atomic.Pointer[func()]
}

func (c *hookClock) Runnable() time.Duration {
	if h := c.hook.Swap(nil); h != nil {
		(*h)()
	}
	return c.ManualClock.Runnable()
}

// A stop that ends between the verdict's look at the stops and the drain's
// look at whether one is in progress gives the rest of the group the bound
// again: the drain does not read it as no stop at all.
func TestAStopThatEndsAsTheVerdictComesGivesTheBoundAgain(t *testing.T) {
	const bound = 100 * time.Millisecond
	clk := &hookClock{ManualClock: &stall.ManualClock{}}
	end := BeginAgentStop()
	t.Cleanup(end)
	done := make(chan struct{}) // a group that never joins
	res := make(chan bool, 1)
	go func() { res <- drainUntil(clk, zap.NewNop(), "t", done, bound, noProgress, nil) }()
	waitArmed(t, clk.ManualClock)
	hook := func() { end() }
	clk.hook.Store(&hook)
	clk.Advance(bound) // the verdict, and the stop ends as it is drawn
	waitArmedNotReturned(t, clk.ManualClock, res, "a stop that ended as the verdict came was read as none")
	clk.Advance(bound)
	select {
	case timedOut := <-res:
		if !timedOut {
			t.Fatal("a group that never joined was said to have joined")
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the bound from the stop's end passed with no progress and the drain did not give up")
	}
}

// A group that joins as the verdict comes is not given up on: a drain held off
// the CPU finds the group's join and its next look ready together, and the
// join wins. Before, it gave up on a group that had joined about half the time.
func TestAGroupThatJoinsAsTheVerdictComesIsNotGivenUpOn(t *testing.T) {
	const bound = 100 * time.Millisecond
	clk := &stall.ManualClock{}
	done := make(chan struct{})
	var looks atomic.Int32
	progress := func() uint64 {
		if looks.Add(1) == 2 { // the first look after the wait's start: the verdict's
			close(done) // the group joins as the verdict is drawn
		}
		return 0
	}
	res := make(chan bool, 1)
	go func() { res <- drainUntil(clk, zap.NewNop(), "t", done, bound, progress, nil) }()
	waitArmed(t, clk)
	clk.Advance(bound)
	select {
	case timedOut := <-res:
		if timedOut {
			t.Fatal("the drain gave up on a group that joined as its verdict came")
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the drain never returned")
	}
}
