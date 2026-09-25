//go:build darwin

package utils

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// The process tree InterruptProcessTree signals, read from the kernel's
// process table. macOS has no /proc. Walking it anyway found no descendants
// at all, so a descendant that had put itself in a process group of its own
// was never signalled and outlived the run, and looking up the runner's group
// failed on every Ctrl+C with "failed to find unique process groups: open
// /proc/<pid>/status" -- an ERROR in an interrupt that had otherwise gone as
// it should, which an editor driving keploy shows in red.

// getProcessGroupID returns pid's process group.
func getProcessGroupID(pid int) (int, error) {
	return syscall.Getpgid(pid)
}

// findChildPIDs returns every descendant of parentPID, from one snapshot of
// the process table (sysctl kern.proc.all).
func findChildPIDs(parentPID int) ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	parents := make(map[int]int, len(procs))
	for _, p := range procs {
		parents[int(p.Proc.P_pid)] = int(p.Eproc.Ppid)
	}
	return descendantsOf(parentPID, parents), nil
}

// descendantsOf returns every descendant of root in a pid -> parent pid table,
// nearest first. The kernel's table is a tree except at its very top
// (kernel_task is its own parent), and each pid is still visited only once
// however the table loops.
func descendantsOf(root int, parents map[int]int) []int {
	children := make(map[int][]int, len(parents))
	for pid, ppid := range parents {
		children[ppid] = append(children[ppid], pid)
	}
	var descendants []int
	seen := map[int]bool{root: true}
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		for _, child := range children[queue[0]] {
			if seen[child] {
				continue
			}
			seen[child] = true
			descendants = append(descendants, child)
			queue = append(queue, child)
		}
	}
	return descendants
}
