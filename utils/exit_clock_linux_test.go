//go:build linux

package utils

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/starvetest"
	"go.keploy.io/server/v3/utils/stall"
	"golang.org/x/sys/unix"
)

// clockRig is an ExitClock on a wall clock and a keploy runnable clock the
// test moves, reading thread accounts the test sets.
type clockRig struct {
	wall  time.Time
	own   *stall.ManualClock
	tasks map[int]taskTime
	ok    bool
	c     *ExitClock
	last  time.Duration
}

func newClockRig(tasks map[int]taskTime) *clockRig {
	r := &clockRig{wall: time.Unix(1000, 0), own: &stall.ManualClock{}, tasks: tasks, ok: true}
	r.c = newExitClock(nil, r.own, func() time.Time { return r.wall },
		func([]Reached) (map[int]taskTime, bool) { return maps.Clone(r.tasks), r.ok })
	return r
}

// look moves wall time by w and keploy's runnable time by own, lets step
// change the threads' accounts, and returns what the clock added.
func (r *clockRig) look(w, own time.Duration, step func(map[int]taskTime)) time.Duration {
	r.wall = r.wall.Add(w)
	r.own.Advance(own)
	if step != nil {
		step(r.tasks)
	}
	now := r.c.Runnable()
	added := now - r.last
	r.last = now
	return added
}

// adds changes thread tid's account by ran, waited and slices, and sets
// whether it is runnable.
func adds(tid int, ran, waited time.Duration, slices uint64, runnable bool) func(map[int]taskTime) {
	return func(m map[int]taskTime) {
		t := m[tid]
		t.ran += ran
		t.waited += waited
		t.slices += slices
		t.runnable = runnable
		m[tid] = t
	}
}

// A stop's stall is counted only in the time the stopped processes could run
// (and keploy with them): w less the time their threads waited for a CPU, and
// never less than the CPU they had. A wait still going on, which the kernel
// adds to a thread's account only once it ends, is counted as it goes, and
// not again when it ends.
func TestTheExitClockCountsOnlyTheTimeTheProcessesCouldRun(t *testing.T) {
	const w = 100 * time.Millisecond
	ms := time.Millisecond
	t.Run("a thread that sleeps could run all of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {ran: ms, slices: 3}})
		for i := range 5 {
			if got := r.look(w, w, nil); got != w {
				t.Fatalf("look %d added %v, want %v", i, got, w)
			}
		}
	})
	t.Run("a thread held off the CPU throughout could run none of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {ran: ms, slices: 3, runnable: true}})
		for i := range 5 {
			if got := r.look(w, w, nil); got != 0 {
				t.Fatalf("look %d added %v of a thread waiting for a CPU since before it, want none", i, got)
			}
		}
		// Its wait ends 90 ms into the next look, and it runs 10 ms: the
		// kernel adds all 590 ms of that wait now, of which 500 were counted.
		if got := r.look(w, w, adds(1, 10*ms, 590*ms, 1, true)); got != 10*ms {
			t.Fatalf("the look its wait ended in added %v, want the 10ms it ran", got)
		}
		// Waiting again, from before this look: none.
		if got := r.look(w, w, nil); got != 0 {
			t.Fatalf("a thread waiting again added %v, want none", got)
		}
	})
	t.Run("a wait counted as it went is not counted again when it ends", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {slices: 3, runnable: true}})
		for range 5 {
			r.look(w, w, nil)
		}
		// Its 510 ms wait ends 10 ms into the look; it runs 1 ms, and then
		// sleeps on its own: it could run the 90 ms after its wait.
		if got := r.look(w, w, adds(1, ms, 510*ms, 1, false)); got != 90*ms {
			t.Fatalf("the look its wait ended in added %v, want 90ms", got)
		}
		// A later wait, begun and ended within a look (80 ms, then 20 ms
		// run): all of it is new, none of the first is taken off it.
		if got := r.look(w, w, adds(1, 20*ms, 80*ms, 1, false)); got != 20*ms {
			t.Fatalf("a later wait's look added %v, want 20ms", got)
		}
	})
	t.Run("a thread runnable at a look is taken to have waited for a CPU all of it that it did not run", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		// Asleep at the last look, woken since, and waiting for a CPU now:
		// when it woke is not known, so all of the look is taken for its
		// wait.
		if got := r.look(w, w, adds(1, 0, 0, 0, true)); got != 0 {
			t.Fatalf("added %v of a thread waiting for a CPU at the look, want none", got)
		}
		// Still waiting at the next look, given no CPU since: all of it.
		if got := r.look(w, w, nil); got != 0 {
			t.Fatalf("added %v of a thread waiting since the last look, want none", got)
		}
	})
	t.Run("a thread given the CPU since the last look and waiting for it again could run only what it ran", func(t *testing.T) {
		// Waiting for a CPU from the first look on: its wait ends 40 ms
		// into the next, it runs 5 ms, and waits again, through the look.
		// The kernel books that wait only once it ends.
		r := newClockRig(map[int]taskTime{1: {slices: 3, runnable: true}})
		if got := r.look(w, w, adds(1, 5*ms, 40*ms, 1, true)); got != 5*ms {
			t.Fatalf("the look it ran in added %v, want the 5ms it ran", got)
		}
		for i := range 3 {
			if got := r.look(w, w, nil); got != 0 {
				t.Fatalf("look %d added %v of a thread still waiting, want none", i, got)
			}
		}
		// That wait ends 40 ms into a look, 395 ms long; it runs 5 ms, and
		// waits again: what of the wait was counted is not counted again.
		if got := r.look(w, w, adds(1, 5*ms, 395*ms, 1, true)); got != 5*ms {
			t.Fatalf("the next look it ran in added %v, want the 5ms it ran", got)
		}
		// It ends 30 ms into the next, 85 ms long, and it runs 5 ms and
		// sleeps on its own: it could run the 70 ms after its wait.
		if got := r.look(w, w, adds(1, 5*ms, 85*ms, 1, false)); got != 70*ms {
			t.Fatalf("the look it went to sleep in added %v, want 70ms", got)
		}
	})
	t.Run("a thread on a loaded host could run what it did not wait", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {}})
		for i := range 5 {
			if got := r.look(w, w, adds(1, 10*ms, 90*ms, 5, i%2 == 0)); got != 10*ms {
				t.Fatalf("look %d added %v, want 10ms", i, got)
			}
		}
	})
	t.Run("a thread spinning on a quarter of a CPU could run a quarter", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {runnable: true}})
		for i := range 5 {
			if got := r.look(w, w, adds(1, 25*ms, 75*ms, 10, true)); got != 25*ms {
				t.Fatalf("look %d added %v, want 25ms", i, got)
			}
		}
	})
	t.Run("eight threads each on a quarter of a CPU could run all of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{})
		for tid := range 8 {
			r.tasks[tid] = taskTime{runnable: true}
		}
		for i := range 5 {
			got := r.look(w, w, func(m map[int]taskTime) {
				for tid := range 8 {
					adds(tid, 25*ms, 75*ms, 10, true)(m)
				}
			})
			if got != w {
				t.Fatalf("look %d added %v, want %v: they had two CPUs' worth", i, got, w)
			}
		}
		// keploy's clock, sampled, may catch up more than w between two
		// looks: the processes still could run no more than w of it.
		got := r.look(w, w+50*ms, func(m map[int]taskTime) {
			for tid := range 8 {
				adds(tid, 25*ms, 75*ms, 10, true)(m)
			}
		})
		if got != w {
			t.Fatalf("a look keploy's clock caught up in added %v, want %v", got, w)
		}
	})
	t.Run("a thread alone on a CPU, running throughout, could run all of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {slices: 1, runnable: true}})
		for i := range 5 {
			if got := r.look(w, w, adds(1, w, 0, 0, true)); got != w {
				t.Fatalf("look %d added %v, want %v", i, got, w)
			}
		}
	})
	t.Run("one of the processes held off the CPU holds the stall of all", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {}, 2: {runnable: true}})
		for i := range 5 {
			if got := r.look(w, w, nil); got != 0 {
				t.Fatalf("look %d added %v, want none while thread 2 waits", i, got)
			}
		}
	})
	t.Run("keploy held off the CPU counts none of it", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {}})
		if got := r.look(w, 0, nil); got != 0 {
			t.Fatalf("added %v while keploy could not run, want none", got)
		}
		if got := r.look(w, 30*ms, nil); got != 30*ms {
			t.Fatalf("added %v while keploy could run 30ms, want that", got)
		}
	})
	t.Run("with no account of the processes it is keploy's runnable time", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {runnable: true}})
		r.ok = false
		if got := r.look(w, 70*ms, nil); got != 70*ms {
			t.Fatalf("added %v, want keploy's 70ms", got)
		}
	})
	t.Run("a look that read nothing sets the next one's account against none", func(t *testing.T) {
		// A thread a second old, asleep; one look reads nothing.
		r := newClockRig(map[int]taskTime{1: {slices: 3, age: 10 * w}})
		r.ok = false
		if got := r.look(w, w, nil); got != w {
			t.Fatalf("the look that read nothing added %v, want keploy's %v", got, w)
		}
		// Read again: across both looks it ran 150 ms, and waited 50 ms
		// for a CPU, all of it in this one. Set against the account read
		// before the look that read nothing, the 150 ms would fill this
		// look; of the CPU it had since, this look holds none it cannot
		// have had before, and it waited 50 ms of it.
		r.ok = true
		if got := r.look(w, w, adds(1, 150*ms, 50*ms, 2, false)); got != 50*ms {
			t.Fatalf("the look after one that read nothing added %v, want the 50ms it did not wait", got)
		}
	})
	t.Run("a thread new since the last look counts its whole account", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {}})
		if got := r.look(w, w, func(m map[int]taskTime) { m[2] = taskTime{waited: w, slices: 1} }); got != 0 {
			t.Fatalf("a new thread that waited all of it left %v, want none", got)
		}
		// A thread id given to another meanwhile: its account is below the
		// last, and counts whole.
		if got := r.look(w, w, func(m map[int]taskTime) { m[2] = taskTime{ran: 40 * ms, waited: 60 * ms, slices: 1} }); got != 40*ms {
			t.Fatalf("a thread id given to another added %v, want the 40ms it ran", got)
		}
		// A thread new since the last look and waiting for a CPU at this
		// one, its age not known: all of the look it did not run is taken
		// for its wait.
		if got := r.look(w, w, func(m map[int]taskTime) { m[3] = taskTime{ran: 10 * ms, waited: 30 * ms, slices: 1, runnable: true} }); got != 10*ms {
			t.Fatalf("a new thread waiting for a CPU added %v, want the 10ms it ran", got)
		}
		// Its id given to another, which waited 20 ms, ran 5 and sleeps:
		// what was counted of the first one's wait is not the second's.
		if got := r.look(w, w, func(m map[int]taskTime) { m[3] = taskTime{ran: 5 * ms, waited: 20 * ms, slices: 1} }); got != 80*ms {
			t.Fatalf("a thread id given to another after a wait added %v, want the 80ms after its wait", got)
		}
	})
	t.Run("a thread new since the last look waited for a CPU no longer than it lived", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		// Started 12 ms before the look (a shell's child, at its exec), it
		// ran 2 ms and waits for a CPU: it waited 10 ms at most.
		if got := r.look(w, w, func(m map[int]taskTime) {
			m[2] = taskTime{ran: 2 * ms, waited: ms, slices: 1, runnable: true, age: 12 * ms}
		}); got != 90*ms {
			t.Fatalf("a thread 12ms old, waiting for a CPU, added %v, want 90ms", got)
		}
		// That one gone, another new to the look but older than it (a
		// thread of a process the stop reads from now on), waiting for a
		// CPU: the 2 ms it ran in its life may all have been before the
		// look, which holds all of the look for its wait.
		if got := r.look(w, w, func(m map[int]taskTime) {
			delete(m, 2)
			m[3] = taskTime{ran: 2 * ms, slices: 1, runnable: true, age: 3 * w}
		}); got != 0 {
			t.Fatalf("a thread new to the look, older than it, waiting for a CPU, added %v, want none", got)
		}
		// Its wait ends 40 ms into the next look, 240 ms long: it may have
		// begun before the first look, of which no more than the look was
		// counted. It runs 5 ms and sleeps: the rest of that wait is taken
		// off this look.
		if got := r.look(w, w, adds(3, 5*ms, 240*ms, 1, false)); got != 5*ms {
			t.Fatalf("the look the wait of a thread older than the look it was new to ended in added %v, want the 5ms it ran", got)
		}
	})
	t.Run("a thread new to the look but older than it ran in it no more than it cannot have run before", func(t *testing.T) {
		r := newClockRig(map[int]taskTime{1: {slices: 3}})
		// 300 ms old, 250 ms on a CPU in its life, waiting for one now:
		// at least 50 of those in the look, and the rest of it a wait.
		if got := r.look(w, w, func(m map[int]taskTime) {
			m[2] = taskTime{ran: 250 * ms, waited: 40 * ms, slices: 9, runnable: true, age: 3 * w}
		}); got != 50*ms {
			t.Fatalf("a thread 300ms old with 250ms on a CPU added %v, want the 50ms of it the look holds", got)
		}
		// Asleep, the same: the 250 ms may have been before the look but
		// for 50, and its 40 ms of waits all in it.
		r = newClockRig(map[int]taskTime{1: {slices: 3}})
		if got := r.look(w, w, func(m map[int]taskTime) {
			m[2] = taskTime{ran: 250 * ms, waited: 40 * ms, slices: 9, age: 3 * w}
		}); got != 60*ms {
			t.Fatalf("a sleeping thread 300ms old with 250ms on a CPU added %v, want the 60ms it did not wait", got)
		}
	})
}

// fakeProc writes a /proc of processes under a temporary directory: each a
// leader state and start time, and threads of it, each a state and an account.
type fakeThread struct {
	tid              int
	state            byte
	ran, wait, slice uint64
	children         string
}

func fakeProc(t *testing.T, procs map[int]struct {
	state   byte
	start   uint64
	threads []fakeThread
}) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, s string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(pid int, state byte, start uint64) string {
		// pid (comm) state, then fields 4..52: starttime is field 22.
		f := make([]string, 49)
		for i := range f {
			f[i] = "0"
		}
		f[22-4] = strconv.FormatUint(start, 10)
		return fmt.Sprintf("%d (app) %c %s\n", pid, state, strings.Join(f, " "))
	}
	for pid, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		write(filepath.Join(dir, "stat"), stat(pid, p.state, p.start))
		for _, th := range p.threads {
			tdir := filepath.Join(dir, "task", strconv.Itoa(th.tid))
			write(filepath.Join(tdir, "stat"), stat(th.tid, th.state, p.start))
			write(filepath.Join(tdir, "schedstat"), fmt.Sprintf("%d %d %d\n", th.ran, th.wait, th.slice))
			write(filepath.Join(tdir, "children"), th.children)
		}
	}
	return root
}

// The account is read of every thread of each process as it was named (pid
// and start time), its leader exited or not, and of the processes it runs
// now: a Go process exiting has a zombie leader while its other threads,
// killed, wait for a CPU to get out, and a shutdown may wait on a child it
// started since the stop began. A pid that names another process now is not
// read.
func TestTheExitClockReadsEveryThreadOfItsProcesses(t *testing.T) {
	type proc = struct {
		state   byte
		start   uint64
		threads []fakeThread
	}
	root := fakeProc(t, map[int]proc{
		100: {state: 'Z', start: 7, threads: []fakeThread{{100, 'Z', 5, 1, 2, ""}, {101, 'R', 6, 9, 3, "400 "}}},
		200: {state: 'S', start: 9, threads: []fakeThread{{200, 'S', 1, 2, 3, ""}}},
		400: {state: 'S', start: 11, threads: []fakeThread{{400, 'S', 4, 4, 4, "401"}}},
		401: {state: 'R', start: 12, threads: []fakeThread{{401, 'R', 0, 0, 0, ""}}},
	})
	// Read 2 s after the system booted: a thread started at tick 7 (of
	// 10ms) is 1.93 s old.
	boot := func() time.Duration { return 2 * time.Second }
	got, ok := readTaskTimesIn(root, []Reached{{PID: 100, Start: 7}, {PID: 200, Start: 8}, {PID: 300, Start: 1}}, boot)
	if !ok {
		t.Fatal("the zombie-led process was not read")
	}
	ms := time.Millisecond
	want := map[int]taskTime{
		100: {ran: 5, waited: 1, slices: 2, age: 1930 * ms},
		101: {ran: 6, waited: 9, slices: 3, runnable: true, age: 1930 * ms},
		400: {ran: 4, waited: 4, slices: 4, age: 1890 * ms},
		401: {runnable: true, age: 1880 * ms},
	}
	if !maps.Equal(got, want) {
		t.Fatalf("read %+v, want %+v (400 and 401 run under 100, pid 200 names another process now, 300 is gone)", got, want)
	}
	if _, ok := readTaskTimesIn(root, []Reached{{PID: 200, Start: 8}}, boot); ok {
		t.Fatal("a process another now has the pid of was read")
	}
}

// startProc starts command under sh for the test, which kills it at its end.
func startProc(t *testing.T, command string) []Reached {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	p, ok := ProcOf(cmd.Process.Pid)
	if !ok {
		t.Fatal("the process is gone")
	}
	return []Reached{p}
}

// liveRig makes ExitClocks of live processes on a wall clock and a keploy
// runnable clock the test moves (looks), so what they count is the
// processes' own account alone.
type liveRig struct {
	wall time.Time
	own  *stall.ManualClock
}

func newLiveRig() *liveRig { return &liveRig{wall: time.Now(), own: &stall.ManualClock{}} }

func (r *liveRig) clock(procs []Reached) *ExitClock {
	return newExitClock(procs, r.own, func() time.Time { return r.wall }, readTaskTimes)
}

// looks looks at clocks n times, each after a pause, moving wall time and
// keploy's runnable time by the time the pause took, and returns the time
// that passed.
func (r *liveRig) looks(n int, clocks ...*ExitClock) (total time.Duration) {
	for range n {
		time.Sleep(20 * time.Millisecond)
		now := time.Now()
		d := now.Sub(r.wall)
		r.wall = now
		total += d
		r.own.Advance(d)
		for _, c := range clocks {
			c.Runnable()
		}
	}
	return total
}

// cpuSince is the CPU time the threads of an account had since an earlier
// one of the same processes, and the time they waited for a CPU, by the
// kernel.
func cpuSince(before, now map[int]taskTime) (ran, waited time.Duration) {
	for tid, t := range now {
		b := before[tid]
		if t.ran < b.ran {
			b = taskTime{}
		}
		ran += t.ran - b.ran
		waited += t.waited - b.waited
	}
	return ran, waited
}

// asleep waits until process p runs program comm and sleeps: an exec'd
// sleep(1) that waits on its timer, with no more to run.
func asleep(t *testing.T, p Reached, comm string) {
	t.Helper()
	for {
		st, ok := ReadProcStat(p.PID)
		if !ok {
			t.Fatalf("process %d is gone", p.PID)
		}
		b, err := os.ReadFile("/proc/" + strconv.Itoa(p.PID) + "/comm")
		if err != nil {
			t.Fatalf("read process %d's command: %v", p.PID, err)
		}
		if st.State == 'S' && strings.TrimSpace(string(b)) == comm {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// On the kernel's own account: a process held off the CPU (runnable, given
// none) could run no more than the CPU it had while a stop waits on it, and
// one that sleeps all of the time.
func TestTheExitClockOfAProcessHeldOffTheCPU(t *testing.T) {
	if !schedAccountKept() {
		t.Skip("this kernel keeps no account of a thread's time on and waiting for a CPU")
	}
	spinner, sleeper := startProc(t, "while :; do :; done"), startProc(t, "exec sleep 600")
	asleep(t, sleeper[0], "sleep")
	release := starvetest.Hold(t, spinner[0].PID)
	defer release()
	rig := newLiveRig()
	held, sleeping := rig.clock(spinner), rig.clock(sleeper)
	first := maps.Clone(held.last)
	total := rig.looks(20, held, sleeping)
	if ran, _ := cpuSince(first, held.last); held.ran > ran {
		t.Errorf("a process held off the CPU could run %v of %v by the clock, more than the %v of CPU it had", held.ran, total, ran)
	}
	if sleeping.ran != total {
		t.Errorf("a process that sleeps could run %v of %v by the clock; want all of it", sleeping.ran, total)
	}
}

// On the kernel's own account: a process that spins on a CPU it shares could
// run only the CPU it had, wherever its slices fall between the looks. The
// kernel books a wait for a CPU only once it ends, so a look that finds it
// waiting again after a slice does not show that wait yet.
func TestTheExitClockOfAProcessSharingACPU(t *testing.T) {
	if !schedAccountKept() {
		t.Skip("this kernel keeps no account of a thread's time on and waiting for a CPU")
	}
	var mine unix.CPUSet
	if err := unix.SchedGetaffinity(0, &mine); err != nil {
		t.Skipf("cannot read this process's CPUs: %v", err)
	}
	cpu := -1
	for i := 0; i < len(mine)*64; i++ {
		if mine.IsSet(i) {
			cpu = i
		}
	}
	if cpu < 0 {
		t.Skip("this process may run on no CPU")
	}
	var one unix.CPUSet
	one.Set(cpu)
	spinner, other := startProc(t, "while :; do :; done"), startProc(t, "while :; do :; done")
	for _, p := range []Reached{spinner[0], other[0]} {
		if err := unix.SchedSetaffinity(p.PID, &one); err != nil {
			t.Skipf("cannot pin process %d to CPU %d: %v", p.PID, cpu, err)
		}
	}
	rig := newLiveRig()
	c := rig.clock(spinner)
	first := maps.Clone(c.last)
	total := rig.looks(40, c)
	ran, waited := cpuSince(first, c.last)
	if waited == 0 {
		t.Fatalf("the spinner waited for no CPU in %v: the test shared none", total)
	}
	if c.ran > ran {
		t.Errorf("a process sharing a CPU could run %v of %v by the clock, more than the %v of CPU it had (it waited %v for one)", c.ran, total, ran, waited)
	}
}
