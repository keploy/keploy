//go:build !linux

package agent

// The single-worker wire's instance claim is Linux-only in the first release.

func procAlive(int, uint64) bool   { return false }
func procStart(int) (uint64, bool) { return 0, false }
func procDescends(int, int) bool   { return false }
