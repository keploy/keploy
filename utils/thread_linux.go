//go:build linux

package utils

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// RunningThread finds a thread of process pid, read from the /proc at root,
// that has not exited. A process whose main thread (its thread-group leader)
// has exited while another thread still runs ("Zl" in ps) is running, yet
// its stat line shows only the main thread's state, Z. Its other threads
// tell whether it runs. tid is such a thread. ok is false where none runs:
// every thread has exited, or the process is gone. An error is a read that
// failed or did not parse (of the threads, a thread's stat or the
// process's stat), other than one of a process or thread gone meanwhile, or
// threads that kept changing over threadListings listings. Exited and
// MarkExit take an error for a process that runs; the adopt scan of children
// fails, and stops none of them.
//
// How it decides:
//
//  1. Where a pidfd can be had for the process (groupExitedIn), the kernel
//     answers first. A pidfd polls readable once the whole thread group has
//     exited: the main thread is a zombie and every other thread is
//     released (pidfd_poll, thread_group_exited). That is one atomic read.
//  2. Else the threads are listed. A listing alone proves nothing: the
//     kernel walks the thread list from the thread it listed last, and when
//     that thread is released during the walk, the walk either stops or
//     resumes by a position count that is now off (fs/proc/base.c, next_tid
//     and first_tid). A thread that runs the whole time can be missing.
//  3. So a listing in which no thread runs is checked against the process's
//     thread count (num_threads, every thread not yet released, read under
//     the signal lock; 0 means the main thread was being released and is no
//     count). The count is read after the listing. Each thread the listing
//     found exited is then read again, and must still be there, still
//     exited, with the same start time. If those threads make up the whole
//     count, then every thread there when the count was read had exited.
//     An exited thread never runs again, and only a running thread can
//     start another, so none runs.
//  4. One thread can pass for another in step 3. A thread missing from the
//     listing that runs an exec takes over the main thread's tid and start
//     time (de_thread). If it then started a thread and exited its own
//     between the main thread's two reads, the main thread would look
//     exited both times. /proc shows no mark that an exec changes, and a
//     pidfd for a single thread needs Linux 6.9 (PIDFD_THREAD). So the
//     result of step 3 is taken only when one of these holds:
//     - the pidfd says the group has exited;
//     - no pidfd can be had (see below);
//     - an exited thread other than the main one passed the second read.
//     That last one works because the main thread is read last in the
//     listing and first in the second read, and an exec completes only
//     after every other thread of the process is released: de_thread waits
//     for notify_count, which counts each other thread (a zombie a tracer
//     holds too, but not a vhost worker: userWorker) and drops at its
//     release. A non-main thread still there at its second read was there
//     throughout the main thread's two reads, so no exec came between them.
//     One exception: since Linux 6.4 the release of a vhost worker also
//     drops notify_count, though it was not counted in it, so in a process
//     with one (a virtual machine monitor using vhost) an exec can complete
//     while a counted thread is still there. Where a listed thread is a
//     vhost worker, the rule is not used. A vhost worker released before it
//     is read cannot be seen, and /proc keeps no record of one, so for such
//     a process this rule can still be wrong: when a thread the listing
//     missed runs an exec, a zombie a tracer holds stays through it, and
//     the exec's release of a vhost worker ends its wait early, all between
//     the main thread's two reads. That is a limit of what the kernel shows
//     user space: neither /proc nor a pidfd records an exec or a released
//     thread.
//  5. Otherwise the threads are listed again. A listing misses a thread
//     only when another is released during it, and a process with no
//     running thread has only so many threads to release: so the threads
//     are listed for as long as each new low in the count says threads are
//     being released, and threadListings times more. A process whose
//     threads keep starting and exiting could keep every listing short of
//     its count with no new low; then the read is an error.
//
// With no pidfd (a kernel or seccomp filter that refuses pidfd_open, or a
// /proc whose self/fdinfo does not confirm that the pidfd names the process
// that /proc numbers pid), step 3 alone decides, and the exec in step 4 is
// the one case it cannot tell.
//
// A thread's start time tells it from a later thread given the same tid
// only to within a clock tick (10 ms). For the kernel to give the same tid
// to another thread in that time, it must hand out every other pid of the
// namespace first.
func RunningThread(root string, pid int) (tid int, ok bool, err error) {
	return runningThreadIn(root, pid, groupExitedIn(root))
}

// threadListings is how many listings of a process's threads RunningThread
// takes at most in a row with no thread of it released (its count no lower
// than before).
const threadListings = 16

// runningThreadIn is RunningThread, asking groupExited whether process pid's
// whole thread group has exited (known false: it cannot be told).
func runningThreadIn(root string, pid int, groupExited func(pid int) (exited, known bool)) (tid int, ok bool, err error) {
	if exited, known := groupExited(pid); known && exited {
		return 0, false, nil
	}
	proc, buf := os.DirFS(root+"/"+strconv.Itoa(pid)+"/task"), make([]byte, statBufSize)
	lowest, worker := -1, false
	for stale := 0; stale < threadListings; {
		tid, runs, exited, gone, err := listThreads(proc, pid, buf)
		if err != nil || runs || gone {
			return tid, runs, err
		}
		count, gone, err := threadCount(root, pid, buf)
		if err != nil || gone {
			return 0, false, err
		}
		// A count lower than any before is threads released: progress,
		// which a process none of whose threads runs makes only so much
		// of.
		if count > 0 && (lowest < 0 || count < lowest) {
			lowest, stale = count, 0
		} else {
			stale++
		}
		if count == 0 {
			// The main thread was being released as its stat was read:
			// the process was reaped (gone at the next listing), or a
			// thread of it ran an exec and holds its pid now.
			continue
		}
		still, other, sawWorker, err := stillExited(proc, pid, exited, buf)
		if err != nil {
			return 0, false, err
		}
		// A vhost worker seen in any listing keeps the rule off for the
		// whole read: one released since is no longer listed.
		worker = worker || sawWorker
		other = other && !worker
		if still >= count {
			if done, known := groupExited(pid); !known || done || other {
				return 0, false, nil
			}
		}
	}
	return 0, false, fmt.Errorf("the threads of process %d kept changing over more than %d listings with no thread released: none of them ran, and its count of threads or its pidfd said one they did not hold was there", pid, threadListings)
}

// stillExited reads again each of exited, process pid's threads a listing
// found exited, its main thread first: still is how many are there, the same
// thread (start time), and exited; other says one of them is not its main
// thread, and none listed is a vhost worker (userWorker); worker says one
// listed is.
func stillExited(proc fs.FS, pid int, exited []taskStat, buf []byte) (still int, other, worker bool, err error) {
	for _, th := range exited {
		worker = worker || userWorker(th.flags)
		now, gone, err := readThread(proc, th.id, pid, buf)
		if err != nil {
			return 0, false, false, err
		}
		// Another thread given its tid since is not the one found exited;
		// one that runs is found when the threads are listed again.
		if !gone && now.start == th.start && (now.state == 'Z' || now.state == 'X') {
			still++
			other = other || th.id != pid
		}
	}
	return still, other && !worker, worker, nil
}

// userWorker reports whether a thread's flags (field 9 of its stat) are a
// user worker's that is no io_uring worker (PF_USER_WORKER, 0x4000, without
// PF_IO_WORKER, 0x10: a vhost worker, Linux 6.4 on). An exec does not wait
// for such a thread to be released (zap_other_threads leaves it out of
// notify_count), so it rules out no exec. Before 6.4 the bit is another
// flag (PF_USED_ASYNC, a module's init), and a thread with it is left out
// the same: one less thread to rule an exec out by, never one more.
func userWorker(flags uint64) bool {
	return flags&(0x4000|0x10) == 0x4000
}

// groupExitedIn tells, through a pidfd, whether the whole thread group of
// process pid, numbered as the /proc at root numbers it, has exited: its
// pidfd polls readable then. known is false where it cannot be told: the
// pidfd cannot be had, or root's self/fdinfo does not confirm that it names
// the process root numbers pid (a /proc of another pid namespace than
// keploy's, or no procfs at all).
func groupExitedIn(root string) func(pid int) (exited, known bool) {
	return func(pid int) (bool, bool) {
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			return false, false
		}
		defer func() { _ = unix.Close(fd) }()
		info, err := os.ReadFile(root + "/self/fdinfo/" + strconv.Itoa(fd))
		if err != nil || !fdinfoNames(info, pid) {
			return false, false
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(fds, 0)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return false, false
			}
			return n > 0 && fds[0].Revents&unix.POLLIN != 0, true
		}
	}
}

// fdinfoNames reports whether a pidfd's fdinfo says it names process pid
// (its Pid line, in the pid namespace of the /proc it was read from).
func fdinfoNames(info []byte, pid int) bool {
	for _, l := range bytes.Split(info, []byte("\n")) {
		if v, ok := bytes.CutPrefix(l, []byte("Pid:")); ok {
			n, err := strconv.Atoi(string(bytes.TrimSpace(v)))
			return err == nil && n == pid
		}
	}
	return false
}

// taskStat is what RunningThread reads of a thread: its state, and its start
// time (field 22 of its stat), which tells it from another given its tid
// later.
type taskStat struct {
	id    int
	state byte
	start string
	flags uint64
}

// listThreads lists process pid's threads, from proc, its task directory,
// and reads each one, its main thread last: tid is the first that runs
// (runs), else exited holds every one listed that had exited, its main
// thread first. gone is a process no longer there.
func listThreads(proc fs.FS, pid int, buf []byte) (tid int, runs bool, exited []taskStat, gone bool, err error) {
	tasks, err := fs.ReadDir(proc, ".")
	if procGone(err) {
		return 0, false, nil, true, nil
	}
	if err != nil {
		return 0, false, nil, false, fmt.Errorf("list process %d's threads: %w", pid, err)
	}
	ids := make([]int, 0, len(tasks))
	leader := false
	for _, t := range tasks {
		id, err := strconv.Atoi(t.Name())
		if err != nil {
			continue
		}
		if id == pid {
			leader = true
			continue
		}
		ids = append(ids, id)
	}
	if leader {
		ids = append(ids, pid)
	}
	for _, id := range ids {
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
	// The main thread is read last, and read again first.
	slices.Reverse(exited)
	return 0, false, exited, false, nil
}

// taskListings is how many times taskIDs reads a process's count of its
// threads at most, looking for a pair of listings it can show hold them all.
const taskListings = 4

// taskIDs lists the threads of process pid from the /proc at root, for a
// read that needs every thread of a process, whatever each is doing
// (readTaskTimesIn). A listing taken while threads exit can leave out one
// that is there throughout (RunningThread), so the threads are listed, the
// process's count of them (num_threads) read, and the threads listed again.
// whole says the first listing held every thread the count held: as many of
// its threads as the count are still there after it (in the second listing,
// or looked up again), and a thread is never released twice, so every
// thread there at the count was in it. The two listings are returned
// together. Where the first falls short, the second is held against a new
// count and a third listing, and so on, taskListings times; then every
// thread any listing held is returned, not whole (a thread gone by then
// reads as gone). A thread started between a listing and the count it is
// held against makes that pair fall short, and the next pair holds it; a
// process that keeps that up for every pair is read not whole, which only
// slows the exit clock (lookTimes). A tid the kernel gives another thread
// after the count could pass for the one listed; it must hand out every
// other pid of the namespace first. gone is a process no longer there.
func taskIDs(root string, pid int) (ids []string, whole, gone bool, err error) {
	dir := root + "/" + strconv.Itoa(pid) + "/task"
	buf := make([]byte, statBufSize)
	var all []string
	seen := map[string]bool{}
	list := func() ([]string, bool, error) {
		ents, err := os.ReadDir(dir)
		if procGone(err) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("list process %d's threads: %w", pid, err)
		}
		var ids []string
		for _, e := range ents {
			if _, err := strconv.Atoi(e.Name()); err == nil {
				ids = append(ids, e.Name())
				if !seen[e.Name()] {
					seen[e.Name()] = true
					all = append(all, e.Name())
				}
			}
		}
		return ids, false, nil
	}
	first, gone, err := list()
	if err != nil || gone {
		return nil, false, gone, err
	}
	for range taskListings {
		count, gone, err := threadCount(root, pid, buf)
		if err != nil || gone {
			return nil, false, gone, err
		}
		second, gone, err := list()
		if err != nil || gone {
			return nil, false, gone, err
		}
		inFirst, inSecond := map[string]bool{}, map[string]bool{}
		for _, id := range first {
			inFirst[id] = true
		}
		for _, id := range second {
			inSecond[id] = true
		}
		still := 0
		for _, id := range first {
			if inSecond[id] {
				still++
				continue
			}
			if _, err := os.Lstat(dir + "/" + id); err == nil {
				still++
			} else if !procGone(err) {
				return nil, false, false, fmt.Errorf("look up thread %s of process %d: %w", id, pid, err)
			}
		}
		if count > 0 && still >= count {
			both := slices.Clone(first)
			for _, id := range second {
				if !inFirst[id] {
					both = append(both, id)
				}
			}
			return both, true, false, nil
		}
		first = second
	}
	return all, false, false, nil
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
	if ReleasedTask(line) {
		// Released between the kernel's read of its state and of its
		// parent: dead, whatever state its line shows.
		state = 'X'
	}
	start, parsed := statField(line, 22)
	if !parsed {
		return taskStat{}, false, fmt.Errorf("thread %d of process %d: its stat line has no start time: %q", id, pid, line)
	}
	f, parsed := statField(line, 9)
	flags, err := strconv.ParseUint(string(f), 10, 64)
	if !parsed || err != nil {
		return taskStat{}, false, fmt.Errorf("thread %d of process %d: its stat line has no flags: %q", id, pid, line)
	}
	return taskStat{id: id, state: state, start: string(start), flags: flags}, false, nil
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
// reads with its parent and group gone (0 and -1), which ParseProcStat
// refuses, in the state the kernel read before it found it released: Z or X,
// or, released between the two reads, R, S or D (ReleasedTask, which
// readThread reads as X).
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
