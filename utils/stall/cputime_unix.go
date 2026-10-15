//go:build !windows

package stall

import (
	"syscall"
	"time"
)

// ProcessCPUTime is the CPU time this process has used, user and system, all
// threads. ok is false when it cannot be read.
func ProcessCPUTime() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
