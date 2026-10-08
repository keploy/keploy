//go:build darwin

package utils

import (
	"testing"

	"golang.org/x/sys/unix"
)

// isZombie reports whether pid has exited and is waiting for its parent to
// reap it.
func isZombie(t *testing.T, pid int) bool {
	t.Helper()
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		t.Fatalf("read process %d: %v", pid, err)
	}
	return p.Proc.P_stat == 5 // SZOMB, sys/proc.h
}
