//go:build linux

package utils

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"syscall"
)

// RunningThread finds a thread of process pid, read from the /proc at root,
// that has not exited. A process whose main thread (its thread-group leader)
// exited while another thread runs on ("Zl" in ps) runs, yet its stat line
// says only its main thread's state, Z: its other threads say whether it
// does. tid is such a thread; ok is false where none runs (every thread of it
// has exited, a zombie, or the process is gone). A thread that exits is
// released at once, unless it is the main thread, so what the listing still
// holds of one is a zombie or a dead task (X), never a running one. An error
// is a read that failed, of its threads or of a thread's stat, but for a
// process or thread gone meanwhile.
func RunningThread(root string, pid int) (tid int, ok bool, err error) {
	dir := root + "/" + strconv.Itoa(pid) + "/task"
	tasks, err := os.ReadDir(dir)
	if procGone(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("list process %d's threads: %w", pid, err)
	}
	proc, buf := os.DirFS(dir), make([]byte, statBufSize)
	for _, t := range tasks {
		id, err := strconv.Atoi(t.Name())
		if err != nil {
			continue
		}
		line, err := readStat(proc, t.Name(), buf)
		if procGone(err) {
			continue
		}
		if err != nil {
			return 0, false, fmt.Errorf("read thread %d of process %d: %w", id, pid, err)
		}
		state, parsed := statState(line)
		if !parsed {
			return 0, false, fmt.Errorf("thread %d of process %d: its stat line has no state: %q", id, pid, line)
		}
		if state != 'Z' && state != 'X' {
			return id, true, nil
		}
	}
	return 0, false, nil
}

// Exited reports whether process pid, whose stat line read st, has exited:
// every thread of it, not its main thread alone (RunningThread). A read of
// its threads that fails is taken for one that runs, so keploy goes on
// treating as running what it cannot tell has ended.
func Exited(pid int, st ProcStat) bool {
	if st.State != 'Z' {
		return false
	}
	_, runs, err := runningThread(pid)
	return err == nil && !runs
}

// runningThread is RunningThread of a process in this host's /proc, as
// Exited and MarkExit read it: a test makes the read fail.
var runningThread = func(pid int) (tid int, ok bool, err error) {
	return RunningThread("/proc", pid)
}

// statState is the state letter of a stat line, the field after the command
// name's last ')': all a thread's line is read for. A task being released
// reads Z or X with its parent and group gone (0 and -1), which ParseProcStat
// refuses; its state is still read.
func statState(line []byte) (byte, bool) {
	end := bytes.LastIndexByte(line, ')')
	if end < 0 {
		return 0, false
	}
	rest := bytes.TrimLeft(line[end+1:], " ")
	if len(rest) == 0 || rest[0] == '\n' {
		return 0, false
	}
	return rest[0], true
}

// procGone reports whether err is a read of a process or thread that is no
// longer there: exited and released before, or while, it was read.
func procGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}
