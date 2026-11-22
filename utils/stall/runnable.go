// Package stall measures stalls in the time a process could run, not in wall
// time.
//
// A bound on "no progress for a while" decides that something is stuck: a
// parser that stopped reading, a consumer that stopped taking, a drain that
// stopped moving. Counted in wall time, it decides that of a process that got
// no CPU for the while (an overloaded host, a CPU-throttled container, a
// frozen one): what it waits on could not run either, and when the CPU comes
// back the bound has long expired and wins the race against the progress
// about to come. Counted in runnable time (Clock), a starved process accrues
// the stall only as fast as it gets to run.
//
// The stall bounds of the agent's supervisor and relay tee, the agent's boot
// beats (utils.BootBeat) and the CLI's bounds on a starting agent, and the
// CLI's record and mock-record stops are measured on the one process clock
// (Process), so they agree on what "the process could run" means. Others
// (the relay's half-close idle wait, mock replay's capture wait) are not yet.
package stall

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Clock is the time a process could run (Runnable, monotonic), and a timer to
// look at it again with. Process is the process's own; tests drive a
// ManualClock.
type Clock interface {
	// Runnable is how long the process could have run so far.
	Runnable() time.Duration
	// After is a timer: its channel fires once, d of wall time from now;
	// stop releases it.
	After(d time.Duration) (c <-chan time.Time, stop func() bool)
}

// Tick is how often the process clock samples the process (sampler).
const Tick = 10 * time.Millisecond

// HangBudget is how long, in time the process could run, something keploy
// waits on may make no progress before keploy holds it stopped: a parser
// that stopped reading (the agent's parser supervisor), and a read of a
// container's configuration the Docker daemon did not answer (containerkey).
// One bound, so the two stop the same way: a stamp waiting on such a read on
// a parser's path is let go when the parser would be.
const HangBudget = 60 * time.Second

// Process returns the process's own runnable clock, and the func that says
// the caller no longer reads it. The clock is sampled (every Tick) only while
// someone holds it: hold it for as long as a stall is being measured on it.
func Process() (Clock, func()) {
	return ProcessClock(), Hold()
}

// ProcessClock is the process's own runnable clock, not held: it moves only
// while someone holds it (Hold, Process).
func ProcessClock() Clock { return processClock{} }

// Hold holds the process clock, sampling it from the first holder on, and
// returns the func that releases it (idempotent).
func Hold() func() { return proc.acquire() }

// processClock reads the process sampler (proc).
type processClock struct{}

func (processClock) Runnable() time.Duration { return time.Duration(proc.ran.Load()) }

func (processClock) After(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

func (processClock) owed() (time.Duration, uint64, uint64) { return proc.owed() }

// proc samples the process while it is held (Process).
var proc = &sampler{src: realSource{}, tick: Tick}

// sampler accumulates the process's runnable time (ran) by sampling it every
// tick, for as long as it has users.
//
//   - A process can run for at most its CPU limit (a container's --cpus, a
//     pod's CPU limit: ProcessCPULimit) of each second of wall time, and what
//     a stall waits on (one goroutine) for at most one CPU's worth: an
//     interval counts scaled by that share. At --cpus 0.1 a bound takes ten
//     times as long in wall time. Without a limit, or at one CPU or more, it
//     is wall time, for a process with CPU to spare, busy or idle.
//   - A sample that comes late, more than two ticks after the last, shows the
//     process was held off the CPU for part of the interval (frozen, stopped,
//     or taken by the host): only what it ran then counts, its CPU time, and
//     never more than the scaled interval. A freeze of any length counts as
//     the CPU the process used before it froze. A sample is late by what its
//     own thread's wait for a CPU (ownWait) does not explain: that wait is
//     one of the threads' waits, which on-time samples pay for (below), and
//     a sample late behind it counted as late would pay for it twice. In a
//     four-CPU quota beside 26 busy loops, where one thread could run about
//     a seventh of the time, a clock that did counted 2.7 s of 117 s.
//
// A sample that comes on time does not show that the process could run the
// whole interval when it has a CPU limit: CFS hands out the quota in per-CPU
// slices, so the sampler can wake on time while another thread is throttled.
// Hence the scaling.
//
// Nor does it show that on an overloaded host: it shows that the sampler ran.
// The sampler sleeps between samples, and the kernel runs a thread that
// slept soon after it wakes, while a thread that has been working waits its
// turn behind the other work on its CPU. A process that used 27 ms of CPU in
// 1.15 s, a thread of it queued for 167 ms, had 13 samples on time, and they
// counted 146 ms: its parser, working through a chunk, was abandoned as gone.
// So on-time samples pay for the time a thread of the process waited for a
// CPU (readWaits): the most any one thread waited, as what a stall waits on
// is one goroutine, on one thread at a time. Late samples have paid already:
// they count what the process ran. A stall about to be judged reads the waits
// first (Meter.Expired). The same goes for a virtual CPU the host did not
// run (steal): on a VM whose host is busy, a thread running on it stops for
// as long as the host takes it, 250 ms at a time on a loaded host, and it
// neither runs nor waits on a run queue meanwhile; so a thread's wait is its
// run delay and the steal of the CPU it is on. Neither shows while it lasts:
// the kernel adds a wait to the run delay as it ends, and steal to the CPU's
// at its first tick back. So a thread held off right now (runnable, and not
// moved since the last read) may be in a wait no read shows yet, and a
// verdict waits for the threads that were held off when the stall was found
// due to have had their turn: it is drawn at a later look, on a read of the
// threads made since that finds none of them held off still (Meter.Expired).
// A thread held off only from later on began to wait after the stall was due
// and does not hold it back, so an overload that holds some thread of the
// process off at every read does not hold a verdict off for ever: it comes
// once the threads waiting when it was due get a CPU.
type sampler struct {
	src  source
	tick time.Duration
	ran  atomic.Int64 // runnable time so far, ns

	// readMu is held while the threads are read (readWaits, begin), one
	// read at a time; taken before mu. The sampler does not wait for it.
	readMu sync.Mutex

	mu    sync.Mutex
	users int
	stop  chan struct{}
	done  chan struct{}
	wake  *waker // ends the sampling's sleep at the last release; made at the first acquire
	// The last sample: wall time, CPU time (cpuOK: it could be read).
	lastWall time.Time
	lastCPU  time.Duration
	cpuOK    bool
	// The last read of the threads' waits (readWaits), what on-time samples
	// counted that no wait has taken, and what they have yet to pay for;
	// the read since which a thread that read found held off right now has
	// been (the earliest, 0 if none is), and how many reads there have been.
	waitWall time.Time
	onTime   time.Duration
	waitOwed time.Duration
	heldFrom uint64
	reads    uint64
	// What a verdict's read (owed) found the threads waited past the last
	// sample, in time no sample has counted yet: the next sample pays it if
	// it counts that time (on time), and it goes either way.
	ahead time.Duration
	// What late samples counted since the last read that no wait has taken
	// (lateGot), and the time of their intervals they did not count
	// (lateGap): a wait a read finds is set against that time before what
	// they counted.
	lateGot time.Duration
	lateGap time.Duration
}

// sampling is one run of the sampler (run): the stop that ends it, and its
// thread's wait for a CPU so far, at its last sample (ownOK: it could be
// read). A run delay is a total since its thread began, so a wait is only
// one thread's account against its own: one thread's set against another's
// is any difference at all, and one past the interval passes a late sample
// as on time, its whole interval counted however long the process was
// frozen. Each run reads its own locked thread, and acquire does not wait
// for the run the last release stopped (only that release does), so the
// account is the run's own, never the sampler's.
type sampling struct {
	stop    chan struct{}
	lastOwn time.Duration
	ownOK   bool
}

// heldEvery is how often the sampler reads how long the process's threads
// waited for a CPU: a read opens a file per thread, so not at every sample. A
// stall about to be judged does not wait for it (Meter.Expired).
const heldEvery = time.Second

// source is what the sampler reads: the wall clock, the process's CPU time,
// its CPU limit, and how long its threads waited for a CPU.
type source interface {
	now() (wall time.Time, cpu time.Duration, ok bool)
	cpuLimit() (cpus float64, ok bool)
	// heldOff is the most any one thread of the process was held off a CPU
	// since the last call (its run delay, and the steal of its CPU), 0
	// where that cannot be read; and for how many calls in a row, this one
	// included, the thread held off now the longest has been, 0 if none is.
	heldOff() (time.Duration, uint64)
	// ownWait is how long the calling thread (the sampler's, locked to it)
	// has waited for a CPU so far (its run delay); false where that cannot
	// be read.
	ownWait() (time.Duration, bool)
}

// acquire adds a user, starting the sampling with the first, and returns the
// func that removes it (idempotent), stopping the sampling with the last.
func (s *sampler) acquire() func() {
	s.readMu.Lock() // begin reads the threads
	s.mu.Lock()
	s.users++
	if s.users == 1 {
		if s.wake == nil {
			s.wake = newWaker()
		}
		s.begin()
		s.stop, s.done = make(chan struct{}), make(chan struct{})
		go s.run(s.stop, s.done, s.wake)
	}
	s.mu.Unlock()
	s.readMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.users--
			var stop, done chan struct{}
			if s.users == 0 {
				stop, done = s.stop, s.done
				s.stop, s.done = nil, nil
			}
			wake := s.wake
			s.mu.Unlock()
			if stop != nil {
				// The sampling sleeps until its next look: woken, it
				// ends now (sleepFor).
				close(stop)
				wake.wake()
				<-done
			}
		})
	}
}

// run samples every tick until stop, sleeping a tick after each sample, not
// on a ticker: a ticker's pending tick is delivered at once after a freeze,
// and would look on time. The thread sleeps in the kernel (sleepFor), not on
// a Go timer: one fires on whichever thread runs the timers, and on a host
// that holds the process off (a CPU quota spent, an overloaded host) that
// thread's wait made the samples late by more than their own thread waited.
// A late sample counts only the CPU the process used, and the threads' waits
// read later are taken again from on-time samples: in a four-CPU quota beside
// 26 busy loops, where a thread could run about a seventh of the time, the
// clock counted 2.7 s of 184 s, 83% of the wall time in late samples. Woken
// in its own sleep, the thread's wait for a CPU is its run delay (ownWait),
// and its samples are on time. The last release wakes that sleep (wake), so
// it returns as soon as the thread runs, as a release did on a Go timer.
func (s *sampler) run(stop, done chan struct{}, wake *waker) {
	defer close(done)
	// One thread samples, so its wait for a CPU between two samples is read
	// from its own account (ownWait).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// A wake the last sampling did not take (a signal ended its sleep after
	// its release closed stop, before the wake came) is taken before the
	// first sleep, so it does not end that sleep at once. The wake taken can
	// be this run's own release's, written before the take: its stop is
	// closed by then, and the run ends here rather than sleep its tick.
	wake.take()
	select {
	case <-stop:
		return
	default:
	}
	r := &sampling{stop: stop}
	s.mu.Lock()
	r.lastOwn, r.ownOK = s.src.ownWait()
	s.mu.Unlock()
	for !sleepFor(s.tick, stop, wake) {
		s.sample(r)
	}
}

// begin starts the sampling's accounts. Called with readMu and mu held.
func (s *sampler) begin() {
	s.lastWall, s.lastCPU, s.cpuOK = s.src.now()
	// What the threads waited while nobody held the clock is not owed, and
	// what they waited a moment ago hardly matters: a stall of one burst of
	// work after another comes here every burst, and pays for no read of the
	// threads unless heldEvery has passed.
	if s.lastWall.Sub(s.waitWall) >= heldEvery {
		s.src.heldOff()
		s.waitWall = s.lastWall
		s.reads++
	}
	s.onTime, s.waitOwed, s.ahead, s.heldFrom = 0, 0, 0, 0
	s.lateGot, s.lateGap = 0, 0
}

// sample adds what the process could run since the last sample, as run r.
func (s *sampler) sample(r *sampling) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != r.stop {
		// A release stopped r after its sleep ended, and an acquire may
		// have begun the next sampling since: r's thread's wait runs from
		// r's last sample, before that sampling began, and would be set
		// against its interval. r samples for no one now.
		return
	}
	wall, cpu, cpuOK, waited := s.read(r)
	share := 1.0
	if l, ok := s.src.cpuLimit(); ok && l > 0 && l < 1 {
		share = l
	}
	iv, ok := max(wall.Sub(s.lastWall), 0), s.cpuOK && cpuOK
	isLate := late(iv-waited, ok, s.tick)
	got := accrue(iv, cpu-s.lastCPU, isLate, share)
	if !isLate {
		// The waits read past the last sample lie in this interval: they
		// are owed out of it, and no later read takes them again.
		ahead := min(got, s.ahead)
		s.waitOwed += ahead
		s.onTime += got - ahead
		pay := min(got, s.waitOwed)
		s.waitOwed -= pay
		got -= pay
	} else {
		// A late sample counts what the process ran, every thread of it,
		// and the waits read past the last sample lie in its interval: in
		// the part it did not count, or in what other threads ran while
		// the one waiting could not. Only what does not fit in the part it
		// did not count comes out of what it counted.
		gap := accrue(iv, 0, false, share) - got
		owe := min(got, max(s.ahead-gap, 0))
		got -= owe
		s.lateGot += got
		s.lateGap += max(gap-s.ahead, 0)
	}
	s.ahead = 0
	s.ran.Add(int64(got))
	s.lastWall, s.lastCPU, s.cpuOK = wall, cpu, cpuOK
	// readMu held is a verdict's read in flight (owed), or a start's
	// (acquire): the sampler does not wait for it, and reads at its next
	// sample.
	if wall.Sub(s.waitWall) >= heldEvery && s.readMu.TryLock() {
		s.readWaits(false)
		s.readMu.Unlock()
	}
}

// ownReads is how many times at most a sample reads the wall clock (read).
const ownReads = 3

// read reads the wall clock and the process's CPU time, and how long the
// sampling thread waited for a CPU since r's last sample (waited). A sample's
// interval runs from one wall read to the next, and the wait set against it
// must have ended between them; a wait shows in the thread's account as it
// ends. So the account is read just before the wall and just after it. A
// wait that ends between those two reads may have ended on either side of
// the wall read: one ended after a frozen sample's wall read, set against
// that sample, passed it as on time, and the frozen time counted as time the
// process could run. While a wait ends between them, all three are read
// again, up to ownReads times; the waits before the last wall read are set
// against this sample, those after it against the next, and one that ends
// between the last two reads against neither, so the samples it lies between
// can only look later. Called with mu held.
func (s *sampler) read(r *sampling) (wall time.Time, cpu time.Duration, cpuOK bool, waited time.Duration) {
	var own time.Duration
	var ownOK bool
	next, nextOK := s.src.ownWait()
	for range ownReads {
		own, ownOK = next, nextOK
		wall, cpu, cpuOK = s.src.now()
		if next, nextOK = s.src.ownWait(); next == own {
			break
		}
	}
	if ownOK && r.ownOK {
		waited = max(own-r.lastOwn, 0)
	}
	r.lastOwn, r.ownOK = next, nextOK
	return wall, cpu, cpuOK, waited
}

// readWaits adds to what on-time samples owe the time the process's threads
// waited for a CPU since the last read. A wait is only owed out of what
// on-time samples counted and no wait has yet taken: what the last read
// carried over (at most heldEvery) and what came since. The rest of it was
// never counted, so what is owed stays within about two reads' worth of
// time, and a stall that is real goes on counting when the host quietens.
// But a verdict's read (ahead: owed) comes between samples, and the sampler
// may be held off then, while the threads it reads run: of the rest, what
// fits in the time since the last sample may lie in it, which the next
// sample counts if it comes on time (its own wait explains its lateness), so
// it is owed out of that sample (sampler.ahead). On a host whose busy loops
// held the sampler and a consumer off one CPU for 200 ms, a look's read set
// 57 ms of the consumer's 256 ms wait against what samples had counted, and
// the next sample counted the rest as time the consumer could run.
//
// The read opens a file per thread, and the thread making it may be held off
// meanwhile, for as long as the rest of the process: it is made outside mu,
// so the sampler does not wait for it, and one read is made at a time
// (readMu). A sample that waited for a verdict's read came late by that
// wait, which its own thread's account does not show, and counted the CPU
// every thread of the process used as time the consumer could run: on a host
// whose busy loops, in another cgroup, crowded two of the four CPUs of a
// process's quota, five of the six late samples of a stall the tee abandoned
// came right after a read, and the six counted 155 ms of the process's CPU
// as time its consumer, waiting, could run. A sample that comes while the
// read is in flight counts its interval by its own lateness, and the read,
// accounted when it ends, takes its waits out of what the samples counted.
// Called with readMu and mu held, and returns with them held.
func (s *sampler) readWaits(ahead bool) {
	s.waitWall = s.lastWall
	before := s.onTime
	lateGot, lateGap := s.lateGot, s.lateGap
	s.mu.Unlock()
	held, heldFor := s.src.heldOff()
	var wall time.Time
	if ahead {
		// The wall clock after the threads were read: a wait the read
		// found ended before it.
		wall, _, _ = s.src.now()
	}
	s.mu.Lock()
	took := min(held, s.onTime)
	rest := held - took
	s.ahead = 0
	if ahead {
		s.ahead = min(rest, max(wall.Sub(s.lastWall), 0))
		rest -= s.ahead
	}
	// The rest lay in time late samples counted before the read, or did
	// not count: set against the time they did not count first, only what
	// does not fit there is owed out of what they counted.
	s.waitOwed += took + min(max(rest-lateGap, 0), lateGot)
	s.lateGot -= lateGot
	s.lateGap -= lateGap
	// What samples counted while the read was in flight came after it
	// began: the next read takes from it too, whatever the carry's bound.
	s.onTime = min(before-took, heldEvery) + s.onTime - before
	s.reads++
	s.heldFrom = 0
	if heldFor > 0 {
		s.heldFrom = s.reads + 1 - min(heldFor, s.reads)
	}
}

// owed reads the waits, and says what on-time samples owe as of now (time on
// the clock that the process could not run, the waits past the last sample
// with it), the read since which a thread held off right now has been (0:
// none is), and which read this is. It reads them only if a sample came
// since the last read: the clock has not moved since then, and stalls at
// their verdict on every connection of a starved process must not read every
// thread at each of their looks, with the CPU it lacks.
func (s *sampler) owed() (owed time.Duration, heldFrom, read uint64) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastWall.After(s.waitWall) {
		s.readWaits(true)
	}
	return s.waitOwed + s.ahead, s.heldFrom, s.reads
}

// accrue is what a process could run in an interval iv between two samples,
// in which it used cpu of CPU time, at share of a CPU at most (see sampler):
// all of it, or for a late sample (late) only what it ran.
func accrue(iv, cpu time.Duration, isLate bool, share float64) time.Duration {
	could := time.Duration(float64(iv) * share)
	if !isLate {
		return could
	}
	return min(could, max(cpu, 0))
}

// late says whether a sample shows the process was held off the CPU: it came
// more than two ticks after the last, past what its thread waited for a CPU
// (iv: the interval less that wait), and there is a CPU clock to tell by.
func late(iv time.Duration, cpuOK bool, tick time.Duration) bool {
	return iv > 2*tick && cpuOK
}

// realSource is the process's own clocks.
type realSource struct{}

func (realSource) now() (time.Time, time.Duration, bool) {
	cpu, ok := ProcessCPUTime()
	return time.Now(), cpu, ok
}

// procWaits is the process's threads' waits; the sampler reads it one read at
// a time (sampler.readMu).
var procWaits threadWaits

func (realSource) heldOff() (time.Duration, uint64) { return procWaits.longest() }

func (realSource) ownWait() (time.Duration, bool) { return threadRunDelay() }

// cpuLimit is ProcessCPULimit, read at most once per cpuLimitRefresh: reading
// the cgroup files at every sample would cost a starved process the CPU it
// lacks. A limit changes rarely (a pod resized in place); the clock follows it
// within the refresh.
func (realSource) cpuLimit() (float64, bool) {
	c := &cpuLimitCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) >= cpuLimitRefresh {
		c.cpus, c.ok = readCPULimit()
		c.at = time.Now()
	}
	return c.cpus, c.ok
}

// cpuLimitRefresh is how long realSource keeps a CPU limit it read.
const cpuLimitRefresh = 10 * time.Second

// readCPULimit reads the process's CPU limit (ProcessCPULimit; tests count
// the reads).
var readCPULimit = ProcessCPULimit

var cpuLimitCache struct {
	mu   sync.Mutex
	at   time.Time
	cpus float64
	ok   bool
}

// Meter measures one stall: how long the process could run since the stall
// began (or since its last progress, Reset), against grace. Safe for
// concurrent use: Reset from the path that makes progress, Expired from the
// one that watches.
type Meter struct {
	clk     Clock
	grace   time.Duration
	check   time.Duration
	start   atomic.Int64 // Runnable at the stall's start, ns
	release func()
	// due marks the stall found due at a look, nil while none is. A mark
	// taken from another start is none: the stall has started again since
	// (Reset, ResetBy, which may land at any point of a look), and is due
	// only once a look finds it due anew.
	due atomic.Pointer[dueMark]

	timerMu   sync.Mutex
	stopTimer func() bool
}

// dueMark is a stall found due at a look: the start it was measured from
// (Meter.start), and the read of the threads (sampler.owed) at that look.
type dueMark struct {
	start int64
	read  uint64
}

// NewMeter starts measuring a stall of grace on clk; a nil clk is the
// process's own (Process), held until Stop.
func NewMeter(clk Clock, grace time.Duration) *Meter {
	m := &Meter{clk: clk, grace: grace, check: CheckEvery(grace), release: func() {}}
	if clk == nil {
		m.clk, m.release = Process()
	}
	m.Reset()
	return m
}

// CheckEvery is how often a stall of grace is looked at (Meter.Wait): every
// tenth of it of wall time, but no more often than every 5 ms and no less
// often than every second, so a stall is acted on within a tenth of its
// bound (and a second) of expiring.
func CheckEvery(grace time.Duration) time.Duration {
	return min(max(grace/10, 5*time.Millisecond), time.Second)
}

// Reset says the watched work made progress: the stall starts again.
func (m *Meter) Reset() { m.start.Store(int64(m.clk.Runnable())) }

// ResetBy says d of the stall so far is spent: the stall starts d later, and
// what the process could run past it counts toward the next. A watcher that
// counts the stall in whole units of d (a reporter's beats) loses no part of
// one between its looks.
func (m *Meter) ResetBy(d time.Duration) { m.start.Add(int64(d)) }

// Stalled is how long the process could run since the stall began.
func (m *Meter) Stalled() time.Duration { return m.since(m.start.Load()) }

// since is how long the process could run since start (a Meter.start).
func (m *Meter) since(start int64) time.Duration {
	return max(m.clk.Runnable()-time.Duration(start), 0)
}

// Expired reports whether the process has had the whole grace to run since
// the stall began. A clock that knows of time on it the process could not run
// (the process clock: sampler.owed) is asked at the verdict, not before it,
// and the verdict waits for what it cannot know yet: the look that finds the
// stall due only marks it, and a later look draws it, on a read of the
// threads made since that finds none of those held off when it was due held
// off still (sampler). So a stall is acted on a look or two after it is due,
// once the threads that were waiting then have had their turn. A stall that
// starts again (Reset, ResetBy) is drawn the same way: the mark is of the
// start it was found due from.
func (m *Meter) Expired() bool {
	start := m.start.Load()
	if m.since(start) < m.grace {
		return false
	}
	c, ok := m.clk.(interface {
		owed() (time.Duration, uint64, uint64)
	})
	if !ok {
		return true
	}
	owed, heldFrom, read := c.owed()
	if m.since(start)-owed < m.grace {
		// Not due after all: found due again, it is marked again, on a
		// read from then.
		m.due.Store(nil)
		return false
	}
	mark := m.due.Load()
	if mark == nil || mark.start != start {
		// Due at this look: drawn at a later one.
		m.due.Store(&dueMark{start: start, read: read})
		return false
	}
	due := mark.read
	// A thread first found held off at the read after the due one or before
	// it may have been waiting when the stall was found due: it has not run
	// since the read before that, and may have stopped just after it. Its
	// wait shows once it has had its turn. A thread held off only from a
	// later read on ran after the stall was due: what it waits now came
	// after, and does not hold the verdict back, so an overload that holds
	// some thread off at every read does not hold it off for ever.
	return read != due && (heldFrom == 0 || heldFrom > due+1)
}

// Grace is the stall's bound.
func (m *Meter) Grace() time.Duration { return m.grace }

// Wait arms the next look at the stall (CheckEvery) and returns its channel.
func (m *Meter) Wait() <-chan time.Time {
	c, stop := m.clk.After(m.check)
	m.timerMu.Lock()
	m.stopTimer = stop
	m.timerMu.Unlock()
	return c
}

// Stop releases the pending look's timer, and the process clock if the meter
// held it. Nil-safe; idempotent.
func (m *Meter) Stop() {
	if m == nil {
		return
	}
	m.timerMu.Lock()
	if m.stopTimer != nil {
		m.stopTimer()
		m.stopTimer = nil
	}
	m.timerMu.Unlock()
	m.release()
}

// ManualClock is a Clock a test drives. Runnable moves only by Advance (and
// Step). Its timers (After) stand for wall time passing:
//   - with Step set, each fires at once, having moved Runnable by Step(d) for
//     the wall time d it stood for: a process with CPU to spare is OnTime, a
//     starved one a Step that returns less than d, a frozen one 0;
//   - without, each waits for the test: Advance fires the pending timers once
//     it has moved Runnable (the process ran while they were pending), and
//     Pass fires them without (wall time passed, and the process could not
//     run).
type ManualClock struct {
	mu      sync.Mutex
	ran     time.Duration
	pending []chan time.Time
	Step    func(d time.Duration) time.Duration
	// Waits counts the timers asked for.
	Waits int
}

// Runnable is the time Advance (and Step) has moved it by.
func (c *ManualClock) Runnable() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ran
}

// Advance moves Runnable by d, then fires the pending timers.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.ran += d
	c.mu.Unlock()
	c.Pass()
}

// Pass fires the pending timers without moving Runnable.
func (c *ManualClock) Pass() {
	c.mu.Lock()
	p := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, ch := range p {
		ch <- time.Time{}
	}
}

// Pending is how many timers wait to fire.
func (c *ManualClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// After is a timer (see ManualClock).
func (c *ManualClock) After(d time.Duration) (<-chan time.Time, func() bool) {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Waits++
	if c.Step != nil {
		c.ran += c.Step(d)
		ch <- time.Time{}
		return ch, func() bool { return true }
	}
	c.pending = append(c.pending, ch)
	return ch, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i, p := range c.pending {
			if p == ch {
				c.pending = append(c.pending[:i], c.pending[i+1:]...)
				return true
			}
		}
		return false
	}
}

// OnTime is a ManualClock Step for a process with CPU to spare: it could run
// for all of every wait.
func OnTime(d time.Duration) time.Duration { return d }
