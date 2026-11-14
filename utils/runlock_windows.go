//go:build windows

package utils

import "os"

// runLockHolderAlive reports whether a process with pid exists; on Windows FindProcess
// fails when it does not.
func runLockHolderAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
