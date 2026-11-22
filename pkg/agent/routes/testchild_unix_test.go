//go:build unix

package routes

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// ownProcessGroup starts cmd as the leader of a process group of its own, so
// killProcessGroup reaches whatever it starts as well.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the process group ownProcessGroup gave cmd.
func killProcessGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// A child still running when its test ends is stopped by the test's cleanup,
// with every process it started (its process group): here a child whose port
// is held by a process it started, which does not end when its stdin closes.
func TestAChildStillRunningWhenItsTestEndsIsStopped(t *testing.T) {
	const name = "TestAChildStillRunningWhenItsTestEndsIsStopped"
	switch os.Getenv(childRoleEnv) {
	case "":
		file := filepath.Join(t.TempDir(), "addr")
		var addr string
		t.Run("the test", func(t *testing.T) {
			c := startTestChild(t, name, []string{childRoleEnv + "=stubborn", childAddrFileEnv + "=" + file}, "-test.timeout=2m")
			addr = heldAddr(t, file, c)
		})
		if addr != "" {
			portLetGo(t, addr, "a process the child started outlived the test")
		}
	case "stubborn":
		// Started in the child's process group, and not as a test's child:
		// it does not end when its stdin closes.
		holder := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.count=1", "-test.timeout=2m")
		holder.Env = append(os.Environ(), testChildEnv+"=", childRoleEnv+"=holder")
		if err := holder.Start(); err != nil {
			t.Fatalf("start the holder: %v", err)
		}
		time.Sleep(2 * time.Minute)
	case "holder":
		holdAPort(t)
	}
}
