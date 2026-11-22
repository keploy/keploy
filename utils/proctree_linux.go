//go:build linux

package utils

import (
	"io/fs"
	"os"
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
