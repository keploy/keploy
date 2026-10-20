//go:build linux

package utils

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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
// (ExitClock: its threads' own time on and waiting for a CPU, and keploy's
// runnable time, stall.Process), or at the user's Ctrl+C, which it offers. A
// starved host takes as long as it takes, and so does an app starved alone
// (a container's --cpus, a cgroup whose quota its siblings spent): the stall
// accrues only as fast as it gets to run. A shutdown that only computes, with
// no process, thread or file of it ending while it has had the bound to run,
// is killed.

// StopStallBudget is how long, in the time both it and keploy could run
// (ExitClock), an app keploy stopped may make no progress on its way out
// before keploy kills it: the watchdog's stall bound (stall.HangBudget). A
// test sets it shorter.
var StopStallBudget = stall.HangBudget

// StopStallSaid is how a kill names the bound it killed on where the app's
// processes could not be read: keploy's runnable time.
func StopStallSaid() string { return StopStallBudget.String() + " of keploy's runnable time" }

// StopStallRanSaid is how a kill names the bound it killed on where the app's
// processes were read (StallRanSaid).
func StopStallRanSaid() string { return StallRanSaid(StopStallBudget) }

// StallRanSaid is how a kill names a stall bound d of an ExitClock where the
// processes were read: of the time both they and keploy could run; on a
// kernel that keeps no account of them, of keploy's runnable time, which the
// clock then is.
func StallRanSaid(d time.Duration) string {
	if !schedAccountKept() {
		return d.String() + " of keploy's runnable time"
	}
	return d.String() + " of its and keploy's runnable time"
}

// ExitMark is how far processes are on their way out: how many still run,
// with how many threads and open files, how much storage they have read and
// written, and whether one waits on storage (an uninterruptible sleep, D).
type ExitMark struct {
	Running, Threads, Fds int
	IO                    uint64
	OnStorage             bool
}

// MarkExit reads how far procs are on their way out, each named by its pid
// and start time: one whose pid now names another process, or a zombie, has
// exited.
func MarkExit(procs []Reached) ExitMark {
	var m ExitMark
	for _, p := range procs {
		st, ok := ReadProcStat(p.PID)
		if !ok || st.StartTime != p.Start || st.State == 'Z' {
			continue
		}
		m.Running++
		if st.State == 'D' {
			m.OnStorage = true
		}
		dir := "/proc/" + strconv.Itoa(p.PID)
		if tasks, err := os.ReadDir(dir + "/task"); err == nil {
			m.Threads += len(tasks)
		}
		if fds, err := os.ReadDir(dir + "/fd"); err == nil {
			m.Fds += len(fds)
		}
		m.IO += readStorageIO(dir + "/io")
	}
	return m
}

// ExitRatchet is the furthest processes have come on their way out: the
// fewest running, threads and open files seen, and the most storage I/O. A
// reading is progress only where it passes that mark, so a count that rises
// and falls back (a socket opened and closed, a thread started and ended: a
// retry loop, a DNS lookup, a pool) is none; and storage I/O counts only
// while one of them waits on it: one that logs as it waits on its own makes
// none. The stop of the daemons an app leaves behind reads them so too
// (stopMembers, pkg/platform/http/adopt_linux.go).
type ExitRatchet struct {
	best ExitMark
	set  bool
}

// Advance takes reading m and reports whether it passed the mark.
func (r *ExitRatchet) Advance(m ExitMark) bool {
	if !r.set {
		r.best, r.set = m, true
		return false
	}
	moved := false
	if m.Running < r.best.Running {
		r.best.Running, moved = m.Running, true
	}
	if m.Threads < r.best.Threads {
		r.best.Threads, moved = m.Threads, true
	}
	if m.Fds < r.best.Fds {
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
// whose app it stops (nil: none).
func WaitExitByEvents(logger *zap.Logger, exited <-chan struct{}, procs []Reached, moved *TeardownScope) StopEnd {
	clock, release := NewExitClock(procs)
	defer release()
	meter := stall.NewMeter(clock, StopStallBudget)
	defer meter.Stop()
	notice := NewStopNowNotice(StopOffer)
	defer notice.Withdraw()
	offering := false
	var mark ExitRatchet
	mark.Advance(MarkExit(procs))
	for {
		select {
		case <-exited:
			return StopExited
		case <-notice.Interrupt():
			return StopPressed
		case <-meter.Wait():
		}
		if looked := exitLooked.Load(); looked != nil {
			(*looked)()
		}
		if !offering && notice.Next() {
			offering = true
			logger.Info("keploy waits for the application to stop, as long as it makes progress on its way out; press Ctrl+C again to kill it now")
		}
		m := MarkExit(procs)
		if len(procs) > 0 && m.Running == 0 {
			return StopExited
		}
		if mark.Advance(m) {
			meter.Reset()
			moved.Note()
			continue
		}
		if meter.Expired() {
			return StopStalled
		}
	}
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
	switch WaitExitByEvents(logger, exited, members, moved) {
	case StopExited:
		return ""
	case StopPressed:
		why = "keploy killed a process of it at the press of Ctrl+C, before it had exited"
	default:
		why = fmt.Sprintf("a process of it made no progress on its way out for %s after keploy interrupted it, so keploy killed it", StopStallRanSaid())
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
