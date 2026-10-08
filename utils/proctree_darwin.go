//go:build darwin

package utils

import (
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
// kern.proc.all), taken by procTable.tree.
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
	table := newProcTable(len(procs))
	for _, p := range procs {
		table.add(int(p.Proc.P_pid), int(p.Eproc.Ppid), int(p.Eproc.Pgid))
	}
	children, groupOf := table.tree(parentPID)
	return children, groupOf, nil
}
