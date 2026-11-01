//go:build linux

package relay

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// starvedChildEnv marks the re-executed test binary that runs the scenario
// while its parent starves it.
const starvedChildEnv = "KEPLOY_TEE_STARVED_CHILD"

// crowdedMode, then a CPU, is a child whose consumer's thread is bound to it.
const crowdedMode = "crowded:"

// TestTee_StarvedProcessIsNotAStalledConsumer reproduces, deterministically,
// the CI flake of TestTee_SlowConsumerLosesNothing (it failed with
// drops=[consumer_gone] at host load 9): a process that gets no CPU for a while
// is not a consumer that stopped reading. The scenario runs in a child process
// that the parent freezes (SIGSTOP) for 200 ms out of every 300 ms, which is
// what an overloaded node or a CPU-throttled container does to the agent. Wall
// time runs on while it is frozen, but neither its consumer nor anything else
// in it can make progress; a stall verdict drawn from wall time alone then
// abandons a live, progressing consumer's chunks.
func TestTee_StarvedProcessIsNotAStalledConsumer(t *testing.T) {
	switch mode := os.Getenv(starvedChildEnv); {
	case mode == "":
	case mode == "hog":
		for {
		}
	case strings.HasPrefix(mode, crowdedMode):
		cpu, err := strconv.Atoi(strings.TrimPrefix(mode, crowdedMode))
		if err != nil {
			t.Fatal(err)
		}
		// Only the consumer's thread: the test goroutine is the consumer.
		runtime.LockOSThread()
		var one unix.CPUSet
		one.Set(cpu)
		if err := unix.SchedSetaffinity(0, &one); err != nil {
			t.Fatal(err)
		}
		starvedSlowConsumerScenario(t, true)
		return
	default:
		starvedSlowConsumerScenario(t, mode == "busy")
		return
	}
	for _, tc := range []struct {
		name      string
		mode      string
		stop, run time.Duration
	}{
		// Long freezes: an overloaded node, or a paused container.
		{"frozen 200ms of every 300ms", "sleep", 200 * time.Millisecond, 100 * time.Millisecond},
		// CFS-style throttling: short, frequent stretches without CPU while
		// the consumer works each chunk longer, in wall time, than the
		// window, but for less than it in CPU.
		{"throttled 40ms of every 50ms", "busy", 40 * time.Millisecond, 10 * time.Millisecond},
		// A tenth of a CPU, as a busy CI runner or a capped agent container
		// gives a process: the run slices are too short for a tick counted
		// per wake-up to stand for the time the process ran.
		{"a tenth of a CPU", "busy", 90 * time.Millisecond, 10 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) { runStarved(t, tc.mode, tc.stop, tc.run) })
	}
	// An overloaded host: nothing stops the process, and the clock's sampler,
	// which sleeps between samples, is on time on a CPU of its own. The
	// consumer, which works, takes turns with four busy processes on its CPU,
	// so that a chunk takes it five times its CPU time, past the window.
	t.Run("a consumer sharing its CPU with four busy processes", func(t *testing.T) {
		if runtime.NumCPU() < 2 {
			t.Skip("needs a CPU for the sampler beside the crowded one")
		}
		cpu := pickCPU(t)
		for range 4 {
			hog := starvedChild("hog")
			hog.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
			startOnCPU(t, hog, cpu)
			t.Cleanup(func() {
				_ = hog.Process.Kill()
				_ = hog.Wait()
			})
		}
		cmd := starvedChild(fmt.Sprintf("%s%d", crowdedMode, cpu))
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := cmd.Run(); err != nil {
			t.Fatalf("starved scenario failed: %v\n%s", err, out.String())
		}
	})
}

// starvedChild is the test binary, to run as mode.
func starvedChild(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestTee_StarvedProcessIsNotAStalledConsumer$", "-test.count=1", "-test.timeout=0")
	cmd.Env = append(os.Environ(), starvedChildEnv+"="+mode)
	return cmd
}

// pickCPU is a CPU this process may run on.
func pickCPU(t *testing.T) int {
	t.Helper()
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		t.Fatal(err)
	}
	cpu := 0
	for !set.IsSet(cpu) {
		cpu++
	}
	return cpu
}

// startOnCPU starts cmd bound to cpu: a child takes the CPU mask of the
// thread that forks it, and hands it to every thread it makes, so the mask is
// set before the exec, on a thread kept for it, and put back.
func startOnCPU(t *testing.T, cmd *exec.Cmd, cpu int) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var was, one unix.CPUSet
	if err := unix.SchedGetaffinity(0, &was); err != nil {
		t.Fatal(err)
	}
	one.Set(cpu)
	if err := unix.SchedSetaffinity(0, &one); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unix.SchedSetaffinity(0, &was); err != nil {
			t.Fatal(err)
		}
	}()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
}

// runStarved runs the scenario in a child that is stopped (SIGSTOP) for stop
// out of every stop+run.
func runStarved(t *testing.T, mode string, stop, run time.Duration) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestTee_StarvedProcessIsNotAStalledConsumer$", "-test.count=1")
	cmd.Env = append(os.Environ(), starvedChildEnv+"="+mode)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.After(2 * time.Minute)
	for {
		_ = cmd.Process.Signal(syscall.SIGSTOP)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("starved scenario failed: %v\n%s", err, out.String())
			}
			return
		case <-time.After(stop):
		}
		_ = cmd.Process.Signal(syscall.SIGCONT)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("starved scenario failed: %v\n%s", err, out.String())
			}
			return
		case <-time.After(run):
		case <-deadline:
			_ = cmd.Process.Kill()
			t.Fatalf("starved scenario did not finish\n%s", out.String())
		}
	}
}

// starvedSlowConsumerScenario is TestTee_SlowConsumerLosesNothing's scenario:
// a consumer that takes a chunk every third of the stall grace must get every
// chunk.
// busy: each chunk costs the consumer CPU (testStallGrace/4 of it) rather than
// a sleep.
func starvedSlowConsumerScenario(t *testing.T, busy bool) {
	tt, rec, _ := newTestTeeWithConsumer(t, 1<<30, 1)
	const n = 20
	for i := 0; i < n; i++ {
		if !tt.push(mkChunk(fmt.Sprintf("c%03d", i))) {
			t.Fatalf("push %d refused", i)
		}
	}
	tt.close()
	var got []string
	for c := range tt.readCh() {
		got = append(got, string(c.Bytes))
		if busy {
			burnCPU(testStallGrace / 4)
		} else {
			time.Sleep(testStallGrace / 3)
		}
	}
	if len(got) != n {
		t.Fatalf("a starved but progressing consumer lost data: got %d chunks, want %d (drops=%v)", len(got), n, rec.snapshot())
	}
}

// burnCPU spins for d of this goroutine's CPU time, however long that takes in
// wall time.
func burnCPU(d time.Duration) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var ru0 syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_THREAD, &ru0)
	for {
		var ru syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_THREAD, &ru)
		used := time.Duration(ru.Utime.Nano()+ru.Stime.Nano()) - time.Duration(ru0.Utime.Nano()+ru0.Stime.Nano())
		if used >= d {
			return
		}
	}
}
