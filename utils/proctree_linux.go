//go:build linux

package utils

import (
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// The process tree InterruptProcessTree signals, read from /proc.

// findChildPIDs returns every descendant of parentPID, and how to read each
// one's process group -- both from one snapshot of the process table, read
// from /proc once (readProcTable) and taken by procTable.tree.
//
// All of it comes before InterruptProcessTree's first signal. It used to read
// every process's status file once for each process it found in the tree,
// and the tree's again for their groups: for an app keploy runs as
// `sh -c "nyc npm start"` (sh, nyc, npm, sh, node), five reads of every
// process on the host. On a host short of memory each read of a /proc file
// waits on reclaim, and a replay held to 120 MB sent its app the SIGINT
// 15.7 s after the test set was cancelled, all of it spent in that walk.
func findChildPIDs(parentPID int) ([]int, func(pid int) (int, error), error) {
	return childPIDsIn(os.DirFS("/proc"), parentPID)
}

// childPIDsIn is findChildPIDs over proc, a /proc.
func childPIDsIn(proc fs.FS, parentPID int) ([]int, func(pid int) (int, error), error) {
	table, err := readProcTable(proc)
	if err != nil {
		return nil, nil, err
	}
	children, groupOf := table.tree(parentPID)
	return children, groupOf, nil
}

// readProcTable reads the process table from proc, a /proc: one read of each
// process's stat file, which holds both its parent and its process group,
// parsed by ParseProcStat.
//
// stat, not status: its pgrp field is the group as seen from this /proc's pid
// namespace, the one kill(2) takes, for a process in nested namespaces too,
// where status's NSpgid line carries one group per namespace. A process that
// exits between the listing and its read, or whose stat cannot be parsed, is
// not in the table.
func readProcTable(proc fs.FS) (procTable, error) {
	entries, err := fs.ReadDir(proc, ".")
	if err != nil {
		return procTable{}, err
	}
	table := newProcTable(len(entries))
	// Every stat is read into the same buffer (statBufSize holds any of them).
	buf := make([]byte, statBufSize)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		line, err := readStat(proc, entry.Name(), buf)
		if err != nil {
			continue
		}
		stat, ok := ParseProcStat(line)
		if !ok {
			continue
		}
		table.add(pid, stat.PPID, stat.PGRP)
	}
	return table, nil
}

// exitedUnreaped reports whether pid is a zombie: it has exited, and waits
// for its parent to reap it (state Z in its stat, and no thread of it runs).
// kill(2) of a zombie succeeds, as of a process that runs, so the interrupt
// asks this too: a group leader that is a zombie has exited, and costs the
// interrupt no grace wait and no SIGKILL. A stat that cannot be read or
// parsed is no zombie. Nor is a process whose main thread exited while
// another thread of it runs on: its stat reads Z, but it holds its files (a
// SIGKILLed Go server's port, until its last thread has exited), and a
// stop that took it for exited returned with its port still bound.
func exitedUnreaped(pid int) bool {
	return exitedUnreapedIn(os.DirFS("/proc"), pid) && !zombieRuns(pid)
}

// zombieRuns reports whether process pid, whose main thread is a zombie,
// still runs: a thread of it runs (RunningThread), or that cannot be read.
func zombieRuns(pid int) bool {
	_, runs, err := RunningThread("/proc", pid)
	return err != nil || runs
}

// exitedUnreapedIn is exitedUnreaped over proc, a /proc.
func exitedUnreapedIn(proc fs.FS, pid int) bool {
	line, err := readStat(proc, strconv.Itoa(pid), make([]byte, statBufSize))
	if err != nil {
		return false
	}
	stat, ok := ParseProcStat(line)
	return ok && stat.State == 'Z'
}

// groupRunsExcept reports whether a process of process group pgid runs, as
// /proc shows it now: one in that group whose stat says it is neither a
// zombie nor dead, and that is not one of except (its pid and start time). A
// /proc that cannot be listed is an error.
func groupRunsExcept(pgid int, except []Reached) (bool, error) {
	return groupRunsIn(os.DirFS("/proc"), pgid, except, zombieRuns, leftToDaemonStop)
}

// groupRunsIn is groupRunsExcept over proc, a /proc, with zombieRuns telling
// whether a process whose stat reads Z runs on in a thread of it, and
// leftToDaemons whether keploy's stop of the daemons ends a process
// (LeaveToDaemonStop; nil: none). A process of the group that runs is not
// counted when it, or a process it runs under, as proc's parents show, is
// one of except, or, but the group's leader, one leftToDaemons ends: what a
// process the caller or the stop of the daemons ends runs, they end with it
// (stopHanded and stopAdopted read each one's children), and a shell left so
// that starts a `sleep` each second would keep the group running for good.
func groupRunsIn(proc fs.FS, pgid int, except []Reached, zombieRuns func(pid int) bool, leftToDaemons func(pid int, start uint64) bool) (bool, error) {
	if pgid <= 1 {
		return false, nil
	}
	entries, err := fs.ReadDir(proc, ".")
	if err != nil {
		return false, err
	}
	buf := make([]byte, statBufSize)
	table := make(map[int]ProcStat, len(entries))
	var group []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		line, err := readStat(proc, entry.Name(), buf)
		if err != nil {
			continue
		}
		stat, ok := ParseProcStat(line)
		if !ok {
			continue
		}
		table[pid] = stat
		if stat.PGRP == pgid && stat.State != 'X' {
			group = append(group, pid)
		}
	}
	// left reports whether process pid, or one it runs under, is one the
	// caller or the stop of the daemons ends. The walk ends at a parent proc
	// does not show, at init, and after as many steps as proc has processes
	// (a loop of parents, as a /proc read while it changes can show).
	left := func(pid int) bool {
		for p, n := pid, 0; n <= len(table); n++ {
			st, ok := table[p]
			if !ok {
				return false
			}
			if slices.Contains(except, Reached{PID: p, Start: st.StartTime, Ignores: true}) {
				return true
			}
			if p != pgid && leftToDaemons != nil && leftToDaemons(p, st.StartTime) {
				return true
			}
			if st.PPID <= 1 {
				return false
			}
			p = st.PPID
		}
		return false
	}
	for _, pid := range group {
		if stat := table[pid]; stat.State == 'Z' && !zombieRuns(pid) {
			continue
		}
		if !left(pid) {
			return true, nil
		}
	}
	return false, nil
}

// groupExited reads no group on Linux: kill(2) of a group of zombies only
// succeeds there, so a signal Linux refuses a group is never one of a group
// that has exited.
func groupExited(int) bool { return false }

// groupReached is each process of process group pgid as /proc shows it now,
// and whether it ignores sig: what a signal sent to the group next reaches.
// A process that exits as /proc is read, a zombie, and a process whose
// dispositions cannot be read are not among them.
func groupReached(pgid int, sig syscall.Signal) []Reached {
	return groupReachedIn(os.DirFS("/proc"), pgid, sig)
}

// groupReachedIn is groupReached over proc, a /proc.
func groupReachedIn(proc fs.FS, pgid int, sig syscall.Signal) []Reached {
	entries, err := fs.ReadDir(proc, ".")
	if err != nil {
		return nil
	}
	buf := make([]byte, statBufSize)
	var reached []Reached
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		line, err := readStat(proc, entry.Name(), buf)
		if err != nil {
			continue
		}
		stat, ok := ParseProcStat(line)
		if !ok || stat.PGRP != pgid || stat.State == 'Z' {
			continue
		}
		ignores, ok := ignoresSignal(proc, entry.Name(), sig)
		if !ok {
			continue
		}
		reached = append(reached, Reached{PID: pid, Start: stat.StartTime, Ignores: ignores})
	}
	return reached
}

// ignoresSignal reads whether process pid of proc, a /proc, ignores sig: the
// SigIgn mask of its status file (proc_pid_status(5)), one bit for each
// signal, sig's at bit sig-1.
func ignoresSignal(proc fs.FS, pid string, sig syscall.Signal) (ignores, ok bool) {
	status, err := fs.ReadFile(proc, pid+"/status")
	if err != nil {
		return false, false
	}
	for _, line := range strings.Split(string(status), "\n") {
		mask, found := strings.CutPrefix(line, "SigIgn:")
		if !found {
			continue
		}
		bits, err := strconv.ParseUint(strings.TrimSpace(mask), 16, 64)
		if err != nil || sig < 1 || sig > 64 {
			return false, false
		}
		return bits&(1<<(uint(sig)-1)) != 0, true
	}
	return false, false
}
