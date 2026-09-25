//go:build !linux && !darwin

package utils

import (
	"fmt"
	"runtime"
)

// Windows interrupts a process tree through console events and taskkill, and
// InterruptProcessTree returns before it reaches these. keploy is built for no
// other platform.

func getProcessGroupID(int) (int, error) {
	return 0, fmt.Errorf("process groups are not read on %s", runtime.GOOS)
}

func findChildPIDs(int) ([]int, error) {
	return nil, fmt.Errorf("process trees are not read on %s", runtime.GOOS)
}
