//go:build linux

package utils

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The process tree InterruptProcessTree signals, read from /proc.

func getProcessGroupID(pid int) (int, error) {
	statusPath := filepath.Join("/proc", strconv.Itoa(pid), "status")
	statusBytes, err := os.ReadFile(statusPath)
	if err != nil {
		return 0, err
	}

	status := string(statusBytes)
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "NSpgid:") {
			return extractIDFromStatusLine(line), nil
		}
	}

	return 0, nil
}

// extractIDFromStatusLine extracts the ID from a status line in the format "Key:\tValue".
func extractIDFromStatusLine(line string) int {
	fields := strings.Fields(line)
	if len(fields) == 2 {
		id, err := strconv.Atoi(fields[1])
		if err == nil {
			return id
		}
	}
	return -1
}

// findChildPIDs takes a parent PID and returns a slice of all descendant PIDs,
// and how to read each one's process group.
func findChildPIDs(parentPID int) ([]int, func(pid int) (int, error), error) {
	var childPIDs []int

	// Recursive helper function to find all descendants of a given PID.
	var findDescendants func(int)
	findDescendants = func(pid int) {
		procDirs, err := os.ReadDir("/proc")
		if err != nil {
			return
		}

		for _, procDir := range procDirs {
			if !procDir.IsDir() {
				continue
			}

			childPid, err := strconv.Atoi(procDir.Name())
			if err != nil {
				continue
			}

			statusPath := filepath.Join("/proc", procDir.Name(), "status")
			statusBytes, err := os.ReadFile(statusPath)
			if err != nil {
				continue
			}

			status := string(statusBytes)
			for _, line := range strings.Split(status, "\n") {
				if strings.HasPrefix(line, "PPid:") {
					fields := strings.Fields(line)
					if len(fields) == 2 {
						ppid, err := strconv.Atoi(fields[1])
						if err != nil {
							break
						}
						if ppid == pid {
							childPIDs = append(childPIDs, childPid)
							findDescendants(childPid)
						}
					}
					break
				}
			}
		}
	}

	// Start the recursion with the initial parent PID.
	findDescendants(parentPID)

	return childPIDs, getProcessGroupID, nil
}
