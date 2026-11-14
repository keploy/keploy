//go:build !windows

package utils

import (
	"errors"
	"syscall"
)

// runLockHolderAlive reports whether a process with pid exists. EPERM means it exists but
// belongs to another user (e.g. a run started with sudo).
func runLockHolderAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
