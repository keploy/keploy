//go:build linux

package learn

import (
	"math"
	"runtime/debug"
	"testing"
)

func TestLinuxHostMemoryMatchesProcMeminfo(t *testing.T) {
	want := machineMemory("/proc/meminfo")
	if want <= 0 {
		t.Fatalf("machineMemory(/proc/meminfo) = %d, want > 0", want)
	}
	if got := hostMemory(); got != want {
		t.Fatalf("hostMemory() = %d, want %d from /proc/meminfo", got, want)
	}
	prev := debug.SetMemoryLimit(math.MaxInt64)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	cg := processCgroupLimit()
	wantLimit := want
	if cg > 0 && cg < wantLimit {
		wantLimit = cg
	}
	if got := memoryLimit(); got != wantLimit {
		t.Errorf("memoryLimit() = %d, want %d (meminfo=%d, cgroup=%d)", got, wantLimit, want, cg)
	}
}
