package testproc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// diesEnv names how TestDyingTestHelper ends, and the directory it writes
// the pids of what it started to.
const diesEnv = "KEPLOY_TESTPROC_DIES"

// TestDyingTestHelper starts a group (a shell that spins on a sleep, as the
// stop tests' apps do, and, for a test that fails, a child of the shell that
// runs until it is killed), or for one that does not, a daemon in a session
// of its own that runs until it is killed; and, once the test that runs it
// says so on its stdin, ends as diesEnv says: "fails" fails a subtest that
// started the group, then waits for its stdin's end; "panics" panics off the
// test's goroutine, as a test binary's timeout does; "killed" waits to be
// killed, as at a job's end. What runs until it is killed waits on this
// process's lifeline (UntilTheTestEnds).
func TestDyingTestHelper(t *testing.T) {
	how, dir, ok := strings.Cut(os.Getenv(diesEnv), " ")
	if !ok {
		t.Skip("a process of TestATestThatEndsEarlyLeavesNothingOfItsGroup")
	}
	if err := SetLifeline(); err != nil {
		t.Fatal(err)
	}
	stdin := bufio.NewReader(os.Stdin)
	run := func(t *testing.T) {
		script := `while :; do sleep 0.01; done`
		if how == "fails" {
			script = UntilTheTestEnds + ` & echo $! > "$1/member"; ` + script
		}
		cmd := exec.Command("sh", "-c", script, "sh", dir)
		Start(t, cmd)
		pids := strconv.Itoa(cmd.Process.Pid)
		other := "member"
		if how != "fails" {
			// Started as a daemon is, by a child that exits: in a session
			// of its own, reparented, out of reach of a kill of the group.
			other = "daemon"
			daemon := exec.Command("sh", "-c", `setsid sh -c "$0" sh "$1" >/dev/null 2>&1 </dev/null &`,
				`echo $$ > "$1"; exec `+UntilTheTestEnds, filepath.Join(dir, other))
			if out, err := daemon.CombinedOutput(); err != nil {
				t.Fatalf("start the daemon: %v\n%s", err, out)
			}
		}
		var pid string
		for {
			b, err := os.ReadFile(filepath.Join(dir, other))
			if err == nil && strings.HasSuffix(string(b), "\n") {
				pid = strings.TrimSpace(string(b))
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		// Each waits once it is cat: the shell that runs it has opened the
		// lifeline, or it would have exited instead.
		for {
			stat, err := os.ReadFile("/proc/" + pid + "/stat")
			if err != nil || strings.Contains(string(stat), ") Z ") {
				t.Fatalf("the %s (%s) ended before it waited on the lifeline", other, pid)
			}
			if strings.Contains(string(stat), "(cat) ") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		pids += " " + pid
		// Whole once there: written, then renamed.
		if err := os.WriteFile(filepath.Join(dir, "pids.part"), []byte(pids), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(dir, "pids.part"), filepath.Join(dir, "pids")); err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.ReadString('\n'); err != nil {
			t.Fatalf("no word to end: %v", err)
		}
		switch how {
		case "fails":
			t.Fatal("the test fails before its end")
		case "panics":
			time.AfterFunc(0, func() { panic("the test timed out") })
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if how != "fails" {
		run(t)
		return
	}
	t.Run("early", run)
	_, _ = io.Copy(io.Discard, stdin)
}

// A test that ends before its end leaves nothing of a group it started:
// one that fails has the group killed by its cleanup, which leaves a
// process the leader started no more than the leader; one whose process
// dies with no cleanup run (its timeout's panic, a kill at a job's end) has
// the leader killed by the kernel. The stop tests' apps were started in a
// group the cleanup killed, with no death signal: a test binary that timed
// out, or was killed as its job ended, left an app's shell waking every
// 10 ms for hours after. And a daemon such a test started, in a session of
// its own, that waits on the test process's lifeline ends with that process
// too, as a sleep in its place did not.
func TestATestThatEndsEarlyLeavesNothingOfItsGroup(t *testing.T) {
	for _, how := range []string{"fails", "panics", "killed"} {
		t.Run(how, func(t *testing.T) {
			dir := t.TempDir()
			// Its output to a file: a pipe would wait on what it leaves.
			out, err := os.Create(filepath.Join(dir, "out"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = out.Close() }()
			cmd := exec.Command(os.Args[0], "-test.run=^TestDyingTestHelper$", "-test.v")
			cmd.Env = append(os.Environ(), diesEnv+"="+how+" "+dir)
			cmd.Stdout, cmd.Stderr = out, out
			word, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = word.Close() }()
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			helper := cmd.Process.Pid
			reaped := make(chan struct{})
			go func() { _ = cmd.Wait(); close(reaped) }()
			// Whatever is left of its session, as a row that fails may leave
			// it (the helper started in a group of its own, out of reach of
			// its own group's kill), ends with the row.
			t.Cleanup(func() {
				_ = syscall.Kill(-helper, syscall.SIGKILL)
				<-reaped
				for _, p := range session(helper) {
					_ = syscall.Kill(p, syscall.SIGKILL)
				}
			})
			said := func() string { b, _ := os.ReadFile(filepath.Join(dir, "out")); return string(b) }

			var pids []int
			for expired := beforeTheDeadline(t); pids == nil; {
				if b, err := os.ReadFile(filepath.Join(dir, "pids")); err == nil {
					for _, f := range strings.Fields(string(b)) {
						pid, err := strconv.Atoi(f)
						if err != nil {
							t.Fatalf("pids %q", b)
						}
						pids = append(pids, pid)
					}
					break
				}
				select {
				case <-reaped:
					t.Fatalf("the helper ended before it started its group:\n%s", said())
				case <-expired:
					t.Fatalf("the helper did not start its group:\n%s", said())
				case <-time.After(10 * time.Millisecond):
				}
			}
			// Each is held by a pidfd before the helper is let end, so that a
			// pid reused once it is gone reads as gone; and what is left is
			// killed through it.
			fds := make([]int, len(pids))
			for i, pid := range pids {
				fd, err := unix.PidfdOpen(pid, 0)
				if err != nil {
					t.Fatalf("pid %d, still waiting on the helper's word: %v", pid, err)
				}
				fds[i] = fd
				t.Cleanup(func() { _ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); _ = unix.Close(fd) })
			}
			if _, err := word.Write([]byte("end\n")); err != nil {
				t.Fatal(err)
			}
			if how == "killed" {
				_ = syscall.Kill(helper, syscall.SIGKILL)
			}
			if how != "fails" {
				<-reaped
			}
			// A test that failed is checked while its process runs on: its
			// cleanup, not its end, ended the group.
			bound := beforeTheDeadline(t)
			for i, fd := range fds {
				if !exits(t, fd, bound) {
					t.Fatalf("process %d (of %v) outlived its test, which %s:\n%s", pids[i], pids, how, said())
				}
			}
			select {
			case <-reaped:
				if how == "fails" {
					t.Fatalf("the helper ended before its stdin's end:\n%s", said())
				}
			default:
			}
			_ = word.Close()
			<-reaped
			if how == "killed" {
				if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
					t.Fatalf("the helper ended %v, not killed:\n%s", cmd.ProcessState, said())
				}
				return
			}
			want := map[string]string{"fails": "--- FAIL: TestDyingTestHelper/early", "panics": "panic: the test timed out"}[how]
			if !strings.Contains(said(), want) {
				t.Fatalf("the helper did not end as one that %s (%q):\n%s", how, want, said())
			}
		})
	}
}

// session is every process of session sid (its stat's field 6).
func session(sid int) []int {
	ents, _ := os.ReadDir("/proc")
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// The fields after the command, which is in parentheses and may hold
		// any byte: state, ppid, pgrp, session.
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		if len(f) > 3 && f[3] == strconv.Itoa(sid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// exits reports whether the process behind pidfd exits before bound fires;
// the bound only turns a hang into a failure.
func exits(t *testing.T, pidfd int, bound <-chan time.Time) bool {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, 10)
		if n > 0 {
			return true
		}
		if err != nil && !errors.Is(err, unix.EINTR) {
			t.Fatalf("poll pidfd: %v", err)
		}
		select {
		case <-bound:
			return false
		default:
		}
	}
}

// A command that cannot start fails its test, and leaves no cleanup.
func TestACommandThatCannotStartFailsItsTest(t *testing.T) {
	ft := &fatalTB{TB: t}
	absent := filepath.Join(t.TempDir(), "absent")
	done := make(chan struct{})
	go func() {
		defer close(done)
		Start(ft, exec.Command(absent))
	}()
	<-done
	if !strings.Contains(ft.fatal, "absent") {
		t.Fatalf("the start failed as %q; want its failure", ft.fatal)
	}
	if len(ft.cleanups) != 0 {
		t.Fatalf("a start that failed left %d cleanups", len(ft.cleanups))
	}
}

// The thread that started a process runs nothing else until the process is
// reaped. The kernel kills the process as that thread ends
// (PR_SET_PDEATHSIG), and the runtime ends a thread that a goroutine locked
// and exited on, as a test's goroutine may: a thread that other goroutines
// run on is one that could end so, and the process with it, while its test
// runs. While the goroutine that waits on the process waits, goroutines that
// each lock a thread and keep it are started, until one runs on its thread
// or on a thread made since (see othersRunOnThisThread). First, the thread
// the goroutine waits on is the process's parent, the thread that started
// it: a goroutine that moved to another thread after it started the process
// left that parent to the runtime.
func TestTheThreadThatStartedAProcessRunsNothingElseUntilItIsReaped(t *testing.T) {
	if err := SetLifeline(); err != nil {
		t.Fatal(err)
	}
	// One P: a goroutine that waits hands its thread to the next one to
	// run, the one it started, unless it is locked to that thread.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	cmd := exec.Command("sh", "-c", "exec "+UntilTheTestEnds)
	type look struct {
		thread   int
		children []int
		err      error
		others   bool
	}
	ran := make(chan look, 1)
	setWaiting(t, func() {
		l := look{thread: unix.Gettid()}
		l.children, l.err = childrenOf(l.thread)
		l.others = othersRunOnThisThread()
		ran <- l
	})
	reaped := Start(t, cmd)
	var l look
	select {
	case l = <-ran:
	case <-beforeTheDeadline(t):
		t.Fatal("Start did not run its hook before it waited on the process")
	}
	if l.err != nil {
		t.Fatal(l.err)
	}
	if !slices.Contains(l.children, cmd.Process.Pid) {
		t.Fatalf("the process (%d) is not a child of the thread (%d) that waits on it, whose children are %v: "+
			"the thread that started it, whose end the kernel kills it on, is one the runtime may end while the test runs",
			cmd.Process.Pid, l.thread, l.children)
	}
	if l.others {
		t.Fatal("a goroutine ran on the thread that started the process, while the goroutine that waits on the process waited, " +
			"and exited locked to it: the runtime ended the thread, and the kernel killed the process with it")
	}
	select {
	case <-reaped:
		t.Fatalf("the process ended (%v) while its test ran", cmd.ProcessState)
	default:
	}
}

// othersRunOnThisThread reports whether another goroutine runs on this
// goroutine's thread while this one waits. It starts goroutines, one at a
// time, that each lock the thread they run on and keep it, as long as this
// one runs: one that runs on this goroutine's thread exits, locked, which
// ends the thread; one that runs on a thread made since this call shows that
// no thread was idle, this one's included, as the runtime makes one only
// then. Each keeps a thread of its own, so no more of them start than there
// were threads, and one.
func othersRunOnThisThread() bool {
	this := unix.Gettid()
	before := threads()
	done := make(chan struct{})
	defer close(done)
	for {
		on := make(chan int)
		go func() {
			runtime.LockOSThread()
			tid := unix.Gettid()
			on <- tid
			if tid == this {
				return
			}
			<-done
			runtime.UnlockOSThread()
		}()
		tid := <-on
		if tid == this {
			return true
		}
		if !before[tid] {
			return false
		}
	}
}

// childrenOf lists the children of thread tid of this process: the
// processes it started (by fork or clone), not those of its other threads.
func childrenOf(tid int) ([]int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/children", tid))
	if err != nil {
		return nil, fmt.Errorf("read thread %d's children: %w", tid, err)
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("thread %d's children: %q is no pid", tid, f)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// threads is the set of this process's threads.
func threads() map[int]bool {
	ents, _ := os.ReadDir("/proc/self/task")
	tids := make(map[int]bool, len(ents))
	for _, e := range ents {
		if tid, err := strconv.Atoi(e.Name()); err == nil {
			tids[tid] = true
		}
	}
	return tids
}

// A test's cleanup returns once the process it killed is reaped: the test's
// cleanups after it (its TempDir's removal among them) run with the process
// gone, and cmd.ProcessState set. The reap is held back until the cleanup is
// seen waiting on a channel (for the reap); one that returns first fails.
func TestACleanupReturnsOnceTheProcessItKilledIsReaped(t *testing.T) {
	if err := SetLifeline(); err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	var once sync.Once
	letReap := func() { once.Do(func() { close(hold) }) }
	setWaiting(t, func() { <-hold })
	ft := &fatalTB{TB: t}
	cmd := exec.Command("sh", "-c", "exec "+UntilTheTestEnds)
	reaped := Start(ft, cmd)
	// A row that fails leaves nothing: not reaped, the pid is still its own.
	t.Cleanup(func() {
		select {
		case <-reaped:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		letReap()
		<-reaped
	})
	if len(ft.cleanups) != 1 {
		t.Fatalf("Start left %d cleanups; want 1", len(ft.cleanups))
	}
	returned := make(chan struct{})
	go runCleanup(ft.cleanups[0], returned)
	for expired := beforeTheDeadline(t); !receiving("testproc.runCleanup"); {
		select {
		case <-returned:
			t.Fatalf("the cleanup returned before the process it killed was reaped (state %v)", cmd.ProcessState)
		case <-expired:
			t.Fatal("the cleanup neither returned nor waited")
		case <-time.After(10 * time.Millisecond):
		}
	}
	letReap()
	<-returned
	select {
	case <-reaped:
	default:
		t.Fatal("the cleanup returned before the process it killed was reaped")
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("the process ended %v, not killed", cmd.ProcessState)
	}
}

// runCleanup runs a test's cleanup, and closes returned once it returns.
func runCleanup(cleanup func(), returned chan<- struct{}) {
	cleanup()
	close(returned)
}

// receiving reports whether a goroutine that runs fn (as a stack names it,
// package.function) waits on a channel receive.
func receiving(fn string) bool {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		head, frames, _ := strings.Cut(g, "\n")
		if strings.Contains(head, "[chan receive") && strings.Contains(frames, fn+"(") {
			return true
		}
	}
	return false
}

// setWaiting sets Start's hook (waiting) until t ends.
func setWaiting(t *testing.T, f func()) {
	waiting = f
	t.Cleanup(func() { waiting = nil })
}

// fatalTB records its Fatalf and Cleanup, and ends its goroutine on Fatalf.
type fatalTB struct {
	testing.TB
	fatal    string
	cleanups []func()
}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (f *fatalTB) Cleanup(c func()) { f.cleanups = append(f.cleanups, c) }

// beforeTheDeadline fires a quarter of the way to the test binary's
// deadline, and never with none.
func beforeTheDeadline(t *testing.T) <-chan time.Time {
	t.Helper()
	dl, ok := t.Deadline()
	if !ok {
		return nil
	}
	timer := time.NewTimer(time.Until(dl) / 4)
	t.Cleanup(func() { timer.Stop() })
	return timer.C
}
