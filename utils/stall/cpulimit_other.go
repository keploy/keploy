//go:build !linux

package stall

// ProcessCPULimit: no CFS quota to read outside Linux.
func ProcessCPULimit() (float64, bool) { return 0, false }
