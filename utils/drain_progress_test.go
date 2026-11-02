package utils

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// A group still making progress is waited for past the stall bound; the bound
// is on progress, not on the total.
func TestDrainErrGroupProgressWaitsWhileProgressing(t *testing.T) {
	var g errgroup.Group
	var p atomic.Uint64
	const stall = 100 * time.Millisecond
	g.Go(func() error {
		for i := 0; i < 20; i++ { // 20 x 25 ms: 5 stall windows in all
			time.Sleep(25 * time.Millisecond)
			p.Add(1)
		}
		return nil
	})
	start := time.Now()
	err, timedOut := DrainErrGroupProgress(zap.NewNop(), "t", &g, stall, p.Load)
	if err != nil || timedOut {
		t.Fatalf("(%v, timedOut=%v) after %v: a draining group was cut off", err, timedOut, time.Since(start))
	}
}

// A group that stops making progress is given up on once the process could
// run for the bound with no progress, on its own runnable clock (stall), and
// not before. The row reads that clock, never wall time: on a loaded host the
// clock moves far slower than wall time (one thread in a four-CPU quota beside
// 26 busy loops can run about a seventh of it), so no wall bound holds there.
// A drain that never gives up fails here by assertion, not at the
// test binary's timeout: once the clock has run hangRun with no verdict, the
// row ends the group itself, and says so.
func TestDrainErrGroupProgressGivesUpOnAStall(t *testing.T) {
	const bound = 100 * time.Millisecond
	// hangRun is run time no verdict waits for: the bound, and five times
	// what the threads' waits for a CPU can still owe the clock at a verdict
	// (about a second: stall.sampler.readWaits), which also covers the looks
	// a verdict waits for a thread held off when the stall was due.
	const hangRun = bound + 5*time.Second
	clk := stall.ProcessClock()
	var g errgroup.Group
	block := make(chan struct{})
	var once sync.Once
	end := func() { once.Do(func() { close(block) }) }
	defer end()
	g.Go(func() error { <-block; return nil })
	// progress runs on this goroutine (the drain calls it): first as the
	// drain starts, its stall already begun, then at each of its looks.
	var first time.Duration
	looks, overdue := 0, false
	progress := func() uint64 {
		r := clk.Runnable()
		if looks == 0 {
			first = r
		} else if r-first >= hangRun && !overdue {
			overdue = true
			end()
		}
		looks++
		return 0
	}
	before := clk.Runnable() // no later than the stall's start
	_, timedOut := DrainErrGroupProgress(zap.NewNop(), "t", &g, bound, progress)
	ran := clk.Runnable() - before
	switch {
	case overdue:
		t.Fatalf("no give-up after %v of run time with no progress, in %d looks: want one at about the bound, %v", ran, looks, bound)
	case !timedOut:
		t.Fatalf("a group that made no progress was not given up on (%v of run time, %d looks)", ran, looks)
	case ran < bound:
		t.Fatalf("given up after %v of run time with no progress, in %d looks: want no earlier than the bound, %v", ran, looks, bound)
	}
}
