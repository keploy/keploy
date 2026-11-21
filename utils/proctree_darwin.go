//go:build darwin

package utils

import (
	"errors"
	"syscall"
	"time"

	"go.uber.org/zap"

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

// groupRunsExcept is groupRuns: macOS reads no snapshot of what a signal
// reached (groupReached), so no process is handed over (except is empty).
func groupRunsExcept(pgid int, _ []Reached) (bool, error) { return groupRuns(pgid) }

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

func init() { signalGroup = signalGroupByMembers }

// memberSignalWait is how long the stop waits on macOS for one kill(2) before
// it goes on without it (killWithin).
const memberSignalWait = time.Second

// errKillWaits is killWithin's answer for a kill(2) that had not returned.
var errKillWaits = errors.New("the signal waits in the kernel for a posix_spawn that is held")

// signalGroupByMembers is the tree interrupt's signal to process group -pid on
// macOS: each process of the group that runs is sent sig by its pid, parents
// before their children (groupMembers), and, for SIGKILL only, the group
// after them.
//
// A signal sent to a process group waits in XNU for each member that is
// inside a posix_spawn whose file actions are held (an open of a FIFO no one
// reads), until that spawn ends: keploy's stop of such an app sent its group
// SIGINT and hung for as long as the spawn was held. So does a signal sent to
// the held spawn's child by its pid. A signal sent by pid to the process that
// made the spawn returns at once, and ends the held spawn whether that
// process takes the signal, catches it (the spawn fails with EINTR) or is
// killed; its child, next in the list, has gone by then. A kill that still
// waits (the spawn of a process that ignores sig) is left to finish when the
// spawn ends, after memberSignalWait: the stop's SIGKILL, sent by pid to
// that process, ends it.
//
// Only SIGKILL goes to the group as well, to reach a process that joined it
// after its list was read. Another signal would reach each process twice, and
// an app that takes a second SIGINT as "stop now" would cut its own graceful
// exit short; a process that joined the group late and runs on is waited for
// (groupRunning) and killed after the grace. A group whose list cannot be
// read is signalled as a group, as before.
func signalGroupByMembers(logger *zap.Logger, pid int, sig syscall.Signal) error {
	pgid := -pid
	members, err := groupMembers(pgid)
	if err != nil {
		logger.Debug("keploy could not list the process group; it signals the group", zap.Int("pgid", pgid), zap.Error(err))
		return SendSignal(logger, pid, sig)
	}
	reached := 0
	var refused error
	for _, member := range members {
		switch err := killWithin(member, sig, memberSignalWait); {
		case err == nil:
			reached++
		case errors.Is(err, errKillWaits):
			reached++
			logger.Debug("a signal waits in the kernel for a posix_spawn that is held; the stop goes on without it",
				zap.Int("pid", member), zap.String("signal", sig.String()))
		case errors.Is(err, syscall.ESRCH):
			// It exited after the list was read.
		default:
			if refused == nil {
				refused = err
			}
		}
	}
	if sig == syscall.SIGKILL {
		switch err := killWithin(pid, sig, memberSignalWait); {
		case err == nil, errors.Is(err, errKillWaits), errors.Is(err, syscall.ESRCH):
		case errors.Is(err, syscall.EPERM) && groupExited(pgid):
			// Zombies only are left.
		default:
			if reached == 0 && refused == nil {
				refused = err
			}
		}
	}
	if reached == 0 && refused != nil {
		logger.Error("failed to send signal to process", zap.Int("pid", pid), zap.Error(refused))
		return refused
	}
	if len(members) == 0 {
		logger.Debug("no process of the process group runs; it has exited", zap.Int("pgid", pgid), zap.String("signal", sig.String()))
	}
	return nil
}

// killWithin is kill(2) of pid with sig, waited for wait at most: a kill that
// has not returned by then answers errKillWaits, and finishes on its own.
func killWithin(pid int, sig syscall.Signal, wait time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- syscall.Kill(pid, sig) }()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return errKillWaits
	}
}

// groupMembers lists the processes of process group pgid that run (none is
// a zombie), parents before their children (parentsFirst).
func groupMembers(pgid int) ([]int, error) {
	pids, err := groupPIDs(pgid)
	if err != nil {
		return nil, err
	}
	parents := make(map[int]int, len(pids))
	for _, pid := range pids {
		if p, err := procRecord(pid); err == nil && !p.Zombie {
			parents[p.PID] = p.PPID
		}
	}
	return parentsFirst(parents), nil
}

// parentsFirst orders the processes of parents (pid -> parent pid) so that
// each comes after its parent, when its parent is among them.
func parentsFirst(parents map[int]int) []int {
	const top = -1
	tree := make(map[int]int, len(parents))
	for pid, ppid := range parents {
		if _, ok := parents[ppid]; ok && ppid != pid {
			tree[pid] = ppid
		} else {
			tree[pid] = top
		}
	}
	ordered := descendantsOf(top, tree)
	if len(ordered) < len(parents) {
		// A loop among them (none in a real table) reaches none of its
		// processes from the top: they come last.
		seen := make(map[int]bool, len(ordered))
		for _, pid := range ordered {
			seen[pid] = true
		}
		for pid := range parents {
			if !seen[pid] {
				ordered = append(ordered, pid)
			}
		}
	}
	return ordered
}
