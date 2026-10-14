//go:build !linux

package relay

// processCPULimit: no CFS quota to read outside Linux.
func processCPULimit() (float64, bool) { return 0, false }
