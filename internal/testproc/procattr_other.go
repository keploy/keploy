//go:build unix && !linux

package testproc

import "syscall"

// procAttr runs a process in a group of its own; there is no death signal
// here (see the package's doc).
func procAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
