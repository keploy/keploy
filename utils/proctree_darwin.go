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

// procRecord reads a process's parent and group, a zombie's too
// (procsock.InfoOrZombie), and groupPIDs lists a process group's processes
// (procsock.GroupPIDs).
var (
	procRecord = procsock.InfoOrZombie
	groupPIDs  = procsock.GroupPIDs
)

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
// signalling each pid as if it led a group.
//
// A zombie is in the table, with its parent and group: it has exited, but a
// group it led can still hold processes that run (its children, which the
// kernel gave to launchd, stay in it), and the interrupt reaches them through
// that group. A process that has gone is not in the table.
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
// not exited. A zombie has exited, and a process that has gone has no record;
// a list keploy cannot read says none runs. A native application's command
// leads a process group of its own (ExecuteCommand), so once the command has
// exited, a process it started that is still in its group is the application
// running on.
func GroupRunsOn(pgid int) bool {
	runs, err := groupRuns(pgid)
	return err == nil && runs
}

// groupRuns is GroupRunsOn with the error of a list that cannot be read.
func groupRuns(pgid int) (bool, error) {
	if pgid <= 1 {
		return false, nil
	}
	pids, err := groupPIDs(pgid)
	if err != nil {
		return false, err
	}
	for _, pid := range pids {
		if p, err := procRecord(pid); err == nil && !p.Zombie {
			return true, nil
		}
	}
	return false, nil
}

// groupExited reports whether process group pgid holds no process that runs,
// as its list reads now: a group of zombies only, or none. kill(2) of such a
// group answers EPERM on macOS, as it does a group whose processes keploy
// may not signal; a list keploy cannot read is not read as exited.
func groupExited(pgid int) bool {
	runs, err := groupRuns(pgid)
	return err == nil && !runs
}

// exitedUnreaped reports whether pid is a zombie: it has exited, and waits
// for its parent to reap it. kill(2) of a zombie succeeds, as of a process
// that runs.
func exitedUnreaped(pid int) bool {
	p, err := procRecord(pid)
	return err == nil && p.Zombie
}
