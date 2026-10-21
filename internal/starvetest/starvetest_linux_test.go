//go:build linux

package starvetest

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// heavyExitEnv, set to a count, makes TestHeavyExitHelper map that many
// pages and say how many mappings it has.
const heavyExitEnv = "KEPLOY_TEST_HEAVY_EXIT"

// TestHeavyExitHelper is not a test: with heavyExitEnv set it is a process
// that calls HeavyExit and prints how many mappings it then has.
func TestHeavyExitHelper(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv(heavyExitEnv))
	if err != nil {
		t.Skip("a process of TestHeavyExitMapsEachPageOnItsOwn")
	}
	if err := HeavyExit(n); err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(3)
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		os.Exit(4)
	}
	os.Stdout.WriteString("maps " + strconv.Itoa(bytes.Count(maps, []byte("\n"))) + "\n")
	os.Exit(0)
}

// HeavyExit leaves each page it maps a mapping of its own, which the
// process's exit tears down: none merge.
func TestHeavyExitMapsEachPageOnItsOwn(t *testing.T) {
	const n = 3000
	cmd := exec.Command(os.Args[0], "-test.run=^TestHeavyExitHelper$")
	cmd.Env = append(os.Environ(), heavyExitEnv+"="+strconv.Itoa(n))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the helper failed: %v: %s", err, out)
	}
	var maps int
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if v, ok := bytes.CutPrefix(sc.Bytes(), []byte("maps ")); ok {
			maps, _ = strconv.Atoi(string(v))
		}
	}
	if maps < n {
		t.Fatalf("the helper has %d mappings after HeavyExit(%d): its pages merged", maps, n)
	}
}

// Ran is the CPU the threads of a process had: it grows by what the process
// runs, and a process that is not there is not read.
func TestRanIsTheCPUAProcesssThreadsHad(t *testing.T) {
	before, ok := Ran(os.Getpid())
	if !ok {
		t.Skip("this kernel keeps no account of a thread's time on a CPU (schedstat)")
	}
	const spin = 50 * time.Millisecond
	var ts unix.Timespec
	cpu := func() time.Duration {
		_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts)
		return time.Duration(ts.Nano())
	}
	for start := cpu(); cpu()-start < spin; {
	}
	after, _ := Ran(os.Getpid())
	if after-before < spin {
		t.Fatalf("Ran grew by %v across %v this process ran", after-before, spin)
	}
	if _, ok := Ran(1 << 30); ok {
		t.Fatal("a process that is not there was read")
	}
}

// Spin returns once its file is there, and not before.
func TestSpinReturnsOnceReleased(t *testing.T) {
	released := filepath.Join(MemDir(t), "released")
	done := make(chan struct{})
	go func() { Spin(released); close(done) }()
	select {
	case <-done:
		t.Fatal("Spin returned before the file was there")
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(released, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	<-done
}

// spinEnv, set to a file, makes TestSpinHelper spin until it exists (Spin).
const spinEnv = "KEPLOY_TEST_SPIN"

// TestSpinHelper is not a test: with spinEnv set it is a process of the
// test's own that spins on its way out until the file it names exists
// (Spin).
func TestSpinHelper(t *testing.T) {
	released := os.Getenv(spinEnv)
	if released == "" {
		t.Skip("a process of TestAHeldTestThatDiesLeavesNothingBehind")
	}
	Spin(released)
	os.Exit(0)
}

// waitEnv, set, makes TestWaitHelper wait for its test (EndWithOwner).
const waitEnv = "KEPLOY_TEST_WAIT"

// TestWaitHelper is not a test: with waitEnv set it is a process of the
// test's own that sleeps, waiting for the test, and ends with it
// (EndWithOwner).
func TestWaitHelper(t *testing.T) {
	if os.Getenv(waitEnv) == "" {
		t.Skip("a process of TestAHeldTestThatDiesLeavesNothingBehind")
	}
	EndWithOwner()
	for {
		time.Sleep(time.Hour)
	}
}

// diesEnv, set to how and a directory, makes TestHeldTestDiesHelper a test
// that holds a process and dies so, writing in the directory.
const diesEnv = "KEPLOY_TEST_HELD_DIES"

// TestHeldTestDiesHelper is not a test: with diesEnv set to how and dir it
// starts a process of its own that spins on its way out (Spin) and is never
// released, one that sleeps waiting for it (EndWithOwner), and a shell that
// spins (ShellSpin), holds the first (Hold),
// writes in dir/held how many processes its session then has, and dies:
// "panics" panics off the test's goroutine, as a test's timeout ends it (no
// cleanup runs); "killed" waits for the SIGKILL of a job's end.
func TestHeldTestDiesHelper(t *testing.T) {
	how, dir, ok := strings.Cut(os.Getenv(diesEnv), " ")
	if !ok {
		t.Skip("a process of TestAHeldTestThatDiesLeavesNothingBehind")
	}
	spinner := exec.Command(os.Args[0], "-test.run=^TestSpinHelper$")
	spinner.Env = append(os.Environ(), spinEnv+"="+filepath.Join(dir, "released"), Owner())
	if err := spinner.Start(); err != nil {
		t.Fatal(err)
	}
	waiter := exec.Command(os.Args[0], "-test.run=^TestWaitHelper$")
	waiter.Env = append(os.Environ(), waitEnv+"=1", Owner())
	if err := waiter.Start(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("sh", "-c", ShellSpin("")).Start(); err != nil {
		t.Fatal(err)
	}
	Hold(t, spinner.Process.Pid)
	// Whole once there: written, then renamed.
	n := strconv.Itoa(len(session(os.Getpid())))
	if err := os.WriteFile(filepath.Join(dir, "held.part"), []byte(n), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "held.part"), filepath.Join(dir, "held")); err != nil {
		t.Fatal(err)
	}
	if how == "panics" {
		time.AfterFunc(0, func() { panic("the test timed out") })
	}
	for {
		time.Sleep(time.Hour)
	}
}

// A test that holds a process and dies before its cleanup runs (its
// timeout's panic, a crash, a kill at a job's end) leaves nothing running:
// the hold's hogs die with it, and a process of its own that spins on its
// way out for the test to release it, one that sleeps waiting for it, or a
// shell's spin, ends once the test's process is gone. The test runs in a
// session of its own, and once its process is reaped every process left in
// that session must end. One that has run for a fifth of a second of CPU
// since, still there, spins on with its test gone: without the hogs' death
// signal and the others' look for their test, the 32 hogs, the held spinner
// and the shell were left so, pinning a CPU until a reboot. One left asleep
// fails the test at its timeout.
func TestAHeldTestThatDiesLeavesNothingBehind(t *testing.T) {
	TakesACPU(t)
	for _, how := range []string{"panics", "killed"} {
		t.Run(how, func(t *testing.T) {
			dir := MemDir(t)
			// Its output to a file: a pipe would wait on what it leaves.
			out, err := os.Create(filepath.Join(dir, "out"))
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestHeldTestDiesHelper$", "-test.v")
			cmd.Env = append(os.Environ(), diesEnv+"="+how+" "+dir)
			cmd.Stdout, cmd.Stderr = out, out
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			sid := cmd.Process.Pid
			// Whatever the test finds left, it ends.
			t.Cleanup(func() {
				_ = syscall.Kill(-sid, syscall.SIGKILL)
				for _, p := range session(sid) {
					_ = syscall.Kill(p, syscall.SIGKILL)
				}
			})
			reaped := make(chan struct{})
			go func() { _ = cmd.Wait(); close(reaped) }()
			said := func() string { b, _ := os.ReadFile(filepath.Join(dir, "out")); return string(b) }
			// How many processes its session had as it held: one that died
			// at once may be gone before this looks.
			held := func() (int, bool) {
				b, err := os.ReadFile(filepath.Join(dir, "held"))
				if err != nil {
					return 0, false
				}
				n, err := strconv.Atoi(string(b))
				return n, err == nil
			}
			n, ok := held()
			for !ok {
				select {
				case <-reaped:
					if n, ok = held(); ok {
						break
					}
					if strings.Contains(said(), "--- SKIP") {
						t.Skipf("the held test skipped:\n%s", said())
					}
					t.Fatalf("the held test ended before it held:\n%s", said())
				case <-time.After(10 * time.Millisecond):
					n, ok = held()
				}
			}
			if n < hogs+4 {
				t.Fatalf("the held test's session had %d processes as it held, want its %d hogs, its spinner, its waiter, its shell and itself", n, hogs)
			}
			if how == "killed" {
				_ = syscall.Kill(sid, syscall.SIGKILL)
			}
			<-reaped
			ran := map[int]time.Duration{}
			for {
				left := session(sid)
				if len(left) == 0 {
					return
				}
				var since time.Duration
				for _, p := range left {
					r, _ := Ran(p)
					if _, ok := ran[p]; !ok {
						ran[p] = r
					}
					since += r - ran[p]
				}
				if since >= 200*time.Millisecond {
					t.Fatalf("%d processes of the held test outlived it, running %v of CPU since it died: %s", len(left), since, describeProcs(left))
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// session is every live process of session sid (its stat's field 6), but a
// zombie: those that have exited and wait to be reaped.
func session(sid int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			continue
		}
		f := bytes.Fields(b[i+1:])
		if len(f) < 4 || string(f[0]) == "Z" {
			continue
		}
		if s, err := strconv.Atoi(string(f[3])); err == nil && s == sid {
			pids = append(pids, pid)
		}
	}
	return pids
}

// describeProcs names each of pids by its command line.
func describeProcs(pids []int) string {
	var each []string
	for _, p := range pids {
		b, _ := os.ReadFile("/proc/" + strconv.Itoa(p) + "/cmdline")
		each = append(each, fmt.Sprintf("%d (%s)", p, strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))))
	}
	return strings.Join(each, ", ")
}

// gateEnv, set to a directory, makes TestCPUGateHelper write in it once it
// passes TakesACPU.
const gateEnv = "KEPLOY_TEST_CPU_GATE"

// TestCPUGateHelper is not a test: with gateEnv set it writes dir/ran once
// TakesACPU lets it run.
func TestCPUGateHelper(t *testing.T) {
	dir := os.Getenv(gateEnv)
	if dir == "" {
		t.Skip("a process of TestATestThatTakesACPURunsOnlyWhereTheHostIsOneForIt")
	}
	TakesACPU(t)
	if err := os.WriteFile(filepath.Join(dir, "ran"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A test that keeps a CPU busy (a hold, a spinner) runs only where
// KEPLOY_TEST_CPU_HOLD=1 says the host is one for it, and skips elsewhere:
// a laptop's routine `go test` of a package pins no CPU.
func TestATestThatTakesACPURunsOnlyWhereTheHostIsOneForIt(t *testing.T) {
	for _, tc := range []struct {
		value string
		runs  bool
	}{{"", false}, {"0", false}, {"1", true}} {
		dir := t.TempDir()
		cmd := exec.Command(os.Args[0], "-test.run=^TestCPUGateHelper$", "-test.v")
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, cpuHoldEnv+"=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, gateEnv+"="+dir)
		if tc.value != "" {
			cmd.Env = append(cmd.Env, cpuHoldEnv+"="+tc.value)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s=%q: the helper failed: %v\n%s", cpuHoldEnv, tc.value, err, out)
		}
		_, ranErr := os.Stat(filepath.Join(dir, "ran"))
		if ran := ranErr == nil; ran != tc.runs {
			t.Errorf("%s=%q: the test ran %v, want %v\n%s", cpuHoldEnv, tc.value, ran, tc.runs, out)
		}
	}
}

// Spin ends with the test whose process its environment names (Owner): it
// spins while that process runs, until released, and exits at once (code
// ownerGoneExit) where it is gone, or its pid names another process now (a
// start time not its own). One that has run for a fifth of a second of CPU
// and not exited spins on.
func TestSpinEndsWithItsTestAlone(t *testing.T) {
	TakesACPU(t)
	gone := exec.Command("sleep", "600")
	if err := gone.Start(); err != nil {
		t.Fatal(err)
	}
	goneStart, ok := startTime(gone.Process.Pid)
	if !ok {
		t.Fatal("cannot read the start time of a process just started")
	}
	_ = gone.Process.Kill()
	_ = gone.Wait()
	mine, _ := startTime(os.Getpid())
	for _, tc := range []struct {
		name, owner string
		ends        bool
	}{
		{"its test runs", Owner(), false},
		{"its test is gone", fmt.Sprintf("%s=%d:%d", ownerEnv, gone.Process.Pid, goneStart), true},
		{"its test's pid names another process", fmt.Sprintf("%s=%d:%d", ownerEnv, os.Getpid(), mine+1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			released := filepath.Join(MemDir(t), "released")
			cmd := exec.Command(os.Args[0], "-test.run=^TestSpinHelper$")
			cmd.Env = append(os.Environ(), spinEnv+"="+released, tc.owner)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			for {
				select {
				case err := <-exited:
					var ee *exec.ExitError
					if !tc.ends || !errors.As(err, &ee) || ee.ExitCode() != ownerGoneExit {
						t.Fatalf("Spin ended (%v), want it to spin on while its test runs", err)
					}
					return
				case <-time.After(10 * time.Millisecond):
				}
				if ran, _ := Ran(cmd.Process.Pid); ran >= 200*time.Millisecond {
					if tc.ends {
						t.Fatalf("Spin ran %v of CPU and spins on, its test gone", ran)
					}
					break
				}
			}
			if err := os.WriteFile(released, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := <-exited; err != nil {
				t.Fatalf("released, Spin's process ended with %v, want 0", err)
			}
		})
	}
}
