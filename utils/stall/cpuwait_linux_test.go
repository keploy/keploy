//go:build linux

package stall

import (
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRunDelay_IsSchedstatsSecondField(t *testing.T) {
	if d, ok := runDelay([]byte("123456 7890 12\n")); !ok || d != 7890 {
		t.Fatalf("runDelay = %v, %v; want 7890 ns", d, ok)
	}
	for _, bad := range []string{"", "123456\n", "123456 x 12\n"} {
		if d, ok := runDelay([]byte(bad)); ok {
			t.Fatalf("runDelay(%q) = %v, want no reading", bad, d)
		}
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
	w.longest()

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
	got := w.longest()
	if got < each/10 {
		b, err := os.ReadFile("/proc/self/schedstat")
		if err == nil {
			if d, ok := runDelay(b); ok && d == 0 {
				t.Skip("this kernel keeps no run delay")
			}
		}
		t.Fatalf("the threads waited for a CPU at most %v by the reader, want about %v", got, each)
	}
}
