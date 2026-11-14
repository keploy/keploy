//go:build darwin

package proxy

import "golang.org/x/sys/unix"

// parentOf is the parent of process pid, read from the kernel's process table
// (sysctl kern.proc.pid), as descends and the starts registry read it: macOS
// has no /proc. It is false when pid names no process.
func parentOf(pid uint32) (uint32, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if err != nil || kp == nil || kp.Proc.P_pid == 0 {
		return 0, false
	}
	return uint32(kp.Eproc.Ppid), true
}
