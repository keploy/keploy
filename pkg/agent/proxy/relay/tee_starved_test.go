//go:build linux

package relay

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// starvedChildEnv marks the re-executed test binary that runs the scenario
// while its parent starves it.
const starvedChildEnv = "KEPLOY_TEE_STARVED_CHILD"

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
	if os.Getenv(starvedChildEnv) != "" {
		starvedSlowConsumerScenario(t, os.Getenv(starvedChildEnv) == "busy")
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
