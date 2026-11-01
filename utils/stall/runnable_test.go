package stall

import (
	"testing"
	"time"
)

// fakeSource is a process's clocks, moved by the test: each step moves the
// wall clock and the CPU time by what it says.
type fakeSource struct {
	wall     time.Time
	cpu      time.Duration
	limit    float64       // CPUs; 0: none
	reads    int           // of the CPU limit
	held     time.Duration // the threads' wait the next heldOff says
	heldNow  bool          // whether heldOff says a thread is held off now (holdOff)
	heldFrom int           // the call of heldOff from which it is
	heldFor  uint64        // the calls of heldOff it has been held off for
	heldN    int           // calls of heldOff
}

// holdOff says a thread is held off from the next read of the threads on, or
// is no longer.
func (f *fakeSource) holdOff(now bool) { f.heldNow, f.heldFrom = now, f.heldN+1 }

func (f *fakeSource) now() (time.Time, time.Duration, bool) { return f.wall, f.cpu, true }
func (f *fakeSource) cpuLimit() (float64, bool) {
	f.reads++
	return f.limit, f.limit > 0
}
func (f *fakeSource) heldOff() (time.Duration, uint64) {
	d := f.held
	f.held = 0
	f.heldN++
	f.heldFor = 0
	if f.heldNow {
		f.heldFor = uint64(f.heldN - f.heldFrom + 1)
	}
	return d, f.heldFor
}

// newFakeSampler is a sampler over a fakeSource, started as acquire starts
// one (without its goroutine: the test samples).
func newFakeSampler(limit float64) (*sampler, *fakeSource) {
	src := &fakeSource{wall: time.Unix(1_000_000, 0), limit: limit}
	s := &sampler{src: src, tick: Tick}
	s.lastWall, s.lastCPU, s.cpuOK = src.now()
	s.waitWall = s.lastWall
	return s, src
}

// samplesUntil samples s after each step of wall and cpu until it has
// accrued grace, and says how many samples that took.
func samplesUntil(s *sampler, src *fakeSource, grace, wall, cpu time.Duration) int {
	n := 0
	for time.Duration(s.ran.Load()) < grace {
		src.wall, src.cpu = src.wall.Add(wall), src.cpu+cpu
		s.sample()
		n++
	}
	return n
}

// A process with CPU to spare, busy or idle, accrues wall time: a bound on a
// stall that is real is not waited on for longer than its wall time.
func TestSampler_OnTimeSamplesCountWallTime(t *testing.T) {
	s, src := newFakeSampler(0)
	if n := samplesUntil(s, src, 2*time.Second, Tick, 0); n != 200 {
		t.Fatalf("accrued 2 s after %d on-time samples, want 200 (2 s of 10 ms)", n)
	}
}

// On an overloaded host the sampler is on time while the thread a stall waits
// on queues behind other work: what on-time samples count pays for that wait.
// A thread that waits three parts in four accrues a quarter of wall time.
func TestSampler_OnTimeSamplesPayForTheTimeAThreadWaited(t *testing.T) {
	s, src := newFakeSampler(0)
	m := NewMeter(heldClock{s}, 2*time.Second)
	n := 0
	for !m.Expired() {
		src.wall, src.cpu = src.wall.Add(Tick), src.cpu+Tick/4
		src.held += Tick * 3 / 4
		s.sample()
		n++
	}
	if n < 800 || n > 804 {
		t.Fatalf("2 s expired after %d on-time samples, a thread waiting 3/4 of them, want about 800 (8 s of wall time)", n)
	}
}

// A late sample counts only what the process ran, so a wait beside it was
// never counted, and on-time samples do not pay for it again.
func TestSampler_AWaitBesideLateSamplesIsNotPaidTwice(t *testing.T) {
	s, src := newFakeSampler(0)
	for range 20 { // 2 s late, using 200 ms of CPU, a thread waiting 1800 ms
		src.wall, src.cpu = src.wall.Add(100*time.Millisecond), src.cpu+10*time.Millisecond
		src.held += 90 * time.Millisecond
		s.sample()
	}
	if got, want := time.Duration(s.ran.Load()), 200*time.Millisecond; got != want {
		t.Fatalf("late samples accrued %v, want what the process ran: %v", got, want)
	}
	before := s.ran.Load()
	for range 10 {
		src.wall = src.wall.Add(Tick)
		s.sample()
	}
	if got, want := time.Duration(s.ran.Load()-before), 10*Tick; got != want {
		t.Fatalf("%v on time after the late samples accrued %v: the wait beside them was paid twice", want, got)
	}
}

// What on-time samples owe is bounded by what they counted: a long stretch
// that kept a process off the CPU does not keep a stall that is real from
// counting once it is over.
func TestSampler_WhatWaitingOwesIsBounded(t *testing.T) {
	s, src := newFakeSampler(0)
	for range 6000 { // a minute on time, a thread said to wait ten times that
		src.wall, src.cpu = src.wall.Add(Tick), src.cpu+Tick/10
		src.held += 10 * Tick
		s.sample()
	}
	if s.waitOwed > heldEvery+Tick {
		t.Fatalf("owes %v after a minute, past a read's worth", s.waitOwed)
	}
	before := s.ran.Load()
	for range 300 { // the host quietens
		src.wall, src.cpu = src.wall.Add(Tick), src.cpu+Tick/10
		s.sample()
	}
	if got := time.Duration(s.ran.Load() - before); got < 3*time.Second-heldEvery-Tick {
		t.Fatalf("3 s on a quiet host accrued %v, want all but a read's worth", got)
	}
}

// Verdicts read the threads at most once a sample: the clock does not move in
// between, and every stalled connection of a starved process looks.
func TestSampler_VerdictsReadTheThreadsOncePerSample(t *testing.T) {
	s, src := newFakeSampler(0)
	src.wall = src.wall.Add(Tick)
	s.sample()
	for range 10 {
		s.owed()
	}
	if src.heldN != 1 {
		t.Fatalf("read the threads %d times for 10 verdicts on one sample, want once", src.heldN)
	}
	src.wall = src.wall.Add(Tick)
	s.sample()
	s.owed()
	if src.heldN != 2 {
		t.Fatalf("read the threads %d times after the next sample, want twice", src.heldN)
	}
}

// A read looks at every thread of the process, so it comes once a heldEvery.
func TestSampler_ReadsTheThreadsOncePerHeldEvery(t *testing.T) {
	s, src := newFakeSampler(0)
	if n := samplesUntil(s, src, 5*heldEvery, Tick, 0); n != int(5*heldEvery/Tick) {
		t.Fatalf("accrued %v after %d samples, with no thread waiting", 5*heldEvery, n)
	}
	if src.heldN != 5 {
		t.Fatalf("read the threads %d times in %v, want 5", src.heldN, 5*heldEvery)
	}
}

// heldClock is a process clock over a sampler the test samples.
type heldClock struct{ s *sampler }

func (c heldClock) Runnable() time.Duration { return time.Duration(c.s.ran.Load()) }
func (c heldClock) After(time.Duration) (<-chan time.Time, func() bool) {
	return nil, func() bool { return true }
}
func (c heldClock) owed() (time.Duration, uint64, uint64) { return c.s.owed() }

// The verdict is what must not wait for the next read: a stall that reaches
// its bound on samples that counted a wait not read yet has not expired, and
// goes on as what the process could run.
func TestMeter_AVerdictReadsTheWaitsFirst(t *testing.T) {
	s, src := newFakeSampler(0)
	m := NewMeter(heldClock{s}, 200*time.Millisecond)
	step := func(n int) {
		for range n {
			src.wall, src.cpu = src.wall.Add(Tick), src.cpu+Tick/4
			s.sample()
		}
	}
	step(20) // the grace, on time
	if m.Stalled() < 200*time.Millisecond {
		t.Fatalf("stalled %v on the samples", m.Stalled())
	}
	src.held = 150 * time.Millisecond // the thread gets the CPU, after waiting that long
	if m.Expired() {
		t.Fatal("expired on 200 ms on time, 150 ms of it a thread's wait")
	}
	step(14)
	if m.Expired() {
		t.Fatalf("expired after %v on the samples, %v owed", m.Stalled(), s.waitOwed)
	}
	step(1)
	if m.Expired() {
		t.Fatal("expired at the look that found it due: the verdict is the next look's")
	}
	if m.Expired() {
		t.Fatal("expired with no read of the threads since the look that found it due")
	}
	step(1)
	if !m.Expired() {
		t.Fatalf("not expired on 200 ms that could run: %v on the samples, %v owed", m.Stalled(), s.waitOwed)
	}
}

// A thread held off right now (the host took its virtual CPU, or it waits on
// a run queue) has a wait the kernel shows only once it ends. The verdict
// waits for it: on a VM whose host took the consumer's CPU for 250 ms, while
// the sampler ran on time on another, the stall was not the consumer's.
func TestMeter_AVerdictWaitsForAThreadHeldOffNow(t *testing.T) {
	s, src := newFakeSampler(0)
	m := NewMeter(heldClock{s}, 100*time.Millisecond)
	step := func() {
		src.wall, src.cpu = src.wall.Add(Tick), src.cpu+Tick/4
		s.sample()
	}
	for range 5 {
		step() // the consumer works
	}
	src.holdOff(true) // the host takes its CPU
	for range 25 {
		step()
		if m.Expired() {
			t.Fatalf("expired at %v on the samples while a thread is held off now", m.Stalled())
		}
	}
	src.holdOff(false)
	src.held = 250 * time.Millisecond // back, and the steal shows
	step()
	if m.Expired() || m.due.Load() != 0 {
		t.Fatalf("expired or due on %v on the samples, %v of it the host's", m.Stalled(), s.waitOwed)
	}
	// The stall goes on as what the process could run: 60 ms of the 100,
	// due 40 ms on, drawn at the look after.
	n := 0
	for !m.Expired() {
		if n++; n > 10 {
			t.Fatal("never expired")
		}
		step()
	}
	if n != 5 {
		t.Fatalf("expired %d samples after the host gave the CPU back, want 5: due at 4, drawn at 5", n)
	}
}

// A thread first found held off at the read after the one that found the
// stall due has not run since that read, and the host may have taken its CPU
// just after it: its wait began before the stall was due, and the verdict
// waits for it too.
func TestMeter_AVerdictWaitsForAThreadHeldOffFromTheReadAfterItWasDue(t *testing.T) {
	s, src := newFakeSampler(0)
	m := NewMeter(heldClock{s}, 100*time.Millisecond)
	step := func() {
		src.wall, src.cpu = src.wall.Add(Tick), src.cpu+Tick/4
		s.sample()
	}
	for n := 0; m.due.Load() == 0; n++ {
		if n > 20 {
			t.Fatal("never due")
		}
		step()
		if m.Expired() {
			t.Fatal("expired at the look that found it due")
		}
	}
	src.holdOff(true) // the next read finds it held off
	for range 25 {
		step()
		if m.Expired() {
			t.Fatalf("expired at %v on the samples while a thread held off since the read after it was due is", m.Stalled())
		}
	}
	src.holdOff(false)
	src.held = 300 * time.Millisecond // back, and the steal shows: since 50 ms before it was due
	step()
	if m.Expired() || m.due.Load() != 0 {
		t.Fatalf("expired or due on %v on the samples, %v of it the host's", m.Stalled(), s.waitOwed)
	}
}

// A late sample shows the process was held off the CPU (frozen, throttled,
// behind other work): only the CPU it used counts.
func TestSampler_LateSamplesCountOnlyWhatTheProcessRan(t *testing.T) {
	// A tenth of a CPU in CFS's 100 ms periods: 10 ms run, 90 ms throttled,
	// so each 10 ms sample comes 100 ms later having used 10 ms of CPU.
	s, src := newFakeSampler(0)
	if n := samplesUntil(s, src, 2*time.Second, 100*time.Millisecond, 10*time.Millisecond); n != 200 {
		t.Fatalf("accrued 2 s after %d throttled samples (%v of wall time), want 200: 2 s of what the process ran",
			n, time.Duration(n)*100*time.Millisecond)
	}

	// A freeze: a sample comes 5 s late, and the process ran 1 ms of it.
	s, src = newFakeSampler(0)
	src.wall, src.cpu = src.wall.Add(5*time.Second), src.cpu+time.Millisecond
	s.sample()
	if got := time.Duration(s.ran.Load()); got != time.Millisecond {
		t.Fatalf("a 5 s freeze counted %v, want the 1 ms the process ran", got)
	}

	// Busy on many cores and late only because of that: CPU time outruns
	// wall time, and the interval is what counts.
	s, src = newFakeSampler(0)
	src.wall, src.cpu = src.wall.Add(50*time.Millisecond), src.cpu+400*time.Millisecond
	s.sample()
	if got := time.Duration(s.ran.Load()); got != 50*time.Millisecond {
		t.Fatalf("a busy multi-core interval counted %v, want its 50 ms of wall time", got)
	}
}

// A process limited to a tenth of a CPU (docker --cpus 0.1, a pod's CPU
// limit) can run for a tenth of each interval, however punctually the sampler
// wakes: CFS hands the quota out in per-CPU slices, so one thread can wake on
// time while another is throttled.
func TestSampler_ACPULimitScalesTheTime(t *testing.T) {
	s, src := newFakeSampler(0.1)
	if n := samplesUntil(s, src, 2*time.Second, Tick, 0); n != 2000 {
		t.Fatalf("accrued 2 s after %d on-time samples (%v of wall time) at a tenth of a CPU, want 2000 (20 s)",
			n, time.Duration(n)*Tick)
	}
	// A limit of a CPU or more leaves it at wall time: what a stall waits on
	// runs on one CPU.
	s, src = newFakeSampler(4)
	if n := samplesUntil(s, src, 100*time.Millisecond, Tick, 0); n != 10 {
		t.Fatalf("at a 4-CPU limit 100 ms took %d samples, want 10", n)
	}
}

// The process's CPU limit is read from its cgroup files at most once per
// refresh, not at each sample: a starved process would pay a read of several
// files a hundred times a second.
func TestRealSource_ReadsTheCPULimitOncePerRefresh(t *testing.T) {
	reads := 0
	saved := readCPULimit
	readCPULimit = func() (float64, bool) { reads++; return 0.1, true }
	cpuLimitCache.mu.Lock()
	cpuLimitCache.at = time.Time{}
	cpuLimitCache.mu.Unlock()
	t.Cleanup(func() {
		readCPULimit = saved
		cpuLimitCache.mu.Lock()
		cpuLimitCache.at = time.Time{}
		cpuLimitCache.mu.Unlock()
	})
	for i := 0; i < 100; i++ {
		if l, ok := (realSource{}).cpuLimit(); !ok || l != 0.1 {
			t.Fatalf("limit %v %v, want the 0.1 CPU limit", l, ok)
		}
	}
	if reads != 1 {
		t.Fatalf("read the CPU limit %d times for 100 samples, want once", reads)
	}
	cpuLimitCache.mu.Lock()
	cpuLimitCache.at = time.Now().Add(-cpuLimitRefresh)
	cpuLimitCache.mu.Unlock()
	(realSource{}).cpuLimit()
	if reads != 2 {
		t.Fatalf("read the CPU limit %d times, want it read again after the refresh", reads)
	}
}

// The process clock samples only while someone holds it, and its runnable
// time moves while it does.
func TestProcess_SamplesWhileHeld(t *testing.T) {
	clk, release := Process()
	before := clk.Runnable()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for clk.Runnable() == before {
		if time.Now().After(deadline) {
			t.Fatal("the process clock did not move while held")
		}
		time.Sleep(Tick)
	}
	_, release2 := Process()
	release()
	release() // a second call is the first's
	proc.mu.Lock()
	users, running := proc.users, proc.stop != nil
	proc.mu.Unlock()
	if users != 1 || !running {
		t.Fatalf("with one holder left: %d users, sampling %v; want 1, sampling", users, running)
	}
	release2()
	proc.mu.Lock()
	users, running = proc.users, proc.stop != nil
	proc.mu.Unlock()
	if users != 0 || running {
		t.Fatalf("with no holder: %d users, sampling %v; want none, stopped", users, running)
	}
	at := clk.Runnable()
	time.Sleep(5 * Tick)
	if clk.Runnable() != at {
		t.Fatal("the process clock moved with no one holding it")
	}
}

// A meter expires once the process could run for its grace since the stall
// began; progress (Reset) starts it again; wall time the process could not run
// does not count.
func TestMeter_ExpiresOnRunnableTimeOnly(t *testing.T) {
	clk := &ManualClock{}
	m := NewMeter(clk, time.Second)
	defer m.Stop()
	clk.Advance(900 * time.Millisecond)
	if m.Expired() {
		t.Fatal("expired before its grace")
	}
	m.Reset()
	clk.Advance(900 * time.Millisecond)
	if m.Expired() {
		t.Fatal("expired before its grace since the progress")
	}
	for i := 0; i < 100; i++ { // wall time passing, the process starved
		c := m.Wait()
		clk.Pass()
		<-c
	}
	if m.Expired() {
		t.Fatal("expired on wall time the process could not run")
	}
	clk.Advance(100 * time.Millisecond)
	if !m.Expired() || m.Stalled() != time.Second {
		t.Fatalf("not expired once the process could run for its grace (stalled %v)", m.Stalled())
	}
}

// ResetBy spends d of the stall and keeps what the process could run past
// it: a watcher counting the stall in whole units loses no part of one.
func TestMeter_ResetByKeepsWhatRanPastIt(t *testing.T) {
	clk := &ManualClock{}
	m := NewMeter(clk, time.Second)
	defer m.Stop()
	clk.Advance(1500 * time.Millisecond)
	m.ResetBy(time.Second)
	if got := m.Stalled(); got != 500*time.Millisecond {
		t.Fatalf("stalled %v after spending a second of 1.5 s, want the 500 ms past it kept", got)
	}
	clk.Advance(500 * time.Millisecond)
	if !m.Expired() {
		t.Fatalf("not expired a second after the spent one (stalled %v)", m.Stalled())
	}
}

// A meter on the process clock holds the clock until it stops.
func TestMeter_HoldsTheProcessClockUntilStopped(t *testing.T) {
	m := NewMeter(nil, time.Hour)
	proc.mu.Lock()
	users := proc.users
	proc.mu.Unlock()
	if users < 1 {
		t.Fatal("a meter on the process clock does not hold it")
	}
	m.Stop()
	m.Stop()
	proc.mu.Lock()
	after := proc.users
	proc.mu.Unlock()
	if after != users-1 {
		t.Fatalf("a stopped meter left %d holders, want %d", after, users-1)
	}
}

// The sampling stops with the clock's last holder and starts again with the
// next: the time in between, which nothing measured, is not counted, however
// much CPU the process used in it. Without the restart's fresh sample, the
// first sample after a gap between connections credits a fresh stall with the
// whole gap.
func TestSampler_ARestartDoesNotCountTheGap(t *testing.T) {
	src := &fakeSource{wall: time.Unix(1_000_000, 0)}
	s := &sampler{src: src, tick: time.Hour} // the test samples, not the goroutine
	s.acquire()()
	src.wall, src.cpu = src.wall.Add(time.Minute), src.cpu+30*time.Second // busy, unmeasured
	release := s.acquire()
	defer release()
	src.wall = src.wall.Add(Tick)
	s.sample()
	if got := time.Duration(s.ran.Load()); got != Tick {
		t.Fatalf("the first sample after a restart counted %v, want the %v since the restart", got, Tick)
	}
}

// A stall of one burst of work after another starts the sampling each burst:
// that reads the threads no more often than sampling on does.
func TestSampler_RestartsReadTheThreadsOncePerHeldEvery(t *testing.T) {
	src := &fakeSource{wall: time.Unix(1_000_000, 0)}
	s := &sampler{src: src, tick: time.Hour} // the test samples, not the goroutine
	for range 100 {
		release := s.acquire()
		src.wall = src.wall.Add(Tick)
		s.sample()
		release()
	}
	if src.heldN > 2 {
		t.Fatalf("read the threads %d times in 100 restarts over %v, want one a %v", src.heldN, 100*Tick, heldEvery)
	}
}

// Nor is what the threads waited in the gap owed, or what was owed before it.
func TestSampler_ARestartOwesNoWaitFromBeforeIt(t *testing.T) {
	src := &fakeSource{wall: time.Unix(1_000_000, 0)}
	s := &sampler{src: src, tick: time.Hour} // the test samples, not the goroutine
	s.acquire()()
	s.waitOwed = 300 * time.Millisecond
	src.wall, src.held = src.wall.Add(time.Minute), 30*time.Second
	release := s.acquire()
	defer release()
	for range 10 {
		src.wall, src.cpu = src.wall.Add(Tick), src.cpu+Tick
		s.sample()
	}
	if got, want := time.Duration(s.ran.Load()), 10*Tick; got != want {
		t.Fatalf("10 ticks after a restart counted %v, want all of them: %v", got, want)
	}
}
