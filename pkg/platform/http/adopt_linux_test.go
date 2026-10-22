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
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), adoptHelperEnv+"="+t.Name())
	// A process of its own that hangs ends with the test binary, at its
	// timeout, rather than outliving it holding what it adopted. The
	// signal comes when the thread that started it exits: it is held for
	// as long as the process runs.
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
	client := newRunClient(fmt.Sprintf("trap '' HUP; sleep 600 & echo $! > %s; echo $$ > %s; while :; do sleep 0.01; done",
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
	cmd := exec.Command(os.Args[0], "-test.run=^TestOwnProcessLeftoverHelper$", "-test.v", "-test.count=1")
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
// killed when the test ends, if it is still there.
func daemonize(t *testing.T, command string, args ...string) int {
	t.Helper()
	pid, err := startDaemon(command, args...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// startDaemon is daemonize's work, safe off the test's goroutine.
func startDaemon(command string, args ...string) (int, error) {
	out, err := exec.Command("/bin/sh", append([]string{"-c", `setsid /bin/sh -c "$0" sh "$@" >/dev/null 2>&1 </dev/null & echo $!`, command}, args...)...).Output()
	if err != nil {
		return 0, fmt.Errorf("the daemonizer: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("the daemonizer printed %q", out)
	}
	return pid, nil
}

// await waits until cond holds; the bound only turns a hang into a failure.
func await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(10 * time.Millisecond)
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
	pid := daemonize(t, "exit 0")
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
	case <-time.After(time.Minute): // the bound only turns a spin into a failure
		t.Fatal("the reaper kept scanning for a child it could not reap yet")
	}
	waitAdopted = real

	fdinfoDir = filepath.Join(t.TempDir(), "gone")
	if got := reapAdopted(zap.NewNop()); len(got) != 0 || !zombieOf(pid, os.Getpid()) {
		t.Fatalf("with its fds unreadable, keploy reaped %v", got)
	}
	fdinfoDir = "/proc/self/fdinfo"
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
	busy := daemonize(t, `trap '' TERM; : > "$1"; while :; do sleep 0.01; done`, ready("busy"))
	// One that opens and closes a file over and over (a retry loop, a DNS
	// lookup): a count that rises and falls back is no progress.
	churn := daemonize(t, `trap '' TERM; : > "$1"; while :; do exec 3</dev/null; sleep 0.05; exec 3<&-; sleep 0.05; done`, ready("churn"))
	slow := daemonize(t, `exec 3</dev/null 4</dev/null 5</dev/null 6</dev/null 7</dev/null 8</dev/null 9</dev/null
trap 'for fd in 3 4 5 6 7 8 9; do eval "exec $fd<&-"; sleep 0.3; done; echo done > "$2"; exit 0' TERM
: > "$1"; while :; do sleep 0.01; done`, ready("slow"), done)
	for _, name := range []string{"polite", "deaf", "busy", "churn", "slow"} {
		await(t, name+" ready", func() bool { _, err := os.Stat(ready(name)); return err == nil })
	}

	core, logs := observer.New(zapcore.DebugLevel)
	// The run's teardown scope: the slow daemon's steps on its way out move
	// it, which the run's drains wait on.
	scope := &utils.TeardownScope{}
	why := stopAdopted(zap.New(core), nil, scope)
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
	why := stopAdopted(zap.New(core), nil, nil)
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
			`sh -c 'trap "while :; do sleep 1; done" TERM; touch `+ready+`; while :; do sleep 1; done' & wait`, nil,
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
// killed on the stall bound.
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
	// No child of its own: one that ended would be a step (a process of it
	// gone).
	churn := daemonize(t, `trap '' TERM; : > "$1"; while :; do exec 3</dev/null; exec 3<&-; done`, ready)
	await(t, "the daemon ready", func() bool { _, err := os.Stat(ready); return err == nil })
	scope := &utils.TeardownScope{}
	core, logs := observer.New(zapcore.InfoLevel)
	done := make(chan struct{})
	go func() { stopAdopted(zap.New(core), nil, scope); close(done) }()
	// Read as the stop decides to kill it: what the kill does after (the
	// kernel freeing what it held) is not the daemon's way out.
	var before uint64
	await(t, "the stall kill", func() bool {
		if logs.FilterMessageSnippet("made no progress on its way out after it was asked to stop; killing it").Len() == 0 {
			return false
		}
		before = scope.Moves()
		return true
	})
	<-done
	if parentIs(churn, os.Getpid()) {
		t.Fatal("the churning daemon outlived its stop")
	}
	if before > 1 {
		t.Fatalf("a daemon that only churned moved the run's teardown %d times before its kill", before)
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
