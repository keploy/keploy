//go:build linux || darwin

package main

import "syscall"

// SetUmask sets the umask for Linux systems
func SetUmask() int {
	return syscall.Umask(0)
}

// RestoreUmask restores the original umask for Linux systems
func RestoreUmask(oldMask int) {
	syscall.Umask(oldMask)
}
