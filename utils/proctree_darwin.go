//go:build darwin

package utils

import (
	"errors"
	"sync"
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

func init() {
	signalGroup = signalGroupByMembers
	signalJoined = signalJoinedMembers
}

// memberSignalWait is how long the stop waits on macOS for one kill(2) before
// it goes on without it (killWithin).
const memberSignalWait = time.Second

// killWaiter tracks the in-flight kill(2) goroutine for one target pid (or
// -pgid) and the strongest follow-up signal asked for while it stays blocked
// in XNU (pending == 0 when none is queued).
type killWaiter struct {
	inFlight syscall.Signal
	pending  syscall.Signal
}

// signalRank orders stop signals by strength so a stronger signal asked for
// while an earlier kill(2) is still waiting in XNU (such as SIGKILL after
// SIGINT) is deferred and sent once the first returns, while an exact repeat
// of the same or weaker signal is not resent.
func signalRank(sig syscall.Signal) int {
	switch sig {
	case syscall.SIGKILL:
		return 3
	case syscall.SIGTERM:
		return 2
	case syscall.SIGINT:
		return 1
	default:
		return 0
	}
}

var (
	// killMember is the stop's kill(2) of one process of a group on macOS
	// (killWithin); a test answers it.
	killMember = killWithin

	// killSyscall is the underlying kill(2) call killWithin runs; a test
	// replaces it to hold calls in flight without a real held posix_spawn.
	killSyscall = syscall.Kill

	// killWaiters tracks targets (pid or -pgid) that currently have an
	// in-flight kill(2) goroutine in the kernel, so at most one goroutine
	// waits per target while unrelated targets are never blocked.
	killWaitersMu sync.Mutex
	killWaiters   = map[int]*killWaiter{}
)

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
// exit short. A process that joins the group after its list was read is sent
// sig once by the group's wait instead (signalJoinedMembers, from sent: each
// pid this signal went to), and, if it runs on, killed after the grace. A
// group whose list cannot be read is signalled as a group, as before, and
// sent is nil: what joins it later is not told from what that signal reached.
func signalGroupByMembers(logger *zap.Logger, pid int, sig syscall.Signal) (sent map[int]bool, err error) {
	pgid := -pid
	members, err := groupMembers(pgid)
	if err != nil {
		logger.Debug("keploy could not list the process group; it signals the group", zap.Int("pgid", pgid), zap.Error(err))
		return nil, SendSignal(logger, pid, sig)
	}
	sent = make(map[int]bool, len(members))
	reached := 0
	var refused error
	for _, member := range members {
		sent[member] = true
		switch err := killMember(logger, member, sig, memberSignalWait); {
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
		switch err := killWithin(logger, pid, sig, memberSignalWait); {
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
		return nil, refused
	}
	if len(members) == 0 {
		logger.Debug("no process of the process group runs; it has exited", zap.Int("pgid", pgid), zap.String("signal", sig.String()))
	}
	return sent, nil
}

// signalJoinedMembers is the group wait's signal on macOS to what joined
// process group pgid after the stop's signal sig went to each of its
// processes by its pid (signalGroupByMembers, which named each in sent): each
// process of the group that runs and that sent does not name is sent sig, by
// its pid, once, and named. So a process forked into the group after its list
// was read is asked to stop as the others were, with one signal, and is
// killed only once the grace has passed. A list that cannot be read signals
// none: the wait reads it again. With no sent (the stop's signal went to the
// group as one, its list unread) it signals none: what that signal reached
// is not known, and a second signal would reach each again.
func signalJoinedMembers(logger *zap.Logger, pgid int, sig syscall.Signal, sent map[int]bool) {
	if sent == nil {
		return
	}
	members, err := groupMembers(pgid)
	if err != nil {
		return
	}
	for _, member := range members {
		if sent[member] {
			continue
		}
		sent[member] = true
		err := killMember(logger, member, sig, memberSignalWait)
		logger.Debug("signalled a process that joined the process group after its signal",
			zap.Int("pgid", pgid), zap.Int("pid", member), zap.String("signal", sig.String()), zap.Error(err))
	}
}

// killWithin is kill(2) of pid with sig, waited for wait at most.
//
// XNU can block kill(2) uninterruptibly while a target process (or a member of
// a target process group) is held inside posix_spawn. Running kill(2) in its
// own goroutine lets the stop move on after wait with errKillWaits, while that
// goroutine -- and the OS thread pinned to the blocked syscall -- stays blocked
// in the kernel until the held spawn ends.
//
// To keep repeated stops in a long-lived agent from piling up blocked
// goroutines for the same target, killWaiters allows at most one in-flight
// kill(2) goroutine per target pid. While that kill is still waiting in XNU:
//   - a stronger signal for the same pid (e.g. SIGKILL after SIGINT) is saved
//     on the waiter and sent as soon as the blocked kill returns;
//   - an exact repeat of the same or weaker signal is skipped without spawning
//     another goroutine;
//   - kills to any other pid -- including the by-pid kill to the process that
//     made a held spawn and the group SIGKILL on -pgid -- always go out and
//     are never refused.
//
// Each goroutine clears pid from killWaiters before delivering its result once
// no stronger follow-up signal remains pending.
func killWithin(logger *zap.Logger, pid int, sig syscall.Signal, wait time.Duration) error {
	killWaitersMu.Lock()
	if w, busy := killWaiters[pid]; busy {
		inFlight := w.inFlight
		deferred := false
		if signalRank(sig) > signalRank(w.inFlight) {
			if w.pending == 0 || signalRank(sig) > signalRank(w.pending) {
				w.pending = sig
				deferred = true
			}
		}
		killWaitersMu.Unlock()
		if logger != nil {
			if deferred {
				logger.Warn("a kill(2) for this target is still waiting in the kernel; deferring stronger signal until it returns",
					zap.Int("pid", pid), zap.String("signal", sig.String()), zap.String("in_flight", inFlight.String()))
			} else {
				logger.Warn("a kill(2) for this target is already waiting in the kernel; skipping repeat signal",
					zap.Int("pid", pid), zap.String("signal", sig.String()), zap.String("in_flight", inFlight.String()))
			}
		}
		return errKillWaits
	}
	w := &killWaiter{inFlight: sig}
	killWaiters[pid] = w
	killWaitersMu.Unlock()

	done := make(chan error, 1)
	go func() {
		// Remove pid from killWaiters before sending on done (once no stronger
		// follow-up signal is pending) so a normal return never races with a
		// follow-up signal (such as SIGKILL after SIGINT) to the same target.
		defer func() {
			if r := recover(); r != nil {
				killWaitersMu.Lock()
				if killWaiters[pid] == w {
					delete(killWaiters, pid)
				}
				killWaitersMu.Unlock()
				panic(r)
			}
		}()
		firstDone := false
		cur := sig
		for {
			err := killSyscall(pid, cur)
			killWaitersMu.Lock()
			next := w.pending
			if next != 0 {
				w.inFlight = next
				w.pending = 0
				killWaitersMu.Unlock()
				if !firstDone {
					firstDone = true
					done <- err
				}
				cur = next
				continue
			}
			delete(killWaiters, pid)
			killWaitersMu.Unlock()
			if !firstDone {
				done <- err
			}
			return
		}
	}()
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
