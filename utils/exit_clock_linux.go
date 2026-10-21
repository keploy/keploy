//go:build linux

package utils

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
// A thread in an uninterruptible sleep (D: in the kernel, most often on
// storage, or on a lock whose holder waits on storage) waits for no CPU, and
// the kernel does not say what it waits on. While the host's storage serves
// I/O, it is taken to wait on that storage, as a runnable thread is taken to
// wait for a CPU: of the time it neither ran nor waited for a CPU, the time
// storage was serving (storageServed: the longest a block device that
// completed I/O, or has I/O in flight, had I/O in flight) is its wait. A
// process whose way out goes through a disk a loaded host keeps busy (a file
// closed and flushed, a page of its program read back, an fsync at shutdown)
// is so not judged for the time the disk served others first.
//
// A device with I/O in flight that completed none since the last look, nor
// did any device it is stacked on (storageBelow: a device-mapper or md
// device's slaves, a partition's disk, and theirs), stands still. One whose
// device below completed I/O is served there: an encrypted or LVM volume
// holds its I/O while its disk works through what is ahead of it, and shows
// none completed for far longer than the disk does. One standing still
// serves slowly (a request takes longer than a look: a loaded virtual or
// cloud disk, a flush), or it is dead (a hung device, a multipath queueing
// with no path), and a look cannot tell which. It serves until it has stood
// still, in one stretch, for the stall bound of the time that would have
// counted had it been dead (the time at stake: what a thread held in the
// kernel would have added). Then it is taken for dead, and serves none for
// as long as it stands still: a thread may be waiting on it, and what waits
// on a dead disk waits for ever. A stop held on a dead disk so goes on at
// twice the bound; one on a slow disk is not judged for a request that
// outlasts a look. A stretch is the device's own, and ends as it, or a
// device under it, completes I/O, or as it has none in flight; time no
// thread waits in the kernel is not at stake, and adds to none.
//
// Storage that serves says nothing of what one thread waits on: a wait on no
// storage (a lock a wedged driver holds, a task a cgroup v1 freezer froze, a
// volume that holds its own I/O, a suspended device-mapper one) is waited on
// for ever while the host's storage serves others. So a thread stands in the
// kernel while it is held there at two looks with no turn on a CPU between
// them (its slices unchanged: a thread held there must be given a CPU to
// leave). Its time there is a wait on storage until it has stood, in one
// stretch, for as long as the kernel lets a task stand there before it
// reports it hung (hungTaskTime: hung_task_timeout_secs, two minutes by
// default), or for the stall bound where that is longer, of the time at stake
// up to the last look; then it is taken for wedged, and its time there counts
// for as long as it stands. The stall bound alone is no measure of one wait:
// on a loaded host's disk (.142, xfs on dm-crypt, 4 fsync writers) one fsync
// held its thread there, given no CPU, for seconds at a time. A stretch is the
// thread's own, and ends at its next turn on a CPU; the time at stake is what
// the waits credited to the processes' threads in the kernel withheld from
// the stall at a look, so a look in which keploy could not run, or a device
// taken for dead had all of their time count already, adds to none. A stop
// held on a thread wedged in the kernel so goes on at the kernel's hung-task
// time and the bound after it, and a look, whatever the host's storage
// serves.
//
// Nor does a wait that ends say the next is not the same: a thread whose way
// out loops through waits in the kernel that each end (a driver's loop of
// short sleeps there, a cgroup's memory.high throttling it, a sync while
// others dirty pages), making no progress between them, has a turn on a CPU
// between any two, and no stretch of it stands for long. Credited for as long
// as the host's disks served others, it accrued the stall only at the rate it
// ran: a test app in waits of 50 ms, never let go, its storage serving
// throughout, held a stop at a 300 ms bound for 72 times the bound, and in
// waits of 300 ms for 177 times (at the default bound, an hour or more). So
// the credit is bounded in all, not only in one stretch: of the time at
// stake since the processes' last progress (ExitMeter.Advance), or since the
// clock was made, their waits in the kernel withhold from the stall no more
// than one wait may stand there (the kernel's hung-task time, or the bound
// where that is longer), however each ends. The credit is the stop's, not a
// thread's: threads that take turns in the kernel spend it as one. Once it
// is spent, every thread's time in the kernel counts, as while storage
// serves none, until their next progress. A stop of processes that make no
// progress so goes on, whatever they wait on in the kernel, for no more than
// that time and the bound (and a look) of the time they and keploy could run
// with none of their time in the kernel taken for a wait.
//
// The rest counts, by the rule a stopping agent's teardown is judged by
// (teardown_work.go): one held in the kernel for the whole bound is taken for
// wedged, as waiting on would wait for ever on one the kernel never lets go.
// That is the time storage served nothing (a wait in the kernel then is on no
// I/O the host's disks show: a lock, a vfork); all of a look in which a block
// device taken for dead stood still; all of a look of a thread taken for
// wedged; and all of it for a thread that can reach a filesystem whose
// service the host's block devices do not show (mountsSeen: NFS, SMB, FUSE,
// 9p, or a type not known), as what it waits on may be that one, dead.
// Storage I/O the thread does itself as it waits is progress (ExitRatchet).
//
// What stays: waits in the kernel on storage that serves, but serves them no
// sooner than the kernel's hung-task time of the time at stake since the
// processes' last progress (an fsync behind a journal commit that must first
// flush gigabytes another process dirtied, on a slow disk), are judged, as
// the kernel reports such a wait hung. Processes that make no progress while
// they wait in the kernel hold the stop for that time before their time there
// counts: at the defaults, two minutes of the time at stake, then the bound's
// one. And a thread that can reach a network filesystem is judged on all of
// its wait in the kernel: a busy server is not told from a dead one.
//
// Where the kernel keeps no such account (a kernel without schedstat), or
// none of the processes can be read, the stall is keploy's runnable time
// alone, as before.

// ExitClock is the time processes on their way out, and keploy, could run
// (see above): a stall.Clock to measure a stop's stall on. Safe for
// concurrent use.
type ExitClock struct {
	procs []Reached
	// bound is the stall the stop judges on: how long, in the time at
	// stake, a device may stand still before it is taken for dead (see
	// above).
	bound time.Duration
	// wedge is how long, in the time at stake, a thread may stand in the
	// kernel before it is taken for wedged: the kernel's hung-task time, or
	// bound where that is longer (see above).
	wedge time.Duration
	// credit is how much of the time at stake, since the processes' last
	// progress, their waits in the kernel may withhold from the stall in
	// all: wedge, for a stop (see above).
	credit time.Duration
	own    stall.Clock
	now    func() time.Time
	read   func([]Reached) (map[int]taskTime, bool)
	disks  func() (diskMark, bool)
	// below is the block devices directly below the one named (storageBelow);
	// nil where none is read.
	below func(name string) []string

	mu       sync.Mutex
	looked   bool
	lastWall time.Time
	lastOwn  time.Duration
	last     map[int]taskTime
	// lastDisks is the host's block devices at the last look; nil where it
	// read none.
	lastDisks diskMark
	// still is, by device, the time at stake in its stretch standing still
	// up to the last look: devices standing still at it only. A look that
	// read no account of the processes, or no disks, ends every stretch;
	// all of it counts.
	still map[string]time.Duration
	// on is, by block device name, the devices directly below it (below),
	// read once a look first finds it, or one above it, standing still.
	on map[string][]string
	// inKernel is, by thread, the time at stake in its stretch standing in
	// the kernel up to the last look: threads standing there at it only.
	inKernel map[int]time.Duration
	// credited is the time at stake the waits in the kernel withheld from
	// the stall, in all, since the processes' last progress (progressed),
	// or since the clock was made, up to the last look.
	credited time.Duration
	// accounted says whether the last look read the processes' account.
	accounted bool
	// unbooked is, for each thread runnable at the last look, what of its
	// waits was counted already and is not in its account yet: its account
	// adds it once the wait ends.
	unbooked map[int]time.Duration
	ran      time.Duration
}

// taskTime is a thread's account in the kernel: the time it ran on a CPU,
// the time it waited for one (the waits that ended), the times it was given
// one, whether it is runnable (running, or waiting to: state R), whether it
// is held in the kernel (an uninterruptible sleep: state D) and, if so,
// whether every filesystem it can reach is on storage the host's block
// devices show (mountsSeen), and how long it had lived when it was read, at
// most (0: not known).
type taskTime struct {
	ran, waited time.Duration
	slices      uint64
	runnable    bool
	blocked     bool
	seen        bool
	age         time.Duration
}

// NewExitClock is the ExitClock of procs, each named by its pid and start
// time, for a stop that judges their stall on bound, measured with keploy's
// own runnable clock, which it holds until the returned func releases it.
func NewExitClock(procs []Reached, bound time.Duration) (*ExitClock, func()) {
	own, release := stall.Process()
	st := storageNow()
	read := func(procs []Reached) (map[int]taskTime, bool) { return readTaskTimesSeen(procs, st.seen) }
	wedge := bound
	if st.hung != nil {
		wedge = max(bound, st.hung())
	}
	c := newExitClock(procs, bound, wedge, wedge, own, time.Now, read, st.disks, st.below)
	return c, release
}

// NewExitMeter is the stall meter of a stop of procs (each named by its pid
// and start time) that judges them on bound: it expires once they made no
// progress (ExitMeter.Advance) for bound of the time both they and keploy
// could run, measured on their ExitClock, which takes a device standing still
// for that bound for dead. stop stops the meter and releases the clock.
func NewExitMeter(procs []Reached, bound time.Duration) (clock *ExitClock, meter *ExitMeter, stop func()) {
	clock, release := NewExitClock(procs, bound)
	meter = &ExitMeter{Meter: stall.NewMeter(clock, bound), clock: clock}
	return clock, meter, func() {
		meter.Stop()
		release()
	}
}

// ExitMeter is the stall meter of a stop (NewExitMeter): a stall.Meter on the
// stop's ExitClock, which reads the processes' progress itself (Advance), so
// that the clock's credit of their waits in the kernel starts again with the
// stall. Its Reset starts the stall again alone (a stop that killed them
// gives them the bound again to be gone), the credit spent as it was.
type ExitMeter struct {
	*stall.Meter
	clock *ExitClock
	mark  ExitRatchet
}

// Advance takes reading r of how far the processes are on their way out
// (ExitRatchet), the first setting the mark, and reports whether it passed
// the mark: their progress, at which the stall starts again, and so does the
// clock's credit of their waits in the kernel (ExitClock). Called from the
// stop's own loop only.
func (m *ExitMeter) Advance(r ExitMark) bool {
	if !m.mark.Advance(r) {
		return false
	}
	m.Reset()
	m.clock.progressed()
	return true
}

// newExitClock is an ExitClock on the given clocks and readers (the tests'),
// first looked at now, which takes a thread standing in the kernel for wedge
// for wedged, and credits the processes' waits in the kernel, since their
// last progress, for credit in all; below may be nil (no device stacked on
// another).
func newExitClock(procs []Reached, bound, wedge, credit time.Duration, own stall.Clock, now func() time.Time, read func([]Reached) (map[int]taskTime, bool), disks func() (diskMark, bool), below func(string) []string) *ExitClock {
	c := &ExitClock{procs: procs, bound: bound, wedge: wedge, credit: credit, own: own, now: now, read: read, disks: disks, below: below,
		on: map[string][]string{}, inKernel: map[int]time.Duration{}, unbooked: map[int]time.Duration{}}
	c.Runnable()
	return c
}

// progressed says the processes made progress on their way out: the credit
// of their waits in the kernel starts again from the last look (see above).
func (c *ExitClock) progressed() {
	c.mu.Lock()
	c.credited = 0
	c.mu.Unlock()
}

// Runnable is how long the processes and keploy could both run since the
// clock was made. Each call looks at them: what they could run since the
// last look is added.
func (c *ExitClock) Runnable() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	wall, own := c.now(), c.own.Runnable()
	tasks, ok := c.read(c.procs)
	disks, disksOK := c.disks()
	still := map[string]time.Duration{}
	inKernel := map[int]time.Duration{}
	if c.looked {
		w := max(wall.Sub(c.lastWall), 0)
		ownRan := max(own-c.lastOwn, 0)
		could := w // no account of theirs: keploy's runnable time alone
		if ok {
			l := lookAt(w, c.last, tasks, c.unbooked)
			// Standing in the kernel for the kernel's hung-task time up to
			// the last look: taken for wedged. With the credit spent, since
			// the processes' last progress up to the last look, each thread
			// held there is.
			spent := c.credited >= c.credit
			for i, h := range l.held {
				if spent || (h.standing && c.inKernel[h.tid] >= c.wedge) {
					l.held[i].wedged = true
				}
			}
			var served time.Duration
			if disksOK && c.lastDisks != nil {
				var standing []string
				served, standing = storageServed(w, c.lastDisks, disks, c.devicesUnder)
				// What would count were the devices standing still dead.
				stake := min(ownRan, l.could(0)) - min(ownRan, l.could(served))
				for _, dev := range standing {
					still[dev] = c.still[dev] + stake
					if still[dev] >= c.bound {
						served = 0 // taken for dead
					}
				}
			}
			// What the waits credited withheld from the stall: the time at
			// stake in each stretch in the kernel going on, and in the
			// credit spent.
			stake := min(ownRan, l.could(0)) - min(ownRan, l.could(served))
			for _, h := range l.held {
				if h.standing {
					inKernel[h.tid] = c.inKernel[h.tid] + stake
				}
			}
			c.credited += stake
			could = l.could(served)
		}
		c.ran += min(ownRan, could)
	}
	c.looked, c.lastWall, c.lastOwn, c.accounted = true, wall, own, ok
	c.lastDisks, c.still, c.inKernel = nil, still, inKernel
	if disksOK {
		c.lastDisks = disks
	}
	if ok {
		c.last = tasks
	} else {
		// The next look that reads them sets their account against none
		// (each thread new to it): one read before this look would span
		// more time than the next look's; lookAt then drops the waits
		// it had counted for them.
		c.last = nil
	}
	return c.ran
}

// After is keploy's own timer.
func (c *ExitClock) After(d time.Duration) (<-chan time.Time, func() bool) {
	return c.own.After(d)
}

// devicesUnder is every block device below the one named name: those it is
// stacked on, and theirs (storageBelow), each device's read once for the
// clock. Called with c.mu held.
func (c *ExitClock) devicesUnder(name string) []string {
	if c.below == nil {
		return nil
	}
	seen := map[string]bool{name: true}
	var all []string
	var walk func(string)
	walk = func(n string) {
		on, ok := c.on[n]
		if !ok {
			on = c.below(n)
			c.on[n] = on
		}
		for _, b := range on {
			if !seen[b] {
				seen[b] = true
				all = append(all, b)
				walk(b)
			}
		}
	}
	walk(name)
	return all
}

// Said is how a kill names a stall bound d measured on the clock: of the time
// both the processes and keploy could run where its last look read their
// account; where it did not (a kernel that keeps none, or nothing of them
// readable), of keploy's runnable time, which the clock then is.
func (c *ExitClock) Said(d time.Duration) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accounted {
		return d.String() + " of its and keploy's runnable time"
	}
	return d.String() + " of keploy's runnable time"
}

// lookTimes is a look's account of the processes' threads (lookAt): of its w
// of wall time, how long they ran and how long they waited for a CPU, summed
// over their threads, and each thread held in the kernel (D) whose
// filesystems are all on the host's block devices.
type lookTimes struct {
	w, ran, waited time.Duration
	held           []heldTime
}

// heldTime is a thread held in the kernel at a look (lookTimes): of the look,
// the time it neither ran nor waited for a CPU, of which what storage served
// is a wait, unless it is taken for wedged; and whether it stood in the
// kernel throughout (held there at the last look too, with no turn on a CPU
// since).
type heldTime struct {
	tid              int
	d                time.Duration
	standing, wedged bool
}

// could is how long, of the look, the processes could run with storage
// serving for served of it: max(w - waited, ran), at most w (see ExitClock),
// a thread held in the kernel and not taken for wedged having waited too for
// what of its time there storage served.
func (l lookTimes) could(served time.Duration) time.Duration {
	waited := l.waited
	for _, h := range l.held {
		if !h.wedged {
			waited += min(h.d, served)
		}
	}
	return min(max(l.w-waited, l.ran, 0), l.w)
}

// lookAt is the account, over w of wall time, of processes whose threads were
// accounted for as before at the last look and as now at this one (see
// lookTimes). A thread runnable now is taken to have waited all of w it did
// not run (one new since the last look, all of its age, at most w, it did not
// run); one not runnable now has no wait for a CPU going on, and waited what
// its account added, less what of that was counted already. One held in the
// kernel now (D) whose filesystems are all on the host's block devices may
// have waited on storage too, for the time it neither ran nor waited for a
// CPU; held there at the last look too, with no turn on a CPU since, it
// stood there throughout. unbooked is, by thread, the part of its waits
// counted and not yet in its account, which lookAt keeps. A thread new since the last look (or a
// thread id given to another meanwhile, its account below the last) counts
// with all of its account, which it gathered since, but one older than the
// look (a process read from now on): of the CPU it had in its life, only what
// it cannot have had before the look. One gone counts with none, its exit
// being progress the stop reads on its own.
func lookAt(w time.Duration, before, now map[int]taskTime, unbooked map[int]time.Duration) lookTimes {
	l := lookTimes{w: w}
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
		// Of w, the time it was there: a thread new since the last look,
		// no longer than it has lived.
		lived := w
		if fresh && t.age > 0 {
			lived = min(w, t.age)
		}
		if !t.runnable {
			// Every wait of it for a CPU ended: what of them was not
			// counted.
			cpu := max(dWaited-unbooked[tid], 0)
			waited += cpu
			delete(unbooked, tid)
			if t.blocked && t.seen {
				// Held in the kernel: of the time it neither ran nor
				// waited for a CPU, what storage served is a wait on it.
				// Held there at the last look too, and given no CPU since,
				// it stood there throughout.
				l.held = append(l.held, heldTime{tid: tid, d: max(lived-dRan-cpu, 0),
					standing: !fresh && b.blocked && t.slices == b.slices})
			}
			continue
		}
		// Waiting for a CPU, or running: all of w it did not run, of
		// which its account has what ended.
		wait := max(lived-dRan, 0)
		waited += wait
		if u := unbooked[tid] + wait - dWaited; u > 0 {
			unbooked[tid] = u
		} else {
			delete(unbooked, tid)
		}
	}
	l.ran, l.waited = ran, waited
	return l
}

// readTaskTimes reads the account of every thread of procs (ExitClock), by
// thread id; ok is false where the kernel keeps none, or no process of procs
// could be read (readTaskTimesIn).
func readTaskTimes(procs []Reached) (map[int]taskTime, bool) {
	return readTaskTimesSeen(procs, mountsSeen)
}

// readTaskTimesSeen is readTaskTimes, telling by seen whether a thread held in
// the kernel can reach only storage the host's block devices show.
func readTaskTimesSeen(procs []Reached, seen func(task string) bool) (map[int]taskTime, bool) {
	if !schedAccountKept() {
		return nil, false
	}
	return readTaskTimesIn("/proc", procs, bootTime, seen)
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
// less than the time the thread had lived. For a thread held in the kernel
// (D), seen tells, given its directory, whether every filesystem it can
// reach is on storage the host's block devices show (mountsSeen): read once
// for each mount namespace (its ns/mnt) at each read, as threads mostly share
// one, and for each thread whose namespace cannot be told.
func readTaskTimesIn(proc string, procs []Reached, boot func() time.Duration, seen func(task string) bool) (map[int]taskTime, bool) {
	tasks := map[int]taskTime{}
	born := map[int]time.Duration{}
	walked := map[int]bool{}
	nsSeen := map[string]bool{}
	seenBy := func(task string) bool {
		ns, err := os.Readlink(task + "/ns/mnt")
		if err != nil {
			return seen(task)
		}
		v, ok := nsSeen[ns]
		if !ok {
			v = seen(task)
			nsSeen[ns] = v
		}
		return v
	}
	var walk func(pid int) bool
	walk = func(pid int) bool {
		if walked[pid] {
			return true
		}
		walked[pid] = true
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
			t := taskTime{ran: durationOf(ran, time.Nanosecond), waited: durationOf(waited, time.Nanosecond), slices: slices}
			if b, err := os.ReadFile(task + "/stat"); err == nil {
				if st, ok := ParseProcStat(b); ok {
					t.runnable = st.State == 'R'
					t.blocked = st.State == 'D'
					born[tid] = durationOf(st.StartTime, startTick)
				}
			}
			if t.blocked {
				t.seen = seenBy(task)
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

// storageReader is how a stop reads the storage what it stops may wait on:
// the host's block devices, the devices directly below one (by its name),
// whether a thread's filesystems are all on them (given its
// /proc/<pid>/task/<tid>), and how long the kernel lets a task stand in the
// kernel before it reports it hung (nil: no longer than the stall bound).
type storageReader struct {
	disks func() (diskMark, bool)
	below func(name string) []string
	seen  func(task string) bool
	hung  func() time.Duration
}

// hostStorage reads /proc/diskstats, /sys/class/block, each thread's
// mountinfo, and the kernel's hung-task time.
var hostStorage = storageReader{
	disks: func() (diskMark, bool) { return readDiskstats("/proc/diskstats") },
	below: func(name string) []string { return storageBelow("/sys/class/block", name) },
	seen:  mountsSeen,
	hung:  func() time.Duration { return hungTaskTime("/proc/sys/kernel/hung_task_timeout_secs") },
}

// hungTaskDefault is the kernel's hung-task time by default
// (CONFIG_DEFAULT_HUNG_TASK_TIMEOUT).
const hungTaskDefault = 120 * time.Second

// hungTaskTime is how long the kernel lets a task stand in an uninterruptible
// sleep, with no turn on a CPU, before it reports it hung, as path (its
// hung_task_timeout_secs) says: where the kernel keeps none (built without
// the detector), or it is off (0), or path cannot be read, the kernel's
// default.
func hungTaskTime(path string) time.Duration {
	b, err := os.ReadFile(path)
	if err != nil {
		return hungTaskDefault
	}
	secs, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || secs == 0 {
		return hungTaskDefault
	}
	return durationOf(secs, time.Second)
}

// durationOf is n units (unit > 0) as a Duration, the longest one when n units
// would overflow it: what /proc and /sys say is parsed as unsigned 64-bit.
func durationOf(n uint64, unit time.Duration) time.Duration {
	if n > math.MaxInt64 || int64(n) > math.MaxInt64/int64(unit) {
		return math.MaxInt64
	}
	return time.Duration(n) * unit
}

// testStorage, where a test sets it, is read in hostStorage's place by the
// ExitClocks made from then on (NewExitClock); atomic, as a test may reset it
// while a stop an earlier test left runs on.
var testStorage atomic.Pointer[storageReader]

// storageNow is the storageReader a new ExitClock reads.
func storageNow() storageReader {
	if st := testStorage.Load(); st != nil {
		return *st
	}
	return hostStorage
}

// diskCount is a block device's account in /proc/diskstats: its name, the
// I/O it completed (reads, writes, discards and flushes), the I/O in flight
// on it, and how long it has had I/O in flight (its io_ticks).
type diskCount struct {
	name           string
	done, inFlight uint64
	busy           time.Duration
}

// diskMark is the host's block devices' accounts, by major:minor.
type diskMark map[string]diskCount

// readDiskstats reads a /proc/diskstats: each line a device's major, minor
// and name, then reads completed, merged, sectors and ms, the same four of
// writes, I/O in flight, ms with I/O in flight and weighted ms; from Linux
// 4.18 the four of discards, and from 5.5 flushes completed and their ms. ok
// is false where it cannot be read, or shows no device.
func readDiskstats(path string) (diskMark, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	m := diskMark{}
	for _, line := range bytes.Split(b, []byte("\n")) {
		f := bytes.Fields(line)
		if len(f) < 14 {
			continue
		}
		done := parseUint(f[3]) + parseUint(f[7])
		if len(f) >= 18 {
			done += parseUint(f[14])
		}
		if len(f) >= 20 {
			done += parseUint(f[18])
		}
		m[string(f[0])+":"+string(f[1])] = diskCount{
			name:     string(f[2]),
			done:     done,
			inFlight: parseUint(f[11]),
			busy:     durationOf(parseUint(f[12]), time.Millisecond),
		}
	}
	return m, len(m) > 0
}

// storageServed is how long, of w between two reads of the host's block
// devices, storage served I/O: the longest that one which completed I/O
// between them, or has I/O in flight, had I/O in flight, at most w; and the
// devices that stood still, with I/O in flight and none completed by them or
// by any device under them (under, by name: see ExitClock; slow, or dead).
// One that had I/O in flight at both and completed none had it in flight
// throughout. A device not read at both has no time between them, nor one
// whose counts went back (another device by its number); neither completed
// I/O between them.
func storageServed(w time.Duration, before, now diskMark, under func(name string) []string) (served time.Duration, still []string) {
	var named map[string]string
	completed := func(name string) bool {
		if named == nil {
			named = make(map[string]string, len(now))
			for dev, n := range now {
				named[n.name] = dev
			}
		}
		dev, ok := named[name]
		if !ok {
			return false
		}
		b, ok := before[dev]
		return ok && now[dev].done > b.done && now[dev].busy >= b.busy
	}
	servedUnder := func(name string) bool {
		if under == nil {
			return false
		}
		for _, u := range under(name) {
			if completed(u) {
				return true
			}
		}
		return false
	}
	for dev, n := range now {
		b, ok := before[dev]
		if !ok || n.done < b.done || n.busy < b.busy {
			continue
		}
		busy := n.busy - b.busy
		switch {
		case n.done > b.done:
		case n.inFlight > 0:
			if !servedUnder(n.name) {
				still = append(still, dev)
			}
			if b.inFlight > 0 {
				busy = w
			}
		default:
			continue
		}
		served = max(served, min(busy, w))
	}
	sort.Strings(still)
	return served, still
}

// storageBelow is the block devices directly below the one named name, as
// sys (a /sys/class/block) shows them: those it is stacked on (its slaves: a
// device-mapper, md or bcache device's), and, for a partition, its disk. Names
// are as /proc/diskstats gives them; sysfs writes a '/' in one (cciss/c0d0) as
// '!'. None where they cannot be read.
func storageBelow(sys, name string) []string {
	dir := filepath.Join(sys, strings.ReplaceAll(name, "/", "!"))
	var below []string
	if ents, err := os.ReadDir(filepath.Join(dir, "slaves")); err == nil {
		for _, e := range ents {
			below = append(below, strings.ReplaceAll(e.Name(), "!", "/"))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "partition")); err == nil {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			below = append(below, strings.ReplaceAll(filepath.Base(filepath.Dir(real)), "!", "/"))
		}
	}
	return below
}

// mountsSeen reports whether every filesystem the thread at task (a
// /proc/<pid>/task/<tid>) can reach, the mounts of its namespace, keeps its
// data on the host's block devices, whose service /proc/diskstats shows, or
// on none: false where one is served over a network or by a process (NFS,
// SMB, Ceph, 9p, FUSE: what waits on one may wait on a server, dead, that
// the host's disks say nothing of), or is of a type not known, or where its
// mounts cannot be read. The thread's own, not its process's: a process
// whose leader has exited shows none.
func mountsSeen(task string) bool {
	b, err := os.ReadFile(task + "/mountinfo")
	if err != nil {
		return false
	}
	mounts := 0
	for _, line := range bytes.Split(b, []byte("\n")) {
		f := bytes.Fields(line)
		if len(f) == 0 {
			continue
		}
		// mount id, parent id, major:minor, root, mount point, options,
		// optional fields up to a lone "-", then the filesystem's type.
		sep := -1
		for i := 6; i < len(f); i++ {
			if len(f[i]) == 1 && f[i][0] == '-' {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(f) || !onSeenStorage[string(f[sep+1])] {
			return false
		}
		mounts++
	}
	return mounts > 0
}

// onSeenStorage is every filesystem type whose data is on the host's block
// devices (a disk's filesystems, and overlay, stacked on them), or on none
// (memory, and the kernel's own views): the types a thread held in the
// kernel may wait on storage of only through those devices. Any other (NFS,
// SMB, Ceph, 9p, virtiofs, FUSE and its subtypes, or a type not listed) is
// not, and a thread that can reach one is not taken to wait on storage that
// serves (mountsSeen).
var onSeenStorage = map[string]bool{
	// On a block device.
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"f2fs": true, "vfat": true, "msdos": true, "exfat": true, "ntfs3": true,
	"jfs": true, "reiserfs": true, "bcachefs": true, "zfs": true,
	"squashfs": true, "iso9660": true, "udf": true, "erofs": true,
	"hfs": true, "hfsplus": true, "nilfs2": true, "zonefs": true,
	"ntfs": true,
	// Stacked on the above.
	"overlay": true,
	// On memory, or the kernel's own.
	"tmpfs": true, "ramfs": true, "rootfs": true, "devtmpfs": true, "proc": true,
	"sysfs": true, "devpts": true, "cgroup": true, "cgroup2": true,
	"mqueue": true, "securityfs": true, "debugfs": true, "tracefs": true,
	"pstore": true, "bpf": true, "configfs": true, "fusectl": true,
	"hugetlbfs": true, "autofs": true, "binfmt_misc": true,
	"efivarfs": true, "nsfs": true, "selinuxfs": true, "rpc_pipefs": true,
	"nfsd": true, "cpuset": true, "resctrl": true, "xenfs": true,
}

// schedAccountKept says whether this kernel keeps each thread's time on and
// waiting for a CPU (schedstat): the reading thread has run, so a kernel that
// keeps none shows it zeros, or no file. Known once.
var schedAccountKept = sync.OnceValue(func() bool {
	_, _, _, ok := readSchedstat("/proc/thread-self/schedstat")
	return ok
})
