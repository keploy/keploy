//go:build windows

package relay

import (
	"time"

	"golang.org/x/sys/windows"
)

// processCPUTime is the CPU time this process has used, user and kernel, all
// threads. ok is false when it cannot be read.
func processCPUTime() (time.Duration, bool) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	// Filetime counts 100 ns intervals.
	ticks := int64(kernel.HighDateTime)<<32 | int64(kernel.LowDateTime)
	ticks += int64(user.HighDateTime)<<32 | int64(user.LowDateTime)
	return time.Duration(ticks * 100), true
}
