//go:build linux

package learn

func processCgroupLimit() int64 {
	return cgroupLimit("/proc/self/cgroup", "/sys/fs/cgroup")
}

func hostMemory() int64 {
	return machineMemory("/proc/meminfo")
}
