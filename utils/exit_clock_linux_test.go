//go:build linux

package utils

import (
	"fmt"
	"maps"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/starvetest"
	"go.keploy.io/server/v3/utils/stall"
	"golang.org/x/sys/unix"
)

// clockRig is an ExitClock on a wall clock and a keploy runnable clock the
// test moves, reading thread accounts and block devices the test sets.
type clockRig struct {
	wall    time.Time
	own     *stall.ManualClock
	tasks   map[int]taskTime
	ok      bool
	disks   diskMark
	disksOK bool
	// below is, by device name, the devices directly below it; belowRead
	// counts the reads of it.
	below     map[string][]string
	belowRead map[string]int
	// readTakes is how long a read of the threads' accounts takes: the
	// wall clock moves on by it as they are read, as it does for a keploy
	// held off the CPU between reading the wall clock and reading them.
	readTakes time.Duration
	c         *ExitClock
	last      time.Duration
}

func newClockRig(tasks map[int]taskTime) *clockRig {
	return newClockRigOf(tasks, time.Hour, diskMark{"259:0": {}})
}

// newClockRigOf is a clockRig of a stop that judges on bound, which takes a
// thread standing in the kernel for that long for wedged, the host's block
// devices first read as disks.
func newClockRigOf(tasks map[int]taskTime, bound time.Duration, disks diskMark) *clockRig {
	return newClockRigWedged(tasks, bound, bound, disks)
}

// newClockRigWedged is newClockRigOf, taking a thread standing in the kernel
// for wedge for wedged. Its credit of the waits in the kernel in all is far
// beyond what its tests look at, so they pin the rules of one stretch; a
// stop's is wedge (newClockRigCredited).
func newClockRigWedged(tasks map[int]taskTime, bound, wedge time.Duration, disks diskMark) *clockRig {
	return newClockRigCredited(tasks, bound, wedge, time.Hour, disks)
}

// newClockRigCredited is newClockRigWedged, crediting the waits in the
// kernel, since the processes' last progress, for credit in all.
func newClockRigCredited(tasks map[int]taskTime, bound, wedge, credit time.Duration, disks diskMark) *clockRig {
	r := &clockRig{wall: time.Unix(1000, 0), own: &stall.ManualClock{}, tasks: tasks, ok: true,
		disks: disks, disksOK: true, below: map[string][]string{}, belowRead: map[string]int{}}
	r.c = newExitClock(nil, bound, wedge, credit, r.own, func() time.Time { return r.wall },
		func([]Reached) (map[int]taskTime, bool) {
			r.wall = r.wall.Add(r.readTakes)
			return maps.Clone(r.tasks), r.ok
		},
		func() (diskMark, bool) { return maps.Clone(r.disks), r.disksOK },
		func(name string) []string { r.belowRead[name]++; return r.below[name] })
	return r
}

// serves makes the host's disk serve I/O for d of the next look: it
// completes I/O, and has I/O in flight for d.
func (r *clockRig) serves(d time.Duration) {
	n := r.disks["259:0"]
	n.done++
	n.busy += d
	r.disks["259:0"] = n
}

// look moves wall time by w and keploy's runnable time by own, lets step
// change the threads' accounts, and returns what the clock added.
func (r *clockRig) look(w, own time.Duration, step func(map[int]taskTime)) time.Duration {
	r.wall = r.wall.Add(w)
	r.own.Advance(own)
	if step != nil {
		step(r.tasks)
	}
	now := r.c.Runnable()
	added := now - r.last
	r.last = now
	return added
}

// adds changes thread tid's account by ran, waited and slices, and sets
// whether it is runnable.
func adds(tid int, ran, waited time.Duration, slices uint64, runnable bool) func(map[int]taskTime) {
	return func(m map[int]taskTime) {
		t := m[tid]
		t.ran += ran
		t.waited += waited
		t.slices += slices
		t.runnable = runnable
		m[tid] = t
	}
}

// A stop's stall is counted only in the time the stopped processes could run
// (and keploy with them): w less the time their threads waited for a CPU, and
// never less than the CPU they had. A wait still going on, which the kernel
// adds to a thread's account only once it ends, is counted as it goes, and
// not again when it ends.
func TestTheExitClockCountsOnlyTheTimeTheProcessesCouldRun(t *testing.T) {
	const w = 100 * time.Millisecond
	ms := time.Millisecond
	t.Run("a thread that sleeps could run all of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {ran: ms, slices: 3}})
		for i := range 5 {
			if got := r.look(w, w, nil); got != w {
				t.Fatalf("look %d added %v, want %v", i, got, w)
			}
		}
	})
	t.Run("a thread held off the CPU throughout could run none of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {ran: ms, slices: 3, runnable: true}})
		for i := range 5 {
			if got := r.look(w, w, nil); got != 0 {
				t.Fatalf("look %d added %v of a thread waiting for a CPU since before it, want none", i, got)
			}
		}
		// Its wait ends 90 ms into the next look, and it runs 10 ms: the
		// kernel adds all 590 ms of that wait now, of which 500 were counted.
		if got := r.look(w, w, adds(1, 10*ms, 590*ms, 1, true)); got != 10*ms {
			t.Fatalf("the look its wait ended in added %v, want the 10ms it ran", got)
		}
		// Waiting again, from before this look: none.
		if got := r.look(w, w, nil); got != 0 {
			t.Fatalf("a thread waiting again added %v, want none", got)
		}
	})
	t.Run("a wait counted as it went is not counted again when it ends", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {slices: 3, runnable: true}})
		for range 5 {
			r.look(w, w, nil)
		}
		// Its 510 ms wait ends 10 ms into the look; it runs 1 ms, and then
		// sleeps on its own: it could run the 90 ms after its wait.
		if got := r.look(w, w, adds(1, ms, 510*ms, 1, false)); got != 90*ms {
			t.Fatalf("the look its wait ended in added %v, want 90ms", got)
		}
		// A later wait, begun and ended within a look (80 ms, then 20 ms
		// run): all of it is new, none of the first is taken off it.
		if got := r.look(w, w, adds(1, 20*ms, 80*ms, 1, false)); got != 20*ms {
			t.Fatalf("a later wait's look added %v, want 20ms", got)
		}
	})
	t.Run("a thread runnable at a look is taken to have waited for a CPU all of it that it did not run", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		// Asleep at the last look, woken since, and waiting for a CPU now:
		// when it woke is not known, so all of the look is taken for its
		// wait.
		if got := r.look(w, w, adds(1, 0, 0, 0, true)); got != 0 {
			t.Fatalf("added %v of a thread waiting for a CPU at the look, want none", got)
		}
		// Still waiting at the next look, given no CPU since: all of it.
		if got := r.look(w, w, nil); got != 0 {
			t.Fatalf("added %v of a thread waiting since the last look, want none", got)
		}
	})
	t.Run("a thread given the CPU since the last look and waiting for it again could run only what it ran", func(t *testing.T) {
		// Waiting for a CPU from the first look on: its wait ends 40 ms
		// into the next, it runs 5 ms, and waits again, through the look.
		// The kernel books that wait only once it ends.
		r := newClockRig(map[int]taskTime{1: {slices: 3, runnable: true}})
		if got := r.look(w, w, adds(1, 5*ms, 40*ms, 1, true)); got != 5*ms {
			t.Fatalf("the look it ran in added %v, want the 5ms it ran", got)
		}
		for i := range 3 {
			if got := r.look(w, w, nil); got != 0 {
				t.Fatalf("look %d added %v of a thread still waiting, want none", i, got)
			}
		}
		// That wait ends 40 ms into a look, 395 ms long; it runs 5 ms, and
		// waits again: what of the wait was counted is not counted again.
		if got := r.look(w, w, adds(1, 5*ms, 395*ms, 1, true)); got != 5*ms {
			t.Fatalf("the next look it ran in added %v, want the 5ms it ran", got)
		}
		// It ends 30 ms into the next, 85 ms long, and it runs 5 ms and
		// sleeps on its own: it could run the 70 ms after its wait.
		if got := r.look(w, w, adds(1, 5*ms, 85*ms, 1, false)); got != 70*ms {
			t.Fatalf("the look it went to sleep in added %v, want 70ms", got)
		}
	})
	t.Run("a look after a read held up is no longer than the time since that read", func(t *testing.T) {
		// A thread spinning on a CPU it shares. keploy, held off the CPU
		// between reading the wall clock and reading the thread's account,
		// reads it 6 ms late: of the 106 ms since the last read, the thread
		// ran 53 and waited 53, given the CPU just then.
		r := newClockRig(map[int]taskTime{1: {slices: 1, runnable: true}})
		r.readTakes = 6 * ms
		got := r.look(w, w, adds(1, 53*ms, 53*ms, 5, true))
		r.readTakes = 0
		// 94 ms after that read, it ran 47 and waited 47 since, and has
		// just gone to sleep: it could run only the 47 ms it ran.
		got += r.look(94*ms, w, adds(1, 47*ms, 47*ms, 5, false))
		if got != 100*ms {
			t.Fatalf("added %v of a thread that ran 100ms and waited 100ms of the 200ms between the reads of it, want the 100ms it ran", got)
		}
	})
	t.Run("a wait counted as it goes past a read held up is counted no longer than it was", func(t *testing.T) {
		// The same, read 6 ms late: of the 106 ms since the last read, it
		// ran 53 and waited 53, given the CPU just then.
		r := newClockRig(map[int]taskTime{1: {slices: 1, runnable: true}})
		r.readTakes = 6 * ms
		got := r.look(w, w, adds(1, 53*ms, 53*ms, 5, true))
		r.readTakes = 0
		// 94 ms after that read, it ran 40 and waited 44 since, and has
		// waited again for 10 ms, a wait the kernel has not booked yet.
		got += r.look(94*ms, w, adds(1, 40*ms, 44*ms, 4, true))
		// That wait ends 10 ms into the next look (20 ms long), and the
		// thread runs the 90 ms after it and has just gone to sleep: of the
		// wait its account books now, the look takes off as counted before
		// only the 10 ms the last look counted, not 16: the 6 ms the read
		// before it was held up are no wait of the thread's.
		got += r.look(w, w, adds(1, 90*ms, 20*ms, 1, false))
		if got != 183*ms {
			t.Fatalf("added %v of a thread that ran 183ms and never slept, want the 183ms it ran", got)
		}
	})
	t.Run("a look is the least time between the reads of a thread at both looks", func(t *testing.T) {
		// Two threads asleep, each read in 1 ms: at the first look from 0
		// to 1 ms and from 1 to 2 ms; at the next, held up 6 ms first, from
		// 106 to 107 and 107 to 108; at the last, the second held up 5 ms,
		// from 200 to 201 and 206 to 207. The look is 105 ms, then 93 ms,
		// the least time between a thread's two reads: neither waited at all.
		base := time.Unix(1000, 0)
		var wall time.Duration
		reads := [][2]time.Duration{{0, 0}, {6 * ms, 0}, {0, 5 * ms}}
		look := 0
		read := func([]Reached) (map[int]taskTime, bool) {
			m := map[int]taskTime{}
			for i, tid := range []int{1, 2} {
				wall += reads[look][i]
				from := base.Add(wall)
				wall += ms
				m[tid] = taskTime{slices: 3, from: from, to: base.Add(wall)}
			}
			look++
			return m, true
		}
		own := &stall.ManualClock{}
		c := newExitClock(nil, time.Hour, time.Hour, time.Hour, own, func() time.Time { return base.Add(wall) }, read,
			func() (diskMark, bool) { return nil, false }, nil)
		for _, at := range []time.Duration{100 * ms, 200 * ms} {
			wall = at
			own.Advance(time.Hour)
			c.Runnable()
		}
		if c.ran != 198*ms {
			t.Fatalf("counted %v of two threads asleep, want 198ms: the least time between each one's two reads, 105ms and 93ms", c.ran)
		}
	})
	t.Run("the waits a thread's account booked after it was read count in the look", func(t *testing.T) {
		// Asleep at the look, it waited 20 ms and ran 10 since the last;
		// and as its state was read its account booked 15 ms more.
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		got := r.look(w, w, func(m map[int]taskTime) {
			adds(1, 10*ms, 20*ms, 1, false)(m)
			th := m[1]
			th.missed = 15 * ms
			m[1] = th
		})
		if got != 65*ms {
			t.Fatalf("added %v, want 65ms: neither the 20ms nor the 15ms it waited", got)
		}
	})
	t.Run("a run a look counted as a thread went to sleep is not counted again", func(t *testing.T) {
		// On a CPU since its last tick, 4 ms, as its account is read, and
		// asleep as its state is: the look counts all of it as time it
		// could run, and its account read again holds those 4 ms.
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		got := r.look(w, w, func(m map[int]taskTime) {
			adds(1, 10*ms, 20*ms, 1, false)(m)
			th := m[1]
			th.ranAgain = th.ran + 4*ms
			m[1] = th
		})
		if got != 80*ms {
			t.Fatalf("the look it went to sleep in added %v, want the 80ms it did not wait", got)
		}
		// Waiting for a CPU since, given none: its account books the 4 ms
		// now, which are no part of this look.
		got = r.look(w, w, func(m map[int]taskTime) {
			adds(1, 4*ms, 0, 0, true)(m)
			th := m[1]
			th.ranAgain = 0
			m[1] = th
		})
		if got != 0 {
			t.Fatalf("the look after added %v of a thread waiting for a CPU throughout, want none", got)
		}
	})
	t.Run("a look takes no read times it cannot vouch for", func(t *testing.T) {
		// Two threads asleep, each read in 1 ms, at four looks. At the
		// second, 100 ms on and held up 6 ms first, the second thread's
		// read is not timed: the first's alone makes the look, 106 - 1. At
		// the third, 200 ms on, the first is held up 4 ms and its id names
		// another thread now (its account below the last), and the second
		// was not timed at the last look: none makes it, so it is from the
		// end of the last read of them all to the start of this one, 200 -
		// 107. At the fourth, 300 ms on, the first's read is timed at 250
		// ms, before the look began: not taken; the second's makes it, 300
		// - 206.
		base := time.Unix(1000, 0)
		var wall time.Duration
		at := func(d time.Duration) time.Time { return base.Add(d) }
		look := 0
		read := func([]Reached) (map[int]taskTime, bool) {
			one, two := taskTime{ran: 9, slices: 3}, taskTime{slices: 3}
			switch look {
			case 0:
				one.from, one.to, two.from, two.to = at(0), at(ms), at(ms), at(2*ms)
				wall = 2 * ms
			case 1:
				one.from, one.to = at(106*ms), at(107*ms)
				wall = 107 * ms
			case 2:
				one.ran = 1
				one.from, one.to, two.from, two.to = at(204*ms), at(205*ms), at(205*ms), at(206*ms)
				wall = 206 * ms
			case 3:
				one.ran = 1
				one.from, one.to, two.from, two.to = at(250*ms), at(251*ms), at(300*ms), at(301*ms)
				wall = 301 * ms
			}
			look++
			return map[int]taskTime{1: one, 2: two}, true
		}
		own := &stall.ManualClock{}
		c := newExitClock(nil, time.Hour, time.Hour, time.Hour, own, func() time.Time { return base.Add(wall) }, read,
			func() (diskMark, bool) { return nil, false }, nil)
		for _, start := range []time.Duration{100 * ms, 200 * ms, 300 * ms} {
			wall = start
			own.Advance(time.Hour)
			c.Runnable()
		}
		if want := (106-1)*ms + (200-107)*ms + (300-206)*ms; c.ran != want {
			t.Fatalf("counted %v of two threads asleep, want %v", c.ran, want)
		}
	})
	t.Run("a stall started again counts no sleep taken for a wait before it", func(t *testing.T) {
		// Asleep for 95 ms of a look, then woken and waiting for a CPU: all
		// of the look is taken for its wait. The processes make progress
		// there, and the stall starts again.
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		m := newExitMeter(r.c, time.Hour)
		m.Advance(ExitMark{Running: 2})
		r.look(w, w, adds(1, 0, 0, 0, true))
		if !m.Advance(ExitMark{Running: 1}) {
			t.Fatal("a process gone was no progress")
		}
		// Its 10 ms wait ends 5 ms into the next look; it runs 5 ms and
		// sleeps the 90 ms left: it could run 95 ms of it. Of its wait, the
		// 5 ms before the stall began count in it too, as a wait.
		r.look(w, w, adds(1, 5*ms, 10*ms, 1, false))
		if got := m.Stalled(); got != 90*ms {
			t.Fatalf("the stall counted %v of a thread that could run 95ms of it, want 90ms: none of its sleep before the stall began", got)
		}
	})
	t.Run("a stop's first stall counts no sleep taken for a wait before it", func(t *testing.T) {
		// Asleep at the clock's first look; 2 ms on, as the stop's meter is
		// made, woken and waiting for a CPU: both are taken for its wait.
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		r.wall = r.wall.Add(2 * ms)
		r.own.Advance(2 * ms)
		adds(1, 0, 0, 0, true)(r.tasks)
		m := newExitMeter(r.c, time.Hour)
		// Its 2 ms wait ends; it runs 4 ms and sleeps 95: it could run 99
		// ms of the look. Of its wait, the 1 ms before the stall began
		// counts in it too, as a wait.
		r.look(w, w, adds(1, 4*ms, 2*ms, 1, false))
		if got := m.Stalled(); got != 98*ms {
			t.Fatalf("the first stall counted %v of a thread that could run 99ms of it, want 98ms: none of its sleep before the stall began", got)
		}
	})
	t.Run("a thread on a loaded host could run what it did not wait", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {}})
		for i := range 5 {
			if got := r.look(w, w, adds(1, 10*ms, 90*ms, 5, i%2 == 0)); got != 10*ms {
				t.Fatalf("look %d added %v, want 10ms", i, got)
			}
		}
	})
	t.Run("a thread spinning on a quarter of a CPU could run a quarter", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {runnable: true}})
		for i := range 5 {
			if got := r.look(w, w, adds(1, 25*ms, 75*ms, 10, true)); got != 25*ms {
				t.Fatalf("look %d added %v, want 25ms", i, got)
			}
		}
	})
	t.Run("eight threads each on a quarter of a CPU could run all of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{})
		for tid := range 8 {
			r.tasks[tid] = taskTime{runnable: true}
		}
		for i := range 5 {
			got := r.look(w, w, func(m map[int]taskTime) {
				for tid := range 8 {
					adds(tid, 25*ms, 75*ms, 10, true)(m)
				}
			})
			if got != w {
				t.Fatalf("look %d added %v, want %v: they had two CPUs' worth", i, got, w)
			}
		}
		// keploy's clock, sampled, may catch up more than w between two
		// looks: the processes still could run no more than w of it.
		got := r.look(w, w+50*ms, func(m map[int]taskTime) {
			for tid := range 8 {
				adds(tid, 25*ms, 75*ms, 10, true)(m)
			}
		})
		if got != w {
			t.Fatalf("a look keploy's clock caught up in added %v, want %v", got, w)
		}
	})
	t.Run("a thread alone on a CPU, running throughout, could run all of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {slices: 1, runnable: true}})
		for i := range 5 {
			if got := r.look(w, w, adds(1, w, 0, 0, true)); got != w {
				t.Fatalf("look %d added %v, want %v", i, got, w)
			}
		}
	})
	t.Run("one of the processes held off the CPU holds the stall of all", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {}, 2: {runnable: true}})
		for i := range 5 {
			if got := r.look(w, w, nil); got != 0 {
				t.Fatalf("look %d added %v, want none while thread 2 waits", i, got)
			}
		}
	})
	t.Run("keploy held off the CPU counts none of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {}})
		if got := r.look(w, 0, nil); got != 0 {
			t.Fatalf("added %v while keploy could not run, want none", got)
		}
		if got := r.look(w, 30*ms, nil); got != 30*ms {
			t.Fatalf("added %v while keploy could run 30ms, want that", got)
		}
	})
	t.Run("with no account of the processes it is keploy's runnable time", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {runnable: true}})
		r.ok = false
		if got := r.look(w, 70*ms, nil); got != 70*ms {
			t.Fatalf("added %v, want keploy's 70ms", got)
		}
	})
	t.Run("a look that read nothing sets the next one's account against none", func(t *testing.T) {
		// A thread a second old, asleep; one look reads nothing.
		r := newClockRig(map[int]taskTime{1: {slices: 3, age: 10 * w}})
		r.ok = false
		if got := r.look(w, w, nil); got != w {
			t.Fatalf("the look that read nothing added %v, want keploy's %v", got, w)
		}
		// Read again: across both looks it ran 150 ms, and waited 50 ms
		// for a CPU, all of it in this one. Set against the account read
		// before the look that read nothing, the 150 ms would fill this
		// look; of the CPU it had since, this look holds none it cannot
		// have had before, and it waited 50 ms of it.
		r.ok = true
		if got := r.look(w, w, adds(1, 150*ms, 50*ms, 2, false)); got != 50*ms {
			t.Fatalf("the look after one that read nothing added %v, want the 50ms it did not wait", got)
		}
	})
	t.Run("a thread new since the last look counts its whole account", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {}})
		if got := r.look(w, w, func(m map[int]taskTime) { m[2] = taskTime{waited: w, slices: 1} }); got != 0 {
			t.Fatalf("a new thread that waited all of it left %v, want none", got)
		}
		// A thread id given to another meanwhile: its account is below the
		// last, and counts whole.
		if got := r.look(w, w, func(m map[int]taskTime) { m[2] = taskTime{ran: 40 * ms, waited: 60 * ms, slices: 1} }); got != 40*ms {
			t.Fatalf("a thread id given to another added %v, want the 40ms it ran", got)
		}
		// A thread new since the last look and waiting for a CPU at this
		// one, its age not known: all of the look it did not run is taken
		// for its wait.
		if got := r.look(w, w, func(m map[int]taskTime) { m[3] = taskTime{ran: 10 * ms, waited: 30 * ms, slices: 1, runnable: true} }); got != 10*ms {
			t.Fatalf("a new thread waiting for a CPU added %v, want the 10ms it ran", got)
		}
		// Its id given to another, which waited 20 ms, ran 5 and sleeps:
		// what was counted of the first one's wait is not the second's.
		if got := r.look(w, w, func(m map[int]taskTime) { m[3] = taskTime{ran: 5 * ms, waited: 20 * ms, slices: 1} }); got != 80*ms {
			t.Fatalf("a thread id given to another after a wait added %v, want the 80ms after its wait", got)
		}
	})
	t.Run("a thread new since the last look waited for a CPU no longer than it lived", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		// Started 12 ms before the look (a shell's child, at its exec), it
		// ran 2 ms and waits for a CPU: it waited 10 ms at most.
		if got := r.look(w, w, func(m map[int]taskTime) {
			m[2] = taskTime{ran: 2 * ms, waited: ms, slices: 1, runnable: true, age: 12 * ms}
		}); got != 90*ms {
			t.Fatalf("a thread 12ms old, waiting for a CPU, added %v, want 90ms", got)
		}
		// That one gone, another new to the look but older than it (a
		// thread of a process the stop reads from now on), waiting for a
		// CPU: the 2 ms it ran in its life may all have been before the
		// look, which holds all of the look for its wait.
		if got := r.look(w, w, func(m map[int]taskTime) {
			delete(m, 2)
			m[3] = taskTime{ran: 2 * ms, slices: 1, runnable: true, age: 3 * w}
		}); got != 0 {
			t.Fatalf("a thread new to the look, older than it, waiting for a CPU, added %v, want none", got)
		}
		// Its wait ends 40 ms into the next look, 240 ms long: it may have
		// begun before the first look, of which no more than the look was
		// counted. It runs 5 ms and sleeps: the rest of that wait is taken
		// off this look.
		if got := r.look(w, w, adds(3, 5*ms, 240*ms, 1, false)); got != 5*ms {
			t.Fatalf("the look the wait of a thread older than the look it was new to ended in added %v, want the 5ms it ran", got)
		}
	})
	t.Run("a thread new to the look but older than it ran in it no more than it cannot have run before", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		// 300 ms old, 250 ms on a CPU in its life, waiting for one now:
		// at least 50 of those in the look, and the rest of it a wait.
		if got := r.look(w, w, func(m map[int]taskTime) {
			m[2] = taskTime{ran: 250 * ms, waited: 40 * ms, slices: 9, runnable: true, age: 3 * w}
		}); got != 50*ms {
			t.Fatalf("a thread 300ms old with 250ms on a CPU added %v, want the 50ms of it the look holds", got)
		}
		// Asleep, the same: the 250 ms may have been before the look but
		// for 50, and its 40 ms of waits all in it.
		r = newClockRig(map[int]taskTime{1: {slices: 3}})
		if got := r.look(w, w, func(m map[int]taskTime) {
			m[2] = taskTime{ran: 250 * ms, waited: 40 * ms, slices: 9, age: 3 * w}
		}); got != 60*ms {
			t.Fatalf("a sleeping thread 300ms old with 250ms on a CPU added %v, want the 60ms it did not wait", got)
		}
	})
}

// However late keploy reads a thread's account after the wall time it read
// before it, and however long the read takes, the clock counts no more of the
// time than the thread could run, from its first look or from any start of a
// stall (ExitMeter.Reset). Of a thousand random timelines of a thread taking
// turns on a CPU it shares, and now and then asleep, each read 41 times (at
// the clock's start, then at 40 looks), the read held up before and after it
// by as much as 8 ms one time in ten and inside it by as much as 3 ms one
// time in twenty, timed, or not, for the thread alone, and its state read
// after its account (and its account again after that, where its state shows
// it not runnable), each held up as much, the clock counted none more than
// the thread ran and slept from the read at the start, or a stall's start, to
// the read at any look after, but for what it ran before that read that the
// kernel booked only after it (the run since the last scheduler tick: a tick
// at most). Measured from the wall time before one read to the wall time
// before the next, with what it counted of the waits as they went kept across
// a stall's start, and no account read again after the state (the clock
// before), the clock counted 158 of them more from its start, by up to 10.1
// ms, and 262 from its start or a stall's, by up to 13.3 ms; with that count
// kept alone, 67 from a stall's start, by up to 8.6 ms; with no account read
// again alone, 6, by up to 4.8 ms.
func TestTheExitClockCountsNoMoreThanAThreadCouldRunHoweverLateItsReadsCome(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	held := func(n int, most time.Duration) time.Duration {
		if rng.Intn(n) == 0 {
			return time.Duration(rng.Int63n(int64(most)))
		}
		return time.Duration(rng.Intn(20)) * time.Microsecond
	}
	for trial := range 1000 {
		life := randomTimeline(rng, 2*time.Second)
		timed := rng.Intn(2) == 0
		base := time.Unix(1000, 0)
		var wall, at, sinceTick time.Duration
		read := func([]Reached) (map[int]taskTime, bool) {
			wall += held(10, 8*time.Millisecond)
			from := wall
			wall += held(20, 3*time.Millisecond)
			at = wall
			tt, lag := life.account(at)
			sinceTick = lag
			wall += held(20, 3*time.Millisecond)
			to := wall
			// Its state, read after its account, and where that shows it
			// not runnable, its account again after that (readTaskTimesIn).
			wall += held(20, 3*time.Millisecond)
			state, _ := life.account(wall)
			tt.runnable = state.runnable
			if !tt.runnable {
				wall += held(20, 3*time.Millisecond)
				if again, _ := life.account(wall); again.ran != tt.ran || again.waited != tt.waited || again.slices != tt.slices {
					tt.missed = max(again.waited-tt.waited, 0)
					tt.turned = again.slices != tt.slices
					tt.ranAgain = again.ran
				}
			}
			if timed {
				tt.from, tt.to = base.Add(from), base.Add(to)
			}
			wall += held(10, 8*time.Millisecond)
			return map[int]taskTime{1: tt}, true
		}
		own := &stall.ManualClock{}
		c := newExitClock(nil, time.Hour, time.Hour, time.Hour, own, func() time.Time { return base.Add(wall) }, read,
			func() (diskMark, bool) { return nil, false }, nil)
		// The stall's starts: the clock's, and those after a look the test
		// starts it again at (as ExitMeter.Reset does), each with its count
		// so far, the moment its read took the account, and the run it held
		// that the kernel booked only after.
		type start struct {
			ran, at, sinceTick time.Duration
		}
		starts := []start{{c.ran, at, sinceTick}}
		for range 40 {
			wall += time.Duration(rng.Intn(25000)+500) * time.Microsecond
			own.Advance(time.Hour)
			c.Runnable()
			for _, st := range starts {
				if counted, could := c.ran-st.ran, life.could(st.at, at)+st.sinceTick; counted > could {
					t.Fatalf("timeline %d (reads timed: %v): from %v to %v the clock counted %v, more than the %v the thread could run", trial, timed, st.at, at, counted, could)
				}
			}
			if rng.Intn(8) == 0 {
				c.restarted()
				starts = append(starts, start{c.ran, at, sinceTick})
			}
		}
	}
}

// timeline is a thread's life as spans in which it is on a CPU ('R'),
// waiting for one ('W'), or asleep ('S').
type timeline []lifeSpan

// lifeSpan is a span of a thread's life (timeline).
type lifeSpan struct {
	state    byte
	from, to time.Duration
}

// timelineTick is the scheduler's tick (HZ=250), at which the kernel books a
// thread's time on a CPU so far.
const timelineTick = 4 * time.Millisecond

// randomTimeline is a thread that takes turns on a CPU it shares for d and
// more: turns and waits of up to 6 ms, and after one turn in twenty a sleep
// of up to 3 ms, after which it waits for the CPU or is given it at once.
func randomTimeline(rng *rand.Rand, d time.Duration) timeline {
	var l timeline
	state := byte('R')
	for at := time.Duration(0); at < d+time.Second; {
		n := 6000
		if state == 'S' {
			n = 3000
		}
		to := at + time.Duration(rng.Intn(n)+1)*time.Microsecond
		l = append(l, lifeSpan{state, at, to})
		at = to
		switch {
		case state == 'R' && rng.Intn(20) == 0:
			state = 'S'
		case state == 'R':
			state = 'W'
		case state == 'W' || rng.Intn(2) == 0:
			state = 'R'
		default:
			state = 'W'
		}
	}
	return l
}

// account is the kernel's account of the thread at t: each wait booked once
// it ends, as the thread is given the CPU, and its time on one at each tick
// and at the end of each turn; and what it has run since the last tick, on a
// CPU at t, that the account does not show yet.
func (l timeline) account(t time.Duration) (tt taskTime, sinceTick time.Duration) {
	for _, s := range l {
		if s.from >= t {
			break
		}
		if t < s.to {
			tt.runnable = s.state != 'S'
		}
		switch {
		case s.state == 'R':
			tt.slices++
			if s.to <= t {
				tt.ran += s.to - s.from
				break
			}
			tick := max(t/timelineTick*timelineTick, s.from)
			tt.ran += tick - s.from
			sinceTick = t - tick
		case s.state == 'W' && s.to <= t:
			tt.waited += s.to - s.from
		}
	}
	return tt, sinceTick
}

// could is how long, from a to b, the thread ran or slept.
func (l timeline) could(a, b time.Duration) (d time.Duration) {
	for _, s := range l {
		if s.state != 'W' {
			d += max(min(s.to, b)-max(s.from, a), 0)
		}
	}
	return d
}

// A thread held in the kernel (D) waits for no CPU, and the kernel does not
// say on what it waits. While the host's storage serves I/O, the time it
// served is taken for a wait on it (a disk a loaded host keeps busy serves
// others first); the time storage served nothing counts, as a wait in the
// kernel then is on no storage, and one held there for ever must still let
// the stop go on. So does all of it where a device stood still with I/O in
// flight (dead storage), or where the thread can reach storage the host's
// disks do not show (a network filesystem).
func TestTheExitClockTakesAWaitInTheKernelOnStorageThatServesForAWait(t *testing.T) {
	const w = 100 * time.Millisecond
	ms := time.Millisecond
	held := taskTime{ran: ms, slices: 3, blocked: true, seen: true}
	t.Run("held in the kernel while no storage serves, it could run all of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: held})
		for i := range 5 {
			if got := r.look(w, w, nil); got != w {
				t.Fatalf("look %d added %v of a thread held in the kernel with storage serving nothing, want %v", i, got, w)
			}
		}
	})
	t.Run("held in the kernel while storage serves throughout, it waited all of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: held})
		for i := range 5 {
			r.serves(w)
			if got := r.look(w, w, nil); got != 0 {
				t.Fatalf("look %d added %v of a thread held in the kernel while storage served throughout, want none", i, got)
			}
		}
	})
	t.Run("held in the kernel while storage serves part of it, it could run the rest", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: held})
		r.serves(30 * ms)
		if got := r.look(w, w, nil); got != 70*ms {
			t.Fatalf("added %v with storage serving 30ms of it, want 70ms", got)
		}
	})
	t.Run("of the time it neither ran nor waited for a CPU, what storage served is a wait", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: held})
		// It waited 20 ms for a CPU, ran 10 and went into the kernel: of
		// the 70 ms left, storage served all.
		r.serves(w)
		got := r.look(w, w, func(m map[int]taskTime) {
			adds(1, 10*ms, 20*ms, 1, false)(m)
			th := m[1]
			th.blocked, th.seen = true, true
			m[1] = th
		})
		if got != 10*ms {
			t.Fatalf("added %v, want the 10ms it ran", got)
		}
	})
	t.Run("a thread new since the last look waited on storage no longer than it lived", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		// Started 50 ms before the look, it waited 10 for a CPU, ran 10
		// and went into the kernel: of the 30 left, storage served all.
		r.serves(w)
		if got := r.look(w, w, func(m map[int]taskTime) {
			m[2] = taskTime{ran: 10 * ms, waited: 10 * ms, slices: 1, blocked: true, seen: true, age: 50 * ms}
		}); got != 60*ms {
			t.Fatalf("added %v, want the 50ms before it began and the 10ms it ran", got)
		}
	})
	t.Run("storage that had I/O in flight longer than the look served no more than the look", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: held, 2: {slices: 1}})
		r.serves(3 * w)
		if got := r.look(w, w, nil); got != 0 {
			t.Fatalf("added %v, want none", got)
		}
	})
	// A device standing still: I/O in flight, none completed, its busy
	// time not moved (a kernel that moves it only as requests start and
	// end shows a device so whose requests outlast the look).
	const bound = 3 * w
	standing := func() *clockRig {
		return newClockRigOf(map[int]taskTime{1: held}, bound, diskMark{"259:0": {}, "8:0": {done: 7, inFlight: 1}})
	}
	completes := func(r *clockRig, dev string) {
		n := r.disks[dev]
		n.done++
		n.busy += w
		r.disks[dev] = n
	}
	// turn is the held thread's turn on a CPU as its I/O completes, too
	// short to count here: its stretch in the kernel ends, so a test of a
	// device's stretch is not one of the thread's.
	turn := adds(1, 0, 0, 1, false)
	t.Run("a device standing still with I/O in flight serves, as a slow one does", func(t *testing.T) {
		r := standing()
		for i := range 2 {
			if got := r.look(w, w, nil); got != 0 {
				t.Fatalf("look %d added %v while a device stood still with I/O in flight, want none", i, got)
			}
		}
	})
	t.Run("one that stood still for the stall bound of the time at stake is dead, and serves none while it stands, whatever another serves", func(t *testing.T) {
		r := standing()
		for i, want := range []time.Duration{0, 0, w, w} {
			r.serves(w)
			if got := r.look(w, w, nil); got != want {
				t.Fatalf("look %d of a device standing still added %v, want %v", i, got, want)
			}
		}
		completes(r, "8:0")
		if got := r.look(w, w, nil); got != 0 {
			t.Fatalf("the look in which it completed I/O added %v, want none", got)
		}
	})
	t.Run("a stretch standing still ends as the device completes I/O", func(t *testing.T) {
		r := standing()
		for i := range 8 {
			var step func(map[int]taskTime)
			if i%3 == 2 {
				completes(r, "8:0")
				step = turn
			}
			if got := r.look(w, w, step); got != 0 {
				t.Fatalf("look %d added %v of a device completing I/O every third look, want none", i, got)
			}
		}
	})
	t.Run("a stretch standing still is the device's own", func(t *testing.T) {
		r := standing()
		r.disks["8:16"] = diskCount{done: 3, inFlight: 1}
		r.look(w, w, nil) // both stand: one look of each's stretch
		for i := range 8 {
			completes(r, []string{"8:0", "8:16"}[i%2])
			if got := r.look(w, w, turn); got != 0 {
				t.Fatalf("look %d added %v of two devices standing still in turn, want none", i, got)
			}
		}
	})
	t.Run("time no thread waits in the kernel is not at stake", func(t *testing.T) {
		r := standing()
		r.tasks[1] = taskTime{ran: ms, slices: 3, seen: true}
		for range 5 {
			r.look(w, w, nil)
		}
		r.tasks[1] = held
		for i := range 2 {
			if got := r.look(w, w, nil); got != 0 {
				t.Fatalf("look %d in the kernel added %v, want none: the device's stretch began with it", i, got)
			}
		}
	})
	t.Run("the time at stake is no more than keploy could run", func(t *testing.T) {
		r := standing()
		for i := range 5 {
			if got := r.look(w, w/4, nil); got != 0 {
				t.Fatalf("look %d added %v, want none", i, got)
			}
		}
	})
	// An encrypted volume (dm-1) on an LVM volume (dm-0) on a partition of
	// a disk, which another partition shares.
	stacked := func() *clockRig {
		r := newClockRigOf(map[int]taskTime{1: held}, bound, diskMark{
			"253:1": {name: "dm-1", done: 2, inFlight: 1}, "253:0": {name: "dm-0", done: 5, inFlight: 1},
			"259:3": {name: "nvme0n1p3", done: 10, inFlight: 1}, "259:1": {name: "nvme0n1p1"}, "259:0": {name: "nvme0n1", done: 40, inFlight: 1},
		})
		r.below = map[string][]string{"dm-1": {"dm-0"}, "dm-0": {"nvme0n1p3"}, "nvme0n1p3": {"nvme0n1"}, "nvme0n1p1": {"nvme0n1"}}
		return r
	}
	t.Run("a volume standing still on a disk completing I/O serves, however long it stands", func(t *testing.T) {
		r := stacked()
		for i := range 12 {
			completes(r, "259:0") // the disk, for another partition's I/O
			if got := r.look(w, w, turn); got != 0 {
				t.Fatalf("look %d added %v while the volumes stood still on a disk completing I/O, want none", i, got)
			}
		}
		for name, n := range r.belowRead {
			if n != 1 {
				t.Errorf("the devices below %s were read %d times, want once", name, n)
			}
		}
	})
	t.Run("a volume standing still on a disk standing still too is dead once they stood still for the bound", func(t *testing.T) {
		r := stacked()
		for i, want := range []time.Duration{0, 0, w, w} {
			if got := r.look(w, w, nil); got != want {
				t.Fatalf("look %d of volumes and their disk standing still added %v, want %v", i, got, want)
			}
		}
	})
	// A thread held in the kernel at two looks, with no turn on a CPU between
	// them (its slices unchanged), stood in the kernel throughout. What it
	// waits on the kernel does not say: busy storage serves it at last, but a
	// wait on no storage (a lock a wedged driver holds, a frozen cgroup) is
	// waited on for ever while the host's storage serves others.
	serving := func() *clockRig { return newClockRigOf(map[int]taskTime{1: held}, bound, diskMark{"259:0": {}}) }
	t.Run("one that stood in the kernel for the stall bound of the time at stake is wedged, and counts while it stands, whatever storage serves", func(t *testing.T) {
		r := serving()
		for i, want := range []time.Duration{0, 0, 0, w, w} {
			r.serves(w)
			if got := r.look(w, w, nil); got != want {
				t.Fatalf("look %d of a thread standing in the kernel, storage serving, added %v, want %v", i, got, want)
			}
		}
		// Its turn on a CPU ends the stretch: it ran 1 ms, and the rest of
		// the look was a wait on storage that served.
		r.serves(w)
		if got := r.look(w, w, adds(1, ms, 0, 1, false)); got != ms {
			t.Fatalf("the look in which it had a turn on a CPU added %v, want the %v it ran", got, ms)
		}
		r.serves(w)
		if got := r.look(w, w, nil); got != 0 {
			t.Fatalf("the look after its turn added %v, want none: its stretch began again", got)
		}
	})
	t.Run("a turn on a CPU as its state is read ends its stretch too", func(t *testing.T) {
		// Held in the kernel at each look, its account the same at each,
		// but given a CPU as its state was read (turned): it stands in the
		// kernel at no look, and storage that serves holds all of it.
		r := serving()
		for i := range 6 {
			r.serves(w)
			got := r.look(w, w, func(m map[int]taskTime) {
				th := m[1]
				th.turned = true
				m[1] = th
			})
			if got != 0 {
				t.Fatalf("look %d of a thread given a CPU as its state was read added %v, want none", i, got)
			}
		}
	})
	t.Run("a run a look counted as a thread went into the kernel is no time it neither ran nor waited after", func(t *testing.T) {
		// On a CPU since its last tick, 4 ms, as its account is read, and
		// held in the kernel as its state is, storage serving none of the
		// look: the look counts all it did not wait.
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		r.look(w, w, func(m map[int]taskTime) {
			adds(1, 10*ms, 20*ms, 1, false)(m)
			th := m[1]
			th.ranAgain, th.blocked, th.seen = th.ran+4*ms, true, true
			m[1] = th
		})
		// Held in the kernel since, storage serving throughout: its
		// account books those 4 ms now, which are no part of this look,
		// all of which was a wait on storage.
		r.serves(w)
		got := r.look(w, w, func(m map[int]taskTime) {
			adds(1, 4*ms, 0, 0, false)(m)
			th := m[1]
			th.ranAgain, th.blocked, th.seen = 0, true, true
			m[1] = th
		})
		if got != 0 {
			t.Fatalf("the look it was held in the kernel throughout, storage serving, added %v, want none", got)
		}
	})
	t.Run("one is taken for wedged no sooner than the kernel reports a task hung, where that is longer than the bound", func(t *testing.T) {
		r := newClockRigWedged(map[int]taskTime{1: held}, bound, 5*w, diskMark{"259:0": {}})
		for i, want := range []time.Duration{0, 0, 0, 0, 0, w, w} {
			r.serves(w)
			if got := r.look(w, w, nil); got != want {
				t.Fatalf("look %d of a thread standing in the kernel, storage serving, added %v, want %v", i, got, want)
			}
		}
	})
	t.Run("a stretch in the kernel ends as the thread has a turn on a CPU", func(t *testing.T) {
		r := serving()
		for i := range 12 {
			r.serves(w)
			step := func(map[int]taskTime) {}
			if i%3 == 2 {
				step = adds(1, ms, 0, 1, false)
			}
			if got := r.look(w, w, step); got > ms {
				t.Fatalf("look %d added %v of a thread whose waits in the kernel end every third look, want at most the %v it ran", i, got, ms)
			}
		}
	})
	t.Run("a stretch in the kernel is the thread's own", func(t *testing.T) {
		// Each has a turn every fourth look, one a look after the other:
		// each stands for three looks at most, short of a bound of four,
		// and at two looks in a row both stand. Taken from the other's
		// stretch, each would be taken for wedged there.
		r := newClockRigOf(map[int]taskTime{1: held, 2: held}, 4*w, diskMark{"259:0": {}})
		for i := range 12 {
			r.serves(w)
			step := func(map[int]taskTime) {}
			if i%4 < 2 {
				step = adds(1+i%4, ms, 0, 1, false)
			}
			if got := r.look(w, w, step); got > ms {
				t.Fatalf("look %d added %v of two threads whose waits in the kernel end in turn, want at most the %v one ran", i, got, ms)
			}
		}
	})
	t.Run("a stretch in the kernel grows by no more than keploy could run", func(t *testing.T) {
		r := serving()
		for i := range 8 {
			r.serves(w)
			if got := r.look(w, w/4, nil); got != 0 {
				t.Fatalf("look %d added %v of a thread standing in the kernel for %v of keploy's runnable time, want none", i, got, time.Duration(i+1)*w/4)
			}
		}
	})
	t.Run("a stretch in the kernel grows by none while a dead device takes all its time", func(t *testing.T) {
		// Two looks served, two with the device under it dead (all of them
		// counts already), then it serves again: up to the last look, the
		// thread had the stall bound of credit only after the fifth.
		r := standing()
		for i, want := range []time.Duration{0, 0, w, w} {
			r.serves(w)
			if got := r.look(w, w, nil); got != want {
				t.Fatalf("look %d added %v, want %v", i, got, want)
			}
		}
		completes(r, "8:0")
		if got := r.look(w, w, nil); got != 0 {
			t.Fatalf("the look in which the device completed I/O added %v, want none", got)
		}
		completes(r, "8:0")
		if got := r.look(w, w, nil); got != w {
			t.Fatalf("the look after it had the bound of credit added %v, want %v", got, w)
		}
	})
	t.Run("a thread that went into the kernel on the turn it had at the last look starts its stretch at the next", func(t *testing.T) {
		// On a CPU at the last look (runnable), it ran on into the kernel
		// on that turn: its slices are unchanged, but it was not held
		// there throughout the look. A bound of two and a half looks: the
		// look it went in on, counted, would take it there a look sooner.
		r := newClockRigOf(map[int]taskTime{1: {ran: ms, slices: 3, runnable: true}}, 5*w/2, diskMark{"259:0": {}})
		r.serves(w)
		r.look(w, w, func(m map[int]taskTime) {
			th := m[1]
			th.ran += 10 * ms
			th.runnable, th.blocked, th.seen = false, true, true
			m[1] = th
		})
		for i, want := range []time.Duration{0, 0, 0, w} {
			r.serves(w)
			if got := r.look(w, w, nil); got != want {
				t.Fatalf("look %d after it went into the kernel added %v, want %v", i, got, want)
			}
		}
	})
	t.Run("a thread new to a look starts no stretch in the kernel", func(t *testing.T) {
		r := serving()
		for i := range 2 {
			r.serves(w)
			r.look(w, w, nil)
			_ = i
		}
		// Thread 1 has a turn and stands again; thread 2 is new, and held.
		r.serves(w)
		r.look(w, w, func(m map[int]taskTime) {
			adds(1, 0, 0, 1, false)(m)
			m[2] = taskTime{slices: 1, blocked: true, seen: true, age: w}
		})
		for i := range 3 {
			r.serves(w)
			if got := r.look(w, w, nil); got != 0 {
				t.Fatalf("look %d added %v, want none: neither thread stood for the bound", i, got)
			}
		}
	})
	t.Run("one that can reach storage the host's disks do not show is taken to wait on none", func(t *testing.T) {
		unseen := held
		unseen.seen = false
		r := newClockRig(map[int]taskTime{1: unseen})
		r.serves(w)
		if got := r.look(w, w, nil); got != w {
			t.Fatalf("added %v of a thread that can reach a network filesystem, want %v", got, w)
		}
	})
	t.Run("one asleep on its own (S) could run all of it, whatever storage serves", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {ran: ms, slices: 3, seen: true}})
		r.serves(w)
		if got := r.look(w, w, nil); got != w {
			t.Fatalf("added %v of a sleeping thread, want %v", got, w)
		}
	})
	t.Run("a look between reads of the disks of which one read none has storage serving none", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: held})
		r.disksOK = false
		r.serves(w)
		if got := r.look(w, w, nil); got != w {
			t.Fatalf("the look that read no disks added %v, want %v", got, w)
		}
		r.disksOK = true
		r.serves(w)
		if got := r.look(w, w, nil); got != w {
			t.Fatalf("the look after one that read no disks added %v, want %v", got, w)
		}
		r.serves(w)
		if got := r.look(w, w, nil); got != 0 {
			t.Fatalf("a look between two reads of the disks added %v, want none", got)
		}
	})
}

// A thread whose waits in the kernel each end, making no progress between
// them, has a turn on a CPU between any two, and stands in the kernel for no
// stretch: a wait that ends does not say the next is not the same. Its waits
// there are credited, since the processes' last progress, for no more than
// the kernel's hung-task time of the time at stake in all, the stop's, of
// whichever of their threads; then every thread's time there counts, as
// while storage serves none, until their next progress.
func TestTheExitClockCreditsWaitsInTheKernelForTheHungTaskTimeInAll(t *testing.T) {
	const w = 100 * time.Millisecond
	const wedge = 3 * w
	held := taskTime{ran: time.Millisecond, slices: 3, blocked: true, seen: true}
	// A stop's clock: its credit in all is its hung-task time.
	stop := func(tasks map[int]taskTime) *clockRig {
		return newClockRigCredited(tasks, wedge, wedge, wedge, diskMark{"259:0": {}})
	}
	// turn is a thread's turn on a CPU between two of its waits in the
	// kernel, too short to count here: no stretch of it stands.
	turn := func(tid int, blocked bool) func(map[int]taskTime) {
		return func(m map[int]taskTime) {
			adds(tid, 0, 0, 1, false)(m)
			th := m[tid]
			th.blocked = blocked
			m[tid] = th
		}
	}
	t.Run("waits that each end are credited for the hung-task time, and count after", func(t *testing.T) {
		r := stop(map[int]taskTime{1: held})
		for i, want := range []time.Duration{0, 0, 0, w, w, w} {
			r.serves(w)
			if got := r.look(w, w, turn(1, true)); got != want {
				t.Fatalf("look %d of a thread in the kernel in turns, storage serving, added %v, want %v", i, got, want)
			}
		}
	})
	t.Run("the credit starts again at the processes' progress, and only there", func(t *testing.T) {
		r := stop(map[int]taskTime{1: held})
		for range 4 {
			r.serves(w)
			r.look(w, w, turn(1, true))
		}
		r.c.progressed()
		for i, want := range []time.Duration{0, 0, 0, w} {
			r.serves(w)
			if got := r.look(w, w, turn(1, true)); got != want {
				t.Fatalf("look %d after the processes' progress added %v, want %v", i, got, want)
			}
		}
	})
	t.Run("the credit is the processes', not one thread's", func(t *testing.T) {
		// Each is in the kernel at every other look, asleep (S) between:
		// counted by each thread's own waits, each would have the
		// hung-task time of credit, twice the stop's.
		r := stop(map[int]taskTime{1: held, 2: {ran: time.Millisecond, slices: 3, seen: true}})
		for i, want := range []time.Duration{0, 0, 0, w, w, w} {
			r.serves(w)
			step := func(m map[int]taskTime) { turn(1, i%2 == 1)(m); turn(2, i%2 == 0)(m) }
			if got := r.look(w, w, step); got != want {
				t.Fatalf("look %d of two threads in the kernel in turns, storage serving, added %v, want %v", i, got, want)
			}
		}
	})
	t.Run("the credit is spent by no more than keploy could run", func(t *testing.T) {
		r := stop(map[int]taskTime{1: held})
		for i := range 12 {
			r.serves(w)
			if got := r.look(w, w/4, turn(1, true)); got != 0 {
				t.Fatalf("look %d added %v, with %v of credit spent, want none", i, got, time.Duration(i)*w/4)
			}
		}
		r.serves(w)
		if got := r.look(w, w/4, turn(1, true)); got != w/4 {
			t.Fatalf("the look after the credit was spent added %v, want the %v keploy could run", got, w/4)
		}
	})
	t.Run("a thread standing in the kernel spends the credit too", func(t *testing.T) {
		// A credit of two looks, short of the hung-task time of three.
		r := newClockRigCredited(map[int]taskTime{1: held}, wedge, wedge, 2*w, diskMark{"259:0": {}})
		for i, want := range []time.Duration{0, 0, w, w} {
			r.serves(w)
			if got := r.look(w, w, nil); got != want {
				t.Fatalf("look %d of a thread standing in the kernel added %v, want %v", i, got, want)
			}
		}
	})
}

// The kernel reports a task hung once it has stood in an uninterruptible
// sleep, given no CPU, for its hung_task_timeout_secs; where it keeps none,
// or the detector is off (0), its default, two minutes.
func TestTheKernelsHungTaskTimeIsReadAsItWritesIt(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, file string
		want       time.Duration
	}{
		{"set", "30\n", 30 * time.Second},
		{"the default", "120\n", 2 * time.Minute},
		{"off", "0\n", hungTaskDefault},
		{"not a number", "x\n", hungTaskDefault},
		{"longer than a Duration holds", "18446744073709551615\n", time.Duration(math.MaxInt64)},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-"))
		if err := os.WriteFile(path, []byte(tc.file), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := hungTaskTime(path); got != tc.want {
			t.Errorf("%s (%q): %v, want %v", tc.name, tc.file, got, tc.want)
		}
	}
	if got := hungTaskTime(filepath.Join(dir, "none")); got != hungTaskDefault {
		t.Errorf("a kernel built without the detector: %v, want %v", got, hungTaskDefault)
	}
	if hungTaskDefault != 2*time.Minute {
		t.Errorf("the kernel's default hung-task time is two minutes, not %v", hungTaskDefault)
	}
	const host = "/proc/sys/kernel/hung_task_timeout_secs"
	if hostStorage.hung == nil {
		t.Fatal("a stop reads no hung-task time of this kernel")
	}
	if got, want := hostStorage.hung(), hungTaskTime(host); got != want {
		t.Errorf("a stop reads a hung-task time of %v, want this kernel's %v (%s)", got, want, host)
	}
}

// A stop's clock takes a thread standing in the kernel for wedged once it has
// stood there for the kernel's hung-task time, or for the stall bound where
// that is longer.
func TestAStopsClockTakesAThreadForWedgedAtTheKernelsHungTaskTime(t *testing.T) {
	for _, tc := range []struct {
		name         string
		hung         func() time.Duration
		bound, wedge time.Duration
	}{
		{"hung-task time longer", func() time.Duration { return time.Minute }, time.Second, time.Minute},
		{"bound longer", func() time.Duration { return time.Second }, time.Minute, time.Minute},
		{"none read", nil, time.Second, time.Second},
	} {
		st := hostStorage
		st.hung = tc.hung
		testStorage.Store(&st)
		c, release := NewExitClock(nil, tc.bound)
		release()
		testStorage.Store(nil)
		if c.wedge != tc.wedge {
			t.Errorf("%s: a thread is taken for wedged after %v, want %v", tc.name, c.wedge, tc.wedge)
		}
		if c.credit != tc.wedge {
			t.Errorf("%s: the waits in the kernel are credited for %v in all, want %v", tc.name, c.credit, tc.wedge)
		}
	}
}

// A kill names the bound it killed on as the clock measured it: of the time
// both the processes and keploy could run where its last look read their
// account; of keploy's runnable time where it did not.
func TestTheExitClockSaysWhatItMeasured(t *testing.T) {
	r := newClockRig(map[int]taskTime{1: {slices: 3}})
	if got, want := r.c.Said(time.Minute), "1m0s of its and keploy's runnable time"; got != want {
		t.Fatalf("read: said %q, want %q", got, want)
	}
	r.ok = false
	r.look(time.Second, time.Second, nil)
	if got, want := r.c.Said(time.Minute), "1m0s of keploy's runnable time"; got != want {
		t.Fatalf("not read: said %q, want %q", got, want)
	}
}

// Storage served, between two reads of the host's block devices, for the
// longest that a device which completed I/O, or has I/O in flight, had I/O
// in flight, at most the time between the reads; one with I/O in flight at
// both that completed none had it throughout. Those with I/O in flight that
// completed none stood still, but for one stacked on a device that completed
// I/O (a volume, a partition): its I/O is served there.
func TestStorageServedIsTheLongestADeviceServingWasBusy(t *testing.T) {
	const w = 100 * time.Millisecond
	ms := time.Millisecond
	// Every device under each, by name: an encrypted volume (dm-1) on an
	// LVM volume (dm-0) on a partition (nvme0n1p3) of a disk.
	under := map[string][]string{
		"dm-1":      {"dm-0", "nvme0n1p3", "nvme0n1"},
		"dm-0":      {"nvme0n1p3", "nvme0n1"},
		"nvme0n1p3": {"nvme0n1"},
	}
	for _, tc := range []struct {
		name        string
		before, now diskMark
		want        time.Duration
		still       []string
	}{
		{"none read", diskMark{}, diskMark{}, 0, nil},
		{"idle", diskMark{"8:0": {done: 5}}, diskMark{"8:0": {done: 5}}, 0, nil},
		{"given I/O since, completing none yet", diskMark{"8:0": {done: 5}}, diskMark{"8:0": {done: 5, inFlight: 2, busy: 40 * ms}}, 40 * ms, []string{"8:0"}},
		{"completing I/O", diskMark{"8:0": {done: 5, busy: 10 * ms}}, diskMark{"8:0": {done: 9, busy: 70 * ms}}, 60 * ms, nil},
		{"the longest of two", diskMark{"8:0": {done: 5}, "8:1": {done: 5}}, diskMark{"8:0": {done: 6, busy: 20 * ms}, "8:1": {done: 7, busy: 50 * ms}}, 50 * ms, nil},
		{"no more than the time between", diskMark{"8:0": {done: 5}}, diskMark{"8:0": {done: 6, busy: 3 * w}}, w, nil},
		{"a device new since", diskMark{}, diskMark{"8:0": {done: 6, busy: 50 * ms}}, 0, nil},
		{"one standing still with I/O in flight, its busy time not moved", diskMark{"8:0": {done: 5}, "8:16": {done: 3, inFlight: 1, busy: ms}}, diskMark{"8:0": {done: 6, busy: 50 * ms}, "8:16": {done: 3, inFlight: 1, busy: ms}}, w, []string{"8:16"}},
		{"one that drained its I/O", diskMark{"8:0": {done: 5}, "8:16": {done: 3, inFlight: 1}}, diskMark{"8:0": {done: 6, busy: 50 * ms}, "8:16": {done: 4, busy: 5 * ms}}, 50 * ms, nil},
		{"one given I/O since, completing none yet, and another completing", diskMark{"8:0": {done: 5}, "8:16": {done: 3}}, diskMark{"8:0": {done: 6, busy: 50 * ms}, "8:16": {done: 3, inFlight: 1, busy: ms}}, 50 * ms, []string{"8:16"}},
		{"two standing still", diskMark{"8:0": {done: 5, inFlight: 1}, "8:16": {done: 3, inFlight: 4}}, diskMark{"8:0": {done: 5, inFlight: 1}, "8:16": {done: 3, inFlight: 4}}, w, []string{"8:0", "8:16"}},
		{"its counts gone back (another device)", diskMark{"8:0": {done: 9, busy: w}}, diskMark{"8:0": {done: 2, inFlight: 1, busy: ms}}, 0, nil},
		{"its busy time gone back (another device)", diskMark{"8:0": {done: 2, busy: w}}, diskMark{"8:0": {done: 9, busy: ms}}, 0, nil},
		{"a volume standing still while the partition under it completes I/O",
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}, "259:3": {name: "nvme0n1p3", done: 10, inFlight: 1}},
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}, "259:3": {name: "nvme0n1p3", done: 12, inFlight: 1, busy: 15 * ms}}, w, nil},
		{"volumes stacked on one another, and a partition, standing still while the disk under them all completes I/O",
			diskMark{"253:1": {name: "dm-1", done: 2, inFlight: 1}, "253:0": {name: "dm-0", done: 5, inFlight: 3}, "259:3": {name: "nvme0n1p3", done: 10, inFlight: 1}, "259:0": {name: "nvme0n1", done: 40}},
			diskMark{"253:1": {name: "dm-1", done: 2, inFlight: 1}, "253:0": {name: "dm-0", done: 5, inFlight: 3}, "259:3": {name: "nvme0n1p3", done: 10, inFlight: 1}, "259:0": {name: "nvme0n1", done: 41, busy: 2 * ms}}, w, nil},
		{"a volume and the partition under it both standing still",
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}, "259:3": {name: "nvme0n1p3", done: 10, inFlight: 1}},
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}, "259:3": {name: "nvme0n1p3", done: 10, inFlight: 1}}, w, []string{"253:0", "259:3"}},
		{"a volume standing still on a partition idle",
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}, "259:3": {name: "nvme0n1p3", done: 10}},
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}, "259:3": {name: "nvme0n1p3", done: 10}}, w, []string{"253:0"}},
		{"a volume standing still on a partition whose counts went back (another device)",
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}, "259:3": {name: "nvme0n1p3", done: 10, busy: w}},
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}, "259:3": {name: "nvme0n1p3", done: 12, busy: ms}}, w, []string{"253:0"}},
		{"a volume standing still on a partition new since",
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}},
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}, "259:3": {name: "nvme0n1p3", done: 12, busy: ms}}, w, []string{"253:0"}},
		{"a volume standing still while a disk it is not on completes I/O",
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}, "8:0": {name: "sda", done: 10}},
			diskMark{"253:0": {name: "dm-0", done: 5, inFlight: 3}, "8:0": {name: "sda", done: 11, busy: ms}}, w, []string{"253:0"}},
	} {
		served, still := storageServed(w, tc.before, tc.now, func(name string) []string { return under[name] })
		if served != tc.want || !slices.Equal(still, tc.still) {
			t.Errorf("%s: served %v, standing still %v; want %v, %v", tc.name, served, still, tc.want, tc.still)
		}
	}
}

// The kernel's counters are unsigned 64-bit: one longer than a Duration holds
// is the longest Duration, never a negative one.
func TestAKernelCounterLongerThanADurationHoldsIsTheLongestOne(t *testing.T) {
	fits := uint64(math.MaxInt64 / int64(startTick))
	for _, tc := range []struct {
		n    uint64
		unit time.Duration
		want time.Duration
	}{
		{0, time.Millisecond, 0},
		{12, time.Millisecond, 12 * time.Millisecond},
		{math.MaxInt64, time.Nanosecond, math.MaxInt64},
		{math.MaxInt64 + 1, time.Nanosecond, math.MaxInt64},
		{math.MaxUint64, time.Nanosecond, math.MaxInt64},
		{fits, startTick, time.Duration(fits) * startTick},
		{fits + 1, startTick, math.MaxInt64},
		{math.MaxUint64, time.Millisecond, math.MaxInt64},
	} {
		if got := durationOf(tc.n, tc.unit); got != tc.want {
			t.Errorf("%d × %v: %v, want %v", tc.n, tc.unit, got, tc.want)
		}
	}
}

// /proc/diskstats is read whatever the kernel's count of fields: discards
// from Linux 4.18 and flushes from 5.5 are I/O completed too.
func TestDiskstatsAreReadAsEachKernelWritesThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diskstats")
	stats := strings.Join([]string{
		" 259       0 nvme0n1 100 1 800 50 200 2 1600 90 3 120 140 10 0 80 4 30 7",
		" 259       1 nvme0n1p1 10 0 80 5 20 0 160 9 0 12 14 1 0 8 0",
		"   8       0 sda 7 0 56 4 8 0 64 6 1 9 10",
		"   8      16 sdb 1 0 8 1 1 0 8 1 0 18446744073709551615 1",
		"   7       0 loop0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0",
		"garbage",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(stats), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := readDiskstats(path)
	if !ok {
		t.Fatal("not read")
	}
	ms := time.Millisecond
	want := diskMark{
		"259:0": {name: "nvme0n1", done: 100 + 200 + 10 + 30, inFlight: 3, busy: 120 * ms},
		"259:1": {name: "nvme0n1p1", done: 10 + 20 + 1, busy: 12 * ms},
		"8:0":   {name: "sda", done: 7 + 8, inFlight: 1, busy: 9 * ms},
		"8:16":  {name: "sdb", done: 1 + 1, busy: math.MaxInt64},
		"7:0":   {name: "loop0"},
	}
	if !maps.Equal(got, want) {
		t.Fatalf("read %+v, want %+v", got, want)
	}
	if _, ok := readDiskstats(filepath.Join(t.TempDir(), "none")); ok {
		t.Fatal("a missing file was read")
	}
	if got, ok := readDiskstats("/proc/diskstats"); ok && len(got) == 0 {
		t.Fatal("this host's diskstats read as no device")
	}
}

// A block device is stacked on the devices sysfs names its slaves (a
// device-mapper, md or bcache device's), and a partition on its disk; one
// sysfs cannot show is on none.
func TestABlockDeviceIsOnItsSlavesAndAPartitionOnItsDisk(t *testing.T) {
	sys := t.TempDir()
	devices := filepath.Join(sys, "devices")
	class := filepath.Join(sys, "class", "block")
	for _, dir := range []string{
		"devices/nvme0/nvme0n1/nvme0n1p3", "devices/nvme0/nvme0n1/nvme0n1p1",
		"devices/virtual/dm-0/slaves", "devices/virtual/dm-1/slaves",
		"devices/virtual/md0/slaves", "devices/cciss/cciss!c0d0/cciss!c0d0p1",
		"devices/virtual/loop0", "class/block",
	} {
		if err := os.MkdirAll(filepath.Join(sys, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"nvme0/nvme0n1/nvme0n1p3", "nvme0/nvme0n1/nvme0n1p1", "cciss/cciss!c0d0/cciss!c0d0p1"} {
		if err := os.WriteFile(filepath.Join(devices, f, "partition"), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{
		"nvme0n1": "nvme0/nvme0n1", "nvme0n1p3": "nvme0/nvme0n1/nvme0n1p3", "nvme0n1p1": "nvme0/nvme0n1/nvme0n1p1",
		"dm-0": "virtual/dm-0", "dm-1": "virtual/dm-1", "md0": "virtual/md0",
		"cciss!c0d0": "cciss/cciss!c0d0", "cciss!c0d0p1": "cciss/cciss!c0d0/cciss!c0d0p1", "loop0": "virtual/loop0",
	} {
		link(filepath.Join("..", "..", "devices", target), filepath.Join(class, name))
	}
	link(filepath.Join("..", "..", "..", "nvme0", "nvme0n1", "nvme0n1p3"), filepath.Join(devices, "virtual", "dm-0", "slaves", "nvme0n1p3"))
	link(filepath.Join("..", "..", "dm-0"), filepath.Join(devices, "virtual", "dm-1", "slaves", "dm-0"))
	link(filepath.Join("..", "..", "..", "nvme0", "nvme0n1", "nvme0n1p1"), filepath.Join(devices, "virtual", "md0", "slaves", "nvme0n1p1"))
	link(filepath.Join("..", "..", "..", "cciss", "cciss!c0d0", "cciss!c0d0p1"), filepath.Join(devices, "virtual", "md0", "slaves", "cciss!c0d0p1"))
	for name, want := range map[string][]string{
		"dm-1":         {"dm-0"},
		"dm-0":         {"nvme0n1p3"},
		"nvme0n1p3":    {"nvme0n1"},
		"nvme0n1":      nil,
		"md0":          {"cciss/c0d0p1", "nvme0n1p1"},
		"cciss/c0d0p1": {"cciss/c0d0"},
		"loop0":        nil,
		"sdz":          nil,
	} {
		got := storageBelow(class, name)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s is on %q, want %q", name, got, want)
		}
	}
	// This host's own, as a stop reads them: whatever it has, a partition
	// is on its disk.
	disks, _ := hostStorage.disks()
	for _, n := range disks {
		if _, err := os.Stat(filepath.Join("/sys/class/block", n.name, "partition")); err == nil {
			if got := hostStorage.below(n.name); len(got) != 1 || !strings.HasPrefix(n.name, got[0]) {
				t.Errorf("this host's partition %s is on %q, not its disk", n.name, got)
			}
		}
	}
}

// A thread's filesystems are on storage the host's disks show only where each
// mount of its namespace is on a block device, or on none: one served over a
// network or by a process (or of a type not known) is not, nor are mounts
// that cannot be read.
func TestAThreadSeesItsStorageOnlyWhereEveryMountIsOnTheHostsDisks(t *testing.T) {
	local := strings.Join([]string{
		"45 1 0:37 /root / rw,relatime shared:1 - btrfs /dev/nvme0n1p6 rw,subvol=/root",
		"47 45 0:41 / /usr/lib/firmware/vendor rw,relatime shared:2 - tmpfs vendorfw rw",
		"60 45 0:22 / /proc rw,nosuid shared:13 - proc proc rw",
		"61 60 0:50 / /proc/sys/fs/binfmt_misc rw,relatime shared:30 master:4 - autofs systemd-1 rw,fd=37",
		"70 45 0:60 / /var/lib/docker/overlay2/x/merged rw,relatime - overlay overlay rw,lowerdir=/a,upperdir=/b",
		"71 45 259:4 / /boot rw,relatime shared:3 - ext4 /dev/nvme0n1p4 rw",
		"72 45 0:61 / /mnt/my\\040dir rw,relatime - xfs /dev/sdb1 rw",
	}, "\n") + "\n"
	for _, tc := range []struct {
		name   string
		mounts string
		want   bool
	}{
		{"on disks and memory", local, true},
		{"and an NFS mount", local + "80 45 0:70 / /mnt/nfs rw,relatime shared:40 - nfs4 server:/export rw,vers=4.2\n", false},
		{"and a FUSE one", local + "81 45 0:71 / /run/user/1000/doc rw,nosuid shared:41 - fuse.portal portal rw\n", false},
		{"and one of a type not known", local + "82 45 0:72 / /mnt/x rw - lustre 10.0.0.1@tcp:/fs rw\n", false},
		{"a line without its separator", local + "83 45 0:73 / /mnt/y rw tmpfs\n", false},
		{"none", "", false},
	} {
		task := t.TempDir()
		if err := os.WriteFile(filepath.Join(task, "mountinfo"), []byte(tc.mounts), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := mountsSeen(task); got != tc.want {
			t.Errorf("%s: seen %v, want %v", tc.name, got, tc.want)
		}
	}
	if mountsSeen(filepath.Join(t.TempDir(), "gone")) {
		t.Error("a thread whose mounts cannot be read sees its storage")
	}
}

// fakeProc writes a /proc of processes under a temporary directory: each a
// leader state and start time, and threads of it, each a state, an account,
// the processes it started, its mounts, and its mount namespace (ns/mnt;
// none written where "").
type fakeThread struct {
	tid              int
	state            byte
	ran, wait, slice uint64
	children         string
	mounts, ns       string
}

func fakeProc(t *testing.T, procs map[int]struct {
	state   byte
	start   uint64
	threads []fakeThread
}) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, s string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(pid int, state byte, start uint64) string {
		// pid (comm) state, then fields 4..52: starttime is field 22.
		f := make([]string, 49)
		for i := range f {
			f[i] = "0"
		}
		f[22-4] = strconv.FormatUint(start, 10)
		return fmt.Sprintf("%d (app) %c %s\n", pid, state, strings.Join(f, " "))
	}
	for pid, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		write(filepath.Join(dir, "stat"), stat(pid, p.state, p.start))
		for _, th := range p.threads {
			tdir := filepath.Join(dir, "task", strconv.Itoa(th.tid))
			write(filepath.Join(tdir, "stat"), stat(th.tid, th.state, p.start))
			write(filepath.Join(tdir, "schedstat"), fmt.Sprintf("%d %d %d\n", th.ran, th.wait, th.slice))
			write(filepath.Join(tdir, "children"), th.children)
			if th.mounts != "" {
				write(filepath.Join(tdir, "mountinfo"), th.mounts)
			}
			if th.ns != "" {
				if err := os.MkdirAll(filepath.Join(tdir, "ns"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(th.ns, filepath.Join(tdir, "ns", "mnt")); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return root
}

// The account is read of every thread of each process as it was named (pid
// and start time), its leader exited or not, and of the processes it runs
// now: a Go process exiting has a zombie leader while its other threads,
// killed, wait for a CPU to get out, and a shutdown may wait on a child it
// started since the stop began. A pid that names another process now is not
// read. A thread held in the kernel (D) is neither runnable nor asleep on its
// own, and whether it can reach only storage the host's disks show is read
// of the mounts of its own namespace (once for threads that share one): a
// zombie leader's show none, and a thread may have a namespace of its own.
func TestTheExitClockReadsEveryThreadOfItsProcesses(t *testing.T) {
	type proc = struct {
		state   byte
		start   uint64
		threads []fakeThread
	}
	local := "45 1 0:37 / / rw shared:1 - xfs /dev/nvme0n1p3 rw\n46 45 0:22 / /proc rw - proc proc rw\n"
	remote := local + "47 45 0:40 / /mnt/nfs rw - nfs4 server:/export rw\n"
	root := fakeProc(t, map[int]proc{
		// 102 and 103 share a namespace, read once (103 shows no mounts of
		// its own); 104 has one of its own, which reaches NFS.
		100: {state: 'Z', start: 7, threads: []fakeThread{{100, 'Z', 5, 1, 2, "", "", ""}, {101, 'R', 6, 9, 3, "400 ", local, "mnt:[1]"}, {102, 'D', 2, 2, 2, "", local, "mnt:[1]"}, {103, 'D', 1, 1, 1, "", "", "mnt:[1]"}, {104, 'D', 1, 1, 1, "", remote, "mnt:[2]"}}},
		200: {state: 'S', start: 9, threads: []fakeThread{{200, 'S', 1, 2, 3, "", "", ""}}},
		400: {state: 'S', start: 11, threads: []fakeThread{{400, 'S', 4, 4, 4, "401", remote, ""}, {403, 'D', 1, 1, 1, "", remote, ""}}},
		401: {state: 'R', start: 12, threads: []fakeThread{{401, 'R', 0, 0, 0, "", "", ""}, {402, 'D', 3, 3, 3, "", "", ""}}},
	})
	// Read 2 s after the system booted: a thread started at tick 7 (of
	// 10ms) is 1.93 s old.
	boot := func() time.Duration { return 2 * time.Second }
	from := time.Now()
	got, ok := readTaskTimesIn(root, []Reached{{PID: 100, Start: 7}, {PID: 200, Start: 8}, {PID: 300, Start: 1}}, boot, mountsSeen)
	to := time.Now()
	if !ok {
		t.Fatal("the zombie-led process was not read")
	}
	// Each thread's read is timed within the read of them all; the rest of
	// its account is as the files say.
	for tid, th := range got {
		if !readWithin(th, from, to) {
			t.Errorf("thread %d was read from %v to %v, not within the read of them all, from %v to %v", tid, th.from, th.to, from, to)
		}
		th.from, th.to = time.Time{}, time.Time{}
		got[tid] = th
	}
	ms := time.Millisecond
	want := map[int]taskTime{
		100: {ran: 5, waited: 1, slices: 2, age: 1930 * ms},
		101: {ran: 6, waited: 9, slices: 3, runnable: true, age: 1930 * ms},
		102: {ran: 2, waited: 2, slices: 2, blocked: true, seen: true, age: 1930 * ms},
		103: {ran: 1, waited: 1, slices: 1, blocked: true, seen: true, age: 1930 * ms},
		104: {ran: 1, waited: 1, slices: 1, blocked: true, age: 1930 * ms},
		400: {ran: 4, waited: 4, slices: 4, age: 1890 * ms},
		403: {ran: 1, waited: 1, slices: 1, blocked: true, age: 1890 * ms},
		401: {runnable: true, age: 1880 * ms},
		402: {ran: 3, waited: 3, slices: 3, blocked: true, age: 1880 * ms},
	}
	if !maps.Equal(got, want) {
		t.Fatalf("read %+v, want %+v (400 and 401 run under 100, pid 200 names another process now, 300 is gone; 102 and 103 can reach only local storage, 104 and 403 an NFS mount, and 402's mounts cannot be read)", got, want)
	}
	if _, ok := readTaskTimesIn(root, []Reached{{PID: 200, Start: 8}}, boot, mountsSeen); ok {
		t.Fatal("a process another now has the pid of was read")
	}
}

// A thread's state is read after its account, and one its state shows not
// runnable may have been waiting for a CPU when its account was read: then
// it has been given the CPU since, which booked the whole wait. Its account
// is read again after its state: the waits it booked since count too
// (missed), a turn on a CPU since ends a stretch in the kernel (turned), its
// time on a CPU then is kept (ranAgain), and one whose account is gone by
// then is taken for runnable.
func TestAThreadNotRunnableIsReadAgainForTheWaitsItBookedAsItsStateWasRead(t *testing.T) {
	type proc = struct {
		state   byte
		start   uint64
		threads []fakeThread
	}
	root := fakeProc(t, map[int]proc{
		500: {state: 'S', start: 7, threads: []fakeThread{{500, 'S', 5, 1, 2, "", "", ""}, {501, 'D', 3, 1, 1, "", "", ""}, {502, 'S', 4, 4, 4, "", "", ""}, {503, 'S', 2, 2, 2, "", "", ""}}},
	})
	// Threads 500, 501 and 503 move on as their state is read: their stat
	// is a FIFO whose writer, once it is opened, moves their account on (or,
	// for 503, which exits, removes it) and only then gives the stat.
	for tid, moved := range map[string]string{"500": "6 3 3\n", "501": "4 2 2\n", "503": ""} {
		dir := filepath.Join(root, "500", "task", tid)
		stat, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			t.Fatal(err)
		}
		fifo := filepath.Join(dir, "stat")
		if err := os.Remove(fifo); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		end := make(chan struct{})
		go func() {
			defer close(end)
			f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
			if err != nil {
				return
			}
			defer func() { _ = f.Close() }()
			account := filepath.Join(dir, "schedstat")
			if moved == "" {
				_ = os.Remove(account)
			} else if os.WriteFile(account+".moved", []byte(moved), 0o644) == nil {
				_ = os.Rename(account+".moved", account)
			}
			_, _ = f.Write(stat)
		}()
		t.Cleanup(func() {
			// A writer still waiting for its reader is let go.
			if f, err := os.OpenFile(fifo, os.O_RDONLY|unix.O_NONBLOCK, 0); err == nil {
				_ = f.Close()
			}
			<-end
		})
	}
	got, ok := readTaskTimesIn(root, []Reached{{PID: 500, Start: 7}}, func() time.Duration { return 0 }, func(string) bool { return true })
	if !ok {
		t.Fatal("the process was not read")
	}
	for tid, th := range got {
		th.from, th.to = time.Time{}, time.Time{}
		got[tid] = th
	}
	want := map[int]taskTime{
		500: {ran: 5, waited: 1, slices: 2, missed: 2, turned: true, ranAgain: 6},
		501: {ran: 3, waited: 1, slices: 1, blocked: true, seen: true, missed: 1, turned: true, ranAgain: 4},
		502: {ran: 4, waited: 4, slices: 4},
		503: {ran: 2, waited: 2, slices: 2, runnable: true},
	}
	if !maps.Equal(got, want) {
		t.Fatalf("read %+v, want %+v (500 and 501 booked waits and turns as their state was read, 502 did not, 503 exited)", got, want)
	}
}

// startProc starts command under sh for the test.
func startProc(t *testing.T, command string) []Reached {
	t.Helper()
	return startCmd(t, exec.Command("sh", "-c", command))
}

// startSleeper starts a process that sleeps until the test's process is
// gone, however that ends: a shell reading its stdin, a pipe no one writes
// to, whose write end the test's process alone holds, so the shell reads the
// pipe's end once that process is gone.
func startSleeper(t *testing.T) []Reached {
	t.Helper()
	cmd := exec.Command("sh", "-c", "read -r line")
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	return startCmd(t, cmd)
}

// startCmd starts cmd for the test, in a process group of its own, which the
// test kills, whole, at its end.
func startCmd(t *testing.T, cmd *exec.Cmd) []Reached {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL); _ = cmd.Wait() })
	p, ok := ProcOf(cmd.Process.Pid)
	if !ok {
		t.Fatal("the process is gone")
	}
	return []Reached{p}
}

// liveRig makes ExitClocks of live processes on the wall clock, each with
// its reads of them kept (readSpans), on a keploy runnable clock the test
// moves (looks) by more than any look, and on storage that serves throughout
// (servingDisks), so what they count is the processes' own account of the
// CPU alone. A process that only spins is held in the kernel too, now and
// then: on a page of it the kernel is migrating (a fault that waits in
// migration_entry_wait, uninterruptible), which the CPU it had does not show,
// and which the clock counts as time it could run where the host's disks
// serve less (ExitClock). On .142 under memory load, the two spinners of 150
// runs of a test like TestTheExitClockOfAProcessSharingACPU were so held 176
// times, for 8 µs to 16 ms. With storage serving throughout, the clock takes
// each such hold a look finds for a wait.
type liveRig struct {
	own   *stall.ManualClock
	spans map[*ExitClock]*readSpans
}

func newLiveRig() *liveRig {
	return &liveRig{own: &stall.ManualClock{}, spans: map[*ExitClock]*readSpans{}}
}

func (r *liveRig) clock(procs []Reached) *ExitClock {
	s := &readSpans{}
	c := newExitClock(procs, time.Hour, time.Hour, time.Hour, r.own, time.Now, s.read, servingDisks(), nil)
	r.spans[c] = s
	return c
}

// looks looks at clocks n times, each after a pause, moving keploy's
// runnable time on by an hour each time (so that it bounds no look), and
// returns the wall time that passed.
func (r *liveRig) looks(n int, clocks ...*ExitClock) time.Duration {
	start := time.Now()
	for range n {
		time.Sleep(20 * time.Millisecond)
		r.own.Advance(time.Hour)
		for _, c := range clocks {
			c.Runnable()
		}
	}
	return time.Since(start)
}

// readSpans is an ExitClock's reader of live processes, which keeps what
// each read of them read. Each read is held up first, by as much as 5 ms, as
// a loaded host holds keploy off the CPU between reading the wall clock and
// reading the processes (ExitClock).
type readSpans struct {
	reads []map[int]taskTime
}

// read reads procs' account, taking every filesystem a thread held in the
// kernel can reach for one on the host's block devices (mountsSeen), as
// servingDisks are.
func (s *readSpans) read(procs []Reached) (map[int]taskTime, bool) {
	time.Sleep(time.Duration(rand.Int63n(int64(5 * time.Millisecond))))
	m, ok := readTaskTimesSeen(procs, func(string) bool { return true })
	s.reads = append(s.reads, maps.Clone(m))
	return m, ok
}

// window is the time from the start of the first read of thread tid's
// account to the end of the last, which holds every moment the kernel took
// one at.
func (s *readSpans) window(t *testing.T, tid int) time.Duration {
	t.Helper()
	first, last := s.reads[0][tid], s.reads[len(s.reads)-1][tid]
	if first.from.IsZero() || last.to.IsZero() {
		t.Fatalf("thread %d was not read, timed, at the first read and the last", tid)
	}
	return last.to.Sub(first.from)
}

// still says whether read i found threads, none of them runnable, nor any
// whose account moved as its state was read.
func (s *readSpans) still(i int) bool {
	m := s.reads[i]
	for _, th := range m {
		if th.runnable || th.missed > 0 || th.turned || th.ranAgain > 0 {
			return false
		}
	}
	return len(m) > 0
}

// asleep is how many reads found threads, and none of them runnable.
func (s *readSpans) asleep() (n int) {
	for _, m := range s.reads {
		if len(m) > 0 && !slices.ContainsFunc(slices.Collect(maps.Values(m)), func(th taskTime) bool { return th.runnable }) {
			n++
		}
	}
	return n
}

// servingDisks is the host's block devices as one disk that serves I/O
// throughout every look: it completes some, and has I/O in flight, between
// any two reads of it.
func servingDisks() func() (diskMark, bool) {
	var d diskCount
	return func() (diskMark, bool) {
		d.done++
		d.busy += time.Hour
		return diskMark{"259:0": d}, true
	}
}

// between is the time between the reads of the one thread of the clock's
// process: from the end of each read of it to the start of the next.
func (s *readSpans) between(t *testing.T) (d time.Duration) {
	t.Helper()
	for i := 1; i < len(s.reads); i++ {
		if len(s.reads[i-1]) != 1 || len(s.reads[i]) != 1 {
			t.Fatalf("read %d threads, then %d: want the one", len(s.reads[i-1]), len(s.reads[i]))
		}
		for tid, th := range s.reads[i] {
			b, ok := s.reads[i-1][tid]
			if !ok {
				t.Fatalf("thread %d is new to read %d", tid, i)
			}
			d += th.from.Sub(b.to)
		}
	}
	return d
}

// cpuSince is the CPU time the threads of an account had since an earlier
// one of the same processes, and the time they waited for a CPU, by the
// kernel.
func cpuSince(before, now map[int]taskTime) (ran, waited time.Duration) {
	for tid, t := range now {
		b := before[tid]
		if t.ran < b.ran {
			b = taskTime{}
		}
		ran += t.ran - b.ran
		waited += t.waited - b.waited
	}
	return ran, waited
}

// spinning waits until process p spins: it has run for 10 ms on a CPU, by
// its own account, which one still starting has not (its program paged in
// from storage, held in the kernel). Its time held there is a wait only for
// what storage served of it (ExitClock), so a test of a process that only
// spins starts once it does: on .142 at load 20, one started 61 ms before the
// first look was held in the kernel for 470 ms, and the few hundred
// microseconds of each look storage did not serve counted, as they must.
// The test's own timeout is the only bound.
func spinning(t *testing.T, p Reached) {
	t.Helper()
	for {
		if ran, _ := starvetest.Ran(p.PID); ran >= 10*time.Millisecond {
			return
		}
		if now, ok := ProcOf(p.PID); !ok || now != p {
			t.Fatalf("process %d is gone", p.PID)
		}
		time.Sleep(time.Millisecond)
	}
}

// asleep waits until process p runs program comm and sleeps, with no more
// to run.
func asleep(t *testing.T, p Reached, comm string) {
	t.Helper()
	for {
		st, ok := ReadProcStat(p.PID)
		if !ok {
			t.Fatalf("process %d is gone", p.PID)
		}
		b, err := os.ReadFile("/proc/" + strconv.Itoa(p.PID) + "/comm")
		if err != nil {
			t.Fatalf("read process %d's command: %v", p.PID, err)
		}
		if st.State == 'S' && strings.TrimSpace(string(b)) == comm {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// On the kernel's own account: a process held off the CPU (runnable, given
// none) could run no more than the CPU it had while a stop waits on it, and
// one that sleeps all of the time.
func TestTheExitClockOfAProcessHeldOffTheCPU(t *testing.T) {
	starvetest.TakesACPU(t)
	if !schedAccountKept() {
		t.Skip("this kernel keeps no account of a thread's time on and waiting for a CPU")
	}
	spinner, sleeper := startProc(t, starvetest.ShellSpin("")), startSleeper(t)
	asleep(t, sleeper[0], "sh")
	spinning(t, spinner[0])
	release := starvetest.Hold(t, spinner[0].PID)
	defer release()
	rig := newLiveRig()
	held, sleeping := rig.clock(spinner), rig.clock(sleeper)
	first := maps.Clone(held.last)
	total := rig.looks(20, held, sleeping)
	if ran, _ := cpuSince(first, held.last); held.ran > ran {
		t.Errorf("a process held off the CPU could run %v of %v by the clock, more than the %v of CPU it had", held.ran, total, ran)
	}
	if between := rig.spans[sleeping].between(t); sleeping.ran != between {
		t.Errorf("a process that sleeps could run %v of the %v between the reads of it (of %v) by the clock; want all of it", sleeping.ran, between, total)
	}
}

// On the kernel's own account: a process that spins and sleeps in turn, on
// a CPU it shares, could run only what it ran and slept, however its turns
// and sleeps fall between the looks, and however late keploy reads it. Read
// first while asleep, its account unmoved as its state was read (so with no
// wait for a CPU going on, nor run the kernel has not booked), it ran and
// slept, from the moment the kernel took that account to the moment it took
// the last, all of that time but its waits for a CPU: those its account
// booked by the last read, and the one it may be in then. So the clock counts
// no more than the time from the start of the first read of its account to
// the end of the last (readSpans.window), less those its account booked. A
// look that finds it asleep counts all of its time but the waits its account
// booked since the last look, less what of them the clock counted as they
// went: what the test looks at, so the looks go on until ten found it asleep.
func TestTheExitClockOfAProcessSharingACPU(t *testing.T) {
	starvetest.TakesACPU(t)
	if !schedAccountKept() {
		t.Skip("this kernel keeps no account of a thread's time on and waiting for a CPU")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash, whose read times out, to sleep in without starting a process")
	}
	var mine unix.CPUSet
	if err := unix.SchedGetaffinity(0, &mine); err != nil {
		t.Skipf("cannot read this process's CPUs: %v", err)
	}
	cpu := -1
	for i := 0; i < len(mine)*64; i++ {
		if mine.IsSet(i) {
			cpu = i
		}
	}
	if cpu < 0 {
		t.Skip("this process may run on no CPU")
	}
	var one unix.CPUSet
	one.Set(cpu)
	fifo := filepath.Join(t.TempDir(), "unwritten")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	sharer, other := startCmd(t, exec.Command(bash, "-c", spinSleep(fifo))), startProc(t, starvetest.ShellSpin(""))
	for _, p := range []Reached{sharer[0], other[0]} {
		if err := unix.SchedSetaffinity(p.PID, &one); err != nil {
			t.Skipf("cannot pin process %d to CPU %d: %v", p.PID, cpu, err)
		}
	}
	for _, p := range []Reached{sharer[0], other[0]} {
		spinning(t, p)
	}
	rig := newLiveRig()
	var c *ExitClock
	for tries := 0; ; tries++ {
		if c = rig.clock(sharer); rig.spans[c].still(0) {
			break
		}
		if now, ok := ProcOf(sharer[0].PID); !ok || now != sharer[0] {
			t.Fatalf("process %d is gone", sharer[0].PID)
		}
		if tries == 5000 {
			t.Fatalf("process %d was found asleep at none of %d reads: it does not sleep", sharer[0].PID, tries)
		}
		time.Sleep(time.Millisecond)
	}
	first, reads := maps.Clone(c.last), rig.spans[c]
	var total time.Duration
	for looks := 0; looks < 40 || reads.asleep() < 11; looks++ {
		if looks == 5000 {
			t.Fatalf("the process was asleep at %d of %d looks: it hardly sleeps", reads.asleep()-1, looks)
		}
		total += rig.looks(1, c)
		if now, ok := ProcOf(sharer[0].PID); !ok || now != sharer[0] {
			t.Fatalf("process %d is gone", sharer[0].PID)
		}
	}
	ran, waited := cpuSince(first, c.last)
	if waited == 0 {
		t.Fatalf("the process waited for no CPU in %v: the test shared none", total)
	}
	window := reads.window(t, sharer[0].PID)
	if could := window - waited; c.ran > could {
		t.Errorf("a process sharing a CPU, spinning and sleeping in turn, could run %v of %v by the clock, more than the %v it could: of the %v from its first read to its last, it waited %v for a CPU (and ran %v; %d of %d looks found it asleep)",
			c.ran, total, could, window, waited, ran, reads.asleep()-1, len(reads.reads)-1)
	}
}

// spinSleep is a bash loop that, while this process (the test's) runs, as
// ShellSpin, counts to three thousand and sleeps 10 ms, in turn: the count
// takes bash milliseconds of CPU, as a rule more than the scheduler gives it
// in one turn on a CPU it shares, so it waits for one, as well as sleeping
// (the test fails where it waited for none). The sleep is a read's timeout
// on fifo, which no one writes, so the shell starts no process to sleep in,
// and its look for the test's process is a kill(2) of no signal, which never
// sleeps.
func spinSleep(fifo string) string {
	return fmt.Sprintf("exec 3<>'%s'; while kill -0 %d; do i=0; while [ $i -lt 3000 ]; do i=$((i+1)); done; read -t 0.01 -u 3; done", fifo, os.Getpid())
}
