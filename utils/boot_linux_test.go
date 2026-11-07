//go:build linux

package utils

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// bootStarvedWork is the CPU time the starved helper's start spends between
// two steps: ten beats of 50 ms when it runs freely, a third of the count the
// CLI gives a start up by.
const bootStarvedWork = 500 * time.Millisecond

// TestBootStarvedHelper is not a test of its own: with KEPLOY_TEST_BOOT_STARVED
// set it is an agent whose start spends bootStarvedWork of CPU time between
// two steps, outside any stage, reporting at 50 ms beats. It writes "ready",
// then starts only on the test's go-ahead (SIGUSR1), which the test gives once
// the starving has begun. A start begun on "ready" ran unstarved, and was
// charged beats, for as long as the test took to see the file: on a loaded
// host most of its work ran before the starving did.
func TestBootStarvedHelper(t *testing.T) {
	dir := os.Getenv("KEPLOY_TEST_BOOT_STARVED")
	if dir == "" {
		return
	}
	helperExitsWithParent()
	goAhead := make(chan os.Signal, 1)
	signal.Notify(goAhead, syscall.SIGUSR1) // before "ready": the test signals only after it
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		os.Exit(3)
	}
	<-goAhead
	bootBeat = 50 * time.Millisecond
	reportBootProgress(context.Background(), zap.NewNop(), pathSink(filepath.Join(dir, BootProgressFileName)), nil)
	BootStep("the helper runs")
	spinCPU(bootStarvedWork)
	BootStep("the helper's work is over")
	MarkBootReady()
	time.Sleep(time.Hour)
	os.Exit(0)
}

// spinCPU spends d of this process's CPU time.
func spinCPU(d time.Duration) {
	start, _ := stall.ProcessCPUTime()
	for {
		if now, _ := stall.ProcessCPUTime(); now-start >= d {
			return
		}
		for i := 0; i < 100000; i++ {
		}
	}
}

// starve lets the process pid run 3 ms of every 60 (SIGSTOP and SIGCONT), a
// twentieth of the time, until the returned func is called, and leaves it
// running. Each run is shorter than the process clock's sampling tick
// (stall.Tick), so every sample it takes is late, as a frozen, stopped or
// CPU-throttled process's are: its clock counts only the CPU time it gets.
func starve(pid int) (stop func()) {
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = syscall.Kill(pid, syscall.SIGCONT) }()
		for {
			_ = syscall.Kill(pid, syscall.SIGCONT)
			select {
			case <-quit:
				return
			case <-time.After(3 * time.Millisecond):
			}
			_ = syscall.Kill(pid, syscall.SIGSTOP)
			select {
			case <-quit:
				return
			case <-time.After(57 * time.Millisecond):
			}
		}
	}()
	return func() { close(quit); <-done }
}

// A starved agent's start is not given up on for the host's starving it. Its
// start spends a third of the CLI's count of beats of its own CPU time
// between two steps, outside any stage; the host stops it but a twentieth of
// the time (starve). On beats of wall time it was charged a beat for every
// beat it waited to run, and its start was given up on long before its work
// was done (120 beats without progress for this work, so starved, measured
// before beats were the agent's runnable time). On beats of its own runnable
// time it is charged for the work alone, however long the host takes to run
// it: neither its own count (Quiet) nor the count an older CLI makes from its
// beats comes near the CLI's.
func TestAStarvedStartIsNotChargedBeatsItCouldNotRun(t *testing.T) {
	dir := t.TempDir()
	helper, exited, out := startHelper(t, "TestBootStarvedHelper", "KEPLOY_TEST_BOOT_STARVED="+dir)
	awaitHelperFile(t, dir, "ready", exited, out)
	// Stopped before it is let go, and starve's first act is to let it run:
	// so the work begins inside the starving, however late this goroutine or
	// starve's runs on a loaded host.
	pid := helper.Process.Pid
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	defer starve(pid)()
	began := time.Now()

	path := filepath.Join(dir, BootProgressFileName)
	var p, last BootProgress
	var maxQuiet, cliQuiet, maxCLIQuiet uint64
	seen := false
	hung := time.Now().Add(5 * time.Minute) // only turns a hang into a failure
	for !p.Ready {
		if time.Now().After(hung) {
			t.Fatalf("the starved helper's start never ended; last report %+v\n%s", p, out.String())
		}
		var ok bool
		if p, ok = readBoot(t, path); !ok || (seen && p.Beat == last.Beat && p.Seq == last.Seq) {
			time.Sleep(time.Millisecond)
			continue
		}
		// An older CLI's count, from the beats it read.
		if seen && p.Seq == last.Seq && !p.Busy && p.Beat > last.Beat {
			cliQuiet += p.Beat - last.Beat
		} else {
			cliQuiet = 0
		}
		seen, last = true, p
		maxQuiet, maxCLIQuiet = max(maxQuiet, p.Quiet), max(maxCLIQuiet, cliQuiet)
	}
	took := time.Since(began)
	t.Logf("the starved start was over at beat %d, %s after its work began; the most beats without progress in a row: %d (its own count), %d (an older CLI's)", p.Beat, took.Round(time.Millisecond), maxQuiet, maxCLIQuiet)
	if took < 4*bootStarvedWork {
		t.Fatalf("its %s of work took %s: it was not starved, and the test proves nothing", bootStarvedWork, took)
	}
	if !p.CountsQuiet {
		t.Fatalf("the report %+v does not carry the agent's own count", p)
	}
	if maxQuiet >= TeardownStallChecks || maxCLIQuiet >= TeardownStallChecks {
		t.Fatalf("a starved start that kept working was charged %d beats without progress (an older CLI: %d): the CLI gives it up at %d", maxQuiet, maxCLIQuiet, TeardownStallChecks)
	}
}

// wedgedWork is a goroutine's work that never finishes: its thread asleep in a
// read that only wake answers, standing in for a bind or a kernel call that
// never returns. It runs fn first, on the goroutine, and returns once the
// thread's syscall file names the read (asleepInRead). Nothing has written to
// the pipe yet, so the thread sleeps in it: from then on a look at the thread
// finds it has not run since, until wake has it answer one read and sleep in
// the next. It is let go when the test ends.
func wedgedWork(t *testing.T, fn func() (end func())) (wake func()) {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil { // blocking: the read sleeps in the kernel
		t.Fatal(err)
	}
	var answered atomic.Uint64 // the reads that returned a byte
	tids, ended := make(chan int, 1), make(chan struct{})
	go func() {
		defer close(ended)
		// The reads are on the thread fn's stage watches, whatever fn does.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		end := fn()
		tids <- syscall.Gettid()
		var b [1]byte
		for {
			n, err := syscall.Read(fds[0], b[:])
			if err == syscall.EINTR {
				continue
			}
			if n <= 0 {
				break // the pipe's write end is closed: the test is over
			}
			answered.Add(1)
		}
		if end != nil {
			end()
		}
	}()
	t.Cleanup(func() {
		_ = syscall.Close(fds[1])
		<-ended
		_ = syscall.Close(fds[0])
	})
	tid := <-tids
	// asleep returns once the thread sleeps in the read after the n-th
	// answered: one that began after that read returned.
	asleep := func(n uint64) {
		t.Helper()
		deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
		for answered.Load() != n || !asleepInRead(t, tid) {
			if time.Now().After(deadline) {
				t.Fatalf("the wedged work's thread %d never slept in its read", tid)
			}
			time.Sleep(time.Millisecond)
		}
	}
	asleep(0)
	return func() {
		t.Helper()
		n := answered.Load()
		if _, err := syscall.Write(fds[1], []byte{0}); err != nil {
			t.Fatal(err)
		}
		asleep(n + 1)
	}
}

// asleepInRead reports whether thread tid of this process is in a read(2), as
// its syscall file (proc(5)) says. The kernel names the system call there only
// for a thread it finds off its CPU and off its run queue; one that runs, or is
// queued to run, reads "running". A named read alone does not show the thread
// asleep. A thread being woken (TASK_WAKING) can be found off its CPU and its
// run queue before it runs, and still names the read it is woken from. And a
// thread may run as soon as the kernel has looked. So asleep also checks the
// count of answered reads: the read it accepts began after the last answered
// one returned. The kernel lets a thread of the same process read the file.
func asleepInRead(t *testing.T, tid int) bool {
	t.Helper()
	b, err := os.ReadFile("/proc/self/task/" + strconv.Itoa(tid) + "/syscall")
	if err != nil {
		t.Fatalf("the kernel does not say what thread %d is doing: %v", tid, err)
	}
	f := strings.Fields(string(b))
	return len(f) > 0 && f[0] == strconv.Itoa(syscall.SYS_READ)
}

// startManualBootReport runs the reporter on a clock the test drives, at beats
// of BootBeat, and returns once it has taken its first look: a stage begun
// after it is first looked at by the first beat, whose look at it is progress
// (a probe's first answer).
func startManualBootReport(t *testing.T) (*stall.ManualClock, string) {
	t.Helper()
	clk := manualBootClock(t)
	path := startBootReport(t, BootBeat)
	awaitPending(t, clk)
	return clk, path
}

// manualBootBeats has the agent run n more beats on clk at once, once the
// reporter's last look is done and before (nil: nothing) has returned, and
// returns the report of the one look the reporter takes for them.
func manualBootBeats(t *testing.T, clk *stall.ManualClock, path string, n int, before func()) BootProgress {
	t.Helper()
	awaitPending(t, clk) // the last beat's look is done
	if before != nil {
		before()
	}
	clk.Advance(time.Duration(n) * BootBeat)
	b := uint64(clk.Runnable() / BootBeat)
	return awaitBootReport(t, path, func(p BootProgress) bool { return p.Beat == b })
}

// startBootReport runs the reporter for the test, at beats of beat of this
// process's runnable time, and returns the path it writes.
func startBootReport(t *testing.T, beat time.Duration) string {
	t.Helper()
	resetBoot(t)
	bootBeat = beat
	path := filepath.Join(t.TempDir(), BootProgressFileName)
	stop := make(chan struct{})
	reported := reportBootProgress(context.Background(), zap.NewNop(), pathSink(path), stop)
	t.Cleanup(func() { close(stop); <-reported })
	return path
}

// A start that stops making progress is still given up on: the wedged-pipe
// control of TestAStarvedStartIsNotChargedBeatsItCouldNotRun. A stage whose
// thread sleeps in a read nothing answers, the agent running freely, counts
// a beat without progress at every beat of its runnable time, and reaches the
// CLI's count. The report names the stage and the last step, and says its
// thread sleeps, and where, when the kernel lets it be read.
func TestAWedgedStartIsStillGivenUp(t *testing.T) {
	path := startBootReport(t, 10*time.Millisecond)
	BootStep("the agent's process runs")
	wedgedWork(t, func() func() { return BeginBootWork("the hooks' load") })
	p := awaitBootReport(t, path, func(p BootProgress) bool { return p.Quiet >= TeardownStallChecks })
	if p.Step != "the hooks' load began" || len(p.Stages) != 1 {
		t.Fatalf("report %+v: want the stage's start as the last step, and the one stage", p)
	}
	if st := p.Stages[0]; st.Name != "the hooks' load" || st.Progressing || st.State != "S" {
		t.Fatalf("stage %+v: want the hooks' load, not progressing, its thread sleeping (S)", st)
	}
	t.Logf("the wedged stage as reported: %+v", p.Stages[0])
}

// A beat at which a stage of the start makes progress starts the count of
// beats without progress over: the CLI gives a start up after
// TeardownStallChecks beats in a row at which none of its stages did, not
// after that many in all. And several beats the agent runs between two looks
// at a stage that made none each count. The stage's thread sleeps in its read,
// woken once between two beats and back asleep before that beat looks: each
// look after the first, but that beat's, finds it has not run since the last.
func TestABeatOfProgressStartsTheCountOver(t *testing.T) {
	clk, path := startManualBootReport(t)
	wake := wedgedWork(t, func() func() { return BeginBootWork("the proxy's start") })
	quiet := func(want uint64, p BootProgress) {
		t.Helper()
		if p.Quiet != want || p.Busy || len(p.Stages) != 1 || p.Stages[0].Progressing {
			t.Fatalf("report %+v: want %d beats without progress, the stage's thread asleep since the last look", p, want)
		}
	}
	progress := func(p BootProgress) {
		t.Helper()
		if p.Quiet != 0 || !p.Busy || len(p.Stages) != 1 || !p.Stages[0].Progressing {
			t.Fatalf("report %+v: a beat at which the stage made progress did not start the count over", p)
		}
	}
	progress(manualBootBeats(t, clk, path, 1, nil)) // the first look
	for q := uint64(1); q < TeardownStallChecks; q++ {
		quiet(q, manualBootBeats(t, clk, path, 1, nil))
	}
	progress(manualBootBeats(t, clk, path, 1, wake)) // one short of the count, the stage runs
	for q := uint64(1); q <= TeardownStallChecks-3; q++ {
		quiet(q, manualBootBeats(t, clk, path, 1, nil))
	}
	p := manualBootBeats(t, clk, path, 3, nil) // three beats, one look
	quiet(TeardownStallChecks, p)
	if want := uint64(1 + TeardownStallChecks + TeardownStallChecks); p.Beat != want {
		t.Fatalf("report %+v: want beat %d", p, want)
	}
}

// A stage of the start that waits for work another goroutine does for it --
// StartProxy waiting on its listener's start -- sees that work only as a
// stage of its own, begun on that goroutine: the waiting stage's own thread
// sleeps throughout, which is no progress. So the proxy begins one there
// (pkg/agent/proxy: the listener's start). Begun, the work's progress is the start's; and work that
// wedges there still stops the start.
//
// The first two rows beat by events, not on the process's clock: each beat
// comes once the worker has run since the beat before, so its look follows
// work the last look could not have seen, and the waiting stage's thread
// sleeps in its read before the first beat looks at it (wedgedWork), so no
// look after the first finds it has run. On the process's clock a beat of
// 10 ms is the Go scheduler's own time slice, and under load the worker's
// goroutine, runnable, can wait out a whole beat for the scheduler with its
// thread parked (S, futex_wait_queue): no progress the kernel shows, and
// none of the stage's at that beat (workProbe).
func TestAStageWaitingForAnotherGoroutineCountsItsWorkAsAStageOfItsOwn(t *testing.T) {
	const beat = 10 * time.Millisecond
	// awaited is the waiting stage, and a worker that spins until the test
	// ends, beginning a stage of its own with begin.
	awaited := func(t *testing.T, begin func() func()) (spun *atomic.Uint64) {
		t.Helper()
		spun = new(atomic.Uint64)
		quit, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			if begin != nil {
				defer begin()()
			}
			for {
				select {
				case <-quit:
					return
				default:
				}
				for i := 0; i < 100000; i++ {
				}
				spun.Add(1)
			}
		}()
		wedgedWork(t, func() func() { return BeginBootWork("the proxy's start") })
		t.Cleanup(func() { close(quit); <-done })
		return spun
	}
	// workedBeat has the agent run one more beat on clk once the worker has
	// made progress since the last beat's look, and returns that beat's
	// report: the look follows work of the worker's it has not seen yet.
	workedBeat := func(t *testing.T, clk *stall.ManualClock, path string, spun *atomic.Uint64) BootProgress {
		t.Helper()
		return manualBootBeats(t, clk, path, 1, func() {
			n := spun.Load()
			deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
			for spun.Load() == n {
				if time.Now().After(deadline) {
					t.Fatal("the worker made no progress")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}

	t.Run("not a stage of its own: the wait is no progress", func(t *testing.T) {
		clk, path := startManualBootReport(t)
		spun := awaited(t, nil)
		// Every beat after the stage's first look counts one more without
		// progress, the worker making progress across each, up to the count:
		// the stage's thread sleeps in its read throughout.
		for q := uint64(0); q <= TeardownStallChecks; q++ {
			if p := workedBeat(t, clk, path, spun); p.Quiet != q {
				t.Fatalf("report %+v: want %d beats without progress, the worker's progress no progress of the start", p, q)
			}
		}
	})

	t.Run("a stage of its own: its progress is the start's", func(t *testing.T) {
		clk, path := startManualBootReport(t)
		spun := awaited(t, func() func() { return BeginBootWork("the proxy listener's start") })
		awaitBootReport(t, path, func(q BootProgress) bool { return q.Seq == 2 }) // both stages begun
		// Every beat finds the worker's stage making progress.
		for i := 0; i < 3*TeardownStallChecks; i++ {
			p := workedBeat(t, clk, path, spun)
			if p.Quiet != 0 || !p.Busy || len(p.Stages) != 2 || p.Stages[0].Name != "the proxy listener's start" || !p.Stages[0].Progressing {
				t.Fatalf("report %+v: a beat of the worker's progress was counted as one without", p)
			}
		}
	})

	t.Run("a stage of its own that wedges still stops the start", func(t *testing.T) {
		path := startBootReport(t, beat)
		wedgedWork(t, func() func() { return BeginBootWork("the proxy's start") })
		wedgedWork(t, func() func() { return BeginBootWork("the proxy listener's start") })
		p := awaitBootReport(t, path, func(p BootProgress) bool { return p.Quiet >= TeardownStallChecks })
		if len(p.Stages) != 2 || p.Stages[0].Name != "the proxy listener's start" || p.Stages[1].Name != "the proxy's start" {
			t.Fatalf("report %+v: want both stages named, in order", p)
		}
	})
}
