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
// released at once, unless it is the main thread (or one a tracer has yet to
// wait for), so what the listing still holds of one is a zombie or a dead
// task (X), never a running one. An error is a read that failed, or a line
// read that does not parse, of its threads, of a thread's stat or of the
// process's, but for a process or thread gone meanwhile.
//
// The listing of a process's threads is no proof that none runs: the kernel
// walks the thread list from the thread it last listed, and where that one
// is released meanwhile the walk ends there, or picks up again by a count of
// the threads before it that is off by those released (fs/proc/base.c,
// next_tid and first_tid), so a thread that runs the whole time can be
// missing from a listing taken while others exit. So a listing in
// which none runs is checked against the process's count of its threads
// (num_threads in its stat, every thread not yet released, read in one go;
// 0 where its main thread was released as it was read, which is no count):
// read after the listing, it is taken only where every thread the count
// holds is one the listing found exited, each read again after the count and
// still there, the same thread (its start time unchanged: not, short of the
// kernel giving out every other pid within a clock tick, another it gave its
// tid since). A thread found exited never runs again and none
// is released twice, so then, as the count was read, every thread there had
// exited, and none can start again (only a thread that runs starts another).
// Else the threads are listed again: one missing from a listing was because
// another was released as it was taken, and a process none of whose threads
// runs has only so many to release. One case this cannot tell: a thread left
// out of the listing that runs an exec takes over the main thread's tid and
// start time, so were it to start a thread and exit its own between the
// count and the second read of the main thread, that read would pass for
// the main thread found exited. Counting again after that read narrows this
// but does not close it: the new thread can run an exec in turn.
func RunningThread(root string, pid int) (tid int, ok bool, err error) {
	proc, buf := os.DirFS(root+"/"+strconv.Itoa(pid)+"/task"), make([]byte, statBufSize)
	for {
		tid, runs, exited, gone, err := listThreads(proc, pid, buf)
		if err != nil || runs || gone {
			return tid, runs, err
		}
		count, gone, err := threadCount(root, pid, buf)
		if err != nil || gone {
			return 0, false, err
		}
		if count == 0 {
			// The stat of a main thread released as it was read: the
			// process was reaped (its threads are gone at the next
			// listing), or a thread of it ran an exec and now holds its
			// pid (and is found then).
			continue
		}
		still := 0
		for _, th := range exited {
			now, gone, err := readThread(proc, th.id, pid, buf)
			if err != nil {
				return 0, false, err
			}
			if gone {
				continue
			}
			// Another thread given its tid since is not the one found
			// exited; one that runs is found when the threads are listed
			// again.
			if now.start == th.start && (now.state == 'Z' || now.state == 'X') {
				still++
			}
		}
		if still >= count {
			return 0, false, nil
		}
	}
}

// taskStat is what RunningThread reads of a thread: its state, and its start
// time (field 22 of its stat), which tells it from another given its tid
// later.
type taskStat struct {
	id    int
	state byte
	start string
}

// listThreads lists process pid's threads, from proc, its task directory,
// and reads each one: tid is the first that runs (runs), else exited holds
// every one listed that had exited. gone is a process no longer there.
func listThreads(proc fs.FS, pid int, buf []byte) (tid int, runs bool, exited []taskStat, gone bool, err error) {
	tasks, err := fs.ReadDir(proc, ".")
	if procGone(err) {
		return 0, false, nil, true, nil
	}
	if err != nil {
		return 0, false, nil, false, fmt.Errorf("list process %d's threads: %w", pid, err)
	}
	for _, t := range tasks {
		id, err := strconv.Atoi(t.Name())
		if err != nil {
			continue
		}
		th, gone, err := readThread(proc, id, pid, buf)
		if err != nil {
			return 0, false, nil, false, err
		}
		if gone {
			continue
		}
		if th.state != 'Z' && th.state != 'X' {
			return id, true, nil, false, nil
		}
		exited = append(exited, th)
	}
	return 0, false, exited, false, nil
}

// readThread reads thread id of process pid from its stat in proc, the
// process's task directory; gone is a thread released before, or as, it was
// read.
func readThread(proc fs.FS, id, pid int, buf []byte) (th taskStat, gone bool, err error) {
	line, err := readStat(proc, strconv.Itoa(id), buf)
	if procGone(err) {
		return taskStat{}, true, nil
	}
	if err != nil {
		return taskStat{}, false, fmt.Errorf("read thread %d of process %d: %w", id, pid, err)
	}
	state, parsed := statState(line)
	if !parsed {
		return taskStat{}, false, fmt.Errorf("thread %d of process %d: its stat line has no state: %q", id, pid, line)
	}
	start, parsed := statField(line, 22)
	if !parsed {
		return taskStat{}, false, fmt.Errorf("thread %d of process %d: its stat line has no start time: %q", id, pid, line)
	}
	return taskStat{id: id, state: state, start: string(start)}, false, nil
}

// statField is field n (from 3, the state) of a stat line, counted from the
// command name's last ')'.
func statField(line []byte, n int) ([]byte, bool) {
	end := bytes.LastIndexByte(line, ')')
	if end < 0 || n < 3 {
		return nil, false
	}
	f := bytes.Fields(line[end+1:])
	if len(f) <= n-3 {
		return nil, false
	}
	return f[n-3], true
}

// threadCount reads how many threads process pid has not yet released: field
// 20 of its stat line (num_threads), from the /proc at root. gone is a
// process no longer there.
func threadCount(root string, pid int, buf []byte) (n int, gone bool, err error) {
	line, err := readStat(os.DirFS(root), strconv.Itoa(pid), buf)
	if procGone(err) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read process %d's stat: %w", pid, err)
	}
	if f, ok := statField(line, 20); ok {
		if n, err := strconv.Atoi(string(f)); err == nil && n >= 0 {
			return n, false, nil
		}
	}
	return 0, false, fmt.Errorf("process %d: its stat line has no thread count: %q", pid, line)
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
