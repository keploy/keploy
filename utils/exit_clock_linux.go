//go:build linux

package utils

import (
	"os"
	"strconv"
	"sync"
	"time"

	"go.keploy.io/server/v3/utils/stall"
	"golang.org/x/sys/unix"
)

// A STOP COUNTS A STALL ONLY IN TIME THE STOPPED PROCESSES COULD RUN.
//
// A stop judges processes on their way out (ExitMark) and kills them once
// they make no progress for a bound. Counted in keploy's runnable time alone,
// the bound ran on while they were held off the CPU and keploy was not: on a
// loaded host, in a cgroup whose quota their siblings spent, a process that
// had acted on the stop's signal at once got no CPU to finish, the bound
// expired, and keploy killed it, judging a stall that was keploy's view of a
// queue. The daemons an app leaves behind were counted in wall-time checks,
// the same.
//
// ExitClock measures the stall on the processes' own account in the kernel
// instead: each thread's schedstat (the time it ran on a CPU, the time it
// waited for one, the times it was given one) and its state. Between two
// looks w of wall time apart, the processes could run
//
//	max(w - waited, ran), at most w
//
// with waited and ran summed over their threads: w less the time they waited
// for a CPU (runnable, not running: behind other work, or their cgroup
// throttled), and never less than the CPU they had. One that sleeps (on a
// timer, a lock, a peer: its own wait) could run all of w; one held off the
// CPU throughout, none of it; one that spins on a share of a CPU, that share.
//
// The kernel adds a wait to a thread's account only once it ends, as the
// thread is given the CPU: a thread waiting still shows none of its wait, and
// when it began is not known. A thread runnable at a look may have waited
// since any moment of w it did not run (since the last look; or since its
// last slice, which, ended before this look, leaves a wait the kernel books
// only at the next), so all of w it did not run is taken for its wait. That
// is counted as it goes, and taken off its account's wait once that wait
// ends, so no wait counts twice. A thread runnable at a look that slept for
// part of w is so counted as waiting for that part too: the stall accrues no
// faster than the processes could run, at times slower.
//
// A stall is counted only in time both they and keploy could run (the least
// of the two, at each look): what a shutdown waits on may be keploy's own (its
// proxy), which keploy, starved, cannot answer. So a starved process is never
// judged to have made no progress for the time it could not run, and one that
// runs and makes none is still killed once it has had the bound to run.
//
// A thread in an uninterruptible sleep (D: on storage, on a lock in the
// kernel) waits for no CPU, and its time counts, by the rule a stopping
// agent's teardown is judged by (teardown_work.go): one held in the kernel for
// the whole bound is taken for wedged, as waiting on would wait for ever on
// one the kernel never lets go (a dead mount). Storage I/O it does while it
// waits on storage is progress (ExitRatchet).
//
// Where the kernel keeps no such account (a kernel without schedstat), or
// none of the processes can be read, the stall is keploy's runnable time
// alone, as before.

// ExitClock is the time processes on their way out, and keploy, could run
// (see above): a stall.Clock to measure a stop's stall on. Safe for
// concurrent use.
type ExitClock struct {
	procs []Reached
	own   stall.Clock
	now   func() time.Time
	read  func([]Reached) (map[int]taskTime, bool)

	mu       sync.Mutex
	looked   bool
	lastWall time.Time
	lastOwn  time.Duration
	last     map[int]taskTime
	// unbooked is, for each thread runnable at the last look, what of its
	// waits was counted already and is not in its account yet: its account
	// adds it once the wait ends.
	unbooked map[int]time.Duration
	ran      time.Duration
}

// taskTime is a thread's account in the kernel: the time it ran on a CPU,
// the time it waited for one (the waits that ended), the times it was given
// one, whether it is runnable (running, or waiting to: state R), and how long
// it had lived when it was read, at most (0: not known).
type taskTime struct {
	ran, waited time.Duration
	slices      uint64
	runnable    bool
	age         time.Duration
}

// NewExitClock is the ExitClock of procs, each named by its pid and start
// time, measured with keploy's own runnable clock, which it holds until the
// returned func releases it.
func NewExitClock(procs []Reached) (*ExitClock, func()) {
	own, release := stall.Process()
	c := newExitClock(procs, own, time.Now, readTaskTimes)
	return c, release
}

// newExitClock is an ExitClock on the given clocks and reader (the tests'),
// first looked at now.
func newExitClock(procs []Reached, own stall.Clock, now func() time.Time, read func([]Reached) (map[int]taskTime, bool)) *ExitClock {
	c := &ExitClock{procs: procs, own: own, now: now, read: read, unbooked: map[int]time.Duration{}}
	c.Runnable()
	return c
}

// Runnable is how long the processes and keploy could both run since the
// clock was made. Each call looks at them: what they could run since the
// last look is added.
func (c *ExitClock) Runnable() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	wall, own := c.now(), c.own.Runnable()
	tasks, ok := c.read(c.procs)
	if c.looked {
		w := max(wall.Sub(c.lastWall), 0)
		could := w // no account of theirs: keploy's runnable time alone
		if ok {
			could = couldRun(w, c.last, tasks, c.unbooked)
		}
		c.ran += min(max(own-c.lastOwn, 0), could)
	}
	c.looked, c.lastWall, c.lastOwn = true, wall, own
	if ok {
		c.last = tasks
	} else {
		// The next look that reads them sets their account against none
		// (each thread new to it): one read before this look would span
		// more time than the next look's; couldRun then drops the waits
		// it had counted for them.
		c.last = nil
	}
	return c.ran
}

// After is keploy's own timer.
func (c *ExitClock) After(d time.Duration) (<-chan time.Time, func() bool) {
	return c.own.After(d)
}

// couldRun is how long, of w of wall time, processes could run whose threads
// were accounted for as before at the last look and as now at this one:
// max(w - waited, ran), at most w (see ExitClock). A thread runnable now is
// taken to have waited all of w it did not run (one new since the last look,
// all of its age, at most w, it did not run); one not runnable now has no
// wait going on, and waited what its account added, less what of that was
// counted already. unbooked is, by thread, the part of its waits counted and
// not yet in its account, which couldRun keeps. A thread new since the last
// look (or a thread id given to another meanwhile, its account below the
// last) counts with all of its account, which it gathered since, but one
// older than the look (a process read from now on): of the CPU it had in its
// life, only what it cannot have had before the look. One gone counts with
// none, its exit being progress the stop reads on its own.
func couldRun(w time.Duration, before, now map[int]taskTime, unbooked map[int]time.Duration) time.Duration {
	var ran, waited time.Duration
	for tid := range unbooked {
		if _, ok := now[tid]; !ok {
			delete(unbooked, tid)
		}
	}
	for tid, t := range now {
		b, ok := before[tid]
		fresh := !ok || t.ran < b.ran || t.waited < b.waited || t.slices < b.slices
		if fresh {
			b = taskTime{}
			delete(unbooked, tid)
		}
		dRan, dWaited := t.ran-b.ran, t.waited-b.waited
		if fresh && t.age > w {
			// New to this look but older than it (a process read from
			// now on): its account is its life's, of which the look holds
			// no more of its time on a CPU than it cannot have had before.
			dRan = max(dRan-(t.age-w), 0)
		}
		ran += dRan
		if !t.runnable {
			// Every wait of it ended: what of them was not counted.
			waited += max(dWaited-unbooked[tid], 0)
			delete(unbooked, tid)
			continue
		}
		// Waiting for a CPU, or running: all of w it did not run, of
		// which its account has what ended; a thread new since the last
		// look, no longer than it has lived.
		lived := w
		if fresh && t.age > 0 {
			lived = min(w, t.age)
		}
		wait := max(lived-dRan, 0)
		waited += wait
		if u := unbooked[tid] + wait - dWaited; u > 0 {
			unbooked[tid] = u
		} else {
			delete(unbooked, tid)
		}
	}
	return min(max(w-waited, ran, 0), w)
}

// readTaskTimes reads the account of every thread of procs (ExitClock), by
// thread id; ok is false where the kernel keeps none, or no process of procs
// could be read (readTaskTimesIn).
func readTaskTimes(procs []Reached) (map[int]taskTime, bool) {
	if !schedAccountKept() {
		return nil, false
	}
	return readTaskTimesIn("/proc", procs, bootTime)
}

// bootTime is how long the system has been up (CLOCK_BOOTTIME, the clock of
// a thread's start time in /proc); 0 where it cannot be read.
func bootTime() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0
	}
	return time.Duration(ts.Nano())
}

// startTick is the unit of a thread's start time in /proc (USER_HZ, 100 on
// every Linux architecture keploy runs on).
const startTick = 10 * time.Millisecond

// readTaskTimesIn is readTaskTimes on proc, a /proc (the tests' own tree): ok
// is false where no process of procs could be read (each gone, or another
// process by its pid now). Each is read with the processes it runs as they
// are now: a shutdown that waits on a child it started after the stop began
// (a shell's trap, a stop command) is as far as that child gets. A process
// whose leader has exited (a zombie) while other threads of it still run is
// read too: those threads are on their way out, and need the CPU to get
// there. Each thread's age is boot, read once every thread has been, less
// its start time, which /proc gives in whole ticks, rounded down: it is never
// less than the time the thread had lived.
func readTaskTimesIn(proc string, procs []Reached, boot func() time.Duration) (map[int]taskTime, bool) {
	tasks := map[int]taskTime{}
	born := map[int]time.Duration{}
	seen := map[int]bool{}
	var walk func(pid int) bool
	walk = func(pid int) bool {
		if seen[pid] {
			return true
		}
		seen[pid] = true
		dir := proc + "/" + strconv.Itoa(pid) + "/task"
		ents, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		var children []int
		listed := false
		for _, e := range ents {
			tid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			task := dir + "/" + e.Name()
			if b, err := os.ReadFile(task + "/children"); err == nil {
				listed = true
				children = append(children, parsePids(b)...)
			}
			// A thread that has neither run nor waited yet shows zeros;
			// one that exited meanwhile, no file.
			b, err := os.ReadFile(task + "/schedstat")
			if err != nil {
				continue
			}
			ran, waited, slices, _ := parseSchedstat(b)
			t := taskTime{ran: time.Duration(ran), waited: time.Duration(waited), slices: slices}
			if b, err := os.ReadFile(task + "/stat"); err == nil {
				if st, ok := ParseProcStat(b); ok {
					t.runnable = st.State == 'R'
					born[tid] = time.Duration(st.StartTime) * startTick
				}
			}
			tasks[tid] = t
		}
		if !listed && proc == "/proc" && !childrenFiles() {
			children = childrenByParent(pid)
		}
		for _, c := range children {
			walk(c)
		}
		return true
	}
	read := false
	for _, p := range procs {
		b, err := os.ReadFile(proc + "/" + strconv.Itoa(p.PID) + "/stat")
		if err != nil {
			continue
		}
		if st, ok := ParseProcStat(b); !ok || st.StartTime != p.Start {
			continue
		}
		if walk(p.PID) {
			read = true
		}
	}
	if now := boot(); now > 0 {
		for tid, at := range born {
			t := tasks[tid]
			t.age = max(now-at, 0)
			tasks[tid] = t
		}
	}
	return tasks, read
}

// schedAccountKept says whether this kernel keeps each thread's time on and
// waiting for a CPU (schedstat): the reading thread has run, so a kernel that
// keeps none shows it zeros, or no file. Known once.
var schedAccountKept = sync.OnceValue(func() bool {
	_, _, _, ok := readSchedstat("/proc/thread-self/schedstat")
	return ok
})
