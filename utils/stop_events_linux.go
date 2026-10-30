//go:build linux

package utils

import (
	"errors"
	"fmt"
	"math/bits"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// A STOP WAITS FOR THE APP'S EXIT, NOT FOR A NUMBER OF SECONDS.
//
// Under the proven switch keploy's stop of an app it started (a test set's
// end, a recording's end) waits for the app's own exit event: a native
// group's leader exiting (its pidfd becomes readable), a container stopping
// (the daemon's wait, or its main process exiting). It kills only when the
// app made no progress on its way out (ExitMark: a process or thread of it
// ending, a file or socket closed, storage read or written while it waits on
// it) for StopStallBudget of the time both it and keploy could run
// (ExitClock: its threads' own time on and waiting for a CPU, and on storage
// that serves, and keploy's runnable time, stall.Process), or at the user's
// Ctrl+C, which it offers. A starved host takes as long as it takes, and so
// does an app starved alone (a container's --cpus, a cgroup whose quota its
// siblings spent) or one whose way out waits on a disk the host keeps busy:
// the stall accrues only as fast as it gets to run. A shutdown that only
// computes, with no process, thread or file of it ending while it has had the
// bound to run, is killed; so is one that only waits in the kernel, once its
// waits there since its last progress outlast the kernel's hung-task time.

// StopStallBudget is how long, in the time both it and keploy could run
// (ExitClock), an app keploy stopped may make no progress on its way out
// before keploy kills it: the watchdog's stall bound (stall.HangBudget). A
// test sets it shorter.
var StopStallBudget = stall.HangBudget

// StopStallSaid is how a kill names the bound it killed on where the app's
// processes could not be read: keploy's runnable time.
func StopStallSaid() string { return StopStallBudget.String() + " of keploy's runnable time" }

// ExitMark is how far processes are on their way out: how many still run,
// with how many threads and open files (files, not descriptors: openFiles),
// how much storage they have read and written, and whether one waits on
// storage (an uninterruptible sleep, D). FilesUnsettled is a read of their
// open files that did not settle (openFiles), of descriptors the kernel
// would not list, or through a task that had begun to exit (taskExiting):
// its Fds are no evidence. FilesUncompared, where it is not
// nil, is why the kernel would not compare a process's files (kcmp(2)): its
// Fds then count its descriptors, each once (openFiles).
type ExitMark struct {
	Running, Threads, Fds int
	IO                    uint64
	OnStorage             bool
	FilesUnsettled        bool
	FilesUncompared       error
}

// MarkExit reads how far procs are on their way out, each named by its pid
// and start time: one whose pid now names another process, or a zombie, has
// exited. One whose main thread exited while another runs on reads as a
// zombie in its stat, but runs (Exited): its open files, which its main
// thread no longer shows, are read through a thread of it that runs.
func MarkExit(procs []Reached) ExitMark {
	var m ExitMark
	for _, p := range procs {
		st, ok := ReadProcStat(p.PID)
		if !ok || st.StartTime != p.Start {
			continue
		}
		dir := "/proc/" + strconv.Itoa(p.PID)
		fdDir, task := dir+"/fd", p.PID
		if st.State == 'Z' {
			// A read of its threads that fails leaves it running, as in
			// Exited: what keploy cannot tell has ended is not taken for so.
			tid, runs, err := runningThread(p.PID)
			if err == nil && !runs {
				continue
			}
			if runs {
				fdDir, task = dir+"/task/"+strconv.Itoa(tid)+"/fd", tid
			}
		}
		m.Running++
		if st.State == 'D' {
			m.OnStorage = true
		}
		m.Threads += processThreads(p.PID)
		// Descriptors the kernel will not list (one not dumpable, or that
		// another user's credentials run now) are no evidence either.
		n, settled := 0, false
		if fds, err := os.ReadDir(fdDir); err == nil {
			var uncompared error
			n, settled, uncompared = openFiles(task, fds)
			if uncompared != nil && m.FilesUncompared == nil {
				m.FilesUncompared = fmt.Errorf("kcmp of process %d's descriptors: %w", p.PID, uncompared)
			}
			// Read through a task that has begun to exit, its files are
			// no evidence: it lets them go (exit_files) before it is
			// released, while other threads of its process hold them.
			settled = settled && !taskExiting(p.PID, task)
		}
		m.Fds += n
		m.FilesUnsettled = m.FilesUnsettled || !settled
		m.IO += readStorageIO(dir + "/io")
	}
	return m
}

// kcmpFile is kcmp(2)'s KCMP_FILE: whether two descriptors name one open file.
const kcmpFile = 0

// pfExiting is the PF_EXITING task flag (include/linux/sched.h, ProcStat.Flags).
const pfExiting = 0x4

// taskExiting reports whether task, process pid or a thread of it (MarkExit
// reads a process's files through it), had begun to exit by the time its
// files were read: it is gone, dead or a zombie, or PF_EXITING is set. A
// task sets PF_EXITING as it begins to exit (exit_signals in do_exit),
// before it lets its files go (exit_files) and so before it is a zombie or
// released, and never clears it; from exit_files on it lists no descriptors
// and kcmp of its files is EBADF, so a read through it then counts none of
// them, or only the first, which it holds uncompared (fileTally), while
// another thread of its process may hold every one. Its state alone says
// nothing of that window: it reads R, S or D until exit_notify. Read once
// its files have been, the flag covers every read that saw them go: a
// thread's own, as a zombie main thread shows the flag set for good.
func taskExiting(pid, task int) bool {
	root := "/proc"
	if task != pid {
		root += "/" + strconv.Itoa(pid) + "/task"
	}
	st, ok := ReadProcStatIn(root, task)
	return !ok || exiting(st)
}

// exiting reports whether a task's stat says it has begun to exit: it is
// dead (X) or a zombie (Z), or PF_EXITING is set (taskExiting).
func exiting(st ProcStat) bool {
	return st.State == 'Z' || st.State == 'X' || st.Flags&pfExiting != 0
}

// openFiles is how many open files a process holds through the descriptors
// its fd directory listed (fds, read through task, the process or a thread of
// it that runs), and whether the read settled: a descriptor naming the file
// another of them names (a duplicate: dup, dup2, stdout and stderr made one,
// a shell saving a descriptor as it redirects it) counts once with it.
// Closing a duplicate releases nothing: a process that copies a descriptor
// and closes the copy over and over (dash runs `exec 3<&-` so: fd 3 saved to
// fd 10, then both closed) holds no fewer files as it does, and read by its
// descriptors its churn would look one step deeper than it is (ExitRatchet).
// Two descriptors name one file where kcmp(2) says so; one closed since the
// listing is not held, and not counted, and one closed and opened again as
// they are read takes none of the others' files with it (fileTally). A read
// that did not settle (a descriptor that kept closing and opening again as it
// was compared, or the kernel refusing to compare part way through) counts
// the files it settled, and is no evidence (ExitMark.FilesUnsettled). kcmp
// compares two descriptors at a time, never all at one instant, so a read
// that settled can still, rarely, count a descriptor that closed and opened
// again between its comparisons beside the one that took its file over
// (fileTally.settle). Where the kernel cannot compare them at all (no kcmp,
// or keploy may not ask: a seccomp filter, a process it may not trace; its
// first comparison is refused: ENOSYS, EPERM, EACCES, or any errno a filter
// gives), each descriptor counts, and uncompared says why. A first
// comparison that fails in passing is no refusal (kcmpRefused): ESRCH is a
// process (or the thread read for it) released after its descriptors were
// listed, EINTR a comparison interrupted, a read that did not settle; EBADF
// is a descriptor closed (fileTally), and where every one reads so because
// the task let its files go as it exits, MarkExit takes its read for none
// (taskExiting). A churn of descriptors then
// reads as steps out again, but the ratchet takes only a new fewest
// (ExitRatchet), so it moves a stop at most once for each descriptor of the
// reading that set the mark: a longer wait, but a bounded one. Taken as no
// evidence instead, the files an app closes as it drains would be no
// progress, and keploy would kill it.
func openFiles(task int, fds []os.DirEntry) (n int, settled bool, uncompared error) {
	if cmp := filesCompared.Load(); cmp != nil {
		return openFilesBy(*cmp, task, fds)
	}
	return openFilesBy(kcmpFiles, task, fds)
}

// filesCompared, where a test sets it, is how openFiles compares two
// descriptors' files in place of kcmp(2); atomic, as a test may reset it
// while a stop it left runs on.
var filesCompared atomic.Pointer[func(task, a, b int) (int, error)]

// SetFilesComparedForTest makes openFiles compare two descriptors' files
// with cmp in place of kcmp(2) (filesCompared), for a test of a stop in
// another package (the daemons' stop, pkg/platform/http); restore puts back
// what was there.
func SetFilesComparedForTest(cmp func(task, a, b int) (int, error)) (restore func()) {
	old := filesCompared.Swap(&cmp)
	return func() { filesCompared.Store(old) }
}

// openFilesBy is openFiles with the kernel's comparison of two descriptors'
// files (kcmpFiles; the tests' own).
func openFilesBy(kcmp func(task, a, b int) (int, error), task int, fds []os.DirEntry) (n int, settled bool, uncompared error) {
	t := fileTally{kcmp: kcmp, task: task, spent: map[int]int{}}
	for _, e := range fds {
		if fd, err := strconv.Atoi(e.Name()); err == nil {
			t.todo = append(t.todo, fd)
		}
	}
	t.share = 4 * (bits.Len(uint(len(t.todo))) + 2)
	switch err := t.settle(); {
	case err == nil:
		return len(t.held), !t.unsettled, nil
	case !t.answered && kcmpRefused(err):
		// Refused from the first comparison: each descriptor counts.
		return len(fds), true, err
	default:
		// Refused part way through, or a first comparison that failed in
		// passing (ESRCH: the process or thread was released after its
		// descriptors were listed; EINTR): the files held by then, no
		// evidence.
		return len(t.held), false, nil
	}
}

// kcmpRefused reports whether err, the first comparison of a process's
// files failing, is the kernel refusing to compare them at all: kcmp(2) not
// built in (ENOSYS), a ptrace rule that keeps keploy from reading the
// process (EPERM), or a seccomp filter, which may give any errno (EPERM,
// ENOSYS and EACCES are the common ones). It is not where the failure is
// one in passing: ESRCH, a task released since its descriptors were listed
// (kcmp finds both tasks before it asks whether keploy may read them),
// EINTR, or EBADF, a descriptor closed (fileTally reads that as an answer;
// it never reaches here). Those say nothing of the host. Any other errno is
// taken for a refusal, so on a host whose filter gives one keploy never
// thought of, a stop counts descriptors (a bounded wait) rather than seeing
// no file close as progress, ever.
func kcmpRefused(err error) bool {
	return !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.EINTR) && !errors.Is(err, unix.EBADF)
}

// fileTally is a read of the open files a process holds through its
// descriptors (openFiles), as it goes: those held, one descriptor of each
// file in kcmp's order of the files, each with the others found naming its
// file; and the listed descriptors left to read. Descriptors close and open
// again as it reads (a churn of them: dash's `exec 3<&-` dups fd 3 to fd 10
// and closes both), so it acts only on what a descriptor says of itself: one
// is let go where it reads closed, never where the other compared with it
// may be the one closed (it may have opened again since); and one held that
// closed and opened again on another file can stand out of the files' order,
// so the files are counted only once each reads before the next (two that
// do not are both read again). Each descriptor makes its own share of the
// comparisons: the search for its file's place, and the checks of its place
// after the one before it. One that spent its share (it kept closing and
// opening again as it was compared) is set aside, not counted, and the read
// did not settle; the others are read on their own shares, so a stable
// duplicate after it is still found.
type fileTally struct {
	kcmp      func(task, a, b int) (int, error)
	task      int
	held      []heldFile
	todo      []int
	share     int         // comparisons each descriptor may make (errShareSpent)
	spent     map[int]int // comparisons each has made
	answered  bool        // kcmp has answered: a refusal now is part way through
	unsettled bool        // a descriptor was set aside
}

// heldFile is a descriptor of a file a process holds, and the other
// descriptors found naming it.
type heldFile struct {
	fd   int
	dups []int
}

// errShareSpent is a descriptor that made its share of a read's comparisons
// (fileTally.share), still closing and opening again under them.
var errShareSpent = errors.New("the descriptor kept changing as its file was read")

// settle reads the descriptors left, then checks that each file held reads
// before the next in kcmp's order; one that does not is read again. One that
// spent its share is set aside (fileTally.unsettled). The files are counted
// once two checks in a row find each in order, with nothing read again
// between them: one check compares two at a time, never all at one instant,
// so a descriptor can pass its check and close, and fd 3, opened again on
// the next cycle's file, pass its own after it (one file counted twice); the
// next check finds the first closed, or out of order on the file it names
// by then.
func (t *fileTally) settle() error {
	clean := 0
	for {
		// Only a check that was not clean leaves descriptors to read again.
		for len(t.todo) > 0 {
			fd := t.todo[0]
			t.todo = t.todo[1:]
			if err := t.hold(fd); errors.Is(err, errShareSpent) {
				t.unsettled = true
			} else if err != nil {
				return err
			}
		}
		done, err := t.ordered()
		switch {
		case err != nil:
			return err
		case !done:
			clean = 0
		case clean == 1:
			return nil
		default:
			clean = 1
		}
	}
}

// hold adds descriptor fd to the files held, in kcmp's order of the files,
// or to the duplicates of the one there that names its file; one closed is
// not held. Where one of two compared was closed, each is asked of itself:
// one closed is let go, and where both are open again (one closed and opened
// again in between), they are compared again. Each comparison is fd's.
func (t *fileTally) hold(fd int) error {
	lo, hi := 0, len(t.held)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		order, err := t.cmp(fd, t.held[mid].fd, fd)
		switch {
		case errors.Is(err, unix.EBADF):
			if open, err := t.open(fd, fd); err != nil || !open {
				return err
			}
			if err := t.drop(mid, fd); err != nil {
				return err
			}
			lo, hi = 0, len(t.held)
		case err != nil:
			return err
		case order == 0:
			t.held[mid].dups = append(t.held[mid].dups, fd)
			return nil
		case order == 1:
			hi = mid
		case order == 2:
			lo = mid + 1
		default:
			return fmt.Errorf("kcmp gave no order of two files (%d)", order)
		}
	}
	t.held = slices.Insert(t.held, lo, heldFile{fd: fd})
	return nil
}

// ordered reports whether each file held reads before the next in kcmp's
// order, so that no two of them are one file; the comparison of two is the
// next one's. Where one does not (one of the two closed and opened again
// since it was held: it names another file, or the other's), both are read
// again, with their duplicates, as either may be the one that changed: one
// held that names another file now is so placed again, on its file then, and
// checked beside the others in one pass; where one of two is closed, each is
// asked of itself, on its own share, and one closed is let go.
func (t *fileTally) ordered() (bool, error) {
	for i := 0; i+1 < len(t.held); i++ {
		cur, next := t.held[i], t.held[i+1]
		order, err := t.cmp(cur.fd, next.fd, next.fd)
		switch {
		case errors.Is(err, unix.EBADF):
			if err := t.spentBy(next.fd, t.drop(i+1, next.fd)); err != nil {
				return false, err
			}
			return false, t.spentBy(cur.fd, t.drop(i, cur.fd))
		case err != nil:
			return false, t.spentBy(next.fd, err)
		case order == 1:
			continue
		case order != 0 && order != 2:
			return false, fmt.Errorf("kcmp gave no order of two files (%d)", order)
		}
		t.held = slices.Delete(t.held, i, i+2)
		t.todo = append(append(append(append(t.todo, cur.fd), cur.dups...), next.fd), next.dups...)
		return false, nil
	}
	return true, nil
}

// spentBy sets held descriptor fd aside where err is its spent share
// (errShareSpent), and is err otherwise. One set aside is not counted, and
// the read did not settle; its duplicates are read again: one may hold its
// file yet.
func (t *fileTally) spentBy(fd int, err error) error {
	if !errors.Is(err, errShareSpent) {
		return err
	}
	for i, h := range t.held {
		if h.fd == fd {
			t.todo = append(t.todo, h.dups...)
			t.held = slices.Delete(t.held, i, i+1)
			t.unsettled = true
			break
		}
	}
	return nil
}

// drop lets go of held file i where its descriptor reads closed now, asked
// on by's share. Its duplicates are read again: one may hold its file yet.
func (t *fileTally) drop(i, by int) error {
	if open, err := t.open(t.held[i].fd, by); err != nil || open {
		return err
	}
	t.todo = append(t.todo, t.held[i].dups...)
	t.held = slices.Delete(t.held, i, i+1)
	return nil
}

// open reports whether descriptor fd is open now, asked on by's share.
func (t *fileTally) open(fd, by int) (bool, error) {
	_, err := t.cmp(fd, fd, by)
	if errors.Is(err, unix.EBADF) {
		return false, nil
	}
	return err == nil, err
}

// cmp is kcmp of descriptors a and b, one of by's share of comparisons
// (errShareSpent).
func (t *fileTally) cmp(a, b, by int) (int, error) {
	if t.spent[by] >= t.share {
		return 0, errShareSpent
	}
	t.spent[by]++
	order, err := t.kcmp(t.task, a, b)
	if err == nil || errors.Is(err, unix.EBADF) {
		t.answered = true
	}
	return order, err
}

// kcmpFiles is kcmp(2) of descriptors a and b of process (or thread) task:
// 0 where they name one file, else 1 or 2 as the first's file orders before
// or after the second's.
func kcmpFiles(task, a, b int) (int, error) {
	r, _, errno := unix.Syscall6(unix.SYS_KCMP, uintptr(task), uintptr(task), kcmpFile, uintptr(a), uintptr(b), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

// processThreads is how many threads process pid has not yet released, as
// MarkExit counts them: its count (num_threads), not a listing of them,
// which, taken while some exit, can leave out one that runs
// (RunningThread). A count of 0 is a main thread released as its stat was
// read: read again, the process is gone (0) or a thread of it ran an exec
// and holds its pid (its count). A test makes it count otherwise.
var processThreads = func(pid int) int {
	buf := make([]byte, statBufSize)
	for range 2 {
		n, gone, err := threadCount("/proc", pid, buf)
		if err != nil || gone {
			return 0
		}
		if n > 0 {
			return n
		}
	}
	return 0
}

// ExitRatchet is the furthest processes have come on their way out: the
// fewest running, threads and open files seen, and the most storage I/O. A
// reading is progress only where it passes that mark, so a count that rises
// and falls back (a socket opened and closed, a thread started and ended: a
// retry loop, a DNS lookup, a pool) is none; and storage I/O counts only
// while one of them waits on it: one that logs as it waits on its own makes
// none. A reading whose open files did not settle (ExitMark.FilesUnsettled:
// descriptors that kept changing as they were read) neither sets nor passes
// the open-file mark: a look that could not tell the files apart is no
// evidence, and so a first look that counted too many cannot make a later,
// true one look like progress. The stop of the daemons an app leaves behind
// reads them so too (stopMembers, pkg/platform/http/adopt_linux.go).
type ExitRatchet struct {
	best  ExitMark
	set   bool
	files bool // best.Fds is a settled reading's
}

// Advance takes reading m and reports whether it passed the mark.
func (r *ExitRatchet) Advance(m ExitMark) bool {
	files := !m.FilesUnsettled
	if !r.set {
		r.best, r.set, r.files = m, true, files
		return false
	}
	moved := false
	if m.Running < r.best.Running {
		r.best.Running, moved = m.Running, true
	}
	if m.Threads < r.best.Threads {
		r.best.Threads, moved = m.Threads, true
	}
	switch {
	case !files:
	case !r.files:
		r.best.Fds, r.files = m.Fds, true
	case m.Fds < r.best.Fds:
		r.best.Fds, moved = m.Fds, true
	}
	if m.IO > r.best.IO {
		if m.OnStorage {
			moved = true
		}
		r.best.IO = m.IO
	}
	return moved
}

// readStorageIO is the bytes a process has read from and written to storage
// (read_bytes and write_bytes of /proc/<pid>/io), 0 where it cannot be read.
func readStorageIO(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var n uint64
	for _, line := range strings.Split(string(b), "\n") {
		for _, key := range []string{"read_bytes:", "write_bytes:"} {
			if v, ok := strings.CutPrefix(line, key); ok {
				if x, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
					n += x
				}
			}
		}
	}
	return n
}

// TreeOf is pid and every process it runs, each named by its pid and start
// time, as /proc shows them now; nil when pid is gone.
func TreeOf(pid int) []Reached {
	if pid <= 0 {
		return nil
	}
	children, _, err := findChildPIDs(pid)
	if err != nil {
		children = nil
	}
	var procs []Reached
	for _, p := range append([]int{pid}, children...) {
		if st, ok := ReadProcStat(p); ok && st.State != 'Z' {
			procs = append(procs, Reached{PID: p, Start: st.StartTime})
		}
	}
	return procs
}

// WaitExitByEvents waits for exited to fire, while procs make progress on
// their way out (ExitMark), looking at them at the stall meter's pace. When
// every one of procs is gone the app has exited too. It ends StopStalled when
// they made no progress for StopStallBudget of the time both they and keploy
// could run (ExitClock) first; with no procs to read, that is keploy's
// runnable time, the bound on no news at all. It ends StopPressed at the
// user's Ctrl+C, which it offers StopOffer after the run's interrupt
// (StopNowNotice), so a stop that waits as long as the app makes
// progress can always be ended. A kill is due on either. Each step they take
// on their way out is a movement of moved, the teardown scope of the run
// whose app it stops (nil: none). bound is how a kill names the stall bound,
// as it was measured (ExitClock.Said).
func WaitExitByEvents(logger *zap.Logger, exited <-chan struct{}, procs []Reached, moved *TeardownScope) (end StopEnd, bound string) {
	notice := NewStopNowNotice(StopOffer)
	defer notice.Withdraw()
	return WaitExitOffered(logger, exited, procs, moved, notice, OfferKillNow)
}

// WaitExitOffered is WaitExitByEvents with its offer to end at the user's
// Ctrl+C made by notice, which the caller made (NewStopNowNotice) and
// withdraws once its stop no longer waits; offer is what the offer says a
// press does now, logged at the wait's first look at which an offer stands.
// A stop that waits more than once (for its processes, then for word that it
// is through) gives each wait the one notice, so one offer stands across
// them: a press taken at an earlier wait's offer, even in the look that
// ended that wait, ends the next at once, and so does a press between them.
// With a notice each, the next wait offered only StopOffer after it began,
// and a press before that ended nothing: the stop waited on, for a second
// offer and press or its stall bound.
func WaitExitOffered(logger *zap.Logger, exited <-chan struct{}, procs []Reached, moved *TeardownScope, notice *StopNowNotice, offer string) (end StopEnd, bound string) {
	clock, meter, stop := NewExitMeter(procs, StopStallBudget)
	defer stop()
	offering := false
	var uncompared FilesUncomparedNote
	first := MarkExit(procs)
	uncompared.Say(logger, first)
	meter.Advance(first)
	for {
		select {
		case <-exited:
			return StopExited, clock.Said(StopStallBudget)
		case <-notice.Interrupt():
			return StopPressed, clock.Said(StopStallBudget)
		case <-meter.Wait():
		}
		if looked := exitLooked.Load(); looked != nil {
			(*looked)()
		}
		if !offering && notice.Next() {
			offering = true
			logger.Info(offer)
		}
		m := MarkExit(procs)
		if len(procs) > 0 && m.Running == 0 {
			return StopExited, clock.Said(StopStallBudget)
		}
		uncompared.Say(logger, m)
		if meter.Advance(m) {
			moved.Note()
			continue
		}
		if meter.Expired() {
			return StopStalled, clock.Said(StopStallBudget)
		}
	}
}

// FilesUncomparedNote is a stop's record that it said the open files of the
// processes it waits for are counted as their descriptors on this host
// (ExitMark.FilesUncompared): it says so once, at the first reading that
// does.
type FilesUncomparedNote struct{ said bool }

// Say logs why reading m counted a process's open files as its descriptors,
// if it did and the stop has not said so yet.
func (n *FilesUncomparedNote) Say(logger *zap.Logger, m ExitMark) {
	if n.said || m.FilesUncompared == nil {
		return
	}
	n.said = true
	logger.Info("keploy counts the open files of the processes it stops as their descriptors on this host, as the kernel will not compare them: one that copies a descriptor and closes the copy as it stops reads as a step out, so the stop may wait longer for it, bounded by its descriptors",
		zap.Error(m.FilesUncompared),
		zap.String("next_step", "nothing is needed; for the files to be compared, run keploy where the kernel has kcmp(2) and lets keploy's user read the application's processes (the same user, or root)"))
}

// exitLooked, where a test sets it, is called at each look of
// WaitExitByEvents at the processes it waits for (the tests count the looks);
// atomic, as a test may reset it while a stop it left runs on.
var exitLooked atomic.Pointer[func()]

// ProcOf is process pid as it runs now, named by its pid and start time; false
// when it is gone (or a zombie).
func ProcOf(pid int) (Reached, bool) {
	st, ok := ReadProcStat(pid)
	if pid <= 0 || !ok || st.State == 'Z' {
		return Reached{}, false
	}
	return Reached{PID: pid, Start: st.StartTime}, true
}

// ExitOf is a channel closed once process p exits, read on its pidfd
// (readable once it has), and the func that releases the watch. p is named
// by its pid and start time: a pid that now names another process, or none,
// has exited.
func ExitOf(p Reached) (<-chan struct{}, func()) {
	gone := func() (<-chan struct{}, func()) {
		c := make(chan struct{})
		close(c)
		return c, func() {}
	}
	if p.PID <= 0 {
		return gone()
	}
	fd, err := unix.PidfdOpen(p.PID, 0)
	if err != nil {
		return gone()
	}
	// The pidfd first, then its pid's start: a pid reused before the open
	// shows another start time. Once open, the pidfd names this process.
	if st, ok := ReadProcStat(p.PID); !ok || st.StartTime != p.Start {
		unix.Close(fd)
		return gone()
	}
	exited, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer unix.Close(fd)
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			// The poll returns on the exit; its bound only lets the watch
			// be released.
			n, err := unix.Poll(fds, 100)
			if err == nil && n > 0 {
				close(exited)
				return
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	var once sync.Once
	return exited, func() { once.Do(func() { close(done) }) }
}

// stopGroupByEvents is interruptProcessTree's wait, under the switch, for a
// process group it signalled: its leader's exit event (as its signal reached
// it, members), while the group makes progress. It kills the group on a stall,
// or at the user's press, and says why; "" when the group exited, or was gone
// by the kill.
func stopGroupByEvents(logger *zap.Logger, leader int, members []Reached, moved *TeardownScope) (killed string) {
	exited, release := leaderExited(leader, members)
	defer release()
	var why string
	switch end, bound := WaitExitByEvents(logger, exited, members, moved); end {
	case StopExited:
		return ""
	case StopPressed:
		why = "keploy killed a process of it at the press of Ctrl+C, before it had exited"
	default:
		why = fmt.Sprintf("a process of it made no progress on its way out for %s after keploy interrupted it, so keploy killed it", bound)
		logger.Debug("a process group made no progress on its way out after its interrupt", zap.Int("pgid", leader), zap.Duration("stall", StopStallBudget))
	}
	switch err := syscall.Kill(-leader, syscall.SIGKILL); {
	case errors.Is(err, syscall.ESRCH):
		// Gone by the kill: none reached it.
		return ""
	case err != nil:
		// Whether it ended is not known: the kill is said.
		logger.Error("error sending SIGKILL to the process group id", zap.Int("pgid", leader), zap.Error(err))
		return why
	}
	// A kill ends it but in the kernel: wait for that, at the same stall
	// rule, and go on without it after.
	WaitExitByEvents(logger, exited, members, moved)
	return why
}

// leaderExited is ExitOf a group's leader as its signal reached it
// (members): a leader not among them (gone, or a zombie, by then) has exited.
func leaderExited(leader int, members []Reached) (<-chan struct{}, func()) {
	for _, m := range members {
		if m.PID == leader {
			return ExitOf(m)
		}
	}
	return ExitOf(Reached{})
}

func init() { groupStopByEvents = stopGroupByEvents }
