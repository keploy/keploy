package routes

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts c in a process group of its own; killProcessGroup
// ends it (the rows' children start no process of their own).
func ownProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func killProcessGroup(c *exec.Cmd) { _ = c.Process.Kill() }
