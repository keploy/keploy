//go:build darwin

package learn

import "golang.org/x/sys/unix"

func processCgroupLimit() int64 { return 0 }

// hostMemory returns the machine's physical memory in bytes via
// sysctl hw.memsize, or 0 if the query fails.
func hostMemory() int64 {
	return darwinHostMemory(func(name string) (uint64, error) {
		return unix.SysctlUint64(name)
	})
}

func darwinHostMemory(sysctl func(string) (uint64, error)) int64 {
	n, err := sysctl("hw.memsize")
	if err != nil {
		return 0
	}
	return clampMemoryBytes(n)
}
