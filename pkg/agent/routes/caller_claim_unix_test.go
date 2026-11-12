//go:build linux || darwin

package routes

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts c in a process group of its own, which
// killProcessGroup kills whole.
func ownProcessGroup(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killProcessGroup(c *exec.Cmd) { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
