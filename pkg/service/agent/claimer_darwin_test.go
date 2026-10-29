//go:build darwin

package agent

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// procStart returns a non-zero start time for this process.
func TestProcStartReturnsThisProcess(t *testing.T) {
	start, ok := procStart(os.Getpid())
	if !ok {
		t.Fatal("procStart should succeed for the current process")
	}
	if start == 0 {
		t.Fatal("procStart returned 0 for the current process")
	}
}

// procStart returns false for a non-existent PID.
func TestProcStartReturnsFalseForDeadPID(t *testing.T) {
	_, ok := procStart(99999999)
	if ok {
		t.Fatal("procStart should return false for a non-existent PID")
	}
}

// procAlive confirms the current process is alive.
func TestProcAliveForCurrentProcess(t *testing.T) {
	start, ok := procStart(os.Getpid())
	if !ok {
		t.Fatal("procStart failed")
	}
	if !procAlive(os.Getpid(), start) {
		t.Fatal("procAlive should return true for the current process with its start time")
	}
}

// procAlive returns false for a wrong start time.
func TestProcAliveReturnsFalseForWrongStartTime(t *testing.T) {
	if procAlive(os.Getpid(), 12345) {
		t.Fatal("procAlive should return false for a wrong start time")
	}
}

// procDescends confirms a child process is a descendant of this process.
func TestProcDescendsForChild(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if !procDescends(cmd.Process.Pid, os.Getpid()) {
		t.Fatalf("procDescends(%d, %d) = false, want true", cmd.Process.Pid, os.Getpid())
	}
}

// procDescends returns true when pid == ancestor.
func TestProcDescendsSelf(t *testing.T) {
	if !procDescends(os.Getpid(), os.Getpid()) {
		t.Fatal("procDescends(self, self) should return true")
	}
}

// procDescends returns false for unrelated processes.
func TestProcDescendsReturnsFalseForUnrelated(t *testing.T) {
	// PID 1 (launchd) is not a descendant of this process
	if procDescends(1, os.Getpid()) {
		t.Fatal("procDescends(1, self) should return false")
	}
}

// waitExitedUnreaped blocks until child pid has exited and leaves it unreaped,
// a zombie: waitid(P_PID, pid, WEXITED|WNOWAIT). x/sys/unix has no Waitid on
// darwin, so it goes through the syscall entry.
func waitExitedUnreaped(t *testing.T, pid int) {
	t.Helper()
	const pPID = 1     // P_PID, sys/wait.h
	var info [128]byte // siginfo_t is 104 bytes on darwin
	for {
		_, _, e := syscall.Syscall6(unix.SYS_WAITID, pPID, uintptr(pid),
			uintptr(unsafe.Pointer(&info[0])), unix.WEXITED|unix.WNOWAIT, 0, 0)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			t.Fatalf("waitid(%d, WEXITED|WNOWAIT): %v", pid, e)
		}
		return
	}
}

// A process that has exited but that its parent has not reaped yet (a zombie)
// is gone, as on Linux: procStart finds no start time and procAlive is false,
// so a test process that exited is not taken for one still running its tests.
// Its parent link still holds until it is reaped.
func TestAZombieIsNotAlive(t *testing.T) {
	cmd := exec.Command("/bin/cat")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	pid := cmd.Process.Pid
	start, ok := procStart(pid)
	if !ok || !procAlive(pid, start) {
		t.Fatalf("procStart/procAlive of the running child %d = %d, %v", pid, start, ok)
	}

	if err := in.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	waitExitedUnreaped(t, pid)
	if _, _, stat, found := getProc(pid); !found || stat != sZomb {
		t.Fatalf("child %d after exit: found=%v p_stat=%d, want a zombie (%d)", pid, found, stat, sZomb)
	}
	if procAlive(pid, start) {
		t.Fatalf("procAlive(%d) = true for a zombie", pid)
	}
	if got, ok := procStart(pid); ok {
		t.Fatalf("procStart(%d) = %d, true for a zombie", pid, got)
	}
	if !procDescends(pid, os.Getpid()) {
		t.Fatalf("procDescends(zombie %d, %d) = false, want true", pid, os.Getpid())
	}

	reaped = true
	if err := cmd.Wait(); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got, ok := procStart(pid); ok && got == start {
		t.Fatalf("child %d still found after it was reaped", pid)
	}
}
