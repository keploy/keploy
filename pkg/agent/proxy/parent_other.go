//go:build !linux && !darwin

package proxy

// parentOf finds no process's parent here, as descends follows none: a
// connection's chain is its own process alone.
func parentOf(uint32) (uint32, bool) { return 0, false }
