//go:build !linux && !darwin

package utils

import (
	"fmt"
	"runtime"
	"syscall"
)

// Windows interrupts a process tree through console events and taskkill, and
// InterruptProcessTree returns before it reaches these. keploy is built for no
// other platform.

func findChildPIDs(int) ([]int, func(pid int) (int, error), error) {
	return nil, nil, fmt.Errorf("process trees are not read on %s", runtime.GOOS)
}

// groupReached reads no process here: InterruptProcessTreeReached says none.
func groupReached(int, syscall.Signal) []Reached { return nil }
