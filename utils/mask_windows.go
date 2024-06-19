//go:build windows

package utils

func SetUmask() int {
	return 0
}

func RestoreUmask(oldMask int) {
	// Do nothing
}
