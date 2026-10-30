//go:build linux

package http

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// Daemonized members (P146, design v14 §3.1: membership is set at fork, and a
// reparent changes nothing).
//
// For a native app the run's members are the keploy CLI's descendants: the
// agent's eBPF filter walks a process's real parents up to the CLI (the
// client it registered), and the proxy's listener check walks /proc's parent
// pids up to it (descends). A daemon the app starts leaves that tree: pg_ctl
// start and redis-server --daemonize fork a child that starts the daemon in a
// session of its own and exits, so the daemon is orphaned and the kernel
// reparents it to the nearest child subreaper, or to init. From then on its
// calls were neither intercepted nor judged (the set passed with them gone to
// the real dependency), and a call to its listener was mocked as an outside
// dependency's.
//
// Under the proven switch the CLI makes itself the child subreaper of its
// descendants (PR_SET_CHILD_SUBREAPER), so an orphaned descendant is
// reparented to the CLI and stays in the tree both walks read. A subreaper
// must reap what it adopts, or every adopted daemon that exits stays a zombie
// for the rest of the run (a fixture that waits for it to go, kill -0 or
// pgrep, would wait for ever). It must reap only those: the CLI's own children
// are waited for by os/exec, and a child reaped under it makes its Wait fail.
// Go holds a pidfd for every child it starts, from the clone that starts it
// until its Wait has reaped it (os.StartProcess, where pidfds work); it lets
// go of one sooner only when no Wait can come any more (Process.Release, or a
// Process collected without one). So a zombie child of the CLI that no pidfd
// of the CLI's names is one it adopted, or one nothing will wait for. That Go
// uses pidfds here is checked once, before the CLI becomes a subreaper, and
// without them nothing changes and a warning says what is lost.
//
// What the CLI adopts is part of the application's tree, and ends with it
// (stopAdopted): when keploy stops the application for a test set, or the
// application ends, the daemons it left are stopped too, as the agent is
// (decision 12): asked to stop, and killed only once they make no progress.
// So a daemon one test set started is never judged in the next, and the
// application started again can bind its ports again.
//
// The alternative weighed, membership by cgroup, needs a cgroup of keploy's
// own for the app (cgroup v2, a writable and delegated hierarchy, which
// containers, CI runners and WSL often lack) and a change to the eBPF filter;
// and the end state, the fork-time instance map (§3.3, the native launcher),
// replaces the parent walk outright. Until then the subreaper keeps the walk
// true across a reparent with a change to the CLI alone.

var (
	adoptOnce sync.Once
	// adopting is set once this process is a child subreaper that reaps
	// what it adopts.
	adopting atomic.Bool
	// cannotAdopt is why this process, asked to adopt the application's
	// daemons, does not (startAdopting failed, or its reaper stopped): an
	// end of the application cannot then be told to have ended them.
	cannotAdopt atomic.Pointer[string]
)

// noteCannotAdopt keeps why keploy does not adopt the application's daemons.
func noteCannotAdopt(why string) { cannotAdopt.Store(&why) }

// adoptDaemons makes this process, the CLI whose tree is the run's
// membership, the child subreaper of its descendants and reaps the ones it
// adopts, once per process; when it cannot, it says so.
func adoptDaemons(logger *zap.Logger) {
	adoptOnce.Do(func() {
		if _, err := startAdopting(logger); err != nil {
			noteCannotAdopt(err.Error())
			logger.Warn("keploy cannot keep a daemon the application starts in this run: a process the application starts and leaves behind (pg_ctl start, redis-server --daemonize) is reparented away from keploy, and its calls are neither intercepted nor judged; nor does keploy's stop of the application wait for a process of it that outlives the shell that started it (`app & wait`), so what that process does as it stops can come after its test set's end is read",
				zap.Error(err),
				zap.String("next_step", "start such a dependency before keploy, where it is an outside dependency, recorded and mocked; or run it in the foreground of the command keploy runs (postgres -D <dir> &, redis-server --daemonize no &), where it stays the command's child; and start the application itself in the foreground of its shell (exec app)"))
		}
	})
}

// startAdopting is adoptDaemons' work: it checks that Go holds a pidfd for
// each child it starts, makes this process a child subreaper, and reaps every
// adopted child as it exits (SIGCHLD) until stop is called.
func startAdopting(logger *zap.Logger) (stop func(), err error) {
	if err := goUsesPidfds(); err != nil {
		return nil, err
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("make the CLI a child subreaper: %w", err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGCHLD)
	done := make(chan struct{})
	adopting.Store(true)
	go func() {
		defer func() {
			// A subreaper that no longer reaps leaves every daemon it adopts a
			// zombie once it exits: the CLI stops being one, and later
			// orphans go to init, as before.
			if r := recover(); r != nil {
				noteCannotAdopt("keploy stopped reaping the daemons it adopts")
				adopting.Store(false)
				_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0)
				utils.HandleRecovery(logger, r, "keploy stopped reaping the daemons it adopts; daemons the application starts from now on leave the run's tree")
			}
		}()
		for {
			reapAll(logger)
			select {
			case <-sig:
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(sig)
			close(done)
		})
	}, nil
}

// reapAll reaps every adopted child that is a zombie now: a pass that reaped
// one is followed by another, and the last by one more, for a zombie a scan
// missed while the list of children changed under it.
func reapAll(logger *zap.Logger) {
	for {
		if len(reapAdopted(logger)) == 0 {
			break
		}
	}
	reapAdopted(logger)
}

// goUsesPidfds reports whether Go holds pidfds for processes here, what
// tells this process's own children from the ones it adopts: os.FindProcess
// opens one (as os.StartProcess does for each child) only where Go found
// pidfds work, CLONE_PIDFD among them.
func goUsesPidfds() error {
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		return fmt.Errorf("find this process: %w", err)
	}
	defer func() { _ = self.Release() }()
	held, err := heldPidfds()
	if err != nil {
		return err
	}
	if !held[os.Getpid()] {
		return errors.New("Go holds no pidfds here (they need Linux 5.4 or later, and no seccomp filter refusing them), so keploy cannot tell its own children from adopted ones")
	}
	return nil
}

// reapAdopted reaps every zombie child of this process that no pidfd of its
// own names, and returns their pids. Each is reaped through a pidfd opened on
// it, so a pid that was reaped and reused meanwhile is never waited for in
// its place; and the pidfds held are read after those are opened, so a child
// of this process that was a zombie then is named by its own. It waits for a
// read of the children that settles to be over (reaping).
func reapAdopted(logger *zap.Logger) []int {
	reaping.Lock()
	defer reaping.Unlock()
	type opened struct{ pid, fd int }
	var zombies []opened
	children, err := childPids()
	if err != nil {
		// Which processes are its children cannot be read: none is reaped
		// now. The next SIGCHLD tries again.
		childrenUnread(logger, err)
		return nil
	}
	for _, pid := range children {
		if st, ok := utils.ReadProcStat(pid); !ok || st.State != 'Z' {
			continue
		}
		fd, err := openPidfd(pid)
		if err != nil {
			continue // reaped already
		}
		zombies = append(zombies, opened{pid, fd})
	}
	if len(zombies) == 0 {
		return nil
	}
	defer func() {
		for _, z := range zombies {
			closePidfd(z.fd)
		}
	}()
	held, err := heldPidfds()
	if err != nil {
		// Which zombies are this process's own cannot be told: none is
		// reaped, or os/exec's Wait for one would fail. The next SIGCHLD
		// tries again.
		failedClosed(logger, err)
		return nil
	}
	var reaped []int
	for _, z := range zombies {
		if held[z.pid] {
			continue
		}
		// A child that cannot be reaped yet (its main thread exited while
		// another runs on, or another tracer holds it) is no reap: the wait
		// returns without a child, and nothing is looked at again before its
		// SIGCHLD. Counted as one, the pass that found it was followed by
		// another for as long as it lasted, a core spinning.
		if signo, err := waitAdopted(z.fd); err == nil && signo == int32(unix.SIGCHLD) {
			reaped = append(reaped, z.pid)
			logger.Debug("reaped a daemon the run started that keploy had adopted", zap.Int("pid", z.pid))
		}
	}
	return reaped
}

// waitAdopted reaps the child pidfd names if it can be reaped now, and
// returns the signal its wait reports: SIGCHLD for one reaped, 0 for one that
// cannot be yet (WNOHANG). A variable for the tests of a child that cannot.
var waitAdopted = func(pidfd int) (int32, error) {
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PIDFD, pidfd, &info, unix.WEXITED|unix.WNOHANG, nil)
	return info.Signo, err
}

// childPids is this process's children, for the reaper: each thread's
// children list (/proc/self/task/<tid>/children), or, where the kernel keeps
// none (CONFIG_PROC_CHILDREN off), every process whose parent it is
// (procChildren). The list is the kernel's quick one, and imprecise by its
// contract: a child listed after one that is reaped while the list is read
// can be skipped (fs/proc/array.c, get_children_pid). A zombie it skips is
// reaped at the reaper's next pass (reapAll); what a stop leaves running is
// never read from it (adoptedRunning). An error is a read that failed: of
// the list of threads, or of a thread's children (but for a thread that
// exited meanwhile, whose children the kernel hands to another thread).
func childPids() ([]int, error) {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, fmt.Errorf("list this process's threads: %w", err)
	}
	var pids []int
	listed := false
	for _, t := range tasks {
		b, err := os.ReadFile("/proc/self/task/" + t.Name() + "/children")
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			// The thread exited, or the kernel keeps no children lists:
			// then no thread's is read, the calling one's included.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read thread %s's children: %w", t.Name(), err)
		}
		listed = true
		for _, f := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	if listed {
		return pids, nil
	}
	children, err := procChildren()
	if err != nil {
		return nil, err
	}
	for _, c := range children {
		pids = append(pids, c.pid)
	}
	return pids, nil
}

// child is a process whose parent this process is, as a read of /proc found
// it: its pid, its start time, and whether it has exited and waits to be
// reaped (a zombie: every thread of it exited, not its main thread alone);
// thread, a thread of it that runs where its main thread has exited, which
// what it runs is read through (0 where its main thread runs).
type child struct {
	pid    int
	start  uint64
	zombie bool
	thread int
}

// procChildren is every process whose parent this process is, from a scan of
// /proc: its pid directories, each one's stat read after. The kernel lists
// those in pid order and resumes each part of the listing from the pid it
// stopped at, never from a count of the entries before it, so a process that
// exists for the whole listing is in it, however many exit meanwhile
// (fs/proc/base.c, proc_pid_readdir). One whose stat says Z is a zombie only
// where no thread of it runs: a process whose main thread exited while
// another runs on ("Zl" in ps) runs (utils.RunningThread). An error is a read
// that failed: of /proc, of a listed process's stat, or of a zombie-looking
// one's threads, but for one that is gone by then (it exited and was
// reaped).
func procChildren() ([]child, error) { return procChildrenIn("/proc", os.Getpid()) }

// procChildrenIn is procChildren of process parent, read from the /proc at
// root (another directory in the tests of a read that fails).
func procChildrenIn(root string, parent int) ([]child, error) {
	d, err := os.Open(root)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", root, err)
	}
	defer func() { _ = d.Close() }()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", root, err)
	}
	var out []child
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue // not a process's directory
		}
		st, gone, err := procStat(root, pid)
		if err != nil {
			return nil, fmt.Errorf("read process %d's stat: %w", pid, err)
		}
		if gone || st.PPID != parent {
			continue
		}
		c := child{pid: pid, start: st.StartTime}
		if st.State == 'Z' {
			tid, runs, err := utils.RunningThread(root, pid)
			if err != nil {
				return nil, fmt.Errorf("read the threads of process %d, whose main thread exited: %w", pid, err)
			}
			c.zombie, c.thread = !runs, tid
		}
		out = append(out, c)
	}
	return out, nil
}

// procStat reads process pid's stat from the /proc at root
// (utils.ParseProcStat); gone says it is not there: it exited and was reaped
// before, or while, it was read (a task being reaped reads as state X, dead,
// or its parent and group gone: 0 and -1, which no line of a process that is
// there has, whatever state it read as: reaped).
func procStat(root string, pid int) (st utils.ProcStat, gone bool, err error) {
	b, err := os.ReadFile(root + "/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return utils.ProcStat{}, true, nil
	}
	if err != nil {
		return utils.ProcStat{}, false, err
	}
	state := statState(b)
	if state == 'X' {
		return utils.ProcStat{}, true, nil
	}
	st, ok := utils.ParseProcStat(b)
	if !ok {
		if reaped(b) {
			return utils.ProcStat{}, true, nil
		}
		return utils.ProcStat{}, false, fmt.Errorf("its stat line does not parse: %q", b)
	}
	return st, false, nil
}

// reaped reports whether a stat line is a task's being released: its parent
// and group, the two fields after its state, gone (0 and -1). The kernel
// prints those (and a session of -1) when it finds the task's signal
// handlers already released, and it reads the state before it looks for
// them: a task that exits and is released between the two reads as R, S or
// D with them (one on a box: "(update-ca-certi) R 0 -1 -1", its PF_EXITING
// set), so the state says nothing here. The group's -1 is the one field of
// such a line utils.ParseProcStat refuses, and no line of a task that is
// there has it; both fields are asked all the same, so a line that does not
// parse for another reason (a read cut short, a parent of 0 included, or no
// stat line at all) is no process gone, but one keploy could not read
// (procStat's error).
func reaped(line []byte) bool {
	end := bytes.LastIndexByte(line, ')')
	if end < 0 {
		return false
	}
	f := bytes.Fields(line[end+1:])
	return len(f) >= 3 && string(f[1]) == "0" && string(f[2]) == "-1"
}

// statState is the state letter of a stat line, the field after the command
// name's last ')'; 0 where there is none.
func statState(line []byte) byte {
	end := bytes.LastIndexByte(line, ')')
	if end < 0 {
		return 0
	}
	rest := bytes.TrimLeft(line[end+1:], " ")
	if len(rest) == 0 {
		return 0
	}
	return rest[0]
}

// settleScans bounds the scans of one settled read of this process's
// children (settledChildren); two in a row that find the same ones running
// end it sooner. A variable for the tests.
var settleScans = 16

// scanChildren is the scan settledChildren repeats (procChildren); a
// variable for the tests of a read that never settles.
var scanChildren = procChildren

// reaping is held by the reaper for each of its passes (reapAdopted), and by
// a read of the children that settles (settledChildren) for all of its
// scans: no adopted child is reaped while that read runs, so one that exits
// during it stays, a zombie, in every scan after its exit.
var reaping sync.Mutex

// settledChildren is this process's children as a read that settled found
// them: procChildren, repeated until two scans in a row find the same ones,
// each by its pid and start time and by whether it runs or has exited (a
// zombie), the last of them; settled is false where settleScans scans did
// not. The reaper reaps none of them while it reads (reaping).
//
// A process becomes this one's child when its parent exits, or an ancestor
// between them does: an orphan goes to its nearest ancestor that is a child
// subreaper, this process. So a child X that runs once a settled read is over
// but is not in its last scan came while that scan ran, at the exit of a
// process E: a child of this one, or a deeper descendant whose ancestor A,
// then running, is one. That child (E, or A) is in the last scan, by the
// listing's order (procChildren): it was this one's before that scan began,
// or it came during it, the same case again, earlier; and no reap took it
// away. It was this one's in time for the scan before to list it, or the two
// differ. Each read it as it was then: the one before running, the last
// running, or, where it read it once it exited, a zombie. So the two scans
// differ, or the last lists it running: the last scan of a settled read
// lists, running, an ancestor of every running child it left out, and it is
// never empty while one runs. A descendant of a process of keploy's own
// (os/exec's children, the agent among them, whose own Wait may reap them
// while the read runs) is no daemon of the application's, and is left out of
// this.
func settledChildren() (children []child, settled bool, err error) {
	reaping.Lock()
	defer reaping.Unlock()
	var prev map[[3]uint64]bool
	for i := 0; i < settleScans; i++ {
		cs, err := scanChildren()
		if err != nil {
			return nil, false, err
		}
		now := make(map[[3]uint64]bool, len(cs))
		for _, c := range cs {
			exited := uint64(0)
			if c.zombie {
				exited = 1
			}
			now[[3]uint64{uint64(c.pid), c.start, exited}] = true
		}
		if prev != nil && maps.Equal(prev, now) {
			return cs, true, nil
		}
		prev, children = now, cs
	}
	return children, false, nil
}

// fdinfoDir is where heldPidfds reads this process's fds: "" for
// /proc/self/fdinfo, another directory in the tests of a read that fails
// (atomic, as the reaper reads it while a test sets it).
var fdinfoDir atomic.Pointer[string]

// fdinfoPath is the directory heldPidfds reads (fdinfoDir).
func fdinfoPath() string {
	if d := fdinfoDir.Load(); d != nil {
		return *d
	}
	return "/proc/self/fdinfo"
}

// ownFds is the pidfds the adopter itself holds (a reap's zombies, a stop's
// daemons): no sign that a child is one os/exec waits for. An adopted
// zombie a stop held a pidfd on was taken for keploy's own, and not reaped
// until a later child's exit. Changed, and read by heldPidfds, under its
// lock: an fd leaves it before it is closed, so its number, given again to
// a pidfd of os/exec's, is never skipped.
var ownFds = struct {
	sync.Mutex
	m map[int]bool
}{m: map[int]bool{}}

// openPidfd opens a pidfd on pid for the adopter (ownFds).
func openPidfd(pid int) (int, error) {
	ownFds.Lock()
	defer ownFds.Unlock()
	fd, err := unix.PidfdOpen(pid, 0)
	if err == nil {
		ownFds.m[fd] = true
	}
	return fd, err
}

// closePidfd closes a pidfd openPidfd opened.
func closePidfd(fd int) {
	ownFds.Lock()
	defer ownFds.Unlock()
	delete(ownFds.m, fd)
	_ = unix.Close(fd)
}

// heldPidfds is the set of pids this process holds a pidfd for, but the
// adopter's own (ownFds): a pidfd's fdinfo names its process ("Pid:"), and no
// other kind of fd has that line. A read that fails is an error, never a
// smaller set (a zombie of this process's own taken for adopted would be
// reaped from under its Wait); an fd closed meanwhile is no pidfd held.
func heldPidfds() (map[int]bool, error) {
	ownFds.Lock()
	defer ownFds.Unlock()
	skip := ownFds.m
	held := map[int]bool{}
	dir := fdinfoPath()
	fds, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list this process's fds: %w", err)
	}
	for _, f := range fds {
		fd, err := strconv.Atoi(f.Name())
		if err != nil || skip[fd] {
			continue
		}
		b, err := os.ReadFile(dir + "/" + f.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read fd %d's fdinfo: %w", fd, err)
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "Pid:"); ok {
				if pid, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && pid > 0 {
					held[pid] = true
				}
				break
			}
		}
	}
	return held, nil
}

// A stop reads the daemons' progress every stopCheck, and escalates once they
// made none for stopStallChecks checks' worth of the time both they and keploy
// could run (stopStall, utils.ExitClock): the count the CLI stops its agent by
// (utils.TeardownStallChecks), never a fixed grace, and never the checks of a
// daemon held off the CPU (a loaded host, its cgroup's quota spent), or held
// in the kernel on a disk that serves others first, which could not run to
// make progress; their waits in the kernel are taken for waits, since their
// last progress, for no longer in all than the kernel lets one task stand
// there before it reports it hung. A press of Ctrl+C a whole stopOffer after
// the stop's signal kills them at once. Variables for the tests.
var (
	stopCheck       = time.Second
	stopStallChecks = uint64(utils.TeardownStallChecks)
	stopOffer       = 5 * time.Second
)

// stopLooked, where a test sets it, is called at each check of a stop of the
// daemons (the tests count them); atomic, as a test may reset it while a stop
// it left runs on.
var stopLooked atomic.Pointer[func()]

// killMembers kills a stop's members once they make no progress on their way
// out (stopMembers). A variable for the tests of a kill the kernel holds: one
// that sends nothing leaves them there, as the kernel holds a killed process
// that waits in it without progress, and the stop finds them still there
// once it gives up on them.
var killMembers = func(ms []member) { signalAll(ms, syscall.SIGKILL, true, nil) }

// stopStall is the stall a stop of the daemons kills on: stopStallChecks
// checks of stopCheck, of the time both they and keploy could run; the
// longest Duration where that is longer.
func stopStall() time.Duration {
	if stopCheck > 0 && stopStallChecks > uint64(math.MaxInt64/int64(stopCheck)) {
		return math.MaxInt64
	}
	return time.Duration(stopStallChecks) * stopCheck
}

// leftToKernel is what an earlier stop killed and found still there, held by
// the kernel without progress, by pid and start time: no later stop waits for
// it again, and each says it left it running while it is there.
var leftToKernel sync.Map

// member is a process a stop ends: a daemon keploy adopted, or one it runs,
// as it was when the stop began, named by a pidfd opened on it, so that a pid
// reused while the stop waits is never signalled in its place.
type member struct {
	pid   int
	fd    int
	start uint64
	pgrp  int
	// leads: a daemon keploy adopted that leads its process group, which is
	// signalled with it, as the application's own groups are.
	leads bool
}

// stopAdopted ends what this process adopted from the application's tree,
// once the application has ended or keploy has stopped it for its test set.
// The daemons and what they run, as they are now, are asked to stop
// (SIGTERM, each daemon's group with it), but for those keploy's stop of the
// application asked already: a process its interrupt reached (reached, from
// utils.InterruptProcessTreeReached) that does not ignore SIGINT is acting
// on it, and is not asked again, as a second signal ends many an application
// mid-shutdown (Go's signal.NotifyContext once its stop is called). They are
// killed only once they make no progress on their way out -- a process or
// thread of theirs ending, a file or socket closed, storage read or written;
// being scheduled is not, a daemon that ignores the request runs on -- for
// stopStall of the time both they and keploy could run (utils.ExitClock: a
// daemon held off the CPU accrues no stall, nor one in the kernel on storage
// that serves until its waits there, since their last progress, outlast the
// kernel's hung-task time), and waited for until gone, so a daemon one test
// set started is never judged in the next and the application started again
// can bind its ports. While it
// waits, a drain does not give up on the run (utils.BeginDaemonStop): the
// next test set starts once it is over. What the kernel holds after the kill
// without progress is left to it, and said. The application served across
// test sets (serveTest) is one run: what it leaves ends with it. What a
// member starts after the stop began is not the stop's: orphaned, it is
// adopted, a member of the next run, and ended with it. A no-op unless
// adoptDaemons made this process a subreaper.
//
// It returns why it had to kill a daemon that made no progress on its way
// out, "" when it did not, or when the kill was the user's (Ctrl+C): a
// shutdown that kill cut is not known; and what of the application it may
// have left running (left, models.AppError.LeftRunning), "" only where it
// knows it left nothing. That is said for a daemon it cannot see, which it
// does not stop; for one the kernel holds after its kill, this stop's or an
// earlier one's; for a process a daemon started on its way out (what it
// adopts is read again once the stop is over); for every daemon where it
// cannot tell its own children from them, or where its children kept
// changing over as many reads of them as it makes (settledChildren); and for
// whatever there is where it was asked to adopt them and could not. What it
// adopts is read from a scan of /proc that never misses a process that is
// its child for the whole read (settledChildren), never from the kernel's
// quick children list, which can (childPids). Each step the daemons take on
// their way out is a movement of moved, the teardown scope of the run whose
// app left them (nil: none).
func stopAdopted(logger *zap.Logger, reached []utils.Reached, moved *utils.TeardownScope) (why, left string) {
	if !adopting.Load() {
		if no := cannotAdopt.Load(); no != nil {
			return "", "whatever the application left running: keploy cannot tell whether its processes ended (it could not adopt them: " + *no + ")"
		}
		return "", ""
	}
	read, err := adoptedRunning(logger)
	if err != nil {
		return "", "every daemon the application left: keploy could not tell its own processes from them (" + err.Error() + "), and stopped none"
	}
	var after []int
	if len(read.pids) > 0 || !read.settled {
		if len(read.pids) > 0 {
			why = stopAll(logger, read.pids, reached, moved)
		}
		// What a daemon started on its way out is adopted now, and runs;
		// what keploy cannot see, and what the kernel holds, are read as
		// they are now.
		read, err = adoptedRunning(logger)
		if err != nil {
			return why, "what the application's daemons started as they stopped: keploy could not tell its own processes from them (" + err.Error() + ")"
		}
		after = read.pids
	}
	return why, leftRunning(read, after)
}

// stopAll stops daemons as stopAdopted does, and says why it had to kill
// one.
func stopAll(logger *zap.Logger, daemons []int, reached []utils.Reached, moved *utils.TeardownScope) string {
	end := utils.BeginDaemonStop()
	defer end()
	ms := members(daemons)
	defer func() {
		for _, m := range ms {
			closePidfd(m.fd)
		}
	}()
	return stopMembers(logger, ms, actingOn(reached), moved)
}

// leftRunning says what of the application's daemons a stop left running, as
// its last read of them found: those keploy cannot see (unseen), which it
// does not stop, those the kernel holds after their kill (held), those the
// daemons started on their way out (after), and, where its children kept
// changing over the read, that what is left cannot be told. "" where none
// was.
func leftRunning(read adoptedRead, after []int) string {
	var parts []string
	if len(read.unseen) > 0 {
		parts = append(parts, fmt.Sprintf("daemons keploy cannot see (pids %v)", read.unseen))
	}
	if len(read.held) > 0 {
		parts = append(parts, fmt.Sprintf("daemons the kernel holds after their kill (pids %v)", read.held))
	}
	if len(after) > 0 {
		parts = append(parts, fmt.Sprintf("processes the daemons started as they stopped (pids %v)", after))
	}
	if !read.settled {
		parts = append(parts, fmt.Sprintf("whatever the application left: keploy's children kept changing over %d reads of them, so what runs on cannot be told", settleScans))
	}
	return strings.Join(parts, " and ")
}

// actingOn is whether a member is acting on keploy's interrupt: reached by
// it, the same process (its start time) as when it was, and not ignoring it.
func actingOn(reached []utils.Reached) func(member) bool {
	acting := make(map[[2]uint64]bool, len(reached))
	for _, r := range reached {
		if !r.Ignores {
			acting[[2]uint64{uint64(r.PID), r.Start}] = true
		}
	}
	return func(m member) bool { return acting[[2]uint64{uint64(m.pid), m.start}] }
}

// adoptedRead is what a stop reads of the daemons this process adopted
// (adoptedRunning).
type adoptedRead struct {
	// pids are the ones that run and that it may stop.
	pids []int
	// unseen are the ones this user may not see (not dumpable, or another
	// user's), which it leaves running; held, the ones an earlier stop
	// killed and left to the kernel, still there.
	unseen, held []int
	// settled is false where its children kept changing over every scan of
	// the read (settledChildren).
	settled bool
}

// adoptedRunning reads this process's children (settledChildren) for the
// ones it adopted (no pidfd of its own names them) and that run, but keploy
// itself (an agent whose sudo is gone); those it may not see, and those an
// earlier stop left to the kernel, it says, and returns apart. One whose main
// thread exited while another runs on runs, and what it runs is read through
// that thread (child.thread). An error is a read that failed, of its children
// or of its own pidfds: which of its children run, or which are its own,
// cannot be told, so it names none, to stop none.
func adoptedRunning(logger *zap.Logger) (adoptedRead, error) {
	children, settled, err := settledChildren()
	if err != nil {
		return adoptedRead{}, fmt.Errorf("read which processes are keploy's children: %w", err)
	}
	held, err := heldPidfds()
	if err != nil {
		failedClosed(logger, err)
		return adoptedRead{}, err
	}
	self, selfErr := os.Stat("/proc/self/exe")
	read := adoptedRead{settled: settled}
	for _, c := range children {
		if c.zombie || held[c.pid] {
			continue
		}
		if _, gone := leftToKernel.Load([2]uint64{uint64(c.pid), c.start}); gone {
			read.held = append(read.held, c.pid)
			continue
		}
		exe, err := os.Stat(exePath(c))
		if err != nil {
			logger.Info("keploy cannot see what a daemon the application left running is (it is not dumpable, or another user's), and leaves it running",
				zap.Int("pid", c.pid), zap.Error(err))
			read.unseen = append(read.unseen, c.pid)
			continue
		}
		if selfErr == nil && os.SameFile(exe, self) {
			continue // keploy's own
		}
		read.pids = append(read.pids, c.pid)
	}
	return read, nil
}

// exePath is where what child c runs is read: its /proc/<pid>/exe, or, where
// its main thread exited while another runs on (whose exe reads as none, its
// memory gone with it), that of the thread that runs.
func exePath(c child) string {
	if c.thread != 0 {
		return "/proc/" + strconv.Itoa(c.pid) + "/task/" + strconv.Itoa(c.thread) + "/exe"
	}
	return "/proc/" + strconv.Itoa(c.pid) + "/exe"
}

// members is each daemon of pids and every process it runs, now, each named
// by a pidfd; a process that is gone, or has exited (every thread of it,
// utils.Exited), or whose pid names another process by the time its pidfd is
// opened, is left out.
func members(pids []int) []member {
	var ms []member
	seen := map[int]bool{}
	var add func(pid, parent int, top bool)
	add = func(pid, parent int, top bool) {
		if seen[pid] {
			return
		}
		seen[pid] = true
		before, ok := utils.ReadProcStat(pid)
		if !ok || before.PPID != parent || utils.Exited(pid, before) {
			return
		}
		fd, err := openPidfd(pid)
		if err != nil {
			return
		}
		if after, ok := utils.ReadProcStat(pid); !ok || after.StartTime != before.StartTime || after.PPID != parent {
			closePidfd(fd)
			return
		}
		ms = append(ms, member{pid: pid, fd: fd, start: before.StartTime, pgrp: before.PGRP, leads: top && before.PGRP == pid})
		for _, child := range processChildren(pid) {
			add(child, pid, false)
		}
	}
	self := os.Getpid()
	for _, pid := range pids {
		add(pid, self, true)
	}
	return ms
}

// stopMembers asks ms to stop, but those acting on keploy's interrupt
// already, kills them once they make no progress on their way out for
// stopStall of the time both they and keploy could run (or at once, at the
// press of Ctrl+C the stop offers), and waits until each has exited, or is
// held by the kernel without progress for as long (leftToKernel, which the
// next read of the daemons says, adoptedRunning). stallKill says why it
// killed for want of progress; "" when it did not.
func stopMembers(logger *zap.Logger, ms []member, acting func(member) bool, moved *utils.TeardownScope) (stallKill string) {
	pids := make([]int, 0, len(ms))
	var stopping []int
	for _, m := range ms {
		pids = append(pids, m.pid)
		if acting(m) {
			stopping = append(stopping, m.pid)
		}
	}
	logger.Info("stopping the daemons the application left running: they are part of its run, and end with it",
		zap.Ints("pids", pids), zap.Ints("acting_on_the_interrupt", stopping))
	// Whether each ignores SIGTERM as it is asked to stop with it: one that
	// does has no way out of its own for a kill to cut. Read now, not at the
	// kill: a handler may ignore further TERMs while it shuts down.
	ignored := ignoresSigtermNow(ms)
	signalAll(ms, syscall.SIGTERM, false, acting)
	signalAll(ms, syscall.SIGCONT, false, nil) // a stopped one handles it now
	notice := utils.NewStopNowNotice(stopOffer)
	defer notice.Withdraw()
	t := time.NewTicker(stopCheck)
	defer t.Stop()
	procs := make([]utils.Reached, 0, len(ms))
	for _, m := range ms {
		procs = append(procs, utils.Reached{PID: m.pid, Start: m.start})
	}
	clock, meter, stopMeter := utils.NewExitMeter(procs, stopStall())
	defer stopMeter()
	meter.Advance(markExit(ms))
	killed, offering := false, false
	kill := func(why string) {
		logger.Warn(why, zap.Ints("pids", running(ms)))
		killMembers(ms)
		killed = true
		meter.Reset()
		// What is left the kill ends, or the kernel holds: no press
		// does more, and a closed channel read again would spin.
		notice.Withdraw()
	}
	for {
		left := running(ms)
		if len(left) == 0 {
			return
		}
		select {
		case <-t.C:
		case <-notice.Interrupt():
			if !killed {
				kill("killing the daemons the application left running at once, at the press of Ctrl+C")
			}
			continue
		}
		if looked := stopLooked.Load(); looked != nil {
			(*looked)()
		}
		if !killed && !offering && notice.Next() {
			offering = true
			logger.Info("keploy waits for the daemons the application left running to stop, as long as they make progress on their way out; press Ctrl+C again to kill them now",
				zap.Ints("pids", left))
		}
		if meter.Advance(markExit(ms)) {
			moved.Note()
			continue
		}
		if !meter.Expired() {
			continue
		}
		if killed {
			logger.Warn("a daemon the application left running was killed and is still there, held by the kernel without progress; keploy goes on without it",
				zap.Ints("pids", left), zap.String("next_step", "check what it waits for in the kernel (cat /proc/<pid>/stack); it ends once that wait does"))
			for _, m := range ms {
				if !m.exited() {
					leftToKernel.Store([2]uint64{uint64(m.pid), m.start}, true)
				}
			}
			return
		}
		// A kill cuts the way out of a process that was on one: one acting
		// on keploy's interrupt, or one that does not ignore the SIGTERM it
		// was asked to stop with. One that ignores it has no way out to
		// cut (a server daemon with no stop of its own): its kill is said,
		// and leaves the set judged.
		cut, uncut := cutBy(ms, acting, ignored)
		stalled := describe(cut)
		kill("a daemon the application left running made no progress on its way out after it was asked to stop; killing it")
		if len(cut) > 0 {
			stallKill = fmt.Sprintf("%s, which the application left running, made no progress on its way out for %s after keploy asked it to stop, so keploy killed it; make it exit once it is asked to stop (SIGINT, or SIGTERM where it ignores SIGINT), or end it in the application's own shutdown", stalled, clock.Said(meter.Grace()))
		} else {
			logger.Info("the daemons keploy killed ignored SIGTERM when it asked them to stop with it: they had no way out of their own to cut, so the set's end is judged",
				zap.String("killed", describe(uncut)),
				zap.String("next_step", "give such a daemon a stop of its own (a SIGTERM handler), or end it in the application's own shutdown"))
		}
	}
}

// signalAll sends sig to each process group a daemon of ms leads, while the
// daemon is the process its pidfd names, as keploy stops the application's
// own groups (the whole group, where pg_ctl stop and docker stop signal the
// leader alone: postgres's backends end on their own then, not at its
// postmaster's word, which a run's end does not need), and through its pidfd
// to each member outside those groups; with every, to each member as well.
// The daemon is the process its pidfd names while that pidfd shows it alive:
// its pid, and so its group's id, cannot have been given to another.
//
// A member skip holds for is not sent sig, nor is its group as a whole: a
// daemon's group is then signalled member by member.
func signalAll(ms []member, sig syscall.Signal, every bool, skip func(member) bool) {
	if skip == nil {
		skip = func(member) bool { return false }
	}
	groups := map[int]bool{}
	for _, m := range ms {
		if m.leads && !skip(m) && unix.PidfdSendSignal(m.fd, 0, nil, 0) == nil {
			groups[m.pgrp] = true
			_ = syscall.Kill(-m.pid, sig)
		}
	}
	for _, m := range ms {
		if (every || !groups[m.pgrp]) && !skip(m) {
			_ = unix.PidfdSendSignal(m.fd, sig, nil, 0)
		}
	}
}

// cutBy splits the members of ms still running by whether a kill now cuts
// their way out: cut, those acting on keploy's interrupt and those that did
// not ignore SIGTERM when the stop asked them to stop with it (ignored);
// uncut, the rest.
func cutBy(ms []member, acting func(member) bool, ignored map[[2]uint64]bool) (cut, uncut []member) {
	for _, m := range ms {
		if m.exited() {
			continue
		}
		if acting(m) || !ignored[[2]uint64{uint64(m.pid), m.start}] {
			cut = append(cut, m)
		} else {
			uncut = append(uncut, m)
		}
	}
	return cut, uncut
}

// ignoresSigtermNow is which members of ms ignore SIGTERM now, by pid and
// start time; one whose status cannot be read is taken for one that does not.
func ignoresSigtermNow(ms []member) map[[2]uint64]bool {
	ignored := make(map[[2]uint64]bool, len(ms))
	for _, m := range ms {
		if ignoresSigterm(m.pid) {
			ignored[[2]uint64{uint64(m.pid), m.start}] = true
		}
	}
	return ignored
}

// ignoresSigterm reports whether process pid ignores SIGTERM: the SIGTERM bit
// (15, bit 14) of its status's SigIgn mask. A process whose status cannot be
// read is taken for one that does not.
func ignoresSigterm(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "SigIgn:"); ok {
			mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			return err == nil && mask&(1<<(uint(syscall.SIGTERM)-1)) != 0
		}
	}
	return false
}

// describe names each member of ms still running: its pid, command and
// state ("process 4242 (sleep, state S)").
func describe(ms []member) string {
	var names []string
	for _, m := range ms {
		if m.exited() {
			continue
		}
		name := "process " + strconv.Itoa(m.pid)
		comm, err := os.ReadFile("/proc/" + strconv.Itoa(m.pid) + "/comm")
		st, ok := utils.ReadProcStat(m.pid)
		switch {
		case err == nil && ok:
			name += fmt.Sprintf(" (%s, state %c)", strings.TrimSpace(string(comm)), st.State)
		case err == nil:
			name += " (" + strings.TrimSpace(string(comm)) + ")"
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "a process"
	}
	return strings.Join(names, ", ")
}

// exited reports whether m has exited: its pidfd is readable then.
func (m member) exited() bool {
	fds := []unix.PollFd{{Fd: int32(m.fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	return err == nil && n > 0 && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0
}

// running is the pids of ms that have not exited.
func running(ms []member) []int {
	var pids []int
	for _, m := range ms {
		if !m.exited() {
			pids = append(pids, m.pid)
		}
	}
	return pids
}

// markExit reads how far a stop's members are on their way out
// (utils.ExitMark): those whose pidfd shows them exited are not read.
func markExit(ms []member) utils.ExitMark {
	procs := make([]utils.Reached, 0, len(ms))
	for _, mb := range ms {
		if !mb.exited() {
			procs = append(procs, utils.Reached{PID: mb.pid, Start: mb.start})
		}
	}
	return utils.MarkExit(procs)
}

// processChildren is the processes pid runs: its threads' children lists, or,
// where the kernel keeps none, every process whose parent it is.
func processChildren(pid int) []int {
	dir := "/proc/" + strconv.Itoa(pid) + "/task"
	tasks, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var pids []int
	listed := false
	for _, t := range tasks {
		b, err := os.ReadFile(dir + "/" + t.Name() + "/children")
		if err != nil {
			continue
		}
		listed = true
		for _, f := range strings.Fields(string(b)) {
			if c, err := strconv.Atoi(f); err == nil {
				pids = append(pids, c)
			}
		}
	}
	if listed {
		return pids
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, p := range procs {
		c, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		if st, ok := utils.ReadProcStat(c); ok && st.PPID == pid {
			pids = append(pids, c)
		}
	}
	return pids
}

var childrenUnreadOnce sync.Once

// childrenUnread says, once at Warn and then at Debug, that the reaper could
// not read this process's children, and so reaped none of the daemons it
// adopted while that lasted.
func childrenUnread(logger *zap.Logger, err error) {
	said := false
	childrenUnreadOnce.Do(func() {
		said = true
		logger.Warn("keploy could not read its children, so it reaps none of the daemons it adopted while that lasts; a daemon that exits then stays a zombie until a later read works", zap.Error(err))
	})
	if !said {
		logger.Debug("keploy could not read its children", zap.Error(err))
	}
}

var failedClosedOnce sync.Once

// failedClosed says, once at Warn and then at Debug, that keploy could not
// tell its own children from the ones it adopted, and so neither reaped nor
// stopped any.
func failedClosed(logger *zap.Logger, err error) {
	said := false
	failedClosedOnce.Do(func() {
		said = true
		logger.Warn("keploy could not read which children it waits for itself, so it neither reaps nor stops the daemons it adopted while that lasts", zap.Error(err))
	})
	if !said {
		logger.Debug("keploy could not read which children it waits for itself", zap.Error(err))
	}
}
