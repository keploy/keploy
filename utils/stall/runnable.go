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

func (processClock) owed() time.Duration { return proc.owed() }

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
//     or waiting behind other work on an overloaded host): only what it ran
//     then counts, its CPU time, and never more than the scaled interval. A
//     freeze of any length counts as the CPU the process used before it froze.
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
// first (Meter.Expired). What that cannot see is the wait in progress, which
// the kernel adds up when it ends: one turn behind the work on a CPU.
type sampler struct {
	src  source
	tick time.Duration
	ran  atomic.Int64 // runnable time so far, ns

	mu    sync.Mutex
	users int
	stop  chan struct{}
	done  chan struct{}
	// The last sample: wall time, CPU time (cpuOK: it could be read).
	lastWall time.Time
	lastCPU  time.Duration
	cpuOK    bool
	// The last read of the threads' waits (readWaits), what on-time samples
	// counted that no wait has taken, and what they have yet to pay for.
	waitWall time.Time
	onTime   time.Duration
	waitOwed time.Duration
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
	// heldOff is the most any one thread of the process waited for a CPU
	// since the last call, 0 where that cannot be read.
	heldOff() time.Duration
}

// acquire adds a user, starting the sampling with the first, and returns the
// func that removes it (idempotent), stopping the sampling with the last.
func (s *sampler) acquire() func() {
	s.mu.Lock()
	s.users++
	if s.users == 1 {
		s.lastWall, s.lastCPU, s.cpuOK = s.src.now()
		// What the threads waited while nobody held the clock is not owed,
		// and what they waited a moment ago hardly matters: a stall of one
		// burst of work after another comes here every burst, and pays for
		// no read of the threads unless heldEvery has passed.
		if s.lastWall.Sub(s.waitWall) >= heldEvery {
			s.src.heldOff()
			s.waitWall = s.lastWall
		}
		s.onTime, s.waitOwed = 0, 0
		s.stop, s.done = make(chan struct{}), make(chan struct{})
		go s.run(s.stop, s.done)
	}
	s.mu.Unlock()
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
			s.mu.Unlock()
			if stop != nil {
				close(stop)
				<-done
			}
		})
	}
}

// run samples every tick until stop. A timer re-armed after each sample, not
// a ticker: a ticker's pending tick is delivered at once after a freeze, and
// would look on time.
func (s *sampler) run(stop, done chan struct{}) {
	defer close(done)
	t := time.NewTimer(s.tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.sample()
			t.Reset(s.tick)
		}
	}
}

// sample adds what the process could run since the last sample.
func (s *sampler) sample() {
	s.mu.Lock()
	defer s.mu.Unlock()
	wall, cpu, cpuOK := s.src.now()
	share := 1.0
	if l, ok := s.src.cpuLimit(); ok && l > 0 && l < 1 {
		share = l
	}
	iv, ok := max(wall.Sub(s.lastWall), 0), s.cpuOK && cpuOK
	got := accrue(iv, cpu-s.lastCPU, ok, s.tick, share)
	if !late(iv, ok, s.tick) {
		s.onTime += got
		pay := min(got, s.waitOwed)
		s.waitOwed -= pay
		got -= pay
	}
	s.ran.Add(int64(got))
	s.lastWall, s.lastCPU, s.cpuOK = wall, cpu, cpuOK
	if wall.Sub(s.waitWall) >= heldEvery {
		s.readWaits()
	}
}

// readWaits adds to what on-time samples owe the time the process's threads
// waited for a CPU since the last read. A wait is only owed out of what
// on-time samples counted and no wait has yet taken, of the last heldEvery:
// the rest of it was never counted, so what is owed stays within a read's
// worth of time, and a stall that is real goes on counting when the host
// quietens. Called with mu held.
func (s *sampler) readWaits() {
	took := min(s.src.heldOff(), s.onTime)
	s.waitOwed += took
	s.onTime, s.waitWall = min(s.onTime-took, heldEvery), s.lastWall
}

// owed reads the waits, and says what on-time samples owe as of now: time on
// the clock that the process could not run. It reads them only if a sample
// came since the last read: the clock has not moved since then, and stalls
// at their verdict on every connection of a starved process must not read
// every thread at each of their looks, with the CPU it lacks.
func (s *sampler) owed() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastWall.After(s.waitWall) {
		s.readWaits()
	}
	return s.waitOwed
}

// accrue is what a process could run in an interval iv between two samples
// tick apart, in which it used cpu of CPU time (cpuOK: both readings were
// had), at share of a CPU at most (see sampler).
func accrue(iv, cpu time.Duration, cpuOK bool, tick time.Duration, share float64) time.Duration {
	could := time.Duration(float64(iv) * share)
	if !late(iv, cpuOK, tick) {
		return could
	}
	return min(could, max(cpu, 0))
}

// late says whether a sample shows the process was held off the CPU: it came
// more than two ticks after the last, and there is a CPU clock to tell by.
func late(iv time.Duration, cpuOK bool, tick time.Duration) bool {
	return iv > 2*tick && cpuOK
}

// realSource is the process's own clocks.
type realSource struct{}

func (realSource) now() (time.Time, time.Duration, bool) {
	cpu, ok := ProcessCPUTime()
	return time.Now(), cpu, ok
}

// procWaits is the process's threads' waits; the sampler's lock guards it.
var procWaits threadWaits

func (realSource) heldOff() time.Duration { return procWaits.longest() }

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

	timerMu   sync.Mutex
	stopTimer func() bool
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
func (m *Meter) Stalled() time.Duration {
	return max(m.clk.Runnable()-time.Duration(m.start.Load()), 0)
}

// Expired reports whether the process has had the whole grace to run since
// the stall began. A clock that knows of time on it the process could not run
// (the process clock: sampler.owed) is asked at the verdict, not before it.
func (m *Meter) Expired() bool {
	if m.Stalled() < m.grace {
		return false
	}
	c, ok := m.clk.(interface{ owed() time.Duration })
	return !ok || m.Stalled()-c.owed() >= m.grace
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
