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
//
// DrainErrGroupStatus and DrainErrGroupProgress measure it on the same clock
// as DrainErrGroupProgressOn's nil (boundClock): a drain bounded by time alone
// is not on wall time either.
func TestADrainStarvedOfCPUDoesNotGiveUp(t *testing.T) {
	const bound = 50 * time.Millisecond
	var p atomic.Uint64
	for _, entry := range []struct {
		name  string
		drain func(clk *stall.ManualClock, g *errgroup.Group) (timedOut bool)
	}{
		{"DrainErrGroupProgressOn", func(clk *stall.ManualClock, g *errgroup.Group) bool {
			_, timedOut := DrainErrGroupProgressOn(clk, zap.NewNop(), "t", g, bound, p.Load)
			return timedOut
		}},
		{"DrainErrGroupProgress", func(clk *stall.ManualClock, g *errgroup.Group) bool {
			_, timedOut := DrainErrGroupProgress(zap.NewNop(), "t", g, bound, p.Load)
			return timedOut
		}},
		{"DrainErrGroupStatus", func(clk *stall.ManualClock, g *errgroup.Group) bool {
			_, timedOut := DrainErrGroupStatus(zap.NewNop(), "t", g, bound)
			return timedOut
		}},
	} {
		clk := &stall.ManualClock{}
		var g errgroup.Group
		block := make(chan struct{})
		t.Cleanup(func() { close(block) })
		g.Go(func() error { <-block; return nil })
		setBoundClock(t, clk) // the clock DrainErrGroupProgressOn is not handed
		res := make(chan bool, 1)
		go func() { res <- entry.drain(clk, &g) }()
		// Four bounds of real wall time, no CPU: neither the clock's wall
		// time (its timers) nor the host's may count.
		for end := time.Now().Add(4 * bound); time.Now().Before(end); {
			waitArmedNotReturned(t, clk, res, entry.name+": a drain starved of CPU gave up")
			clk.Pass()
		}
		waitArmedNotReturned(t, clk, res, entry.name+": a drain starved of CPU gave up")
		clk.Advance(bound)
		wantReturned(t, clk, res, true, entry.name+": a drain with no progress for the bound of run time did not give up")
	}
}

// setBoundClock has the drains and AwaitAgentExit measure their bounds on
// clk (boundClock) until the test ends. A row sets it before it starts the
// wait, and the wait reads it before it arms its first look (waitArmed).
func setBoundClock(t *testing.T, clk stall.Clock) {
	t.Helper()
	boundClock = clk
	t.Cleanup(func() { boundClock = nil })
}

// wantReturned waits for a wait on clk to return want, and fails at once if
// it armed another look instead: it went on waiting.
func wantReturned(t *testing.T, clk *stall.ManualClock, res <-chan bool, want bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for {
		select {
		case got := <-res:
			if got != want {
				t.Fatalf("%s (returned %v)", msg, got)
			}
			return
		default:
		}
		if clk.Pending() != 0 {
			t.Fatalf("%s (it went on waiting)", msg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (it never returned)", msg)
		}
		time.Sleep(time.Millisecond)
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

// waitArmedNotReturned waits for a wait (a drain, AwaitAgentExit) to arm its
// next look on clk, and fails at once if it returned instead: it gave up (or
// ended) at the look before.
func waitArmedNotReturned(t *testing.T, clk *stall.ManualClock, res <-chan bool, gaveUp string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for clk.Pending() == 0 {
		select {
		case got := <-res:
			t.Fatalf("%s (returned %v)", gaveUp, got)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the drain never armed its next look")
		}
		time.Sleep(time.Millisecond)
	}
}

// A stop's end gives the rest of the group a whole bound again, from the first
// verdict after it, also when the drain was still measuring its stall as the
// stop ended:
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
		waitArmedNotReturned(t, clk, res, "gave the rest of the group less than a whole bound from the first verdict after the stop's end")
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
// join wins. Before, a drain that found both ready gave up on the group about
// half the time (its select took either); in this row, where the group joins
// at the verdict's look at progress, every time.
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

// An agent's exit that comes as its bound runs out is seen: AwaitAgentExit
// looks at the exit once more before it gives up. Before, the bound's timer
// and the exit could be ready together and the wait took either, so a
// retried start failed with "the previous keploy agent has not exited" for an
// agent that had; in this row, where the agent exits at the verdict, every
// time.
func TestAnExitThatComesAsTheBoundRunsOutIsSeen(t *testing.T) {
	const bound = 100 * time.Millisecond
	clk := &hookClock{ManualClock: &stall.ManualClock{}}
	setBoundClock(t, clk)
	gone := make(chan struct{})
	res := make(chan bool, 1)
	go func() { res <- AwaitAgentExit(gone, bound, nil) }()
	waitArmed(t, clk.ManualClock)
	exit := func() { close(gone) }
	clk.hook.Store(&exit)
	clk.Advance(bound) // the verdict, and the agent exits as it is drawn
	wantReturned(t, clk.ManualClock, res, true, "an agent's exit that came as its bound ran out was not seen")
}

// A stop of the agent that ends while its exit's bound runs gives the exit a
// whole bound again: the agent exits at its stop's end or after it, and the
// bound run while the stop was in progress was the stop's time. Before, a stop
// that ended just before the bound ran out left the exit only what was left of
// it.
func TestAnAgentStopThatEndsAsTheExitWaitsGivesTheBoundAgain(t *testing.T) {
	const bound = 100 * time.Millisecond
	for _, exits := range []bool{false, true} {
		clk := &stall.ManualClock{}
		setBoundClock(t, clk)
		end := BeginAgentStop()
		t.Cleanup(end) // a failed row leaves no stop in progress to hold later ones
		gone := make(chan struct{})
		res := make(chan bool, 1)
		go func() { res <- AwaitAgentExit(gone, bound, nil) }()
		waitArmed(t, clk)
		clk.Advance(bound - time.Millisecond) // most of the bound, the stop in progress
		waitArmedNotReturned(t, clk, res, "gave up on the exit before the bound")
		end()
		clk.Advance(time.Millisecond) // the bound begun before the stop's end runs out
		waitArmedNotReturned(t, clk, res, "gave the agent's exit only what was left of the bound after its stop ended")
		clk.Advance(bound - time.Millisecond)
		waitArmedNotReturned(t, clk, res, "gave the agent's exit less than a whole bound after its stop ended")
		if exits {
			close(gone)
			select {
			case ok := <-res:
				if !ok {
					t.Fatal("an exit in the bound given after the stop's end was not seen")
				}
			case <-time.After(time.Minute): // only turns a hang into a failure
				t.Fatal("an exit in the bound given after the stop's end did not end the wait")
			}
			continue
		}
		clk.Advance(time.Millisecond)
		wantReturned(t, clk, res, false, "an agent that did not exit in the bound after its stop's end was said to have")
	}
}

// A stop of daemons, or of the application, is not the agent's: it neither
// holds an agent's exit past its bound while in progress nor gives the exit
// the bound again when it ends. Its own exit does not wait on them.
func TestAStopThatIsNotTheAgentsDoesNotHoldItsExit(t *testing.T) {
	const bound = 100 * time.Millisecond
	for _, c := range []struct {
		name  string
		begin func() func()
		ends  bool
	}{
		{"daemons, in progress", BeginDaemonStop, false},
		{"daemons, ended", BeginDaemonStop, true},
		{"application, in progress", BeginAppStop, false},
		{"application, ended", BeginAppStop, true},
	} {
		clk := &stall.ManualClock{}
		setBoundClock(t, clk)
		end := c.begin()
		t.Cleanup(end)
		never := make(chan struct{})
		res := make(chan bool, 1)
		go func() { res <- AwaitAgentExit(never, bound, nil) }()
		waitArmed(t, clk)
		clk.Advance(bound - time.Millisecond)
		waitArmedNotReturned(t, clk, res, c.name+": gave up on the exit before the bound")
		if c.ends {
			end()
		}
		clk.Advance(time.Millisecond)
		wantReturned(t, clk, res, false, c.name+": a stop that is not the agent's held the agent's exit past its bound")
		end()
	}
}
