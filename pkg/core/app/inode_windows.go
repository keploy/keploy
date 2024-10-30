//go:build windows

package app

import (
	"fmt"
	"golang.org/x/sys/windows"
)

// getInode retrieves a unique identifier for a process on Windows.
// Since there is no /proc filesystem, we use the process handle.
func getInode(pid int) (uint64, error) {
	// Open the process with PROCESS_QUERY_INFORMATION access.
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, fmt.Errorf("failed to open process: %w", err)
	}
	defer windows.CloseHandle(handle)

	// Use the handle itself as a unique identifier.
	// In real scenarios, you may use ProcessId or other identifiers.
	processID := uint64(pid)

	return processID, nil
}
