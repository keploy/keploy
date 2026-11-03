//go:build !linux && !darwin

package agent

// The single-worker wire's instance claim is Linux and Darwin only.

func procAlive(int, uint64) bool   { return false }
func procStart(int) (uint64, bool) { return 0, false }
func procDescends(int, int) bool   { return false }
