package supervisor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap/zaptest"
)

// waitPending waits for the watchdog to arm its next look on clk.
func waitPending(t *testing.T, clk *stall.ManualClock) {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for clk.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the watchdog never armed its next look")
		}
		time.Sleep(time.Millisecond)
	}
}

// A parser starved of CPU is not hung: the budget is time the process could
// run. Before, it was wall time, and a parser on an overloaded host, in a
// CPU-throttled container or a frozen one was retired (its connection fell
// through to passthrough, and its mocks went unrecorded) once it had waited
// for the CPU as long as the budget, however soon it would have run. Once the
// process could run for the budget with no activity, it is hung.
func TestAStarvedParserIsNotHung(t *testing.T) {
	const budget = 50 * time.Millisecond
	clk := &stall.ManualClock{}
	s := New(Config{Logger: zaptest.NewLogger(t), HangBudget: budget, Clock: clk})
	defer s.Close()
	s.MarkPendingWork()
	// Looks for four budgets of real wall time, the process given no CPU:
	// neither the clock's wall time (its timers) nor the host's may count.
	for end := time.Now().Add(4 * budget); time.Now().Before(end); {
		waitPending(t, clk)
		clk.Pass()
	}
	waitPending(t, clk) // the last look found no hang, and armed the next
	select {
	case <-s.hungCh:
		t.Fatal("a parser starved of CPU was declared hung")
	default:
	}
	clk.Advance(budget + time.Millisecond)
	select {
	case <-s.hungCh:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("a parser idle for the budget of run time was not declared hung")
	}
}

// Run retires a parser hung for the budget of run time, and not one that
// waited the same in wall time without the CPU to run.
func TestRunRetiresOnlyAParserHungInRunnableTime(t *testing.T) {
	const budget = 50 * time.Millisecond
	clk := &stall.ManualClock{}
	s := New(Config{Logger: zaptest.NewLogger(t), HangBudget: budget, Clock: clk})
	starved := make(chan struct{})
	res := make(chan Result, 1)
	go func() {
		res <- s.Run(context.Background(), func(ctx context.Context, _ *Session) error {
			s.MarkPendingWork()
			close(starved)
			<-ctx.Done()
			return ctx.Err()
		}, &Session{})
	}()
	<-starved
	for end := time.Now().Add(4 * budget); time.Now().Before(end); {
		waitPending(t, clk)
		clk.Pass()
	}
	waitPending(t, clk)
	select {
	case r := <-res:
		t.Fatalf("a starved parser was retired: %s", r.Status)
	default:
	}
	clk.Advance(2 * budget)
	select {
	case r := <-res:
		if r.Status != StatusHung || !r.FallthroughToPassthrough {
			t.Fatalf("a parser hung for the budget of run time ended %s (fallthrough %v), want hung", r.Status, r.FallthroughToPassthrough)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("a parser hung for the budget of run time was not retired")
	}
}

// The process clock is sampled (a hundred times a second) only while held,
// so a supervisor holds it only while it has pending work to measure: an idle
// connection, most of a pooled one's life, costs the process no sampling.
// Not parallel: it swaps holdProcessClock, which only New reads.
func TestAnIdleSupervisorDoesNotHoldTheProcessClock(t *testing.T) {
	var holds, releases atomic.Int32
	saved := holdProcessClock
	holdProcessClock = func() func() {
		holds.Add(1)
		return func() { releases.Add(1) }
	}
	t.Cleanup(func() { holdProcessClock = saved })
	const budget = 20 * time.Millisecond // a look every 5 ms
	s := New(Config{Logger: zaptest.NewLogger(t), HangBudget: budget})
	defer s.Close()
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(10 * budget) // looks with nothing pending
	if n := holds.Load(); n != 0 {
		t.Fatalf("held the process clock %d times with no pending work", n)
	}
	s.MarkPendingWork()
	waitFor("did not hold the process clock with work pending", func() bool { return holds.Load() == 1 })
	s.ClearPendingWork()
	waitFor("kept the process clock once the work was done", func() bool { return releases.Load() == 1 })
	s.MarkPendingWork()
	waitFor("did not hold the process clock again for the next request", func() bool { return holds.Load() == 2 })
	s.Close()
	if r := releases.Load(); r != 2 {
		t.Fatalf("Close left the process clock held (%d holds, %d releases)", holds.Load(), r)
	}
}
