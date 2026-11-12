//go:build darwin

package proxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// A LIVE row names the listener's program by ProgramOf, and so does the
// judge that lists a program off the run's clock: on macOS it was "" for
// every process, so a LIVE row could not be told from another program's.
func TestProgramOfNamesTheExecutable(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	if got := ProgramOf(os.Getpid()); got != want {
		t.Fatalf("ProgramOf(self) = %q, want %q", got, want)
	}

	cmd := exec.Command("/bin/sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	if got := ProgramOf(pid); got != "/bin/sleep" {
		t.Fatalf("ProgramOf(child) = %q, want /bin/sleep", got)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = cmd.Wait()
	reaped = true
	if got := ProgramOf(pid); got != "" {
		t.Fatalf("ProgramOf(reaped child) = %q, want \"\"", got)
	}
	for _, pid := range []int{0, -1} {
		if got := ProgramOf(pid); got != "" {
			t.Fatalf("ProgramOf(%d) = %q, want \"\"", pid, got)
		}
	}
}
