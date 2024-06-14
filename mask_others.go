//go:build !linux

package main

// SetUmask is a no-op for non-Linux systems
func SetUmask() int {
	return 0
}

// RestoreUmask is a no-op for non-Linux systems
func RestoreUmask(oldMask int) {
	// Do nothing
}
