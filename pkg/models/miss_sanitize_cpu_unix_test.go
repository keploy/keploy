//go:build linux || darwin

package models

import (
	"time"

	"golang.org/x/sys/unix"
)

// threadCPU is the CPU time the calling thread has run for, by its own clock
// (CLOCK_THREAD_CPUTIME_ID): time it waited for a CPU, or that a CPU quota
// held it back, is no part of it. False if the clock cannot be read.
func threadCPU() (time.Duration, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts); err != nil {
		return 0, false
	}
	return time.Duration(ts.Nano()), true
}
