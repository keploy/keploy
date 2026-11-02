//go:build linux

package testproc

import "syscall"

// procAttr runs a process in a group of its own, killed by the kernel when
// the thread that started it ends.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
