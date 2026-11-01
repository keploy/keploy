//go:build linux

package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/client/app"
	"go.keploy.io/server/v3/pkg/models"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// adoptHelperEnv names the test a process of its own runs: a subreaper is a
// property of the whole process, which the other tests of this package share.
const adoptHelperEnv = "KEPLOY_TEST_ADOPT_HELPER"

// inOwnProcess runs t in a process of its own and reports whether this is
// that process; in the test's own, it fails or skips t as that process did.
// The test's own process ends every process it still runs as the test ends
// (endWhatItStarted), so one that ends early leaves none behind.
func inOwnProcess(t *testing.T) bool {
	t.Helper()
	if os.Getenv(adoptHelperEnv) == t.Name() {
		if err := goUsesPidfds(); err != nil {
			t.Skipf("keploy adopts nothing here: %v", err)
		}
		// The first cleanup, so the last to run.
		t.Cleanup(endWhatItStarted)
		return true
	}
	cmd := exec.Command(os.Args[0], ownProcessArgs(t, t.Name())...)
	cmd.Env = append(os.Environ(), adoptHelperEnv+"="+t.Name())
	// A process of its own that hangs ends at its own timeout
	// (ownProcessArgs), and if this test binary ends first, with it,
	// rather than outliving it holding what it adopted. The signal comes
	// when the thread that started it exits: it is held for as long as
	// the process runs.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	runtime.LockOSThread()
	out, err := cmd.CombinedOutput()
	runtime.UnlockOSThread()
	if err != nil {
		t.Fatalf("the test's own process failed: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("--- SKIP")) {
		t.Skipf("skipped in its own process:\n%s", out)
	}
	return false
}

// ownProcessArgs runs test name alone in a copy of this test binary, with a
// timeout of its own: three quarters of the time left to this binary's. So
// the bounds of the copy's waits (beforeTheDeadline) have a deadline to come
// before, and they say what it waited on while this binary still reads its
// output, instead of this binary's timeout ending both and saying only that
// it ran out. (The copy's own timeout panic runs no cleanups, so its output
// ends only once nothing it started still holds it.) With no timeout here,
// the copy has none either.
func ownProcessArgs(t *testing.T, name string) []string {
	args := []string{"-test.run=^" + name + "$", "-test.v", "-test.count=1"}
	if dl, ok := t.Deadline(); ok {
		if left := time.Until(dl) * 3 / 4; left > 0 {
			args = append(args, "-test.timeout="+left.String())
		}
	}
	return args
}

// endWhatItStarted kills every process this one still runs: each it started,
// and each those started (sweep). A test in its own process that ends before
// the stop it judges (failed, or skipped) leaves none: an application keploy
// runs writes to this process's output, which the test binary that started
// it reads to its end, and one left behind would hold that open, the test
// binary waiting on its end (an app's `exec sleep 600`) instead of the
// test's.
func endWhatItStarted() {
	self := os.Getpid()
	sweep(func() []utils.Reached {
		var procs []utils.Reached
		for _, p := range utils.TreeOf(self) {
			if p.PID != self {
				procs = append(procs, p)
			}
		}
		return procs
	})
}

// sweep kills every process tree finds, each named by its pid and start time.
// Each found is stopped first (SIGSTOP), before tree is looked at again, until
// a look finds none not found, so none starts another or leaves its children
// to init meanwhile; then each is killed. Each is signalled through a pidfd
// of it as it was found: a pid that names another process by the time it is
// opened (or none) is signalled nothing. A process killed is not waited for.
func sweep(tree func() []utils.Reached) {
	found := map[utils.Reached]int{}
	defer func() {
		for _, fd := range found {
			if fd >= 0 {
				_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
				_ = unix.Close(fd)
			}
		}
	}()
	for {
		fresh := false
		for _, p := range tree() {
			if _, ok := found[p]; ok {
				continue
			}
			fresh = true
			fd, err := unix.PidfdOpen(p.PID, 0)
			if now, ok := utils.ProcOf(p.PID); err != nil || !ok || now != p {
				// Gone, or another process by its pid now.
				if err == nil {
					_ = unix.Close(fd)
				}
				found[p] = -1
				continue
			}
			found[p] = fd
			_ = unix.PidfdSendSignal(fd, unix.SIGSTOP, nil, 0)
		}
		if !fresh {
			return
		}
	}
}

// leftoverDirEnv names the directory TestOwnProcessLeftoverHelper writes in.
const leftoverDirEnv = "KEPLOY_TEST_LEFTOVER_DIR"

// TestOwnProcessLeftoverHelper is not a test: with leftoverDirEnv set it is a
// test's own process (inOwnProcess) that starts an application as keploy
// runs a native one, a shell ignoring SIGHUP that starts a child it leaves
// running and then starts one after another for as long as it runs; writes
// the pid and start time of each of the two once it runs; and fails while
// they do, before any stop of them.
func TestOwnProcessLeftoverHelper(t *testing.T) {
	dir := os.Getenv(leftoverDirEnv)
	if dir == "" {
		t.Skip("a process of TestATestInItsOwnProcessLeavesNothingBehind")
	}
	if !inOwnProcess(t) {
		return
	}
	// The app and its child ignore SIGHUP, as a daemon does: the hangup the
	// kernel sends a process group orphaned with members stopped does not end
	// them, so only a kill does.
	client := newRunClient(fmt.Sprintf("trap '' HUP; sleep 600 & echo $! > %s; echo $$ > %s; while kill -0 \"$DAEMON_OWNER\"; do sleep 0.01; done",
		filepath.Join(dir, "child.pid"), filepath.Join(dir, "app.pid")))
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_ = client.Run(context.Background(), models.RunOptions{})
	}()
	// The app writes its child's pid, then its own.
	var procs []utils.Reached
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "app.pid")); err == nil && bytes.HasSuffix(b, []byte("\n")) {
			break
		}
		select {
		case <-ran:
			t.Fatal("the run ended before its app wrote its pid")
		case <-time.After(time.Millisecond):
		}
	}
	var said []byte
	for _, name := range []string{"app", "child"} {
		b, err := os.ReadFile(filepath.Join(dir, name+".pid"))
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(string(bytes.TrimSpace(b)))
		if err != nil {
			t.Fatalf("the app wrote %q, not its %s's pid", b, name)
		}
		p, ok := utils.ProcOf(pid)
		if !ok {
			t.Fatalf("the %s (%d) was gone once the app ran", name, pid)
		}
		procs = append(procs, p)
		said = fmt.Appendf(said, "%d %d\n", p.PID, p.Start)
	}
	if err := os.WriteFile(filepath.Join(dir, "procs"), said, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("failing while the app (%d) runs", procs[0].PID)
}

// A test run in its own process (inOwnProcess) that ends before the stop it
// judges (failed, or skipped: a hold the kernel refuses) leaves nothing it
// started running. An application keploy runs writes to keploy's own output,
// which the test binary that started the test's process reads to its end:
// one left behind (an app's `exec sleep 600`) holds it open, and the test
// binary waits on the app's end, not the test's, until its own timeout, and
// says only that it timed out, not why the test failed.
func TestATestInItsOwnProcessLeavesNothingBehind(t *testing.T) {
	if err := goUsesPidfds(); err != nil {
		t.Skipf("keploy adopts nothing here: %v", err)
	}
	dir := t.TempDir()
	// A file, not a pipe: the wait for the process ends with it, whatever
	// it left holding the file.
	out, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.Command(os.Args[0], ownProcessArgs(t, "TestOwnProcessLeftoverHelper")...)
	cmd.Env = append(os.Environ(), adoptHelperEnv+"=TestOwnProcessLeftoverHelper", leftoverDirEnv+"="+dir)
	cmd.Stdout, cmd.Stderr = out, out
	runErr := cmd.Run()
	said, _ := os.ReadFile(filepath.Join(dir, "out"))
	b, err := os.ReadFile(filepath.Join(dir, "procs"))
	if err != nil {
		t.Fatalf("the helper did not say its app's pids (%v): %v\n%s", runErr, err, said)
	}
	if runErr == nil || !bytes.Contains(said, []byte("failing while the app")) {
		t.Fatalf("the helper did not fail as it should (%v):\n%s", runErr, said)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		name := []string{"app", "app's child"}[min(i, 1)]
		var p utils.Reached
		if _, err := fmt.Sscanf(line, "%d %d", &p.PID, &p.Start); err != nil {
			t.Fatalf("the helper said %q, not a pid and start", line)
		}
		if !killedSince(p) {
			_ = syscall.Kill(p.PID, syscall.SIGKILL)
			t.Errorf("the %s (%d) the test's own process started outlived it, on its way to no end: whatever reads that process's output to its end would wait on its end", name, p.PID)
		}
	}
}

// killedSince reports whether process p, named by its pid and start time, has
// had a SIGKILL sent before the call: it is gone, a zombie, on its way out of
// the kernel's exit, or has the SIGKILL pending. A SIGKILL wakes the process
// it is sent to before the send returns, and a process so woken runs to its
// exit, never asleep (S) or stopped (T) again: one seen so after the send was
// not killed. One seen running, or in the kernel, may have taken the SIGKILL
// and not yet begun its exit; it is looked at again once its pidfd says it
// exited, or after a pause, until one of the above tells.
func killedSince(p utils.Reached) bool {
	fd, err := unix.PidfdOpen(p.PID, 0)
	if err != nil {
		return true
	}
	defer unix.Close(fd)
	for {
		if now, ok := utils.ProcOf(p.PID); !ok || now != p {
			return true
		}
		if killedOrGone(p.PID) {
			return true
		}
		st, ok := utils.ReadProcStat(p.PID)
		if !ok || st.StartTime != p.Start {
			return true
		}
		switch st.State {
		case 'S', 'T', 't':
			return false
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if n, _ := unix.Poll(fds, 10); n > 0 {
			return true
		}
	}
}

// sigstopSent reports whether process pid is stopped, or has a SIGSTOP on its
// way: its status is read first, its pending signals under the lock the
// kernel holds as it takes the SIGSTOP and stops the process, and then its
// state, so one of the two shows a SIGSTOP sent before the reads.
func sigstopSent(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || (k != "SigPnd" && k != "ShdPnd") {
			continue
		}
		if mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64); err == nil && mask&(1<<(syscall.SIGSTOP-1)) != 0 {
			return true
		}
	}
	st, ok := utils.ReadProcStat(pid)
	return ok && st.State == 'T'
}

// startTree starts command (sh -c) as a group leader of its own and returns
// its pid once it runs n processes in all; the test kills the group as it
// ends.
func startTree(t *testing.T, command string, n int) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-reaped
	})
	for len(utils.TreeOf(cmd.Process.Pid)) < n {
		select {
		case <-reaped:
			t.Fatalf("%q exited before it ran %d processes", command, n)
		case <-time.After(time.Millisecond):
		}
	}
	return cmd.Process.Pid
}

// The sweep of a test's own process (endWhatItStarted) stops each process it
// finds before it looks again: one not stopped could start another after the
// look, or exit and leave its children to init, out of the sweep's sight.
// Then it kills each. Here a shell ignoring SIGHUP and its two children:
// each found by a look has a SIGSTOP sent by the next, and each is killed by
// the sweep's end.
func TestTheSweepStopsWhatItFindsBeforeItLooksAgainThenKillsIt(t *testing.T) {
	if err := goUsesPidfds(); err != nil {
		t.Skipf("no pidfds here: %v", err)
	}
	leader := startTree(t, "trap '' HUP; sleep 600 & sleep 600 & wait", 3)
	var found []utils.Reached
	looks := 0
	sweep(func() []utils.Reached {
		looks++
		for _, p := range found {
			if !sigstopSent(p.PID) {
				t.Errorf("look %d: process %d, found by an earlier look, was not stopped before it", looks, p.PID)
			}
		}
		procs := utils.TreeOf(leader)
		found = append(found, procs...)
		return procs
	})
	if looks < 2 {
		t.Fatalf("the sweep looked %d times, want a look after the first finds none new", looks)
	}
	if len(found) < 3 {
		t.Fatalf("the sweep found %v, want the shell and its two children", found)
	}
	for _, p := range found {
		if !killedSince(p) {
			t.Errorf("process %d, which the sweep found, outlived it", p.PID)
		}
	}
}

// A process the sweep found is signalled through a pidfd of it as it was
// found (its pid and start time): a pid that names another process by the
// time the sweep opens it is signalled nothing. Here a live process is found
// by its pid with another start, as one that exited and left its pid to it:
// the sweep neither stops nor kills it.
func TestTheSweepSignalsNoProcessOtherThanTheOneItFound(t *testing.T) {
	if err := goUsesPidfds(); err != nil {
		t.Skipf("no pidfds here: %v", err)
	}
	pid := startTree(t, "trap '' HUP; exec sleep 600", 1)
	p, ok := utils.ProcOf(pid)
	if !ok {
		t.Fatalf("process %d is gone", pid)
	}
	gone := utils.Reached{PID: p.PID, Start: p.Start + 1}
	sweep(func() []utils.Reached { return []utils.Reached{gone} })
	if killedSince(p) {
		t.Fatalf("the sweep killed process %d, which by then was not the one it found", pid)
	}
	if sigstopSent(pid) {
		t.Fatalf("the sweep stopped process %d, which by then was not the one it found", pid)
	}
}

// daemonize starts command (sh -c) in a session of its own from a child that
// exits at once, as pg_ctl start does, and returns its pid once that child
// has exited: the daemon is orphaned, and reparented by then. The daemon is
// killed when the test ends, if it is still there. A daemon that loops loops
// only while the process that started it runs (ownerEnv).
func daemonize(t *testing.T, command string, args ...string) int {
	t.Helper()
	pid, err := startDaemon(command, args...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// daemonExitingAdopted daemonizes a daemon that exits once it is adopted,
// and returns its pid then. One that exits at once may exit before the
// child that started it does, which then reaps it itself (dash does, at its
// next command): it is never adopted, and a test that waits for it to be
// (a zombie of this process, the reaper's reap of it) waits for ever. This
// one holds its fifo open for read until it is written, which the test does
// once daemonize returned: its starter has exited, and it is adopted.
func daemonExitingAdopted(t *testing.T) int {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "exit")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	pid := daemonize(t, `exec cat "$1"`, fifo)
	f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close() // cat reads EOF and exits
	return pid
}

// ownerEnv names, in the environment of every process these tests start, the
// pid of the test process that started it: set in this process's own
// environment as it starts (init), it is inherited by each daemon a test
// starts (startDaemon), each application keploy runs for a test, and each
// script those start. A test's own process (inOwnProcess) runs init again, so
// there the owner is that process. Every one of them that loops with no end
// of its own loops `while kill -0 "$DAEMON_OWNER"`, in its TERM trap too: one
// whose test process was killed, its cleanups never run (a test's own
// process ends on its parent-death SIGKILL, whatever ended the test binary),
// ends with it, not looping on with no one to kill it. The check is a
// builtin's kill(2): it opens no file and starts no process, so a process's
// descriptors and children are as the test has them.
const ownerEnv = "DAEMON_OWNER"

func init() {
	if err := os.Setenv(ownerEnv, strconv.Itoa(os.Getpid())); err != nil {
		panic(err)
	}
}

// startDaemon is daemonize's work, safe off the test's goroutine.
func startDaemon(command string, args ...string) (int, error) {
	cmd := exec.Command("/bin/sh", append([]string{"-c", `setsid /bin/sh -c "$0" sh "$@" >/dev/null 2>&1 </dev/null & echo $!`, command}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("the daemonizer: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("the daemonizer printed %q", out)
	}
	return pid, nil
}

// beforeTheDeadline fires a quarter of the way to the test binary's
// deadline, and never with none: a wait it bounds fails only when it takes
// over a quarter of the time the binary has left, far longer than a loaded
// host takes for what does happen, and says what it waited on before the
// binary's own panic. go test always gives the binary a deadline; a binary
// run directly has one only with -test.timeout, as CI's direct runs pass.
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

// await waits until cond holds; its bound (beforeTheDeadline) only turns a
// hang into a failure.
func await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	expired := beforeTheDeadline(t)
	for !cond() {
		select {
		case <-expired:
			t.Fatalf("%s did not happen", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// parentIs reports whether process pid runs with parent ppid.
func parentIs(pid, ppid int) bool {
	st, ok := utils.ReadProcStat(pid)
	return ok && st.PPID == ppid && st.State != 'Z'
}

// zombieOf reports whether pid has exited and waits, a zombie child of ppid.
func zombieOf(pid, ppid int) bool {
	st, ok := utils.ReadProcStat(pid)
	return ok && st.PPID == ppid && st.State == 'Z'
}

// A daemon the run starts (a child that starts it in a session of its own and
// exits, as pg_ctl start does) is reparented to keploy's CLI, not to init, so
// it stays in the tree that is the run's membership; once it exits it is
// reaped, never left a zombie; and the CLI's own children, many at once while
// adopted ones exit, are left to their own Wait, which reads each one's exit
// status.
func TestDaemonsAreAdoptedAndReaped(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()

	dir := t.TempDir()
	type daemon struct {
		pid  int
		fifo string
	}
	var ds []daemon
	for i := 0; i < 8; i++ {
		fifo := filepath.Join(dir, fmt.Sprintf("fifo-%d", i))
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		// cat holds its fifo open for read until it is written.
		ds = append(ds, daemon{daemonize(t, `exec cat "$1"`, fifo), fifo})
	}
	for _, d := range ds {
		if !parentIs(d.pid, os.Getpid()) {
			st, _ := utils.ReadProcStat(d.pid)
			t.Fatalf("daemon %d: parent %d, want keploy's CLI, %d", d.pid, st.PPID, os.Getpid())
		}
	}

	// The CLI's own children exit while the daemons do: each Wait reads its
	// child's own status, none reaped from under it.
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(code int) {
			defer wg.Done()
			err := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code)).Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != code {
				errs <- fmt.Errorf("own child exiting %d: Wait = %v", code, err)
			}
		}(1 + i%100)
	}
	for _, d := range ds {
		f, err := os.OpenFile(d.fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close() // cat reads EOF and exits
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, d := range ds {
		await(t, fmt.Sprintf("daemon %d reaped", d.pid), func() bool {
			st, ok := utils.ReadProcStat(d.pid)
			return !ok || st.PPID != os.Getpid()
		})
	}
}

// A child that cannot be reaped yet (its main thread exited while another
// runs on; another tracer holds it) is no reap: a wait that returns without a
// child counted as one, the reaper scanned again for as long as it lasted, a
// core spinning. And one this process cannot tell from its own (its fds
// cannot be read) is not reaped at all, nor any other: os/exec's Wait for an
// own child reaped from under it fails.
func TestNoReapIsCountedAndAnUnreadableFdTableReapsNothing(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	pid := daemonExitingAdopted(t)
	await(t, "the daemon exiting, adopted", func() bool { return zombieOf(pid, os.Getpid()) })

	real := waitAdopted
	waitAdopted = func(int) (int32, error) { return 0, nil } // WNOHANG: not waitable yet
	if got := reapAdopted(zap.NewNop()); len(got) != 0 {
		t.Fatalf("a wait that returned no child was taken for the reap of %v", got)
	}
	done := make(chan struct{})
	go func() { reapAll(zap.NewNop()); close(done) }()
	select {
	case <-done:
	case <-beforeTheDeadline(t): // the bound only turns a spin into a failure
		t.Fatal("the reaper kept scanning for a child it could not reap yet")
	}
	waitAdopted = real

	restore := breakFdinfo(t)
	if got := reapAdopted(zap.NewNop()); len(got) != 0 || !zombieOf(pid, os.Getpid()) {
		t.Fatalf("with its fds unreadable, keploy reaped %v", got)
	}
	restore()
	if got := reapAdopted(zap.NewNop()); len(got) != 1 || got[0] != pid {
		t.Fatalf("reaped %v, want the adopted daemon %d", got, pid)
	}
}

// What the CLI adopted ends with the application: a daemon that exits when
// asked is stopped and never killed; one that ignores the request is killed
// once it makes no progress on its way out, idle or busy (a loop that runs
// on is no progress); one whose way out takes long but makes progress
// (closing its files one by one) is waited for until it is done, however
// long that takes; and the CLI's own children are not touched.
func TestAdoptedDaemonsEndWithTheApplication(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	// One quiet second escalates; the slow daemon's way out takes two, in
	// steps of 0.3 s: twice the bound, so a stop that did not restart the
	// stall at each of its steps would kill it, even counting the stall
	// at half the wall time (a host so loaded that its daemons wait for a
	// CPU half of it).
	stopCheck, stopStallChecks = 20*time.Millisecond, 50

	own := exec.Command("/bin/sh", "-c", "exec sleep 120")
	if err := own.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = own.Process.Kill(); _ = own.Wait() }()

	dir := t.TempDir()
	done := filepath.Join(dir, "done")
	ready := func(name string) string { return filepath.Join(dir, name+".ready") }
	polite := daemonize(t, `: > "$1"; exec sleep 120`, ready("polite"))
	deaf := daemonize(t, `trap '' TERM; : > "$1"; exec sleep 120`, ready("deaf"))
	busy := daemonize(t, `trap '' TERM; : > "$1"; while kill -0 "$DAEMON_OWNER"; do sleep 0.01; done`, ready("busy"))
	// One that opens and closes a file over and over (a retry loop, a DNS
	// lookup): a count that rises and falls back is no progress.
	churn := daemonize(t, `trap '' TERM; : > "$1"; while kill -0 "$DAEMON_OWNER"; do exec 3</dev/null; sleep 0.05; exec 3<&-; sleep 0.05; done`, ready("churn"))
	slow := daemonize(t, `exec 3</dev/null 4</dev/null 5</dev/null 6</dev/null 7</dev/null 8</dev/null 9</dev/null
trap 'for fd in 3 4 5 6 7 8 9; do eval "exec $fd<&-"; sleep 0.3; done; echo done > "$2"; exit 0' TERM
: > "$1"; while kill -0 "$DAEMON_OWNER"; do sleep 0.01; done`, ready("slow"), done)
	for _, name := range []string{"polite", "deaf", "busy", "churn", "slow"} {
		await(t, name+" ready", func() bool { _, err := os.Stat(ready(name)); return err == nil })
	}

	core, logs := observer.New(zapcore.DebugLevel)
	// The run's teardown scope: the slow daemon's steps on its way out move
	// it, which the run's drains wait on.
	scope := &utils.TeardownScope{}
	why, left := stopAdopted(zap.New(core), nil, scope)
	if left != "" {
		t.Errorf("the stop ended every daemon and says it left %q running", left)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("polite %d, deaf %d, busy %d, churn %d, slow %d", polite, deaf, busy, churn, slow)
			for _, e := range logs.All() {
				t.Logf("%s %v", e.Message, e.ContextMap())
			}
		}
	})

	for _, pid := range []int{polite, deaf, busy, churn, slow} {
		if parentIs(pid, os.Getpid()) {
			t.Fatalf("daemon %d is still running after the application ended", pid)
		}
		// And reaped: no exited daemon is left a zombie of the CLI's, the
		// pidfds the stop held on them notwithstanding.
		await(t, fmt.Sprintf("daemon %d reaped", pid), func() bool { return !zombieOf(pid, os.Getpid()) })
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatalf("the daemon whose way out made progress was killed before it was done: %v", err)
	}
	if scope.Moves() == 0 {
		t.Fatal("the daemons' way out made progress, and moved the run's teardown not once")
	}
	killed := logs.FilterMessageSnippet("killing it").All()
	if len(killed) != 1 {
		t.Fatalf("%d kills, want one, of the daemons that ignored the request", len(killed))
	}
	// The daemons killed ignore SIGTERM, which the stop asked them to stop
	// with: they had no way out of their own to cut, so the kill is not said
	// as one that cut it, and a set's end is judged.
	if why != "" {
		t.Fatalf("stopAdopted says %q; want no cut way out said for daemons that ignore SIGTERM", why)
	}
	if !parentIs(own.Process.Pid, os.Getpid()) {
		t.Fatal("the CLI's own child was stopped with what it adopted")
	}
}

// A stop that waits for a daemon holds the drains (the next test set starts
// once it is over), offers, a while after the stop's signal, to kill it at
// the next press of Ctrl+C, and does at that press; and it stops only what
// was there as it began: a daemon adopted while it waits (the next set's)
// is not its to stop.
func TestAPressKillsTheDaemonsAStopWaitsFor(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks, stopOffer = 20*time.Millisecond, 1<<40, 0 // only a press ends it

	dir := t.TempDir()
	deaf := daemonize(t, `trap '' TERM; : > "$1"; exec sleep 120`, filepath.Join(dir, "ready"))
	await(t, "the daemon ready", func() bool { _, err := os.Stat(filepath.Join(dir, "ready")); return err == nil })

	utils.MarkInterrupted() // the stop's own signal
	core, logs := observer.New(zapcore.DebugLevel)
	var late int
	var lateErr error
	held := make(chan [2]bool, 1)
	go func() {
		for logs.FilterMessageSnippet("press Ctrl+C again").Len() == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		held <- [2]bool{utils.StopHoldsDrains(), utils.AgentStopInProgress()}
		late, lateErr = startDaemon("exec sleep 120")
		utils.MarkInterruptedAgain()
	}()
	why, _ := stopAdopted(zap.New(core), nil, nil)
	if lateErr != nil {
		t.Fatal(lateErr)
	}
	t.Cleanup(func() { _ = syscall.Kill(late, syscall.SIGKILL) })
	if h := <-held; !h[0] || h[1] {
		t.Errorf("while it waited the stop held the drains %v, and was an agent's stop %v: want held, and no agent's", h[0], h[1])
	}
	if parentIs(deaf, os.Getpid()) {
		t.Fatal("the daemon is still running after the press")
	}
	if logs.FilterMessageSnippet("at the press of Ctrl+C").Len() != 1 {
		t.Fatalf("the daemon was not killed at the press: %v", logs.All())
	}
	if why != "" {
		t.Fatalf("a kill at the user's press is said as a kill for want of progress: %q", why)
	}
	if !parentIs(late, os.Getpid()) {
		t.Fatal("the stop ended a daemon adopted after it began")
	}
}

// The stop at a set's end interrupts the app, and a shell that waits on the
// app it started in the background (`app & wait`) dies on it at once: the
// app, orphaned, is one of the daemons the run adopted, and the stop of
// those ends it. When it makes no progress on its way out and is killed, the
// app's run says so (models.AppError.StopKilled), and the set's end is not
// judged. The teardown's stop says nothing.
func TestARunSaysTheSetEndKilledWhatTheAppLeftStopping(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	for _, tc := range []struct {
		name   string
		cause  error
		killed bool
	}{
		{"the stop at a set's end", models.ErrAppStoppedForSetEnd, true},
		{"the teardown's stop", context.Canceled, false},
	} {
		dir := t.TempDir()
		ready := filepath.Join(dir, "ready")
		cfg := &config.Config{}
		cfg.Agent.AgentURI = "http://127.0.0.1:1/agent"
		client := New(zap.NewNop(), nil, cfg)
		client.apps.Store(uint64(0), app.NewApp(zap.NewNop(),
			deafApp(ready), nil,
			models.SetupOptions{CommandType: string(utils.Native)}))
		ctx, cancel := context.WithCancelCause(context.Background())
		done := make(chan models.AppError, 1)
		go func() { done <- client.Run(ctx, models.RunOptions{}) }()
		await(t, tc.name+": the app ready", func() bool { _, err := os.Stat(ready); return err == nil })
		cancel(tc.cause)
		appErr := <-done
		if killed := strings.Contains(appErr.StopKilled, "made no progress on its way out"); killed != tc.killed {
			t.Fatalf("%s: run ended %+v; want a kill said %v", tc.name, appErr, tc.killed)
		}
		// The kill names what it killed, and what to do about it.
		if tc.killed && (!strings.Contains(appErr.StopKilled, "(sh, state ") || !strings.Contains(appErr.StopKilled, "make it exit once it is asked to stop")) {
			t.Fatalf("%s: the kill is said as %q; want the process named, with its state, and the next step", tc.name, appErr.StopKilled)
		}
	}
}

// Only a step on a daemon's way out moves the run's teardown: one that opens
// and closes a file over and over, ignoring the request to stop, moves it at
// most once (its first look below where the stop found it is a file fewer,
// utils.ExitRatchet), not at each of the dozens of closes it makes, and is
// killed on the stall bound. Where the kernel will not compare its files, the
// stop counts its descriptors and says so, once: its churn of fd 3, and of
// the fd 10 dash saves it to, then reads as up to two closes below the
// reading that set the mark, and moves the teardown at most twice, bounded.
// KEPLOY_TEST_KCMP_REQUIRED=1 (go-test.yaml's run of it as root) fails it
// there instead, so the comparison runs in CI.
func TestADaemonThatOnlyChurnsMovesNoTeardown(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 25
	ready := filepath.Join(t.TempDir(), "ready")
	churn := daemonize(t, churningDaemon, ready)
	await(t, "the daemon ready", func() bool { _, err := os.Stat(ready); return err == nil })
	// Whether the kernel compares the daemon's files: its fd 0 asked of
	// itself (kcmp's KCMP_FILE, 0).
	_, _, refused := unix.Syscall6(unix.SYS_KCMP, uintptr(churn), uintptr(churn), 0, 0, 0, 0)
	scope := &utils.TeardownScope{}
	// Read as the stop decides to kill it, on the stop's own goroutine as it
	// says so: what the kill does after (the kernel freeing what it held, the
	// stop's next look finding the daemon gone, a move) is not the daemon's
	// way out. Read later, once the test saw the kill in the log, the count
	// can take that look in too: on a loaded box the stop gets to it first.
	before, uncompared := atomic.Int64{}, atomic.Int64{}
	before.Store(-1)
	core, _ := observer.New(zapcore.InfoLevel)
	logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if strings.Contains(e.Message, "made no progress on its way out after it was asked to stop; killing it") {
			before.CompareAndSwap(-1, int64(scope.Moves()))
		}
		if strings.Contains(e.Message, "counts the open files of the processes it stops as their descriptors") {
			uncompared.Add(1)
		}
		return nil
	}))
	stopAdopted(logger, nil, scope)
	if parentIs(churn, os.Getpid()) {
		t.Fatal("the churning daemon outlived its stop")
	}
	switch n, said := before.Load(), uncompared.Load(); {
	case n < 0:
		t.Fatal("the stop did not kill the churning daemon on its stall")
	case said > 1:
		t.Fatalf("the stop said %d times that it counts the daemon's descriptors; want once", said)
	case (refused != 0) != (said == 1):
		t.Fatalf("the kernel's answer to a comparison of the daemon's files was %v, and the stop said %d times that it counts its descriptors; want once where it refuses, else never", refused, said)
	case said == 1 && os.Getenv("KEPLOY_TEST_KCMP_REQUIRED") == "1":
		t.Fatal("KEPLOY_TEST_KCMP_REQUIRED=1, but the kernel would not compare the daemon's files: the stop counted its descriptors")
	case said == 1 && n > 2:
		t.Fatalf("a daemon that only churned, its descriptors counted, moved the run's teardown %d times before its kill; want at most 2", n)
	case said == 1:
		t.Logf("the kernel would not compare the daemon's files: the stop counted its descriptors, and moved the run's teardown %d times", n)
	case n > 1:
		t.Fatalf("a daemon that only churned moved the run's teardown %d times before its kill", n)
	}
}

// deafApp is an application that does not stop when asked: a shell that
// waits on the one it started in the background, which catches a TERM and
// loops on, until it is killed or until the test process that started it is
// gone (ownerEnv). It writes its pid to file ready once it runs.
func deafApp(ready string) string {
	return `sh -c 'trap : TERM; echo $$ > ` + ready + `; while kill -0 "$DAEMON_OWNER"; do sleep 1; done' & wait`
}

// churningDaemon is the daemon TestADaemonThatOnlyChurnsMovesNoTeardown
// stops: it ignores the request to stop and opens and closes a file over and
// over, with no pause, until it is killed, or until the process that started
// it is gone (ownerEnv); it creates file $1 once it runs. No child of
// its own: one that ended would be a step (a process of it gone).
const churningDaemon = `trap '' TERM; : > "$1"; while kill -0 "$DAEMON_OWNER"; do exec 3</dev/null; exec 3<&-; done`

// killedDirEnv names the directory TestOwnProcessKilledHelper writes in.
const killedDirEnv = "KEPLOY_TEST_KILLED_DIR"

// TestOwnProcessKilledHelper is not a test: with killedDirEnv set it is a
// test's own process (inOwnProcess) that adopts as keploy does, starts the
// churning daemon (churningDaemon) as TestADaemonThatOnlyChurnsMovesNoTeardown
// does, runs an application that does not stop when asked (deafApp) as keploy
// does, writes the pid and start time of the daemon and of the app's looping
// shell once they run, and then waits on its input, to be killed.
func TestOwnProcessKilledHelper(t *testing.T) {
	dir := os.Getenv(killedDirEnv)
	if dir == "" {
		t.Skip("a process of TestADaemonEndsWithTheProcessThatStartedIt")
	}
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	ready := filepath.Join(dir, "ready")
	pid := daemonize(t, churningDaemon, ready)
	await(t, "the daemon ready", func() bool { _, err := os.Stat(ready); return err == nil })
	p, ok := utils.ProcOf(pid)
	if !ok {
		t.Fatalf("the daemon (%d) was gone once it ran", pid)
	}
	appReady := filepath.Join(dir, "app")
	client := newRunClient(deafApp(appReady))
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_ = client.Run(context.Background(), models.RunOptions{})
	}()
	var b []byte
	for {
		if b, err = os.ReadFile(appReady); err == nil && bytes.HasSuffix(b, []byte("\n")) {
			break
		}
		select {
		case <-ran:
			t.Fatal("the run ended before its app wrote its pid")
		case <-time.After(time.Millisecond):
		}
	}
	appPid, err := strconv.Atoi(string(bytes.TrimSpace(b)))
	if err != nil {
		t.Fatalf("the app wrote %q, not its pid", b)
	}
	a, ok := utils.ProcOf(appPid)
	if !ok {
		t.Fatalf("the app (%d) was gone once it ran", appPid)
	}
	// Written whole, then named: the test reads none half written.
	said := filepath.Join(dir, "daemon")
	if err := os.WriteFile(said+".tmp", fmt.Appendf(nil, "%d %d %d %d\n", p.PID, p.Start, a.PID, a.Start), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(said+".tmp", said); err != nil {
		t.Fatal(err)
	}
	// Until it is killed. Should the test that started it end first, its
	// input ends with it, and its cleanups kill the daemon.
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// A daemon a test starts, and an application keploy runs for it, end with the
// test process that started them, however that process ends. A test's own
// process (inOwnProcess) ends on a SIGKILL, its parent-death signal, as the
// test binary ends, whatever ended it: a TERM from a job's stop on a CI or
// test machine, which the binary does not wait out, or its timeout. So
// killed, it runs none of its cleanups (daemonize's kill of the daemon, the
// kill of what it still runs), and the churning daemon, which ignores a TERM
// and loops with no pause, was left spinning a CPU, until it was killed by
// hand; an app that does not stop when asked was left looping too.
func TestADaemonEndsWithTheProcessThatStartedIt(t *testing.T) {
	if err := goUsesPidfds(); err != nil {
		t.Skipf("keploy adopts nothing here: %v", err)
	}
	dir := t.TempDir()
	out, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	cmd := exec.Command(os.Args[0], ownProcessArgs(t, "TestOwnProcessKilledHelper")...)
	cmd.Env = append(os.Environ(), adoptHelperEnv+"=TestOwnProcessKilledHelper", killedDirEnv+"="+dir)
	cmd.Stdout, cmd.Stderr = out, out
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var waitErr error
	ended := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(ended)
	}()
	// Should the test fail before its kill, the helper's input ends: it
	// runs its cleanups, and the test waits for its end.
	t.Cleanup(func() {
		_ = in.Close()
		<-ended
	})
	var b []byte
	for {
		if b, err = os.ReadFile(filepath.Join(dir, "daemon")); err == nil {
			break
		}
		select {
		case <-ended:
			said, _ := os.ReadFile(out.Name())
			t.Fatalf("the helper ended (%v) before it said its daemon:\n%s", waitErr, said)
		case <-time.After(time.Millisecond):
		}
	}
	var p, a utils.Reached
	if _, err := fmt.Sscanf(string(b), "%d %d %d %d", &p.PID, &p.Start, &a.PID, &a.Start); err != nil {
		t.Fatalf("the helper said %q, not two pids and starts", b)
	}
	for _, q := range []utils.Reached{p, a} {
		t.Cleanup(func() {
			if now, ok := utils.ProcOf(q.PID); ok && now == q {
				_ = syscall.Kill(q.PID, syscall.SIGKILL)
			}
		})
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-ended
	for _, q := range []struct {
		what string
		p    utils.Reached
	}{{"the churning daemon", p}, {"the app that does not stop when asked", a}} {
		await(t, "the end of "+q.what+", the process that started it killed", func() bool {
			now, ok := utils.ProcOf(q.p.PID)
			return !ok || now != q.p
		})
	}
}

// The stall a stop of the daemons kills on is stopStallChecks checks of
// stopCheck, and the longest Duration where that is longer: a test's "only a
// press ends it" never wraps to a short one.
func TestTheDaemonsStallIsItsChecksAndNeverWraps(t *testing.T) {
	check, checks := stopCheck, stopStallChecks
	t.Cleanup(func() { stopCheck, stopStallChecks = check, checks })
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	if got := stopStall(); got != 200*time.Millisecond {
		t.Fatalf("10 checks of 20ms are %v, want 200ms", got)
	}
	stopCheck, stopStallChecks = 3, math.MaxUint64/2
	if got := stopStall(); got != math.MaxInt64 {
		t.Fatalf("a stall past the longest Duration is %v, want the longest", got)
	}
}

// A stop that cannot tell its own children from the daemons the application
// left (its read of its own fd table fails: EMFILE, an fdinfo it cannot read)
// stops none of them, and says so: the application is not known to have
// ended.
func TestAStopThatCannotTellItsOwnSaysWhatItLeftRunning(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	pid := daemonize(t, "exec sleep 120")
	await(t, "the daemon adopted", func() bool { return parentIs(pid, os.Getpid()) })
	defer breakFdinfo(t)()
	_, left := stopAdopted(zap.NewNop(), nil, nil)
	if !parentIs(pid, os.Getpid()) {
		t.Fatalf("premise: daemon %d was stopped", pid)
	}
	if !strings.Contains(left, "could not tell its own processes") {
		t.Fatalf("left %q; want it to say keploy could not tell its own processes from the daemons", left)
	}
}

// An application that leaves a daemon keploy cannot see (not dumpable: its
// /proc/<pid>/exe is not this user's to read) ends its run with LeftRunning
// naming it, as the replay reads the run: whether keploy's stop at the set's
// end ended the run, or the application exited on its own (a run that ended
// with a type other than a stop's, whose set's end still says what of it was
// left).
func TestARunSaysTheDaemonItCannotSeeIsLeftRunning(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 to make a daemon that is not dumpable")
	}
	if os.Geteuid() == 0 {
		t.Skip("root sees every daemon")
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	for _, ownExit := range []bool{false, true} {
		dir := t.TempDir()
		ready := filepath.Join(dir, "ready")
		pidFile := filepath.Join(dir, "pid")
		script := filepath.Join(dir, "d.py")
		if err := os.WriteFile(script, []byte(`import ctypes, os, signal, sys, time
signal.signal(signal.SIGINT, signal.SIG_IGN)
signal.signal(signal.SIGTERM, signal.SIG_IGN)
ctypes.CDLL(None).prctl(4, 0, 0, 0, 0)
open(sys.argv[2], "w").write(str(os.getpid()))
open(sys.argv[1], "w").close()
time.sleep(600)
`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if b, err := os.ReadFile(pidFile); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		})
		then := "exec sleep 600"
		if ownExit {
			then = "until [ -e " + ready + " ]; do sleep 0.05; done; exit 3"
		}
		cfg := &config.Config{}
		cfg.Agent.AgentURI = "http://127.0.0.1:1/agent"
		client := New(zap.NewNop(), nil, cfg)
		client.apps.Store(uint64(0), app.NewApp(zap.NewNop(),
			`sh -c 'setsid python3 `+script+` `+ready+` `+pidFile+` >/dev/null 2>&1 </dev/null &'; `+then, nil,
			models.SetupOptions{CommandType: string(utils.Native)}))
		ctx, cancel := context.WithCancelCause(context.Background())
		done := make(chan models.AppError, 1)
		go func() { done <- client.Run(ctx, models.RunOptions{}) }()
		await(t, "the daemon ready", func() bool { _, err := os.Stat(ready); return err == nil })
		if !ownExit {
			cancel(models.ErrAppStoppedForSetEnd)
		}
		appErr := <-done
		cancel(nil)
		if ownExit && appErr.AppErrorType == models.ErrCtxCanceled {
			t.Fatalf("the application's own exit ended its run as a stop's (%+v)", appErr)
		}
		if !strings.Contains(appErr.LeftRunning, "daemons keploy cannot see") {
			t.Fatalf("own exit %v: LeftRunning %q (type %q); want the daemon keploy cannot see named", ownExit, appErr.LeftRunning, appErr.AppErrorType)
		}
	}
}

// A daemon whose way out starts a process (a TERM handler that starts one in
// the background and exits) leaves it adopted once the daemon is gone; the
// stop, which stops the daemons it found as it began, reads what it adopts
// again once it is over and says what runs.
func TestAChildADaemonForksOnItsWayOutIsSaidLeftRunning(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 50
	dir := t.TempDir()
	child := filepath.Join(dir, "child")
	script := filepath.Join(dir, "d.sh")
	if err := os.WriteFile(script, []byte(`trap 'sh -c "echo \$\$ > $1; exec sleep 300" & sleep 0.3; exit 0' TERM
: > "$1.ready"
while kill -0 "$DAEMON_OWNER"; do sleep 0.05; done
`), 0o644); err != nil {
		t.Fatal(err)
	}
	daemonize(t, `exec sh "$1" "$2"`, script, child)
	await(t, "the daemon ready", func() bool { _, err := os.Stat(child + ".ready"); return err == nil })
	_, left := stopAdopted(zap.NewNop(), nil, nil)
	b, err := os.ReadFile(child)
	if err != nil {
		t.Fatalf("premise: the daemon's way out started no process: %v", err)
	}
	cpid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	t.Cleanup(func() { _ = syscall.Kill(cpid, syscall.SIGKILL) })
	if !parentIs(cpid, os.Getpid()) {
		t.Fatalf("premise: process %d, which the daemon started on its way out, is not adopted and running", cpid)
	}
	if want := fmt.Sprintf("processes the daemons started as they stopped (pids [%d])", cpid); !strings.Contains(left, want) {
		t.Fatalf("left %q; want %q", left, want)
	}
}

// A daemon the kernel holds after its kill (one waiting in it without
// progress: the kill withheld here) is still there when the stop gives up on
// it, and the stop says so; a later stop, which does not wait for it again,
// says it is still there, for as long as it is.
func TestAStopSaysTheDaemonTheKernelHoldsAfterItsKill(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	kill := killMembers
	killMembers = func([]member) {}
	defer func() { killMembers = kill }()
	ready := filepath.Join(t.TempDir(), "ready")
	pid := daemonize(t, `trap '' TERM; : > "$1"; exec sleep 120`, ready)
	await(t, "the daemon ready", func() bool { _, err := os.Stat(ready); return err == nil })
	want := fmt.Sprintf("daemons the kernel holds after their kill (pids [%d])", pid)
	if _, left := stopAdopted(zap.NewNop(), nil, nil); !strings.Contains(left, want) {
		t.Fatalf("left %q; want %q", left, want)
	}
	core, logs := observer.New(zapcore.InfoLevel)
	if _, left := stopAdopted(zap.New(core), nil, nil); !strings.Contains(left, want) {
		t.Fatalf("the next stop: left %q; want %q, the daemon still there", left, want)
	}
	if n := logs.FilterMessageSnippet("stopping the daemons").Len(); n != 0 {
		t.Fatalf("the next stop waited for the daemon the kernel holds again (%d stops)", n)
	}
}

// breakFdinfo makes heldPidfds' read of this process's fds fail, as an
// EMFILE or an fdinfo it cannot read does, until the func it returns is
// called.
func breakFdinfo(t *testing.T) (restore func()) {
	t.Helper()
	gone := filepath.Join(t.TempDir(), "gone")
	fdinfoDir.Store(&gone)
	return func() { fdinfoDir.Store(nil) }
}

// The read a stop decides what it left running from never misses a daemon
// that is keploy's child for the whole read, however many others are reaped
// while it reads. The kernel's quick children list (/proc/<pid>/task/<tid>/
// children) can: a child listed after one that is reaped during the read is
// skipped (fs/proc/array.c, get_children_pid), and keploy's reaper reaps
// adopted children as the stop reads. Here daemons keep being adopted, and
// the oldest killed, among short-lived adopted processes the reaper reaps;
// each read is checked for every daemon adopted before it began and still
// running after it.
func TestAStopsReadNeverMissesADaemonWhileOthersAreReaped(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	me := os.Getpid()
	// Spawners whose children are orphaned to this process and exit soon
	// after, reaped by its reaper.
	var spawners []utils.Reached
	for i := 0; i < 3; i++ {
		pid := daemonize(t, `while kill -0 "$DAEMON_OWNER"; do (sleep 0.02 &); (sleep 0.04 &); sleep 0.004; done`)
		p, ok := utils.ProcOf(pid)
		if !ok {
			t.Fatalf("spawner %d ended as it started", pid)
		}
		spawners = append(spawners, p)
	}
	var live []int
	t.Cleanup(func() {
		for _, p := range live {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
	})
	const reads, window = 1500, 48
	misses, missed := 0, map[int]bool{}
	for i := 0; i < reads; i++ {
		// A daemon adopted now is listed after the orphans adopted before
		// it, some of which the reaper reaps as the read runs; the oldest
		// is killed, and reaped, among the rest.
		pid, err := startDaemon("exec sleep 600")
		if err != nil {
			t.Fatal(err)
		}
		live = append(live, pid)
		if len(live) > window {
			_ = syscall.Kill(live[0], syscall.SIGKILL)
			live = live[1:]
		}
		var before []int
		for _, p := range live {
			if parentIs(p, me) {
				before = append(before, p)
			}
		}
		got := map[int]bool{}
		children, err := procChildren()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		for _, c := range children {
			got[c.pid] = true
		}
		miss := false
		for _, p := range before {
			if !got[p] && parentIs(p, me) {
				miss, missed[p] = true, true
			}
		}
		if miss {
			misses++
		}
	}
	if misses > 0 {
		t.Fatalf("%d of %d reads missed a daemon that was keploy's child for the whole read (%d daemons in all): a stop that read its daemons then would neither stop it nor say it left it running", misses, reads, len(missed))
	}
	// Each spawner still running as this process's child (its pid not
	// reused) ran for every read: none of the reads went without the reaps
	// they race.
	for _, p := range spawners {
		if now, ok := utils.ProcOf(p.PID); !ok || now != p || !parentIs(p.PID, me) {
			t.Fatalf("spawner %d ended before the reads did: the reads after it raced no reap of its orphans", p.PID)
		}
	}
}

// A read of keploy's children settles once two scans in a row find the same
// ones, each running or exited (a zombie) as before, and returns the last;
// one whose children keep changing over every scan it makes is not settled,
// and the stop then says that what the application left running cannot be
// told, however little it found. A child that exits during the read is a
// change, and stays in every scan after (no reap takes it away while the read
// runs): one that came and exited while the read ran, leaving its own child
// to keploy in its place, keeps the read from settling on nothing before the
// scan that lists that child.
func TestAReadOfTheChildrenSettlesOrSaysItCannotTell(t *testing.T) {
	scan, scans := scanChildren, settleScans
	t.Cleanup(func() { scanChildren, settleScans = scan, scans })
	a, b := child{pid: 101, start: 7}, child{pid: 102, start: 9}
	script := [][]child{{a}, {a, b}, {a, b, {pid: 103, zombie: true}}, {a, b, {pid: 103, zombie: true}}}
	n := 0
	scanChildren = func() ([]child, error) {
		cs := script[min(n, len(script)-1)]
		n++
		return cs, nil
	}
	got, settled, err := settledChildren()
	if err != nil || !settled || n != 4 || len(got) != 3 {
		t.Fatalf("settledChildren = %v, settled %v, err %v after %d scans; want the fourth scan's, settled (a zombie that appears is a change, one that stays is none)", got, settled, err, n)
	}
	// A child that ran is a zombie in the next scan: a change.
	script = [][]child{{a}, {{pid: 101, start: 7, zombie: true}}, {{pid: 101, start: 7, zombie: true}}}
	n = 0
	if _, settled, err := settledChildren(); err != nil || !settled || n != 3 {
		t.Fatalf("settled %v, err %v after %d scans; want settled at the third: the child that exited is a change", settled, err, n)
	}
	// A child T the first scan missed (it came to keploy after the scan
	// passed its pid) exits during the second, which lists it a zombie, and
	// its child X comes to keploy at a pid the second had passed: the first
	// two list nothing running, yet differ; the third lists X.
	tz, x := child{pid: 500, start: 3, zombie: true}, child{pid: 400, start: 4}
	script = [][]child{{}, {tz}, {tz, x}, {tz, x}}
	n = 0
	got, settled, err = settledChildren()
	if err != nil || !settled || n != 4 || !slices.Contains(got, x) {
		t.Fatalf("settledChildren = %v, settled %v, err %v after %d scans; want settled on the fourth, X running in it, never on the first two's nothing", got, settled, err, n)
	}
	// No reap runs while the read does: a zombie it lists stays, and none
	// is reaped between two scans it compares.
	scanChildren = func() ([]child, error) {
		if reaping.TryLock() {
			reaping.Unlock()
			t.Error("a scan of the read ran while the reaper could reap")
		}
		return nil, nil
	}
	if _, _, err := settledChildren(); err != nil {
		t.Fatal(err)
	}
	// A child adopted at every scan: never two the same.
	n = 0
	scanChildren = func() ([]child, error) {
		n++
		return []child{a, {pid: 200 + n, start: uint64(n)}}, nil
	}
	settleScans = 5
	if _, settled, err := settledChildren(); settled || err != nil || n != 5 {
		t.Fatalf("settled %v, err %v after %d scans; want not settled after the 5 the bound allows", settled, err, n)
	}
	if left := leftRunning(adoptedRead{}, nil); !strings.Contains(left, "kept changing over 5 reads of them, so what runs on cannot be told") {
		t.Fatalf("left %q; want it to say what runs on cannot be told", left)
	}
	if left := leftRunning(adoptedRead{settled: true}, nil); left != "" {
		t.Fatalf("a settled read that found nothing left %q, want nothing", left)
	}
	// A scan that fails is the read's error, never fewer children.
	scanChildren = func() ([]child, error) { return nil, errors.New("list /proc: too many open files") }
	if _, _, err := settledChildren(); err == nil {
		t.Fatal("a scan that failed read as a settled one")
	}
}

// The scan of /proc reads each listed process's stat: one that is gone by
// then (its directory, or its stat, no longer there; a task being reaped,
// state X, or its parent and group gone, in any state) is no child; one that is
// this process's child, a zombie or not, is, and one whose stat says Z is a
// zombie only where none of its threads runs (its main thread may have
// exited while another runs on); a stat that cannot be read otherwise is the
// scan's error, never a shorter list.
func TestTheScanOfProcSaysWhatItCouldNotRead(t *testing.T) {
	root := t.TempDir()
	me := 4242
	stat := func(pid int, line string) {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stat(100, fmt.Sprintf("100 (sleep) S %d 100 100 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 777 0 0\n", me))
	stat(101, fmt.Sprintf("101 (sh) Z %d 101 101 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 778 0 0\n", me))
	stat(102, "102 (other) S 1 102 102 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 779 0 0\n")
	stat(103, "103 (sleep) X 0 -1 -1 0 -1 4227084 96 0 0 0 0 0 0 0 35 15 0 0 4979432 0 0\n")
	// A process whose main thread exited while another runs on ("Zl"): its
	// stat says Z, a thread of it does not. And one whose every thread has
	// exited, its main thread's task left.
	task := func(pid, tid int, state byte) {
		dir := filepath.Join(root, strconv.Itoa(pid), "task", strconv.Itoa(tid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		line := fmt.Sprintf("%d (worker) %c %d %d %d 0 -1 0 0 0 0 0 0 0 0 0 20 0 2 0 780 0 0\n", tid, state, me, pid, pid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stat(106, fmt.Sprintf("106 (worker) Z %d 106 106 0 -1 0 0 0 0 0 0 0 0 0 20 0 2 0 780 0 0\n", me))
	task(106, 106, 'Z')
	task(106, 107, 'S')
	stat(108, fmt.Sprintf("108 (worker) Z %d 108 108 0 -1 0 0 0 0 0 0 0 0 0 20 0 3 0 781 0 0\n", me))
	task(108, 108, 'Z')
	task(108, 109, 'X')
	// A thread, and a process, being released: Z, its parent and group gone.
	released := "%d (sleep) Z 0 -1 -1 0 -1 4227084 100 0 0 0 0 0 0 0 25 5 0 0 6060366 0 0\n"
	if err := os.MkdirAll(filepath.Join(root, "108", "task", "110"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "108", "task", "110", "stat"), []byte(fmt.Sprintf(released, 110)), 0o644); err != nil {
		t.Fatal(err)
	}
	stat(111, fmt.Sprintf(released, 111))
	// A process released between the kernel's read of its state and of its
	// parent and group: it still reads as running, sleeping or on storage
	// (the first as a box read one, PF_EXITING set).
	stat(113, "113 (update-ca-certi) R 0 -1 -1 0 -1 4227404 3601 67036 0 0 0 6 52 11 20 0 0 0 7217377 2748416 196 0 1 1 0 0\n")
	stat(114, "114 (sh) S 0 -1 -1 0 -1 4227404 100 0 0 0 0 0 0 0 20 0 1 0 7217378 0 0\n")
	stat(115, "115 (sh) D 0 -1 -1 0 -1 4227404 100 0 0 0 0 0 0 0 20 0 1 0 7217379 0 0\n")
	// The line keploy#4999's CI read, as the reap race row reaped a sleep.
	stat(33119, "33119 (sleep) S 0 -1 -1 0 -1 4227084 87 0 0 0 0 0 0 0 20 0 0 0 51682 0 0 0 0 0 0 0 0 0 0 0 0 1 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0 0\n")
	if err := os.MkdirAll(filepath.Join(root, "104"), 0o755); err != nil { // gone: no stat
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil { // no process's
		t.Fatal(err)
	}
	got, err := procChildrenIn(root, me)
	if err != nil {
		t.Fatal(err)
	}
	want := []child{{pid: 100, start: 777}, {pid: 101, start: 778, zombie: true}, {pid: 106, start: 780, thread: 107}, {pid: 108, start: 781, zombie: true}}
	sort.Slice(got, func(i, j int) bool { return got[i].pid < got[j].pid })
	if !slices.Equal(got, want) {
		t.Fatalf("children %+v, want %+v", got, want)
	}
	// The threads of one whose stat says Z that cannot be read.
	if err := os.WriteFile(filepath.Join(root, "101", "task"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := procChildrenIn(root, me); err == nil {
		t.Fatalf("threads that could not be read read as children %+v", got)
	}
	if err := os.Remove(filepath.Join(root, "101", "task")); err != nil {
		t.Fatal(err)
	}
	// A stat that says Z and does not parse, and is no task's release: one
	// cut short, its parent there, or its parent 0 and its group not -1.
	for _, line := range []string{fmt.Sprintf("112 (sh) Z %d 112", me), "112 (sh) Z 0 112"} {
		stat(112, line)
		if got, err := procChildrenIn(root, me); err == nil {
			t.Fatalf("the stat %q read as a process gone, children %+v", line, got)
		}
	}
	if err := os.RemoveAll(filepath.Join(root, "112")); err != nil {
		t.Fatal(err)
	}
	// A stat that is there and cannot be read.
	if err := os.MkdirAll(filepath.Join(root, "105", "stat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := procChildrenIn(root, me); err == nil {
		t.Fatalf("a stat that could not be read read as children %+v", got)
	}
	if _, err := procChildrenIn(filepath.Join(root, "gone"), me); err == nil {
		t.Fatal("a /proc that could not be listed read as no children")
	}
}

// A stop whose first read of its children did not settle reads them again,
// and says what that read says: nothing, where it settled on nothing; that it
// cannot tell, where the read failed.
func TestAStopReadsAgainWhereItsFirstReadDidNotSettle(t *testing.T) {
	scan, was := scanChildren, adopting.Load()
	adopting.Store(true) // stops nothing here: no child runs
	t.Cleanup(func() { scanChildren = scan; adopting.Store(was) })
	n := 0
	scanChildren = func() ([]child, error) {
		n++
		if n <= settleScans {
			// Gone at once each time: never two scans the same.
			return []child{{pid: 1 << 22, start: uint64(n)}}, nil
		}
		return nil, nil
	}
	if _, left := stopAdopted(zap.NewNop(), nil, nil); left != "" || n <= settleScans {
		t.Fatalf("left %q after %d scans; want the second read, settled on nothing, to say nothing", left, n)
	}
	scanChildren = func() ([]child, error) { return nil, errors.New("list /proc: too many open files") }
	if _, left := stopAdopted(zap.NewNop(), nil, nil); !strings.Contains(left, "keploy could not tell its own processes from them (read which processes are keploy's children: list /proc: too many open files)") {
		t.Fatalf("left %q; want it to say the read of keploy's children failed", left)
	}
}

// A stop whose daemons' children keep being adopted over every read of them
// (a daemon that keeps starting processes it leaves) stops what it found,
// and says that what the application left running cannot be told.
func TestAStopWhoseReadNeverSettlesSaysItCannotTell(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	pid := daemonize(t, "exec sleep 120")
	await(t, "the daemon adopted", func() bool { return parentIs(pid, os.Getpid()) })
	scan := scanChildren
	t.Cleanup(func() { scanChildren = scan })
	n := 0
	scanChildren = func() ([]child, error) {
		cs, err := procChildren()
		n++
		// And one more each scan, gone at once: never two scans the same.
		return append(cs, child{pid: 1 << 22, start: uint64(n)}), err
	}
	_, left := stopAdopted(zap.NewNop(), nil, nil)
	if parentIs(pid, os.Getpid()) {
		t.Fatalf("the daemon %d the read found was not stopped", pid)
	}
	if !strings.Contains(left, "kept changing over") {
		t.Fatalf("left %q; want it to say what runs on cannot be told", left)
	}
}

// A stop of the daemons whose first look compared their files, and whose
// later looks could not (a daemon ran a program keploy's user may not read,
// or changed its credentials), says that it counts descriptors at the first
// of those later looks, naming the daemon, and not again at the next, as the
// app's own stop does (utils' TestAStopSaysItCountsDescriptorsFromTheLookThatDid).
func TestTheDaemonsStopSaysItCountsDescriptorsFromTheLookThatDid(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 50
	dir := t.TempDir()
	ready, release := filepath.Join(dir, "ready"), filepath.Join(dir, "release")
	if err := unix.Mkfifo(release, 0o600); err != nil {
		t.Fatal(err)
	}
	// Held open by the test, so the daemon's open of it never blocks, and
	// its read ends at the test's write, or at EOF once the test is gone.
	fifo, err := os.OpenFile(release, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fifo.Close() })
	// On TERM it waits for the test (a builtin's read: it starts nothing
	// the stop would read as the daemon's), then exits.
	pid := daemonize(t, `trap 'read x <"$2"; exit 0' TERM; : > "$1"; while kill -0 "$DAEMON_OWNER"; do sleep 0.05; done`, ready, release)
	await(t, "the daemon adopted", func() bool { return parentIs(pid, os.Getpid()) })
	await(t, "the daemon ready", func() bool { _, err := os.Stat(ready); return err == nil })
	// Each descriptor is a file of its own, until the kernel refuses.
	compared := func(_, a, b int) (int, error) {
		switch {
		case a < b:
			return 1, nil
		case a > b:
			return 2, nil
		}
		return 0, nil
	}
	refused := func(int, int, int) (int, error) { return 0, syscall.EPERM }
	t.Cleanup(utils.SetFilesComparedForTest(compared))
	var said atomic.Int64
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if strings.Contains(e.Message, "counts the open files of the processes it stops as their descriptors") {
			said.Add(1)
		}
		return nil
	}))
	// The stop's first look compares; the kernel refuses from the loop's
	// first look on, and the daemon exits once the loop has looked twice.
	// Each count is taken as the loop is about to look again: what the
	// looks before said.
	var looks atomic.Int32
	atFirst, atSecond := atomic.Int64{}, atomic.Int64{}
	looked := func() {
		switch looks.Add(1) {
		case 1:
			atFirst.Store(said.Load())
			utils.SetFilesComparedForTest(refused)
		case 2:
			atSecond.Store(said.Load())
			_, _ = fifo.WriteString("go\n")
		}
	}
	stopLooked.Store(&looked)
	defer stopLooked.Store(nil)
	if _, left := stopAdopted(logger, nil, nil); left != "" {
		t.Fatalf("left %q; want nothing: the daemon exited", left)
	}
	if parentIs(pid, os.Getpid()) {
		t.Fatalf("premise: daemon %d was not stopped", pid)
	}
	if n := looks.Load(); n < 2 {
		t.Fatalf("the stop looked %d times after its first look; want 2", n)
	}
	if n := atFirst.Load(); n != 0 {
		t.Fatalf("a stop whose first look compared the daemon's files said %d times that it counts descriptors; want never", n)
	}
	if n := atSecond.Load(); n != 1 {
		t.Fatalf("a stop whose look counted the daemon's descriptors had said so %d times by its next look; want once, at that look", n)
	}
	if all := logs.FilterMessageSnippet("as their descriptors").All(); len(all) != 1 ||
		!strings.Contains(fmt.Sprint(all[0].ContextMap()["error"]), "process "+strconv.Itoa(pid)+"'s descriptors: operation not permitted") {
		t.Fatalf("a stop whose later looks counted the daemon's descriptors said so %d times (%v); want once, naming daemon %d and why", len(all), all, pid)
	}
}

// A stop whose second read, once its daemons are stopped, cannot tell its
// own children from what they started on their way out (its fds unreadable
// by then) says so: what the daemons started may run on.
func TestAStopThatCannotTellItsOwnOnceItsDaemonsStoppedSaysSo(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 50
	ready := filepath.Join(t.TempDir(), "ready")
	// Its way out takes a few of the stop's checks.
	pid := daemonize(t, `trap 'sleep 0.3; exit 0' TERM; : > "$1"; while kill -0 "$DAEMON_OWNER"; do sleep 0.05; done`, ready)
	await(t, "the daemon ready", func() bool { _, err := os.Stat(ready); return err == nil })
	var restore func()
	looked := func() {
		if restore == nil {
			restore = breakFdinfo(t)
		}
	}
	stopLooked.Store(&looked)
	defer func() {
		stopLooked.Store(nil)
		if restore != nil {
			restore()
		}
	}()
	_, left := stopAdopted(zap.NewNop(), nil, nil)
	if restore == nil {
		t.Fatal("premise: the stop never checked on the daemon's way out")
	}
	if parentIs(pid, os.Getpid()) {
		t.Fatalf("premise: daemon %d was not stopped", pid)
	}
	if !strings.Contains(left, "what the application's daemons started as they stopped: keploy could not tell its own processes from them") {
		t.Fatalf("left %q; want it to say keploy could not tell its own processes from what the daemons started", left)
	}
}

// An adopted daemon that has exited and is not reaped yet (a zombie the
// reaper has not come to) runs nothing: a stop neither stops it nor says it
// left it running.
func TestAZombieNotReapedYetIsNothingLeftRunning(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	// Set before the reaper starts, which reads it: no adopted child can be
	// reaped yet.
	waitAdopted = func(int) (int32, error) { return 0, nil }
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	pid := daemonExitingAdopted(t)
	await(t, "the daemon exited, adopted", func() bool { return zombieOf(pid, os.Getpid()) })
	if _, left := stopAdopted(zap.NewNop(), nil, nil); left != "" {
		t.Fatalf("left %q; want nothing: daemon %d exited, a zombie not reaped yet", left, pid)
	}
}

// The reaper reaps under the lock a read of the children holds for all of
// its scans (reaping), so no reap lands between two scans the read compares.
func TestTheReaperReapsUnderTheLockAReadOfTheChildrenHolds(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	var held, free atomic.Int32
	reap := waitAdopted
	// Set before the reaper starts, which reads it.
	waitAdopted = func(fd int) (int32, error) {
		if reaping.TryLock() {
			reaping.Unlock()
			free.Add(1)
		} else {
			held.Add(1)
		}
		return reap(fd)
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	pid := daemonExitingAdopted(t)
	await(t, "the daemon reaped", func() bool { _, ok := utils.ReadProcStat(pid); return !ok })
	if held.Load() == 0 || free.Load() != 0 {
		t.Fatalf("%d reaps under the lock, %d without it; want every one under it", held.Load(), free.Load())
	}
}

// zombieLeaderEnv makes this test binary, started with it set to a file's
// path, a process whose main thread exits while another runs on, writing a
// count to that file as it runs: its stat says Z, as ps says Zl, and it
// runs.
const zombieLeaderEnv = "KEPLOY_TEST_MAIN_THREAD_EXITS"

func init() {
	beat := os.Getenv(zombieLeaderEnv)
	if beat == "" {
		return
	}
	// init runs on the main thread: the goroutine stays on it.
	runtime.LockOSThread()
	go func() {
		for n := 1; ; n++ {
			_ = os.WriteFile(beat, []byte(strconv.Itoa(n)), 0o644)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	for {
		if _, err := os.Stat(beat); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// The main thread alone exits (exit, not exit_group).
	_, _, _ = syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0)
}

// beatOf is the count a process zombieLeaderEnv started wrote last; -1 where
// it has written none.
func beatOf(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1
	}
	return n
}

// A daemon whose main thread exited while another thread runs on ("Zl" in
// ps: its stat says Z) runs: the stop stops it, and says it left nothing,
// rather than take it for a zombie that runs nothing, which it would neither
// stop nor say it left running, the next set then moving the application's
// base while it runs. What it runs is read through the thread that runs (its
// main thread's exe reads as none), so it is told from keploy's own.
func TestAStopStopsADaemonWhoseMainThreadExited(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	// A copy of this binary: what keploy itself runs is not the
	// application's.
	dir := t.TempDir()
	bin := filepath.Join(dir, "daemon")
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, self, 0o755); err != nil {
		t.Fatal(err)
	}
	beat := filepath.Join(dir, "beat")
	// No collection (a stop of the world would wait for ever on the main
	// thread, which holds a P it never gives back), and Ps to spare.
	pid := daemonize(t, `exec env `+zombieLeaderEnv+`="$1" GOGC=off GOMAXPROCS=4 "$2" -test.run=^$`, beat, bin)
	me := os.Getpid()
	await(t, "the daemon adopted, its main thread exited", func() bool {
		st, ok := utils.ReadProcStat(pid)
		return ok && st.PPID == me && st.State == 'Z'
	})
	first := beatOf(beat)
	await(t, "the daemon's other thread running", func() bool { return beatOf(beat) > first+2 })
	_, left := stopAdopted(zap.NewNop(), nil, nil)
	if st, ok := utils.ReadProcStat(pid); ok && st.PPID == me && !utils.Exited(pid, st) {
		t.Fatalf("the daemon %d (its main thread exited, another running) runs on after the stop, which said it left %q", pid, left)
	}
	if left != "" {
		t.Fatalf("left %q; want nothing: the stop ended the daemon", left)
	}
}

// A reaper that stops (it panicked) leaves keploy unable to tell whether
// the application's processes end: it no longer adopts them, and every stop
// after says so.
func TestAReaperThatStopsLeavesEveryStopSayingItCannotTell(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	// Set before the reaper starts, which reads it.
	waitAdopted = func(int) (int32, error) { panic("a reap that failed") }
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	daemonExitingAdopted(t) // adopted, then exits: the reaper's first reap
	await(t, "the reaper stopped", func() bool { return !adopting.Load() })
	_, left := stopAdopted(zap.NewNop(), nil, nil)
	if !strings.Contains(left, "keploy cannot tell whether its processes ended (it could not adopt them: keploy stopped reaping the daemons it adopts)") {
		t.Fatalf("left %q; want it to say keploy cannot tell, as its reaper stopped", left)
	}
}

// Where keploy was asked to adopt the application's daemons and could not (Go
// without pidfds, a subreaper refused), a stop cannot tell whether the
// application's processes ended, and says so; where it was never asked (the
// switch off, an application in a container), it says nothing.
func TestAStopWhereKeployCannotAdoptSaysItCannotTell(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	if _, left := stopAdopted(zap.NewNop(), nil, nil); left != "" {
		t.Fatalf("never asked to adopt: left %q, want nothing", left)
	}
	noteCannotAdopt("make the CLI a child subreaper: operation not permitted")
	defer cannotAdopt.Store(nil)
	_, left := stopAdopted(zap.NewNop(), nil, nil)
	if !strings.Contains(left, "keploy cannot tell whether its processes ended") || !strings.Contains(left, "operation not permitted") {
		t.Fatalf("left %q; want it to say keploy cannot tell whether the application's processes ended, and why", left)
	}
}
