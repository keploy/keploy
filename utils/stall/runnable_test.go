package stall

import (
	"testing"
	"time"
)

// fakeSource is a process's clocks, moved by the test: each step moves the
// wall clock and the CPU time by what it says.
type fakeSource struct {
	wall  time.Time
	cpu   time.Duration
	limit float64 // CPUs; 0: none
	reads int     // of the CPU limit
}

func (f *fakeSource) now() (time.Time, time.Duration, bool) { return f.wall, f.cpu, true }
func (f *fakeSource) cpuLimit() (float64, bool) {
	f.reads++
	return f.limit, f.limit > 0
}

// newFakeSampler is a sampler over a fakeSource, started as acquire starts
// one (without its goroutine: the test samples).
func newFakeSampler(limit float64) (*sampler, *fakeSource) {
	src := &fakeSource{wall: time.Unix(1_000_000, 0), limit: limit}
	s := &sampler{src: src, tick: Tick}
	s.lastWall, s.lastCPU, s.cpuOK = src.now()
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
