//go:build linux

package stall

import (
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
// last read is held off now.
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
	if most != 20*tick || held {
		t.Fatalf("got %v held %v; want %v, none held now", most, held, 20*tick)
	}
	// 1 has not moved, runnable: held off now, whatever the steal says yet.
	most, held = w.account(map[string]threadStat{
		"1": {exec: 200, delay: 10, runs: 2, runnable: true, cpu: "cpu0"},
		"2": {exec: 100, delay: 10, runs: 1, cpu: "cpu1"},
	}, map[string]int64{"cpu0": 25, "cpu1": 35})
	if most != 0 || !held {
		t.Fatalf("got %v held %v; want 0, held now", most, held)
	}
	// Back on its CPU: its wait (5 ms queued) and the steal (12 ticks) show.
	most, held = w.account(map[string]threadStat{
		"1": {exec: 300, delay: 10 + int64(5*time.Millisecond), runs: 3, runnable: true, cpu: "cpu0"},
		"3": {runnable: true, cpu: "cpu1"}, // new, not run yet: not known to be held
	}, map[string]int64{"cpu0": 37, "cpu1": 35})
	if want := 5*time.Millisecond + 12*tick; most != want || held {
		t.Fatalf("got %v held %v; want %v, none held now", most, held, want)
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
