//go:build windows

package main

func SetUmask() int {
	return 0
}

func RestoreUmask(oldMask int) {
	// Do nothing
}
