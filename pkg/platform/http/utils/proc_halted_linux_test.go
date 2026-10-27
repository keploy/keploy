//go:build linux

package utils

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A process has halted once SIGKILL is pending for it -- for its main thread
// or for the whole process -- or its main thread has exited (a zombie, or
// dead): killed; or once it is stopped by a signal or a tracer. Not otherwise.
func TestStatusHalted(t *testing.T) {
	for _, tc := range []struct {
		name, status, want string
	}{
		{"running", "State:\tS (sleeping)\nSigPnd:\t0000000000000000\nShdPnd:\t0000000000000000\n", ""},
		{"in a removal", "State:\tD (disk sleep)\nSigPnd:\t0000000000000002\nShdPnd:\t0000000000004000\n", ""},
		{"SIGKILL pending for its thread", "State:\tD (disk sleep)\nSigPnd:\t0000000000000100\nShdPnd:\t0000000000000000\n", "killed"},
		{"SIGKILL pending for the process", "State:\tD (disk sleep)\nSigPnd:\t0000000000000000\nShdPnd:\t0000000000000100\n", "killed"},
		{"main thread a zombie, a thread held", "State:\tZ (zombie)\nThreads:\t2\nSigPnd:\t0000000000000000\n", "killed"},
		{"exited, not reaped yet", "State:\tZ (zombie)\nThreads:\t1\n", ""},
		{"dead", "State:\tX (dead)\nThreads:\t1\n", ""},
		{"stopped", "State:\tT (stopped)\nSigPnd:\t0000000000000000\n", "stopped"},
		{"stopped by its tracer", "State:\tt (tracing stop)\n", "stopped"},
	} {
		got := statusHalted([]byte(tc.status))
		if (got == "") != (tc.want == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: halted = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ProcessHalted reads a live process: this one runs, one stopped (SIGSTOP)
// is stopped, and one exited but not reaped yet, or reaped, is not halted
// (its parent reaps it now, or has).
func TestProcessHalted(t *testing.T) {
	if why := ProcessHalted(os.Getpid()); why != "" {
		t.Fatalf("this process is taken for a halted one: %s", why)
	}
	cmd := startStandIn(t)
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, pid, &info, unix.WSTOPPED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	if why := ProcessHalted(pid); !strings.Contains(why, "stopped") {
		t.Fatalf("a stopped process is taken for %q", why)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	if why := ProcessHalted(pid); why != "" {
		t.Fatalf("an exited, unreaped process is taken for a halted one: %s", why)
	}
	_ = cmd.Wait()
	if why := ProcessHalted(pid); why != "" {
		t.Fatalf("a reaped process is taken for a halted one: %s", why)
	}
}

// The command sudo runs is found among its descendants, past a monitor
// process of sudo's own; here sudo and its monitor are scripts named sudo.
func TestCommandUnderSudo(t *testing.T) {
	top, want := startSudoChain(t, t.TempDir())
	if got := CommandUnderSudo(top.Process.Pid); got != want {
		t.Fatalf("CommandUnderSudo = %d, want the command's pid %d", got, want)
	}
}

// startStandIn starts a process that runs until it is killed or the test
// binary is gone: its loop ends with that binary (kill -0 $PPID), and the
// kernel kills it when the thread that started it ends (Pdeathsig) -- which
// ends a stopped one too, as a loop cannot.
func startStandIn(t *testing.T) *exec.Cmd {
	t.Helper()
	// Pdeathsig follows the thread that started it: the test keeps to this
	// thread until it ends.
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	cmd := exec.Command("sh", "-c", `while kill -0 "$PPID" 2>/dev/null; do sleep 0.05; done`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// startSudoChain starts, in dir, a stand-in for sudo running a command under
// a monitor process of its own -- scripts named sudo, the command a script
// named agent -- and returns sudo and the command's pid once it is up. The
// command runs until the test binary is gone (its loop on that binary's pid),
// and sudo and its monitor end with it; the test's end kills the process
// group, while sudo is not reaped.
func startSudoChain(t *testing.T, dir string) (*exec.Cmd, int) {
	t.Helper()
	ready := filepath.Join(dir, "ready")
	agent := filepath.Join(dir, "agent")
	monitor := filepath.Join(dir, "m", "sudo")
	top := filepath.Join(dir, "sudo")
	if err := os.Mkdir(filepath.Dir(monitor), 0o755); err != nil {
		t.Fatal(err)
	}
	script := map[string]string{
		// The command: says its pid once it is up, and runs while the test
		// binary does.
		agent: "#!/bin/sh\necho $$ > \"" + ready + ".tmp\"\nmv \"" + ready + ".tmp\" \"" + ready + "\"\n" +
			"while kill -0 \"$TEST_PID\" 2>/dev/null; do sleep 0.05; done\n",
		monitor: "#!/bin/sh\n\"" + agent + "\"\n",
		top:     "#!/bin/sh\n\"" + monitor + "\"\n",
	}
	for path, body := range script {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(top)
	cmd.Env = append(os.Environ(), "TEST_PID="+strconv.Itoa(os.Getpid()))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		select {
		case <-reaped:
		default: // not reaped: its process group is still its own
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-reaped
		}
	})
	hung := time.After(time.Minute) // only turns a hang into a failure
	for {
		if b, err := os.ReadFile(ready); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			return cmd, pid
		}
		select {
		case <-hung:
			t.Fatal("the command never started")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

const standInOrphanEnv = "KEPLOY_TEST_HALTED_ORPHAN"

// TestHaltedStandInOrphanParent is not a test of its own: with
// KEPLOY_TEST_HALTED_ORPHAN set to a directory it is a test binary that
// starts there the stand-ins of this file's tests -- one stopped, as
// TestProcessHalted leaves it, and the sudo chain -- writes their pids to the
// file "pids", and waits to be killed. It exits once its stdin, a pipe the
// test binary that started it holds open, reads end of file.
func TestHaltedStandInOrphanParent(t *testing.T) {
	dir := os.Getenv(standInOrphanEnv)
	if dir == "" {
		return
	}
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(5)
	}()
	stopped := startStandIn(t)
	if err := stopped.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	top, agent := startSudoChain(t, dir)
	pids := fmt.Sprintf("%d %d %d", stopped.Process.Pid, top.Process.Pid, agent)
	if err := os.WriteFile(filepath.Join(dir, "pids"), []byte(pids), 0o600); err != nil {
		t.Fatal(err)
	}
	select {} // killed here, mid-test: no cleanup runs
}

// No stand-in of this file's tests outlives a test binary killed mid-test (a
// go test timeout, a job slot's memory cap), a stopped one included.
func TestNoHaltedStandInOutlivesItsTest(t *testing.T) {
	dir := t.TempDir() // the parent's: removed here, as the parent is killed
	// Pdeathsig follows the thread that started the parent: the test keeps
	// to this thread until it ends.
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	parent := exec.Command(os.Args[0], "-test.run=^TestHaltedStandInOrphanParent$")
	parent.Env = append(os.Environ(), standInOrphanEnv+"="+dir)
	parent.Stdout, parent.Stderr = os.Stderr, os.Stderr
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	stdin, err := parent.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = parent.Wait(); close(reaped) }()
	t.Cleanup(func() {
		select {
		case <-reaped:
		default: // not reaped: its process group is still its own
			_ = syscall.Kill(-parent.Process.Pid, syscall.SIGKILL)
			<-reaped
		}
		_ = stdin.Close()
	})
	var pids [3]int
	hung := time.After(time.Minute) // only turns a hang into a failure
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "pids")); err == nil {
			if _, err := fmt.Sscanf(string(b), "%d %d %d", &pids[0], &pids[1], &pids[2]); err == nil {
				break
			}
		}
		select {
		case <-reaped:
			t.Fatal("the parent ended before its stand-ins were up")
		case <-hung:
			t.Fatal("the parent never said its stand-ins' pids")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := parent.Process.Kill(); err != nil { // SIGKILL, mid-test
		t.Fatal(err)
	}
	<-reaped
	gone := map[int]bool{}
	hung = time.After(time.Minute) // only turns a leak into a failure
	for _, pid := range pids {
		for processAlive(pid) {
			select {
			case <-hung:
				for _, p := range pids {
					if !gone[p] && processAlive(p) { // this test's own leaks, not a pid reused
						_ = syscall.Kill(p, syscall.SIGKILL)
					}
				}
				t.Fatalf("stand-in %d outlived the test binary that started it", pid)
			case <-time.After(5 * time.Millisecond):
			}
		}
		gone[pid] = true
	}
}

// processAlive reports whether process pid still runs: it exists, and is not
// a zombie -- which a PID 1 that reaps nothing would keep for ever.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	f := strings.Fields(string(b[i+1:]))
	return i >= 0 && len(f) > 0 && f[0][0] != 'Z' && f[0][0] != 'X'
}
