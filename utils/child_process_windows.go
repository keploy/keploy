//go:build windows

package utils

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ProcessEntry32 represents a process entry for use with Process32First/Process32Next.
type ProcessEntry32 struct {
	Size              uint32
	CntUsage          uint32
	ProcessID         uint32
	DefaultHeapID     uintptr
	ModuleID          uint32
	Threads           uint32
	ParentProcessID   uint32
	PriorityClassBase int32
	Flags             uint32
	ExeFile           [windows.MAX_PATH]uint16
}

// findChildPIDs takes a parent PID and returns a slice of all descendant PIDs.
func findChildPIDs(parentPID int) ([]int, error) {
	var childPIDs []int

	// Create a snapshot of all processes in the system.
	handle, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to create process snapshot: %v", err)
	}
	defer windows.CloseHandle(handle)

	// Initialize the ProcessEntry32 structure.
	var entry ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	// Iterate through processes using Process32First and Process32Next.
	for {
		err := windows.Process32Next(handle, (*windows.ProcessEntry32)(unsafe.Pointer(&entry)))
		if err != nil {
			// Break the loop when no more processes are found.
			if err == windows.ERROR_NO_MORE_FILES {
				break
			}
			return nil, fmt.Errorf("failed to enumerate processes: %v", err)
		}

		// Check if the parent process ID matches the specified PID.
		if int(entry.ParentProcessID) == parentPID {
			childPID := int(entry.ProcessID)
			childPIDs = append(childPIDs, childPID)

			// Recursively find descendants of the child process.
			descendants, err := findChildPIDs(childPID)
			if err != nil {
				return nil, err
			}
			childPIDs = append(childPIDs, descendants...)
		}
	}

	return childPIDs, nil
}
