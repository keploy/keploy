package relay

import (
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// fakeStallClock drives a stall window (stallMeter) without real time: each
// wait fires at once, and advance says how far the wall clock and the
// process's CPU time move while it is pending.
type fakeStallClock struct {
	mu      sync.Mutex
	wall    time.Time
	cpu     time.Duration
	advance func(d time.Duration) (wall, cpu time.Duration)
	asked   []time.Duration
	limit   float64 // CPUs; 0: none
}

// onTime is a process with CPU to spare: each wait ends when it is due.
func onTime(d time.Duration) (time.Duration, time.Duration) { return d, 0 }

func newFakeStallClock(advance func(time.Duration) (time.Duration, time.Duration)) *fakeStallClock {
	return &fakeStallClock{wall: time.Unix(1_000_000, 0), advance: advance}
}

func (f *fakeStallClock) now() (time.Time, time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wall, f.cpu, true
}

func (f *fakeStallClock) cpuLimit() (float64, bool) { return f.limit, f.limit > 0 }

func (f *fakeStallClock) after(d time.Duration) (<-chan time.Time, func() bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, d)
	w, c := f.advance(d)
	f.wall, f.cpu = f.wall.Add(w), f.cpu+c
	ch := make(chan time.Time, 1)
	ch <- f.wall
	return ch, func() bool { return true }
}

func (f *fakeStallClock) waits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

// newTestTeeClock is newTestTeeWithConsumer on an injected stall clock, with a
// consumer that never goes away.
func newTestTeeClock(t *testing.T, capBytes int64, buf int, clk stallClock) (*tee, *dropRecorder) {
	t.Helper()
	rec := &dropRecorder{}
	t2 := newTee(fakeconn.FromClient, capBytes, buf, testStallGrace, nil, rec.record, nil)
	t2.clock = clk
	t2.start(make(chan struct{}))
	t.Cleanup(func() {
		t2.close()
		t2.waitDone()
	})
	return t2, rec
}

// A process with CPU to spare, busy or idle, has the grace in wall time: a
// consumer that stopped reading is not waited on for longer than before.
func TestStallMeter_OnTimeSamplesCountWallTime(t *testing.T) {
	clk := newFakeStallClock(onTime)
	m := newStallMeter(clk, 2*time.Second)
	n := 0
	for {
		m.wait()
		n++
		if m.expired() {
			break
		}
	}
	if n != 200 {
		t.Fatalf("expired after %d on-time samples, want 200 (2 s of 10 ms)", n)
	}
	for _, d := range clk.asked {
		if d != stallTick {
			t.Fatalf("sampled every %v, want every %v: a longer tick lets a whole slice of a starved process's run count, a shorter one burns CPU", d, stallTick)
		}
	}
}

// A late sample shows the process was held off the CPU (frozen, throttled,
// behind other work): only the CPU it used counts. A tick pending across a
// freeze does not count as a tick.
func TestStallMeter_LateSamplesCountOnlyWhatTheProcessRan(t *testing.T) {
	// A tenth of a CPU in CFS's 100 ms periods: 10 ms run, 90 ms throttled,
	// so each 10 ms wait ends 100 ms later having used 10 ms of CPU.
	clk := newFakeStallClock(func(time.Duration) (time.Duration, time.Duration) {
		return 100 * time.Millisecond, 10 * time.Millisecond
	})
	m := newStallMeter(clk, 2*time.Second)
	n := 0
	for {
		m.wait()
		n++
		if m.expired() {
			break
		}
	}
	if n != 200 {
		t.Fatalf("expired after %d throttled samples (%v of wall time), want 200: 2 s of what the process ran",
			n, time.Duration(n)*100*time.Millisecond)
	}

	// A freeze: a wait ends 5 s late, and the process ran 1 ms of it.
	clk = newFakeStallClock(func(time.Duration) (time.Duration, time.Duration) { return 5 * time.Second, time.Millisecond })
	m = newStallMeter(clk, 2*time.Second)
	m.wait()
	if m.expired() {
		t.Fatal("a 5 s freeze counted as run time: a starved consumer's chunks would be abandoned")
	}
	if m.ran != time.Millisecond {
		t.Fatalf("a freeze counted %v, want the 1 ms the process ran", m.ran)
	}

	// Busy on many cores and late only because of that: CPU time outruns
	// wall time, and the interval is what counts.
	clk = newFakeStallClock(func(time.Duration) (time.Duration, time.Duration) {
		return 50 * time.Millisecond, 400 * time.Millisecond
	})
	m = newStallMeter(clk, 2*time.Second)
	m.wait()
	m.expired()
	if m.ran != 50*time.Millisecond {
		t.Fatalf("a busy multi-core interval counted %v, want its 50 ms of wall time", m.ran)
	}
}

// A process limited to a tenth of a CPU (docker --cpus 0.1, a pod's CPU
// limit) can run for a tenth of each interval, however punctually its drain
// wakes: CFS hands the quota out in per-CPU slices, so the drain's thread can
// wake on time while the consumer's is throttled. Measured under --cpus 0.1:
// on-time samples with 60 us of CPU in each 10 ms, and a busy consumer's
// chunks abandoned after 150 ms of wall time.
func TestStallMeter_ACPULimitScalesTheWindow(t *testing.T) {
	clk := newFakeStallClock(onTime)
	clk.limit = 0.1
	m := newStallMeter(clk, 2*time.Second)
	n := 0
	for {
		m.wait()
		n++
		if m.expired() {
			break
		}
	}
	if n != 2000 {
		t.Fatalf("expired after %d on-time samples (%v of wall time) at a tenth of a CPU, want 2000 (20 s)",
			n, time.Duration(n)*stallTick)
	}
	// A limit of a CPU or more leaves the window at wall time: one consumer
	// runs on one CPU.
	clk = newFakeStallClock(onTime)
	clk.limit = 4
	m = newStallMeter(clk, 100*time.Millisecond)
	for i := 0; i < 10; i++ {
		m.wait()
		if m.expired() != (i == 9) {
			t.Fatalf("at a 4-CPU limit the window is not its wall time (sample %d)", i+1)
		}
	}
}

// A consumer that stopped reading is abandoned after one window of run time,
// on a process held off the CPU as on one that is not.
func TestTee_StalledConsumerIsAbandonedAfterAWindowOfRunTime(t *testing.T) {
	clk := newFakeStallClock(func(time.Duration) (time.Duration, time.Duration) {
		return 100 * time.Millisecond, 10 * time.Millisecond // a tenth of a CPU
	})
	tt, rec := newTestTeeClock(t, 1<<30, 1, clk)
	for i := 0; i < 5; i++ {
		tt.push(mkChunk("x"))
	}
	tt.close()
	tt.waitDone()
	if n := rec.count(DropConsumerGone); n != 1 {
		t.Fatalf("consumer_gone reported %d times, want 1", n)
	}
	if got, want := clk.waits(), int(testStallGrace/(10*time.Millisecond)); got != want {
		t.Fatalf("abandoned after %d samples, want %d: %v of run time at a tenth of a CPU", got, want, testStallGrace)
	}
}

// The process's CPU limit is read from its cgroup files at most once per
// refresh, not once per stall meter: a teardown waits on each queued chunk in
// turn, and a starved process would pay a read of several files per chunk.
func TestRealStallClock_ReadsTheCPULimitOncePerRefresh(t *testing.T) {
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
		if m := newStallMeter(realStallClock{}, time.Second); m.share != 0.1 {
			t.Fatalf("share %v, want the 0.1 CPU limit", m.share)
		}
	}
	if reads != 1 {
		t.Fatalf("read the CPU limit %d times for 100 stall meters, want once", reads)
	}
	cpuLimitCache.mu.Lock()
	cpuLimitCache.at = time.Now().Add(-cpuLimitRefresh)
	cpuLimitCache.mu.Unlock()
	newStallMeter(realStallClock{}, time.Second)
	if reads != 2 {
		t.Fatalf("read the CPU limit %d times, want it read again after the refresh", reads)
	}
}
