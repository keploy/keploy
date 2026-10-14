//go:build !windows

package relay

import (
	"syscall"
	"time"
)

// processCPUTime is the CPU time this process has used, user and system, all
// threads. ok is false when it cannot be read.
func processCPUTime() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
