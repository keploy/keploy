//go:build !windows

package main

import "syscall"

func SetUmask() int {
	return syscall.Umask(0)
}

func RestoreUmask(oldMask int) {
	syscall.Umask(oldMask)
}
