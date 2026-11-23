//go:build !linux

package utils

// listenPortsOf is not known off Linux: nil.
func listenPortsOf(int) []int { return nil }
