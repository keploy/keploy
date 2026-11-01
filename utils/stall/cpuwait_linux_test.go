//go:build linux

package stall

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestParse_ReadsTheKernelsAccounts(t *testing.T) {
	if e, d, r, ok := parseSchedstat([]byte("123456 7890 12\n")); !ok || e != 123456 || d != 7890 || r != 12 {
		t.Fatalf("parseSchedstat = %v %v %v %v; want 123456 7890 12", e, d, r, ok)
	}
	for _, bad := range []string{"", "123456 7890\n", "123456 x 12\n"} {
		if _, _, _, ok := parseSchedstat([]byte(bad)); ok {
			t.Fatalf("parseSchedstat(%q) read, want no reading", bad)
		}
	}
	// ppid through exit_signal (fields 4 to 38), then the CPU (39).
	tail := strings.Repeat(" 1", 35) + " 7 0 0\n"
	if run, cpu, ok := parseStat([]byte("42 (a (b) c) R" + tail)); !ok || !run || cpu != "cpu7" {
		t.Fatalf("parseStat = %v %q %v; want runnable on cpu7", run, cpu, ok)
	}
	if run, _, ok := parseStat([]byte("42 (x) S" + tail)); !ok || run {
		t.Fatalf("parseStat of a sleeper = %v %v; want not runnable", run, ok)
	}
	if _, _, ok := parseStat([]byte("42 (x) R 1 2 3\n")); ok {
		t.Fatal("parseStat read a short stat")
	}
	steal := parseSteal([]byte("cpu  1 2 3 4 5 6 7 99 0 0\ncpu0 1 2 3 4 5 6 7 40 0 0\ncpu1 1 2 3 4 5 6 7 59 0 0\nintr 5\n"))
	if len(steal) != 2 || steal["cpu0"] != 40 || steal["cpu1"] != 59 {
		t.Fatalf("parseSteal = %v; want cpu0 40, cpu1 59", steal)
	}
}

// A thread is held off by the steal of the CPU it runs or waits on, not by
// that of the CPU it slept on; a runnable thread that has not moved since the
// last read is held off now, for as many reads in a row as it has not moved.
func TestThreadWaits_CountStealAndSeeAWaitInProgress(t *testing.T) {
	var w threadWaits
	tick := time.Second / userHZ
	w.account(map[string]threadStat{
		"1": {exec: 100, delay: 10, runs: 1, cpu: "cpu0"},
		"2": {exec: 100, delay: 10, runs: 1, cpu: "cpu1"},
	}, map[string]int64{"cpu0": 5, "cpu1": 5})
	// 1 ran on cpu0 while the host took it for 20 ticks; 2 slept on cpu1,
	// which the host took for 30.
	most, held := w.account(map[string]threadStat{
		"1": {exec: 200, delay: 10, runs: 2, runnable: true, cpu: "cpu0"},
		"2": {exec: 100, delay: 10, runs: 1, cpu: "cpu1"},
	}, map[string]int64{"cpu0": 25, "cpu1": 35})
	if most != 20*tick || held != 0 {
		t.Fatalf("got %v held for %d reads; want %v, none held now", most, held, 20*tick)
	}
	// 1 has not moved, runnable: held off now, whatever the steal says yet;
	// and at the next read, for two reads.
	for want := range uint64(2) {
		most, held = w.account(map[string]threadStat{
			"1": {exec: 200, delay: 10, runs: 2, runnable: true, cpu: "cpu0"},
			"2": {exec: 100, delay: 10, runs: 1, cpu: "cpu1"},
		}, map[string]int64{"cpu0": 25, "cpu1": 35})
		if most != 0 || held != want+1 {
			t.Fatalf("got %v held for %d reads; want 0, held for %d", most, held, want+1)
		}
	}
	// Back on its CPU: its wait (5 ms queued) and the steal (12 ticks) show.
	most, held = w.account(map[string]threadStat{
		"1": {exec: 300, delay: 10 + int64(5*time.Millisecond), runs: 3, runnable: true, cpu: "cpu0"},
		"3": {runnable: true, cpu: "cpu1"}, // new, not run yet: not known to be held
	}, map[string]int64{"cpu0": 37, "cpu1": 35})
	if want := 5*time.Millisecond + 12*tick; most != want || held != 0 {
		t.Fatalf("got %v held for %d reads; want %v, none held now", most, held, want)
	}
	// 1 is held off again: a new hold, counted from this read.
	if _, held = w.account(map[string]threadStat{
		"1": {exec: 300, delay: 10 + int64(5*time.Millisecond), runs: 3, runnable: true, cpu: "cpu0"},
	}, map[string]int64{"cpu0": 37}); held != 1 {
		t.Fatalf("held for %d reads; want a new hold, of 1", held)
	}
}

// The reader sees the wait the kernel keeps: two threads bound to one CPU
// take turns, so each waits about as long as the other runs.
func TestThreadWaits_SeeAThreadKeptOffItsCPU(t *testing.T) {
	var was unix.CPUSet
	if err := unix.SchedGetaffinity(0, &was); err != nil {
		t.Fatal(err)
	}
	cpu := 0
	for !was.IsSet(cpu) {
		cpu++
	}
	var w threadWaits
	_, _ = w.longest()

	const each = 100 * time.Millisecond
	done, leave := make(chan struct{}), make(chan struct{})
	defer close(leave)
	for range 2 {
		go func() {
			// Not unlocked: the thread leaves with its CPU mask, once the
			// test has read it (a read does not see a thread that left).
			runtime.LockOSThread()
			defer func() {
				done <- struct{}{}
				<-leave
			}()
			var one unix.CPUSet
			one.Set(cpu)
			if err := unix.SchedSetaffinity(0, &one); err != nil {
				t.Error(err)
				return
			}
			var ts0, ts unix.Timespec
			_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts0)
			for time.Duration(ts.Nano()-ts0.Nano()) < each {
				_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts)
			}
		}()
	}
	<-done
	<-done
	got, _ := w.longest()
	if got < each/10 {
		b, err := os.ReadFile("/proc/self/schedstat")
		if err == nil {
			if _, d, _, ok := parseSchedstat(b); ok && d == 0 {
				t.Skip("this kernel keeps no run delay")
			}
		}
		t.Fatalf("the threads waited for a CPU at most %v by the reader, want about %v", got, each)
	}
}

// overloadSource is a process's clocks on a VM whose every CPU is busy, and
// taken by the host at times: wall and CPU time move by the test's steps, and
// its threads' accounts are what world says at each read, through the
// accounting the process's own reads go through (threadWaits).
type overloadSource struct {
	fakeSource
	w     threadWaits
	world func(read int) (map[string]threadStat, map[string]int64)
	n     int
}

func (o *overloadSource) heldOff() (time.Duration, uint64) {
	o.n++
	return o.w.account(o.world(o.n))
}

// overload is a process of six worker threads on four CPUs, every one of them
// busy: each worker waits on a run queue two reads in a row and runs the next
// two, so at every read some are held off; the host takes every CPU for a tick
// at every other read; and the consumer's thread is what consumer makes it.
func overload(consumer func(read int, st *threadStat)) func(int) (map[string]threadStat, map[string]int64) {
	workers := make([]threadStat, 6)
	var cons threadStat
	var steal int64
	return func(read int) (map[string]threadStat, map[string]int64) {
		seen := make(map[string]threadStat, len(workers)+1)
		for i := range workers {
			st := &workers[i]
			st.runnable, st.cpu = true, fmt.Sprintf("cpu%d", i%4)
			if (read+i)%4 >= 2 { // it ran: its wait ended
				st.exec += int64(time.Millisecond)
				st.delay += int64(2 * time.Millisecond)
				st.runs++
			}
			seen[fmt.Sprint("w", i)] = *st
		}
		consumer(read, &cons)
		seen["consumer"] = cons
		if read%2 == 0 {
			steal++
		}
		cpus := make(map[string]int64, 4)
		for c := range 4 {
			cpus[fmt.Sprintf("cpu%d", c)] = steal
		}
		if cons.cpu == "cpu9" {
			cpus["cpu9"] = cons9Steal(read)
		}
		return seen, cpus
	}
}

// overloadLooks makes the looks at a stall on a sampler over world: each a
// sample on time, then the verdict; and says how many reads of the threads
// there have been.
func overloadLooks(world func(int) (map[string]threadStat, map[string]int64), grace time.Duration) (look func() (expired, due bool), reads func() int) {
	src := &overloadSource{fakeSource: fakeSource{wall: time.Unix(1_000_000, 0)}, world: world}
	s := &sampler{src: src, tick: Tick}
	s.begin()
	m := NewMeter(heldClock{s}, grace)
	return func() (bool, bool) {
		src.wall, src.cpu = src.wall.Add(Tick), src.cpu+Tick/2
		s.sample()
		e := m.Expired()
		return e, m.due.Load() != 0
	}, func() int { return src.n }
}

// The verdict waits for the threads held off when a stall was found due, not
// for every thread held off at every read: on a host so busy that some thread
// of the process waits for a CPU at every read, a consumer that stopped
// (asleep in a read, or spinning without reading) still gets its verdict, and
// within a count of looks: those threads' turn, two reads, and one more.
func TestMeter_AStoppedConsumerIsJudgedUnderAnOverloadThatHoldsAThreadOffAtEveryRead(t *testing.T) {
	const grace, bound = 100 * time.Millisecond, 100
	for _, tc := range []struct {
		name     string
		consumer func(read int, st *threadStat)
	}{
		{"asleep in a read", func(_ int, st *threadStat) {
			st.runnable, st.cpu = false, "cpu0"
		}},
		{"spinning without reading", func(read int, st *threadStat) {
			// As busy as the workers, on their CPUs.
			st.runnable, st.cpu = true, "cpu1"
			if read%4 >= 2 {
				st.exec += int64(time.Millisecond)
				st.delay += int64(2 * time.Millisecond)
				st.runs++
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			look, _ := overloadLooks(overload(tc.consumer), grace)
			dueAt := 0
			for n := 1; ; n++ {
				if n > bound {
					t.Fatalf("no verdict in %d looks (due since look %d): a stopped consumer is held off for as long as the host is busy", bound, dueAt)
				}
				expired, due := look()
				if expired {
					if dueAt == 0 || n-dueAt > 3 {
						t.Fatalf("judged at look %d, due at look %d: want within 3 looks of being due", n, dueAt)
					}
					return
				}
				if !due {
					dueAt = 0
				} else if dueAt == 0 {
					dueAt = n
				}
			}
		})
	}
}

// heldByTheHost is a consumer the host has held off since before its stall
// was due, among the overload: runnable on a CPU (cpu9) the host does not
// run, from read 2 to read 41, then given it back, its 400 ms of steal
// showing then; asleep in a read after.
func heldByTheHost(read int, st *threadStat) {
	st.cpu = "cpu9"
	switch {
	case read < 2:
		st.runnable = true
		st.exec, st.runs = int64(time.Millisecond), 1
	case read < 42:
		st.runnable = true // held off: nothing moves, the steal does not show
	case read == 42:
		st.runnable, st.runs = true, 2
		st.exec += int64(time.Millisecond)
	default:
		st.runnable = false
	}
}

// cons9Steal is the steal of heldByTheHost's CPU: none until the host gives it
// back at read 42, its 40 ticks showing then.
func cons9Steal(read int) int64 {
	if read < 42 {
		return 0
	}
	return 40
}

// Nor does narrowing the wait to those threads give the verdict to a stall
// that was the host's: a consumer held off since before its stall was due
// holds the verdict back while the overload holds others off at every read,
// and when its CPU comes back the steal shows and the stall is not due; it
// goes on, and is judged once the consumer stops.
func TestMeter_AConsumerHeldOffSinceBeforeItsStallWasDueHoldsItsVerdict(t *testing.T) {
	look, reads := overloadLooks(overload(heldByTheHost), 100*time.Millisecond)
	wasDue := false
	for n := 1; reads() < 41; n++ {
		expired, due := look()
		if expired {
			t.Fatalf("judged at look %d (read %d), while the consumer's CPU is the host's", n, reads())
		}
		wasDue = wasDue || due
	}
	if !wasDue {
		t.Fatal("never due while held off: the test proves nothing")
	}
	if expired, due := look(); expired || due || reads() != 42 {
		t.Fatalf("expired %v due %v at read %d, the one its CPU came back at with 400 ms of steal", expired, due, reads())
	}
	for n := 1; ; n++ {
		if n > 100 {
			t.Fatal("no verdict in 100 looks after the consumer stopped")
		}
		if expired, _ := look(); expired {
			return
		}
	}
}
