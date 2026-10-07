package utils

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
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
	start, err := strconv.ParseUint(string(fields[19]), 10, 64)
	if err != nil {
		return ProcStat{}, false
	}
	return ProcStat{State: fields[0][0], PPID: int(ppid), PGRP: int(pgrp), StartTime: start}, true
}
