//go:build linux

package stall

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestParse_ReadsTheKernelsAccounts(t *testing.T) {
	if e, d, r, ok := parseSchedstat([]byte("123456 7890 12\n")); !ok || e != 123456 || d != 7890 || r != 12 {
		t.Fatalf("parseSchedstat = %v %v %v %v; want 123456 7890 12", e, d, r, ok)
	}
	for _, bad := range []string{"", "123456 7890\n", "123456 x 12\n", "123456 7890 x\n"} {
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
	if _, _, ok := parseStat([]byte("42 (x) R" + strings.Repeat(" 1", 35) + "\n")); ok {
		t.Fatal("parseStat read a stat that ends before the CPU")
	}
	if _, cpu, ok := parseStat([]byte("42 (x) R" + strings.Repeat(" 1", 35) + " x 0 0\n")); ok {
		t.Fatalf("parseStat read a CPU that is not a number: %q", cpu)
	}
	// A CPU line without steal (kernels before 2.6.11) and other lines as long
	// as a CPU's are skipped.
	steal := parseSteal([]byte("cpu  1 2 3 4 5 6 7 99 0 0\ncpu0 1 2 3 4 5 6 7 40 0 0\ncpu1 1 2 3 4 5 6 7 59 0 0\ncpu2 1 2 3 4 5 6 7\nintr 5\nsoftirq 1 2 3 4 5 6 7 8 9 10\n"))
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

// The host gives a virtual CPU back, so its steal shows at its first tick,
// while the thread on it is still queued behind others: at that read the
// thread is runnable and has not moved. The steal held it off, and is charged
// then: at the next read the thread has run, and the steal shows no more.
func TestThreadWaits_StealThatShowsWhileTheThreadIsStillQueuedIsCharged(t *testing.T) {
	var w threadWaits
	tick := time.Second / userHZ
	w.account(map[string]threadStat{
		"1": {exec: 100, delay: 10, runs: 1, runnable: true, cpu: "cpu0"},
	}, map[string]int64{"cpu0": 5})
	most, _ := w.account(map[string]threadStat{ // back, still queued
		"1": {exec: 100, delay: 10, runs: 1, runnable: true, cpu: "cpu0"},
	}, map[string]int64{"cpu0": 30})
	total := most
	most, _ = w.account(map[string]threadStat{ // it ran, after 5 ms queued
		"1": {exec: 200, delay: 10 + int64(5*time.Millisecond), runs: 2, cpu: "cpu0"},
	}, map[string]int64{"cpu0": 30})
	total += most
	if want := 25*tick + 5*time.Millisecond; total != want {
		t.Fatalf("charged %v over the two reads, want the 25 ticks of steal and the 5 ms queued: %v", total, want)
	}
}

// A CPU's steal that reads lower than at the last read (a /proc/stat that is
// not the kernel's own, or CPUs renumbered under the process) is no time given
// back: it takes nothing off a thread's run delay.
func TestThreadWaits_StealThatReadsLowerTakesNoWaitOff(t *testing.T) {
	var w threadWaits
	w.account(map[string]threadStat{
		"1": {exec: 100, delay: 10, runs: 1, cpu: "cpu0"},
	}, map[string]int64{"cpu0": 50})
	most, _ := w.account(map[string]threadStat{ // it ran, after 30 ms queued
		"1": {exec: 200, delay: 10 + int64(30*time.Millisecond), runs: 2, cpu: "cpu0"},
	}, map[string]int64{"cpu0": 20})
	if most != 30*time.Millisecond {
		t.Fatalf("charged %v; want the 30 ms the thread was queued, the lower steal taking none of it off", most)
	}
}

// tidSource is a process's clocks that do not move, noting the thread the
// sampler reads its own wait for a CPU on, at each read, and closing done at
// the want-th read.
type tidSource struct {
	fakeSource
	tids  map[int]int // thread id: reads of the sampler's own wait made on it
	looks int
	want  int
	done  chan struct{}
}

func (f *tidSource) ownWait() (time.Duration, bool) {
	runtime.Gosched() // a goroutine not bound to its thread may resume on another
	f.tids[unix.Gettid()]++
	if f.looks++; f.looks == f.want {
		close(f.done)
	}
	return 0, true
}

// The sampler reads its own wait for a CPU from the thread it runs on, and
// sets it against how late its samples are: so it samples on one thread. A
// goroutine not bound to its thread resumes on whichever thread has a P free
// after it sleeps or yields, and with every P kept busy that is mostly
// another one; another thread's run delay says nothing of this one's waits.
func TestSampler_ReadsItsOwnWaitOnOneThread(t *testing.T) {
	procs := max(runtime.GOMAXPROCS(0), 4)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(procs))
	stop := make(chan struct{})
	spun := make(chan struct{}, procs)
	for range procs {
		go func() {
			defer func() { spun <- struct{}{} }()
			for {
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
	}
	defer func() {
		close(stop)
		for range procs {
			<-spun
		}
	}()
	src := &tidSource{tids: map[int]int{}, want: 30, done: make(chan struct{})}
	s := &sampler{src: src, tick: time.Millisecond}
	release := s.acquire()
	select {
	case <-src.done:
	case <-time.After(time.Minute): // only turns a hang into a failure
		release()
		t.Fatalf("the sampler read its own wait %d times in a minute; want %d", src.looks, src.want)
	}
	release() // the sampler's goroutine has ended: its reads are all in
	if len(src.tids) != 1 {
		t.Fatalf("the sampler read its own wait on %d threads (%v); want one", len(src.tids), src.tids)
	}
}

// fakeProcFiles writes files, by path under dir, holding their contents;
// "" removes one.
func fakeProcFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := dir + "/" + name
		if body == "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(path[:strings.LastIndexByte(path, '/')], 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The reader takes a thread only as the kernel shows all of its account,
// and the CPUs' steal from the kernel's times. A thread whose run delay
// cannot be read, or whose state cannot (it exited between the reads of
// its two files), is not seen at that read: its account is all new at the
// next, and its wait so far is counted then, not as though it had none. And
// steal that shows in the kernel's times holds off a thread runnable on
// that CPU. Where the threads cannot be listed, nothing is read.
func TestThreadWaits_ReadOnlyTheAccountsTheKernelShows(t *testing.T) {
	// ppid through exit_signal (fields 4 to 38), then the CPU (39): cpu0.
	runnable := "R" + strings.Repeat(" 1", 35) + " 0 0 0\n"
	steal := func(ticks int) string {
		return fmt.Sprintf("cpu  1 2 3 4 5 6 7 %d 0 0\ncpu0 1 2 3 4 5 6 7 %d 0 0\nintr 5\n", ticks, ticks)
	}
	read := func(t *testing.T, w *threadWaits, dir string, files map[string]string) (time.Duration, uint64) {
		t.Helper()
		fakeProcFiles(t, dir, files)
		return w.longestIn(dir+"/task", dir+"/stat")
	}
	t.Run("a run delay that cannot be read", func(t *testing.T) {
		var w threadWaits
		dir := t.TempDir()
		if most, held := read(t, &w, dir, map[string]string{
			"task/2/schedstat": "x 2000 3\n", "task/2/stat": "2 (a) " + runnable, "stat": steal(10),
		}); most != 0 || held != 0 {
			t.Fatalf("first read: %v, held for %d reads; want nothing, the thread not seen", most, held)
		}
		if most, held := read(t, &w, dir, map[string]string{"task/2/schedstat": "500 2000 3\n"}); most != 2000 || held != 0 {
			t.Fatalf("second read: %v, held for %d reads; want the thread's 2µs of waits, all new, and none held", most, held)
		}
	})
	t.Run("a state that cannot be read", func(t *testing.T) {
		var w threadWaits
		dir := t.TempDir()
		if most, held := read(t, &w, dir, map[string]string{
			"task/3/schedstat": "1000 4000 3\n", "stat": steal(10),
		}); most != 0 || held != 0 {
			t.Fatalf("first read: %v, held for %d reads; want nothing, the thread not seen", most, held)
		}
		if most, held := read(t, &w, dir, map[string]string{"task/3/stat": "3 (a) " + runnable}); most != 4000 || held != 0 {
			t.Fatalf("second read: %v, held for %d reads; want the thread's 4µs of waits, all new, and none held", most, held)
		}
	})
	t.Run("steal in the kernel's times", func(t *testing.T) {
		var w threadWaits
		dir := t.TempDir()
		read(t, &w, dir, map[string]string{
			"task/1/schedstat": "1000 2000 3\n", "task/1/stat": "1 (a) " + runnable, "stat": steal(10),
		})
		if most, held := read(t, &w, dir, map[string]string{"stat": steal(15)}); most != 5*time.Second/userHZ || held != 1 {
			t.Fatalf("got %v, held for %d reads; want the 5 ticks of steal of its CPU, held for one", most, held)
		}
	})
	t.Run("a kernel that keeps no account", func(t *testing.T) {
		// Every thread shows 0 0 0, the one reading included, though it
		// runs: none moves, and none is found held off, however long it
		// is runnable, so no verdict waits for it for ever.
		sleeping := "S" + strings.Repeat(" 1", 35) + " 0 0 0\n"
		var w threadWaits
		dir := t.TempDir()
		files := map[string]string{
			"task/1/schedstat": "0 0 0\n", "task/1/stat": "1 (a) " + runnable,
			"task/2/schedstat": "0 0 0\n", "task/2/stat": "2 (a) " + runnable,
		}
		for i := range 3 {
			files["stat"] = steal(10 + 5*i)
			if most, held := read(t, &w, dir, files); most != 0 || held != 0 {
				t.Fatalf("read %d: %v, held for %d reads; want nothing: the kernel keeps no account", i+1, most, held)
			}
		}
		// The control: one thread has run, so the kernel keeps accounts,
		// and the other, runnable and not run yet, is held off from the
		// read after the one that first saw it.
		files["task/1/schedstat"], files["task/1/stat"] = "500 0 1\n", "1 (a) "+sleeping
		if _, held := read(t, &w, dir, files); held != 0 {
			t.Fatalf("first read with accounts: held for %d reads; want none: both threads new", held)
		}
		if _, held := read(t, &w, dir, files); held != 1 {
			t.Fatalf("second read with accounts: held for %d reads; want the thread not run yet held, for one", held)
		}
	})
	t.Run("no threads listed", func(t *testing.T) {
		var w threadWaits
		dir := t.TempDir()
		if most, held := read(t, &w, dir, map[string]string{"stat": steal(10)}); most != 0 || held != 0 || w.reads != 0 {
			t.Fatalf("got %v, held for %d reads, %d reads; want no read", most, held, w.reads)
		}
	})
}

// The reader sees the wait the kernel keeps: two threads bound to one CPU
// take turns, so each waits about as long as the other runs. The two must be
// on the kernel's run queue together, so each has a P of its own (with one
// P, the Go scheduler takes them in turns, and the one not running sleeps),
// and the row reads the two threads' own accounts, not the most any thread
// of the test binary waited.
func TestThreadWaits_SeeAThreadKeptOffItsCPU(t *testing.T) {
	if was := runtime.GOMAXPROCS(0); was < 2 {
		runtime.GOMAXPROCS(2)
		t.Cleanup(func() { runtime.GOMAXPROCS(was) })
	}
	var was unix.CPUSet
	if err := unix.SchedGetaffinity(0, &was); err != nil {
		t.Fatal(err)
	}
	cpu := 0
	for !was.IsSet(cpu) {
		cpu++
	}

	const each = 100 * time.Millisecond
	tids := make(chan int, 2)
	start, done, leave := make(chan struct{}), make(chan struct{}), make(chan struct{})
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
			err := unix.SchedSetaffinity(0, &one)
			tids <- unix.Gettid()
			if err != nil {
				t.Error(err)
				return
			}
			<-start // both are bound before either runs
			var ts0, ts unix.Timespec
			_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts0)
			for time.Duration(ts.Nano()-ts0.Nano()) < each {
				_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts)
			}
		}()
	}
	pinned := []string{strconv.Itoa(<-tids), strconv.Itoa(<-tids)}
	var w threadWaits
	_, _ = w.longest()
	before := w.last
	close(start)
	<-done
	<-done
	if t.Failed() {
		return
	}
	_, _ = w.longest()
	for _, tid := range pinned {
		was, ok1 := before[tid]
		now, ok2 := w.last[tid]
		if !ok1 || !ok2 {
			t.Fatalf("the reader did not see thread %s at both reads", tid)
		}
		if got := time.Duration(now.delay - was.delay); got < each/10 {
			b, err := os.ReadFile("/proc/self/schedstat")
			if err == nil {
				if _, d, _, ok := parseSchedstat(b); ok && d == 0 {
					t.Skip("this kernel keeps no run delay")
				}
			}
			t.Fatalf("thread %s, bound to cpu%d beside the other, waited for it %v by the reader, want about %v", tid, cpu, got, each)
		}
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
		return e, m.due.Load() != nil
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

// The sampler sleeps in the kernel: a sleep ends once its length has passed,
// and a release (stop closed, then the waker woken) ends a long one: at once
// with an eventfd, which the sleep then reads empty (it took the wake), and
// at its next Tick without one. Each is waited on for far more of the test's
// own short sleeps than a Tick takes, so a sleep that does not end fails
// here, not on the test's timeout.
func TestSleepForEndsAtItsLengthOrItsRelease(t *testing.T) {
	const most = 10000 // the test's 1 ms sleeps: ten seconds and more
	ends := func(t *testing.T, ended <-chan bool, want bool) {
		t.Helper()
		for i := 0; ; i++ {
			select {
			case got := <-ended:
				if got != want {
					t.Fatalf("sleepFor said stopped=%v, want %v", got, want)
				}
				return
			default:
			}
			if i == most {
				t.Fatalf("sleepFor still sleeping after %d of the test's 1 ms sleeps", most)
			}
			time.Sleep(time.Millisecond)
		}
	}
	ev := newWaker()
	if ev.fd < 0 {
		t.Fatal("the kernel made no eventfd")
	}
	for _, c := range []struct {
		name string
		w    *waker
	}{{"an eventfd", ev}, {"no eventfd", &waker{fd: -1}}} {
		t.Run(c.name, func(t *testing.T) {
			t.Run("a sleep ends at its length", func(t *testing.T) {
				ended := make(chan bool, 1)
				go func() { ended <- sleepFor(5*time.Millisecond, make(chan struct{}), c.w) }()
				ends(t, ended, false)
			})
			t.Run("a release ends a long sleep", func(t *testing.T) {
				stop, ended := make(chan struct{}), make(chan bool, 1)
				go func() { ended <- sleepFor(time.Hour, stop, c.w) }()
				time.Sleep(2 * Tick) // most likely asleep by now; either way it must end
				close(stop)
				c.w.wake()
				ends(t, ended, true)
				if c.w.fd >= 0 {
					wakeTaken(t, c.w)
				}
			})
		})
	}
}

// wakeTaken fails the test unless w's eventfd reads empty: the sleep took the
// wake, and the next one does not end at once.
func wakeTaken(t *testing.T, w *waker) {
	t.Helper()
	var b [8]byte
	if n, err := unix.Read(w.fd, b[:]); err != unix.EAGAIN {
		t.Fatalf("the eventfd read %d bytes (%v) after the sleep ended; want none (EAGAIN): the sleep left its wake", n, err)
	}
}

// The last release wakes the sampler's sleep, so it returns once the
// sampling thread runs, not at the sampler's next look: a sampler that looks
// once an hour still lets go at once (on a Go timer it did; the sleep in the
// kernel must too, or every last release waits a Tick: a meter per chunk
// that a closed tee stops, and the agent's boot reporter at its end). The
// sleep takes the wake before the sampling ends, and the sampler keeps the
// one eventfd for all its samplings.
func TestSampler_TheLastReleaseWakesItsSleep(t *testing.T) {
	s := &sampler{src: &fakeSource{}, tick: time.Hour}
	fd := -1
	for run := range 2 {
		release := s.acquire()
		released := make(chan struct{})
		go func() { release(); close(released) }()
		select {
		case <-released:
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("sampling %d: the release of a sampler that looks once an hour has not returned in a minute", run)
		}
		if s.wake.fd < 0 {
			t.Fatal("the sampler made no eventfd")
		}
		if run == 0 {
			fd = s.wake.fd
		} else if s.wake.fd != fd {
			t.Fatalf("sampling %d polled eventfd %d, the first %d; want one per sampler", run, s.wake.fd, fd)
		}
		wakeTaken(t, s.wake)
	}
}
