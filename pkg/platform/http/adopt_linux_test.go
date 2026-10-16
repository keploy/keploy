//go:build linux

package http

import (
	"bytes"
	"errors"
	"fmt"
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
func inOwnProcess(t *testing.T) bool {
	t.Helper()
	if os.Getenv(adoptHelperEnv) == t.Name() {
		if err := goUsesPidfds(); err != nil {
			t.Skipf("keploy adopts nothing here: %v", err)
		}
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
	// Two quiet seconds escalate; the slow daemon's way out takes two, in
	// steps a seventh as long.
	stopCheck, stopStallChecks = 20*time.Millisecond, 100

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
	stopAdopted(zap.New(core))
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
	killed := logs.FilterMessageSnippet("killing it").All()
	if len(killed) != 1 {
		t.Fatalf("%d kills, want one, of the daemons that ignored the request", len(killed))
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
	stopAdopted(zap.New(core))
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
	if !parentIs(late, os.Getpid()) {
		t.Fatal("the stop ended a daemon adopted after it began")
	}
}
