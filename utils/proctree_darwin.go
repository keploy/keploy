//go:build darwin

package utils

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// The process tree InterruptProcessTree signals, read from the kernel's
// process table. macOS has no /proc. Walking it anyway found no descendants
// at all, so a descendant that had put itself in a process group of its own
// was never signalled and outlived the run, and looking up the runner's group
// failed on every Ctrl+C with "failed to find unique process groups: open
// /proc/<pid>/status" -- an ERROR in an interrupt that had otherwise gone as
// it should, which an editor driving keploy shows in red.

// findChildPIDs returns every descendant of parentPID, and how to read each
// one's process group -- both from one snapshot of the process table (sysctl
// kern.proc.all).
//
// The groups come from that snapshot, not from getpgid(2). getpgid refuses a
// zombie with ESRCH -- a child its parent has not waited for yet, which a
// test that starts a subprocess and never waits for it leaves behind -- and
// cannot find a process that exited after the snapshot was taken. Either
// failed the lookup of every group in the tree, so such an interrupt logged
// "failed to find unique process groups: no such process" at ERROR, and fell
// back to signalling each pid as if it led a group. The table holds every
// process's group, a zombie's included.
func findChildPIDs(parentPID int) ([]int, func(pid int) (int, error), error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, nil, err
	}
	parents := make(map[int]int, len(procs))
	groups := make(map[int]int, len(procs))
	for _, p := range procs {
		parents[int(p.Proc.P_pid)] = int(p.Eproc.Ppid)
		groups[int(p.Proc.P_pid)] = int(p.Eproc.Pgid)
	}
	groupOf := func(pid int) (int, error) {
		pgid, ok := groups[pid]
		if !ok {
			return 0, fmt.Errorf("process %d is not in the process table", pid)
		}
		return pgid, nil
	}
	return descendantsOf(parentPID, parents), groupOf, nil
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
