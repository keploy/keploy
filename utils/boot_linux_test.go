//go:build linux

package utils

import (
	"context"
	"os"
	"path/filepath"
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
// two steps, outside any stage, reporting at 50 ms beats. It writes "ready" as
// that work begins.
func TestBootStarvedHelper(t *testing.T) {
	dir := os.Getenv("KEPLOY_TEST_BOOT_STARVED")
	if dir == "" {
		return
	}
	helperExitsWithParent()
	bootBeat = 50 * time.Millisecond
	reportBootProgress(context.Background(), zap.NewNop(), pathSink(filepath.Join(dir, BootProgressFileName)), nil)
	BootStep("the helper runs")
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		os.Exit(3)
	}
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
	defer starve(helper.Process.Pid)()
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
// read nothing will ever answer, as a bind or a kernel call that never returns.
// It runs fn first, on the goroutine, and is let go when the test ends.
func wedgedWork(t *testing.T, fn func() (end func())) {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil { // blocking: the read sleeps in the kernel
		t.Fatal(err)
	}
	begun, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		end := fn()
		close(begun)
		var b [1]byte
		_, _ = syscall.Read(fds[0], b[:])
		if end != nil {
			end()
		}
	}()
	<-begun
	t.Cleanup(func() {
		_, _ = syscall.Write(fds[1], []byte{0})
		<-ended
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
	})
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

// A stage of the start that waits for work another goroutine does for it --
// StartProxy waiting on its listener's start and on its DNS servers' binds --
// sees that work only as a stage of its own, begun on that goroutine: the
// waiting stage's own thread sleeps throughout, which is no progress. So the
// proxy begins one there (pkg/agent/proxy: the listener's start, each DNS
// server's bind). Begun, the work's progress is the start's; and work that
// wedges there still stops the start.
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

	t.Run("not a stage of its own: the wait is no progress", func(t *testing.T) {
		path := startBootReport(t, beat)
		spun := awaited(t, nil)
		// Its first beat without progress, and then the count: the worker
		// made progress between them.
		awaitBootReport(t, path, func(p BootProgress) bool { return p.Quiet >= 1 })
		before := spun.Load()
		awaitBootReport(t, path, func(p BootProgress) bool { return p.Quiet >= TeardownStallChecks })
		if spun.Load() == before {
			t.Fatal("the worker made no progress while the start was counted as making none")
		}
	})

	t.Run("a stage of its own: its progress is the start's", func(t *testing.T) {
		path := startBootReport(t, beat)
		awaited(t, func() func() { return BeginBootWork("the proxy listener's start") })
		// From the first beat that finds both stages begun, every beat finds
		// the worker's making progress.
		p := awaitBootReport(t, path, func(q BootProgress) bool { return len(q.Stages) == 2 })
		first := p.Beat
		for p.Beat < first+3*TeardownStallChecks {
			if p.Quiet != 0 {
				t.Fatalf("report %+v: a beat of the worker's progress was counted as one without", p)
			}
			p = awaitBootReport(t, path, func(q BootProgress) bool { return q.Beat > p.Beat })
		}
	})

	t.Run("a stage of its own that wedges still stops the start", func(t *testing.T) {
		path := startBootReport(t, beat)
		wedgedWork(t, func() func() { return BeginBootWork("the proxy's start") })
		wedgedWork(t, func() func() { return BeginBootWork("the UDP DNS server's bind") })
		p := awaitBootReport(t, path, func(p BootProgress) bool { return p.Quiet >= TeardownStallChecks })
		if len(p.Stages) != 2 || p.Stages[0].Name != "the UDP DNS server's bind" || p.Stages[1].Name != "the proxy's start" {
			t.Fatalf("report %+v: want both stages named, in order", p)
		}
	})
}
