//go:build linux

package utils

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// mainThreadExitsEnv makes this test binary, started with it set to a file's
// path, a process whose main thread exits while another runs on, writing a
// count to that file as it runs: its stat says Z, as ps says Zl, and it runs.
const mainThreadExitsEnv = "KEPLOY_TEST_UTILS_MAIN_THREAD_EXITS"

func init() {
	beat := os.Getenv(mainThreadExitsEnv)
	if beat == "" {
		return
	}
	// init runs on the main thread: the goroutine stays on it.
	runtime.LockOSThread()
	go func() {
		for n := 1; ; n++ {
			_ = os.WriteFile(beat, []byte(strconv.Itoa(n)), 0o644)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	for {
		if _, err := os.Stat(beat); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// The main thread alone exits (exit, not exit_group).
	_, _, _ = syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0)
}

// startMainThreadExited starts this test binary as a process whose main
// thread has exited while another runs on, and returns it once its stat says
// Z and its other thread has written; it is killed, and waited for, when the
// test ends.
func startMainThreadExited(t *testing.T) *exec.Cmd {
	t.Helper()
	beat := filepath.Join(t.TempDir(), "beat")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	// No collection (a stop of the world would wait for ever on the main
	// thread, which holds a P it never gives back), and Ps to spare.
	cmd.Env = append(os.Environ(), mainThreadExitsEnv+"="+beat, "GOGC=off", "GOMAXPROCS=4")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(time.Minute) // turns a hang into a failure
	for {
		st, ok := ReadProcStat(cmd.Process.Pid)
		if b, err := os.ReadFile(beat); err == nil && len(b) > 0 && ok && st.State == 'Z' {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d's main thread did not exit with another running (stat ok %v, state %c)", cmd.Process.Pid, ok, st.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A process whose main thread exited while another runs on says Z in its
// stat line, yet runs: RunningThread finds the thread that runs, and Exited
// says it has not exited. Once every thread of it has exited it has, a
// zombie, and RunningThread finds none.
func TestRunningThreadTellsAProcessWhoseMainThreadExitedFromAZombie(t *testing.T) {
	cmd := startMainThreadExited(t)
	pid := cmd.Process.Pid
	tid, runs, err := RunningThread("/proc", pid)
	if err != nil || !runs || tid == pid || tid == 0 {
		t.Fatalf("RunningThread = %d, %v, %v; want a thread of %d other than its main one, running", tid, runs, err, pid)
	}
	if st, ok := ReadProcStat(pid); !ok || Exited(pid, st) {
		t.Fatalf("Exited(%d) with its other thread running", pid)
	}
	_ = cmd.Process.Kill()
	deadline := time.Now().Add(time.Minute) // turns a hang into a failure
	for {
		_, runs, err := RunningThread("/proc", pid)
		if err == nil && !runs {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still has a thread running after its kill (%v)", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st, ok := ReadProcStat(pid); !ok || !Exited(pid, st) {
		t.Fatalf("process %d not Exited once killed (a zombie its Wait has not reaped)", pid)
	}
}

// RunningThread reads each thread's stat for its state alone: a thread being
// released (Z or X, its parent and group gone) is none that runs, and one
// listed whose stat is gone exited meanwhile; a listing, or a thread's stat,
// that cannot be read is an error, never a process taken for exited.
func TestRunningThreadSaysWhatItCouldNotRead(t *testing.T) {
	root := t.TempDir()
	thread := func(pid, tid int, line string) {
		dir := filepath.Join(root, strconv.Itoa(pid), "task", strconv.Itoa(tid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if line != "" {
			if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	line := func(tid int, state string) string {
		return fmt.Sprintf("%d (worker) %s 4242 7 7 0 -1 0 0 0 0 0 0 0 0 0 20 0 2 0 780 0 0\n", tid, state)
	}
	thread(7, 7, line(7, "Z"))
	thread(7, 8, "8 (worker) Z 0 -1 -1 0 -1 4227084 100 0 0 0 0 0 0 0 25 5 0 0 6060366 0 0\n")
	thread(7, 9, "9 (worker) X 0 -1 -1 0 -1 4227084 100 0 0 0 0 0 0 0 25 5 0 0 6060366 0 0\n")
	thread(7, 10, "") // gone before its stat was read
	if tid, runs, err := RunningThread(root, 7); err != nil || runs {
		t.Fatalf("RunningThread = %d, %v, %v; want none running, every thread exited or released", tid, runs, err)
	}
	thread(7, 11, line(11, "S"))
	if tid, runs, err := RunningThread(root, 7); err != nil || !runs || tid != 11 {
		t.Fatalf("RunningThread = %d, %v, %v; want thread 11, sleeping", tid, runs, err)
	}
	if _, runs, err := RunningThread(root, 99); err != nil || runs {
		t.Fatalf("a process that is gone: runs %v, err %v; want neither", runs, err)
	}
	// A stat that is there and cannot be read, and one with no state.
	thread(12, 12, "")
	if err := os.MkdirAll(filepath.Join(root, "12", "task", "12", "stat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunningThread(root, 12); err == nil {
		t.Fatal("a thread's stat that could not be read read as no thread running")
	}
	thread(13, 13, "13 (worker")
	if _, _, err := RunningThread(root, 13); err == nil || !strings.Contains(err.Error(), "no state") {
		t.Fatalf("a stat with no state: err %v; want it said", err)
	}
	// A listing of the threads that cannot be read.
	if err := os.MkdirAll(filepath.Join(root, "14"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "14", "task"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunningThread(root, 14); err == nil {
		t.Fatal("threads that could not be listed read as none running")
	}
}
