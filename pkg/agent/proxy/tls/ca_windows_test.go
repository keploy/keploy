package tls

import (
	"os/exec"
	"syscall"
)

// setOwnProcessGroup makes cmd start in a process group of its own.
func setOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessGroup kills cmd's process, if it started. Windows kills no group
// by its id; the children these tests start are test binaries that start no
// process of their own.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
