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
		_, timedOut := drainErrGroupProgress(clk, zap.NewNop(), "t", &g, bound, p.Load)
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
		_, timedOut := drainErrGroupProgress(clk, zap.NewNop(), "t", &g, time.Second, p.Load)
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
