//go:build windows

package learn

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32DLL              = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32DLL.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx matches the Win32 MEMORYSTATUSEX structure passed to
// GlobalMemoryStatusEx.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

func processCgroupLimit() int64 { return 0 }

// hostMemory returns the machine's physical memory in bytes via
// GlobalMemoryStatusEx (ullTotalPhys), or 0 if the call fails.
func hostMemory() int64 {
	return windowsHostMemory(globalMemoryStatusEx)
}

func globalMemoryStatusEx(ms *memoryStatusEx) bool {
	if err := procGlobalMemoryStatusEx.Find(); err != nil {
		return false
	}
	r1, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(ms)))
	return r1 != 0
}

func windowsHostMemory(query func(*memoryStatusEx) bool) int64 {
	var ms memoryStatusEx
	ms.length = uint32(unsafe.Sizeof(ms))
	if !query(&ms) {
		return 0
	}
	return clampMemoryBytes(ms.totalPhys)
}
