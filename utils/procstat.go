package utils

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"strconv"
)

// ProcStat is what keploy reads of a process from its /proc/<pid>/stat line
// (proc_pid_stat(5)), as ParseProcStat, the one parser of that line, reads it.
type ProcStat struct {
	// State is field 3: R, S, D, Z, T, I and so on.
	State byte
	// PPID is field 4, the parent's pid.
	PPID int
	// PGRP is field 5, the process group, as the pid namespace of the /proc
	// it was read from numbers it: the one kill(2) takes from there, for a
	// process in a nested pid namespace too.
	PGRP int
	// Flags is field 9, the task's PF_* flags (include/linux/sched.h):
	// PF_EXITING (0x4) is set from the moment it begins to exit (do_exit).
	Flags uint32
	// StartTime is field 22, when the process started, in clock ticks after
	// the system booted.
	StartTime uint64
}

// procStatFields is how many fields of a stat line follow the command name
// up to StartTime, the last one ProcStat holds: fields 3 to 22.
const procStatFields = 20

// statBufSize holds any /proc/<pid>/stat line. Beside the command name, a
// line on Linux 6.12 is a state letter and fifty numbers of at most 20 digits
// each. A user process's name is at most 15 bytes, and a kernel thread's
// somewhat longer, a few dozen bytes ("kworker/u32:19-ext4-rsv-conversion" is
// 34). So even with every number at its widest a line stays under 1.2 KiB,
// and in practice it is a few hundred bytes (the longest on a 6.12 host was
// 363). 4096 bytes holds any of them.
const statBufSize = 4096

// ReadProcStat reads and parses /proc/<pid>/stat. It is false when the line
// cannot be read (the process has exited, or there is no /proc) or parsed.
func ReadProcStat(pid int) (ProcStat, bool) {
	return ReadProcStatIn("/proc", pid)
}

// ReadProcStatIn is ReadProcStat of the /proc at procRoot.
func ReadProcStatIn(procRoot string, pid int) (ProcStat, bool) {
	line, err := readStat(os.DirFS(procRoot), strconv.Itoa(pid), make([]byte, statBufSize))
	if err != nil {
		return ProcStat{}, false
	}
	return ParseProcStat(line)
}

// readStat reads <pid>/stat from proc, a /proc, into buf and returns what it
// read.
func readStat(proc fs.FS, pid string, buf []byte) ([]byte, error) {
	f, err := proc.Open(pid + "/stat")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	n := 0
	for n < len(buf) {
		m, err := f.Read(buf[n:])
		n += m
		if errors.Is(err, io.EOF) {
			return buf[:n], nil
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("stat does not fit its buffer")
}

// ReleasedTask reports whether a stat line is a task's being released: its
// parent and group, the two fields after its state, read 0 and -1. The kernel
// prints those (and a session of -1) where it finds the task's signal
// handlers released already, and it reads the state before it looks for them
// (do_task_stat, fs/proc/array.c): a task released between the two reads as
// R, S or D with them, its PF_EXITING set, so its state says nothing here. No
// line of a task that is there has a group of -1, the one field of such a
// line ParseProcStat refuses; the parent's 0 is asked too, so a line cut
// short or of another shape is not taken for one. ParseProcStat goes on
// refusing it: taken for a group, a -1 would make a kill of the group
// (kill(-pgid)) one of process 1.
func ReleasedTask(line []byte) bool {
	end := bytes.LastIndexByte(line, ')')
	if end < 0 {
		return false
	}
	f := bytes.Fields(line[end+1:])
	return len(f) >= 3 && string(f[1]) == "0" && string(f[2]) == "-1"
}

// ParseProcStat parses a /proc/<pid>/stat line: "pid (comm) state ppid pgrp
// session ...". The command name can itself hold spaces and parentheses, so
// the fields are counted from the line's last ')'. A line parses only whole up
// to StartTime, with a one-character state and numbers where ProcStat has
// them, none negative, and the two pids within the kernel's 32-bit pid_t: the
// kernel writes every line in full, so anything else is a torn read or not a
// stat line. It allocates nothing.
func ParseProcStat(line []byte) (ProcStat, bool) {
	end := bytes.LastIndexByte(line, ')')
	if end < 0 {
		return ProcStat{}, false
	}
	var fields [procStatFields][]byte
	rest := line[end+1:]
	for i := range fields {
		rest = bytes.TrimLeft(rest, " ")
		n := bytes.IndexAny(rest, " \n")
		if n < 0 {
			n = len(rest)
		}
		if n == 0 {
			return ProcStat{}, false
		}
		fields[i], rest = rest[:n], rest[n:]
	}
	if len(fields[0]) != 1 {
		return ProcStat{}, false
	}
	ppid, err := strconv.ParseInt(string(fields[1]), 10, 32)
	if err != nil || ppid < 0 {
		return ProcStat{}, false
	}
	pgrp, err := strconv.ParseInt(string(fields[2]), 10, 32)
	if err != nil || pgrp < 0 {
		return ProcStat{}, false
	}
	flags, err := strconv.ParseUint(string(fields[6]), 10, 32)
	if err != nil {
		return ProcStat{}, false
	}
	start, err := strconv.ParseUint(string(fields[19]), 10, 64)
	if err != nil {
		return ProcStat{}, false
	}
	return ProcStat{State: fields[0][0], PPID: int(ppid), PGRP: int(pgrp), Flags: uint32(flags), StartTime: start}, true
}
