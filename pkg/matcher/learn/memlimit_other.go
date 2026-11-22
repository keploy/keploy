//go:build !linux && !darwin && !windows

package learn

func processCgroupLimit() int64 { return 0 }

func hostMemory() int64 {
	return machineMemory("/proc/meminfo")
}
