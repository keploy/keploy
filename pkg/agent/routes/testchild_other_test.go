//go:build !unix

package routes

import "os/exec"

// ownProcessGroup does nothing here: these systems have no process groups to
// kill as one. A child still ends with its test's process (endWithTheTest).
func ownProcessGroup(*exec.Cmd) {}

// killProcessGroup kills the child itself.
func killProcessGroup(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
}
