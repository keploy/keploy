//go:build darwin

package utils

import (
	"syscall"

	"go.keploy.io/server/v3/pkg/agent/procsock"
)

// The process tree InterruptProcessTree signals, read from the kernel's
// process table. macOS has no /proc. Walking it anyway found no descendants
// at all, so a descendant that had put itself in a process group of its own
// was never signalled and outlived the run, and looking up the runner's group
// failed on every Ctrl+C with "failed to find unique process groups: open
// /proc/<pid>/status" -- an ERROR in an interrupt that had otherwise gone as
// it should, which an editor driving keploy shows in red.

// procRecord reads a process's parent, group and state (procsock.Info).
var procRecord = procsock.Info

// findChildPIDs returns every descendant of parentPID, and how to read each
// one's process group -- both from one snapshot of the process table, taken
// by procTable.tree. The table is the processes proc_info lists, each with
// the parent and group its short record holds (procsock), never the kern.proc
// sysctls: those wait for any process on the machine that is held in an exec
// or a posix_spawn, and an interrupt waited with them.
//
// The groups come from that snapshot, not from getpgid(2), which cannot find
// a process that exited after the snapshot was taken: that failed the lookup
// of every group in the tree, so such an interrupt logged "failed to find
// unique process groups: no such process" at ERROR, and fell back to
// signalling each pid as if it led a group. A process whose record cannot be
// read, one that has gone or is exiting, is not in the table, so not in the
// tree: no signal reaches it any more.
func findChildPIDs(parentPID int) ([]int, func(pid int) (int, error), error) {
	pids, err := procsock.PIDs()
	if err != nil {
		return nil, nil, err
	}
	table := newProcTable(len(pids))
	for _, pid := range pids {
		if p, err := procRecord(pid); err == nil {
			table.add(p.PID, p.PPID, p.PGID)
		}
	}
	children, groupOf := table.tree(parentPID)
	return children, groupOf, nil
}

// groupReached reads no process on macOS, which has no /proc:
// InterruptProcessTreeReached says none there.
func groupReached(int, syscall.Signal) []Reached { return nil }

// GroupRunsOn reports whether a process of process group pgid runs: one
// proc_info lists in that group (procsock.GroupPIDs) whose record says it has
// not exited. A zombie has exited, and so has a process whose record cannot
// be read (gone, or exiting); a list keploy cannot read says none runs. A native application's command leads a process group of its own
// (ExecuteCommand), so once the command has exited, a process it started that
// is still in its group is the application running on.
func GroupRunsOn(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	pids, err := procsock.GroupPIDs(pgid)
	if err != nil {
		return false
	}
	for _, pid := range pids {
		if p, err := procRecord(pid); err == nil && !p.Zombie {
			return true
		}
	}
	return false
}
