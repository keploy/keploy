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

// mostStill is how many of a drain's looks in a row may find its stall where
// the last one left it: a look comes every 10 ms of wall time at the most
// often (stall.CheckEvery of the rows' 100 ms bound), so these are 100 s and
// more in which the process clock counted nothing at all. That is a clock
// that stopped, not a slow one, and a stall that never moves never runs a
// hang guard's run time.
const mostStill = 10_000

// A group that stops making progress is given up on once the process could
// run for the bound with no progress, on its own runnable clock (stall), and
// not before. The row reads the stall the drain itself measures, on the meter
// the drain hands over before its first look, never wall time and never a
// read of the clock of its own: the process clock is shared, so a holder
// elsewhere in the package can move it between such a read and the meter's
// start, and a stall counted from that read over-states the drain's by as
// much, which hides a give-up that early. On a loaded host the clock moves far
// slower than wall time (one thread in a four-CPU quota beside 26 busy loops
// can run about a seventh of it), so no wall bound holds there. A drain that
// never gives up fails here by assertion, not at the test binary's timeout:
// once its stall has run hangRun with no verdict, or has not moved in
// mostStill of the drain's looks in a row (which come on a wall timer
// whatever the clock does), the row ends the group itself, and says so.
func TestDrainErrGroupProgressGivesUpOnAStall(t *testing.T) {
	const bound = 100 * time.Millisecond
	// hangRun is run time no verdict waits for: the bound, and five times
	// what the threads' waits for a CPU can still owe the clock at a verdict
	// (about a second: stall.sampler.readWaits), which also covers the looks
	// a verdict waits for a thread held off when the stall was due.
	const hangRun = bound + 5*time.Second
	var g errgroup.Group
	block := make(chan struct{})
	var once sync.Once
	end := func() { once.Do(func() { close(block) }) }
	defer end()
	g.Go(func() error { <-block; return nil })
	// The drain hands each wait's meter over (watch), then calls progress on
	// this goroutine: as the wait starts, then at each of its looks, before
	// the look's verdict. A verdict is drawn only at a look after the one
	// that found the stall due (stall.Meter.Expired), so the stall read at
	// the verdict's look is the bound or more on a drain that waits for it.
	var m *stall.Meter
	var stalled, last time.Duration
	looks, still, overdue := 0, 0, false
	watch := func(w *stall.Meter) { m = w }
	progress := func() uint64 {
		looks++
		if m == nil {
			t.Errorf("look %d: the drain looked at progress before it handed its meter over", looks)
			return 0
		}
		stalled = m.Stalled()
		if stalled == last {
			still++
		} else {
			still = 0
		}
		last = stalled
		if (stalled >= hangRun || still >= mostStill) && !overdue {
			overdue = true
			end()
		}
		return 0
	}
	_, timedOut := drainErrGroupProgress(nil, zap.NewNop(), "t", &g, bound, progress, watch)
	switch {
	case overdue:
		t.Fatalf("no give-up after %v of the drain's stall, in %d looks (the last %d with it still): want one at about the bound, %v", stalled, looks, still, bound)
	case !timedOut:
		t.Fatalf("a group that made no progress was not given up on (%v of the drain's stall, %d looks)", stalled, looks)
	case stalled < bound:
		t.Fatalf("given up at a look that read %v of the drain's stall, in %d looks: want no earlier than the bound, %v", stalled, looks, bound)
	}
}

// The exported drain says it gave up, with no error, on a group that makes no
// progress: the row above reaches the give-up through the unexported seam it
// reads the drain's stall from, and this one through DrainErrGroupProgress
// itself. Its hang guard reads the process clock the drain measures on, from
// the drain's first call of progress, as its wait starts: that comes after
// the drain's meter started, and the clock only moves forward, so the guard
// never reads more stall than the drain has. It ends the group, as the row
// above does, only once the drain is hangRun into its stall, or the clock
// has not moved in mostStill of its looks in a row, with no verdict.
func TestDrainErrGroupProgressSaysItGaveUpOnAStall(t *testing.T) {
	const bound = 100 * time.Millisecond
	const hangRun = bound + 5*time.Second // the row above's, for its reasons
	var g errgroup.Group
	block := make(chan struct{})
	var once sync.Once
	end := func() { once.Do(func() { close(block) }) }
	defer end()
	g.Go(func() error { <-block; return nil })
	clk := stall.ProcessClock() // held by the drain's meter while it waits
	var from, seen, last time.Duration
	looks, still, overdue := 0, 0, false
	progress := func() uint64 {
		if looks++; looks == 1 {
			from = clk.Runnable()
		}
		seen = clk.Runnable() - from
		if seen == last {
			still++
		} else {
			still = 0
		}
		last = seen
		if (seen >= hangRun || still >= mostStill) && !overdue {
			overdue = true
			end()
		}
		return 0
	}
	err, timedOut := DrainErrGroupProgress(zap.NewNop(), "t", &g, bound, progress)
	switch {
	case overdue:
		t.Fatalf("no give-up after %v of the process clock since the drain's wait started, in %d looks (the last %d with it still): want one at about the bound, %v", seen, looks, still, bound)
	case err != nil || !timedOut:
		t.Fatalf("(%v, timedOut=%v) for a group that made no progress, after %d looks: want (nil, true)", err, timedOut, looks)
	}
}
