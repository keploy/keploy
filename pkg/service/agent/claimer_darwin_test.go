//go:build darwin

package agent

import (
	"os"
	"os/exec"
	"testing"
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
