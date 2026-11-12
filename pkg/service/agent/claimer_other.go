//go:build !linux && !darwin && !windows

package agent

// The single-worker wire's instance claim is Linux, Darwin and Windows only.

func procAlive(int, uint64) bool   { return false }
func procStart(int) (uint64, bool) { return 0, false }
func procDescends(int, int) bool   { return false }

// zombieExited is true: procStatStart reads a /proc, which this system has
// not, so it never asks.
var zombieExited = func(int) bool { return true }
