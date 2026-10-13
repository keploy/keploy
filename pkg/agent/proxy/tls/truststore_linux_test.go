//go:build linux

package tls

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// None of these tests sleeps, polls or waits on a clock. A caller that has to
// queue for the trust-store lock logs msgTrustStoreBusy, a helper process
// announces each state on its stdout, a tool that must be caught mid-run
// reports through a FIFO and blocks on another; the tests block on those. And
// where a broken rule would leave a test waiting forever, the test is built so
// that it is told instead: it asks without blocking (tryFlock), or releases
// the thing being waited for first and then checks what happened.

// ---------------------------------------------------------------------------
// helper process
// ---------------------------------------------------------------------------

// The trust-store lock and the run claims exist to be seen from another
// process, and the property that matters most — the kernel drops them when the
// holder dies — cannot be shown without a process to kill. The helper is this
// test binary run again with helperModeEnv set; TestHelperProcess is its entry
// point and does nothing in an ordinary run.
const (
	helperModeEnv   = "KEPLOY_TLS_TEST_HELPER"
	helperStateEnv  = "KEPLOY_TLS_TEST_HELPER_STATE"
	helperRunEnv    = "KEPLOY_TLS_TEST_HELPER_RUN"
	helperStoreEnv  = "KEPLOY_TLS_TEST_HELPER_STORE"
	helperScriptEnv = "KEPLOY_TLS_TEST_HELPER_SCRIPT"
)

func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "lock":
		os.Exit(helperLock(os.Getenv(helperStateEnv)))
	case "lock-and-run":
		os.Exit(helperLockAndRun(os.Getenv(helperStateEnv), os.Getenv(helperScriptEnv)))
	case "claim":
		os.Exit(helperClaim(os.Getenv(helperStateEnv), os.Getenv(helperRunEnv)))
	case "agent":
		os.Exit(helperAgent(os.Getenv(helperStoreEnv)))
	}
	fmt.Printf("FAILED unknown helper mode %q\n", mode)
	os.Exit(2)
}

// helperLock takes the trust-store lock in stateDir and holds it until its
// stdin closes.
func helperLock(stateDir string) int {
	trustStateDir = stateDir
	_, unlock, err := lockTrustStore(context.Background(), zap.NewNop())
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		return 1
	}
	fmt.Println("LOCKED")
	_, _ = io.Copy(io.Discard, os.Stdin)
	unlock()
	return 0
}

// helperLockAndRun takes the trust-store lock in stateDir and runs a shell
// script under it with storeCommand, as a regeneration is run.
func helperLockAndRun(stateDir, script string) int {
	trustStateDir = stateDir
	work, unlock, err := lockTrustStore(context.Background(), zap.NewNop())
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		return 1
	}
	defer unlock()
	fmt.Println("LOCKED")
	if out, err := runStoreCommand(storeCommand(work, "sh", "-c", script)); err != nil {
		fmt.Printf("FAILED %v: %s\n", err, out)
		return 1
	}
	fmt.Println("RAN")
	return 0
}

// helperClaim claims a run in stateDir and holds the claim until its stdin
// closes.
func helperClaim(stateDir, runID string) int {
	trustStateDir = stateDir
	claim, err := claimRun(runID)
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		return 1
	}
	fmt.Println("CLAIMED")
	_, _ = io.Copy(io.Discard, os.Stdin)
	claim.release()
	return 0
}

// helperProc is a running helper, as the test that started it sees it.
type helperProc struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
	done  bool
}

// startHelper runs the helper in the given mode with extra environment
// ("KEY=value"). It is killed when the test ends, if it is still running.
func startHelper(t *testing.T, mode string, env ...string) *helperProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(append(os.Environ(), helperModeEnv+"="+mode), env...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the helper process: %v", err)
	}
	h := &helperProc{t: t, cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout)}
	t.Cleanup(h.kill)
	return h
}

// expect blocks until the helper prints its next line and fails the test unless
// that line starts with want. It returns the rest of the line's fields. A
// helper that dies instead closes its stdout, which ends the wait too.
func (h *helperProc) expect(want string) []string {
	h.t.Helper()
	line, err := h.out.ReadString('\n')
	if err != nil {
		h.t.Fatalf("the helper process exited before reporting %s (%v)", want, err)
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != want {
		h.t.Fatalf("the helper process reported %q, want %s", strings.TrimSpace(line), want)
	}
	return fields[1:]
}

// kill ends the helper the way a crash or an OOM kill would: nothing in it gets
// to run. By the time kill returns the process is gone and reaped.
func (h *helperProc) kill() {
	if h.done {
		return
	}
	h.done = true
	_ = h.cmd.Process.Kill()
	_, _ = io.Copy(io.Discard, h.out)
	_ = h.cmd.Wait()
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// useTrustState points the trust-store lock and the run claims at a directory
// of this test's own, and returns it.
func useTrustState(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "run", "keploy")
	prev := trustStateDir
	trustStateDir = dir
	t.Cleanup(func() { trustStateDir = prev })
	return dir
}

// busyLogger returns a logger, a channel that receives once for every caller
// that starts queueing for the trust-store lock, and everything logged.
func busyLogger() (*zap.Logger, <-chan struct{}, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	busy := make(chan struct{}, 64)
	logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if e.Message == msgTrustStoreBusy {
			busy <- struct{}{}
		}
		return nil
	}))
	return logger, busy, logs
}

// mustLockTrustStore takes the trust-store lock for the test itself, standing
// in for another agent that is in the middle of changing the store.
func mustLockTrustStore(t *testing.T) func() {
	t.Helper()
	_, unlock, err := lockTrustStore(context.Background(), zap.NewNop())
	if err != nil {
		t.Fatalf("taking the trust-store lock: %v", err)
	}
	return unlock
}

// trustStoreLockFree reports, without waiting, whether nobody holds the
// trust-store lock. It takes the lock for an instant to find out.
func trustStoreLockFree(t *testing.T) bool {
	t.Helper()
	f, err := os.OpenFile(trustStoreLockPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ok, err := tryFlock(f)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func testCertPEM(t *testing.T) []byte {
	t.Helper()
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	return ca.certPEM
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// mkfifo makes a FIFO at a fresh path in dir and returns the path.
func mkfifo(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// readFIFOLine opens a FIFO for reading and returns the first line written to
// it. The open blocks until a writer comes; a writer that is gone before it
// writes a line ends the read with what it got.
func readFIFOLine(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	line, _ := bufio.NewReader(f).ReadString('\n')
	return strings.TrimSpace(line)
}

// openGate lets a script blocked reading the FIFO at path go on. The FIFO is
// opened read-write, which never blocks, and closed, which is end-of-file for
// a reader that is there; one that is already gone is not waited for.
func openGate(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// ---------------------------------------------------------------------------
// the trust-store lock
// ---------------------------------------------------------------------------

// TestLockTrustStore_OneHolderAtATime is the serialisation itself: while one
// caller holds the lock a second one queues, and gets it when the first lets go.
func TestLockTrustStore_OneHolderAtATime(t *testing.T) {
	useTrustState(t)
	unlockFirst := mustLockTrustStore(t)

	logger, busy, _ := busyLogger()
	type result struct {
		unlock func()
		err    error
	}
	second := make(chan result, 1)
	go func() {
		_, unlock, err := lockTrustStore(context.Background(), logger)
		second <- result{unlock, err}
	}()

	select {
	case <-busy:
	case r := <-second:
		t.Fatalf("a second caller took the trust-store lock while the first still held it (err=%v)", r.err)
	}

	unlockFirst()
	r := <-second
	if r.err != nil {
		t.Fatalf("the second caller did not get the lock once it was free: %v", r.err)
	}
	r.unlock()
}

// TestLockTrustStore_ReleasedWhenTheHolderDies: a holder that is killed takes
// nobody else down with it — the kernel releases what it held.
func TestLockTrustStore_ReleasedWhenTheHolderDies(t *testing.T) {
	state := useTrustState(t)

	h := startHelper(t, "lock", helperStateEnv+"="+state)
	h.expect("LOCKED")
	if trustStoreLockFree(t) {
		t.Fatal("the lock is free while another process holds it")
	}

	h.kill()
	if !trustStoreLockFree(t) {
		t.Fatal("the lock of a killed holder was not released")
	}
}

// TestLockTrustStore_AnAbandonedWaitGivesTheLockBack: ctx is how a caller gives
// up its place in the queue. Giving up must not take the lock when the turn
// comes after all — the call returns the context's error — and must not keep
// it either: the next caller gets it.
func TestLockTrustStore_AnAbandonedWaitGivesTheLockBack(t *testing.T) {
	useTrustState(t)
	returned := make(chan struct{}, 1)
	hook := func() { returned <- struct{}{} }
	abandonedWaitReturned.Store(&hook)
	t.Cleanup(func() { abandonedWaitReturned.Store(nil) })

	unlockFirst := mustLockTrustStore(t)
	logger, busy, _ := busyLogger()
	gaveUp := errors.New("the caller gave up")
	ctx, cancel := context.WithCancelCause(context.Background())
	got := make(chan error, 1)
	go func() {
		_, unlock, err := lockTrustStore(ctx, logger)
		if err == nil {
			unlock()
		}
		got <- err
	}()
	select {
	case <-busy:
	case err := <-got:
		t.Fatalf("took the trust-store lock while it was held (err=%v)", err)
	}

	// Give up, then let the turn come: however the two meet, the call must
	// come back without the lock. (Released first so that a wait that ignored
	// its context would return here too — with the lock — instead of hanging.)
	cancel(gaveUp)
	unlockFirst()
	if err := <-got; !errors.Is(err, gaveUp) {
		t.Fatalf("an abandoned wait returned %v, want an error wrapping the context's cause", err)
	}

	<-returned
	if !trustStoreLockFree(t) {
		t.Fatal("an abandoned wait kept the lock after its turn came")
	}
}

// TestLockTrustStore_ABusyLockIsNotWaitedForWithADoneContext: a caller that
// has already given up does not queue at all.
func TestLockTrustStore_ABusyLockIsNotWaitedForWithADoneContext(t *testing.T) {
	useTrustState(t)
	unlock := mustLockTrustStore(t)
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logger, busy, _ := busyLogger()
	if _, _, err := lockTrustStore(ctx, logger); !errors.Is(err, context.Canceled) {
		t.Fatalf("lockTrustStore with a done context and a busy lock returned %v, want context.Canceled", err)
	}
	select {
	case <-busy:
		t.Fatal("a caller with a done context queued for the lock")
	default:
	}
}

// TestLockTrustStore_FreeLockIsTakenDespiteADoneContext: ctx bounds the wait,
// not the work. Teardown relies on it — after a second interrupt its context is
// done before it starts, and it must still regenerate when nobody else is in
// the way.
func TestLockTrustStore_FreeLockIsTakenDespiteADoneContext(t *testing.T) {
	useTrustState(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, unlock, err := lockTrustStore(ctx, zap.NewNop())
	if err != nil {
		t.Fatalf("a free lock was refused because the context was done: %v", err)
	}
	unlock()
}

// TestLockTrustStore_WorkIsNotCancelledWithTheCaller: what runs under the lock
// runs to its end. A regeneration killed half-way leaves the store half-written
// for the whole machine (on Fedora, OpenSSL's default directory empty).
func TestLockTrustStore_WorkIsNotCancelledWithTheCaller(t *testing.T) {
	useTrustState(t)
	ctx, cancel := context.WithCancel(context.Background())
	work, unlock, err := lockTrustStore(ctx, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	cancel()
	if work.Err() != nil {
		t.Fatal("the context to change the store under was cancelled with the caller's")
	}
	if err := storeCommand(work, "true").Run(); err != nil {
		t.Fatalf("a store command did not run after the caller's context was cancelled: %v", err)
	}
}

// TestLockTrustStore_NamesItsHolder: an agent that has to wait says which
// process it is waiting for, so a stuck holder can be found.
func TestLockTrustStore_NamesItsHolder(t *testing.T) {
	useTrustState(t)
	unlockFirst := mustLockTrustStore(t)

	logger, busy, logs := busyLogger()
	got := make(chan error, 1)
	go func() {
		_, unlock, err := lockTrustStore(context.Background(), logger)
		if err == nil {
			unlock()
		}
		got <- err
	}()
	select {
	case <-busy:
	case err := <-got:
		t.Fatalf("took the trust-store lock while it was held (err=%v)", err)
	}

	entries := logs.FilterMessage(msgTrustStoreBusy).All()
	if len(entries) != 1 {
		t.Fatalf("got %d %q entries, want 1", len(entries), msgTrustStoreBusy)
	}
	if got, want := entries[0].ContextMap()["held_by_pid"], strconv.Itoa(os.Getpid()); got != want {
		t.Errorf("the waiter named pid %v as the holder, want %s", got, want)
	}

	unlockFirst()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
}

// TestLockTrustStore_LockFileIsPrivate: the lock file must not be openable by
// other users — whoever can open it can hold the lock and stall every agent on
// the machine. keploy runs with umask 0, so the mode has to be explicit.
func TestLockTrustStore_LockFileIsPrivate(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	dir := useTrustState(t)
	mustLockTrustStore(t)()

	fi, err := os.Stat(trustStoreLockPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the lock file has mode %o, want 600", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm&0o022 != 0 {
		t.Errorf("the lock's directory has mode %o: group or others could replace the lock file", perm)
	}
}

// TestStoreCommand_TheLockOutlivesAKilledAgentWhileItsCommandRuns: an agent
// killed in the middle of a regeneration must not free the lock while the tool
// it started is still running — the next agent would regenerate beside it —
// and the tool must still be able to finish: it reports on its stdout, as the
// distro tools do, and that must not kill it now that its agent is gone. The
// tool reports its start through one FIFO and blocks on another, so it is
// caught mid-run without any clock.
func TestStoreCommand_TheLockOutlivesAKilledAgentWhileItsCommandRuns(t *testing.T) {
	state := useTrustState(t)
	dir := t.TempDir()
	started := mkfifo(t, dir, "started")
	gate := mkfifo(t, dir, "gate")
	finished := filepath.Join(dir, "finished")
	script := fmt.Sprintf("echo $$ > %q; read _ < %q || true; echo 'done; the store is updated'; echo >&2 'and so says stderr'; touch %q",
		started, gate, finished)

	h := startHelper(t, "lock-and-run", helperStateEnv+"="+state, helperScriptEnv+"="+script)
	h.expect("LOCKED")
	if pid := readFIFOLine(t, started); pid == "" {
		t.Fatal("the store command did not start")
	}

	h.kill()
	if trustStoreLockFree(t) {
		t.Fatal("the trust-store lock was released when the agent was killed, while the tool it started was still running")
	}

	// The tool finishes: then, and only then, the lock is free. (Blocking is
	// safe here: the tool exits as soon as its gate opens.)
	openGate(t, gate)
	_, unlock, err := lockTrustStore(context.Background(), zap.NewNop())
	if err != nil {
		t.Fatalf("the lock was not released once the tool exited: %v", err)
	}
	unlock()
	if !exists(finished) {
		t.Fatal("the tool died when it wrote to its output after its agent was killed, before it finished")
	}
}

// TestRunStoreCommand_ReturnsTheOutput: a store command's output still comes
// back, for the error message, and its exit status with it.
func TestRunStoreCommand_ReturnsTheOutput(t *testing.T) {
	out, err := runStoreCommand(exec.Command("sh", "-c", "echo out; echo err >&2; exit 3"))
	if got := string(out); got != "out\nerr\n" {
		t.Errorf("got output %q, want both streams", got)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Errorf("got error %v, want exit status 3", err)
	}
}

// ---------------------------------------------------------------------------
// run claims
// ---------------------------------------------------------------------------

const testRunID = "0123456789abcdef"

// TestRunIsLive_FollowsTheOwningProcess is the ownership rule. While the
// process that claimed a run is alive the run is live; once that process is
// gone — killed here, so nothing of its own ran to clean up — it is not, and
// its claim is cleared. No pid is consulted and no time passes: the only input
// is whether the kernel still holds the owner's lock.
func TestRunIsLive_FollowsTheOwningProcess(t *testing.T) {
	state := useTrustState(t)
	h := startHelper(t, "claim", helperStateEnv+"="+state, helperRunEnv+"="+testRunID)
	h.expect("CLAIMED")

	live, err := runIsLive(testRunID)
	if err != nil || !live {
		t.Fatalf("the run of a running process is not live (live=%v, err=%v)", live, err)
	}

	h.kill()

	live, err = runIsLive(testRunID)
	if err != nil || live {
		t.Fatalf("the run of a killed process is still live (live=%v, err=%v)", live, err)
	}
	if exists(filepath.Join(runClaimsDir(), testRunID)) {
		t.Error("the claim of a killed process was not cleared")
	}
}

// TestRunIsLive_AClaimHeldInThisProcess: a claim conflicts with every other
// open of its file, this process's included.
func TestRunIsLive_AClaimHeldInThisProcess(t *testing.T) {
	useTrustState(t)
	claim, err := claimRun(testRunID)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.release()

	if live, err := runIsLive(testRunID); err != nil || !live {
		t.Fatalf("a run claimed in this process is not live (live=%v, err=%v)", live, err)
	}
}

// TestRunIsLive_WithoutAClaim covers what a keploy from before claims, a
// reboot, and an id that no run could have all look like: nothing to hold, so
// nothing live.
func TestRunIsLive_WithoutAClaim(t *testing.T) {
	useTrustState(t)
	for _, id := range []string{testRunID, "", "not-a-run", "../../etc", "0123456789ABCDEF"} {
		if live, err := runIsLive(id); err != nil || live {
			t.Errorf("runIsLive(%q) = %v, %v; want false, nil", id, live, err)
		}
	}
}

// TestRunClaim_ReleaseClearsIt: a released claim leaves nothing behind, and the
// run is no longer live.
func TestRunClaim_ReleaseClearsIt(t *testing.T) {
	useTrustState(t)
	claim, err := claimRun(testRunID)
	if err != nil {
		t.Fatal(err)
	}
	claim.release()
	if exists(filepath.Join(runClaimsDir(), testRunID)) {
		t.Error("release left the claim file")
	}
	if live, err := runIsLive(testRunID); err != nil || live {
		t.Errorf("a released run is still live (live=%v, err=%v)", live, err)
	}
}

// TestRunClaim_IsANewFile: a claim is a file the agent creates itself — its
// mode and owner its own — never one it finds under that name.
func TestRunClaim_IsANewFile(t *testing.T) {
	useTrustState(t)
	if err := os.MkdirAll(runClaimsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	found := filepath.Join(runClaimsDir(), testRunID)
	if err := os.WriteFile(found, []byte("someone else's"), 0o644); err != nil {
		t.Fatal(err)
	}
	if claim, err := claimRun(testRunID); err == nil {
		claim.release()
		t.Fatal("claimed a run through a file that was already there")
	}
	if got, _ := os.ReadFile(found); string(got) != "someone else's" {
		t.Error("the file that was there was changed")
	}
}

// TestRunClaim_IsPrivate: whoever can open a claim can lock it, and a run
// whose claim someone else holds is never swept. So no other user may open
// one: the claims directory is 0700 and each claim 0600, under keploy's umask
// of 0, and also when the directory already existed with a looser mode.
func TestRunClaim_IsPrivate(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	useTrustState(t)
	if err := os.MkdirAll(runClaimsDir(), 0o777); err != nil {
		t.Fatal(err)
	}

	claim, err := claimRun(testRunID)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.release()

	di, err := os.Stat(runClaimsDir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("the claims directory has mode %o, want 700", perm)
	}
	fi, err := os.Stat(filepath.Join(runClaimsDir(), testRunID))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the claim has mode %o, want 600", perm)
	}
}

// TestSweep_ALockOnTheAnchorItselfDoesNotKeepIt: an anchor is a world-readable
// file, and any local user can flock a file they can read. What makes a run
// live is its claim under /run/keploy, so a lock on its anchor — here a shared
// one, as an unprivileged reader would take — does not keep a dead run's
// anchor from being swept.
func TestSweep_ALockOnTheAnchorItselfDoesNotKeepIt(t *testing.T) {
	s := newFakeTrustStore(t)
	anchor := s.anchor(testRunID)
	if err := os.WriteFile(anchor, testCertPEM(t), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}

	unlock := mustLockTrustStore(t)
	sweepRunLeftovers(zap.NewNop())
	unlock()
	if exists(anchor) {
		t.Fatal("a dead run's anchor was kept because someone held a lock on the anchor file")
	}
}

// TestWriteAnchor_ReadableNotWritable: the anchor is public and must be
// world-readable, and must not be world-writable even under keploy's umask of
// 0 — also when a same-named file was already there.
func TestWriteAnchor_ReadableNotWritable(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	pemBytes := testCertPEM(t)
	for _, tc := range []struct {
		name     string
		leftover bool
	}{
		{name: "new file"},
		{name: "over a same-named leftover", leftover: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			anchor := filepath.Join(t.TempDir(), "keploy-mitm-"+testRunID+".crt")
			if tc.leftover {
				stale := append([]byte("a longer, stale file that must not survive in any part\n"), pemBytes...)
				if err := os.WriteFile(anchor, append(stale, stale...), 0o666); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeAnchor(anchor, pemBytes); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(anchor)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(pemBytes) {
				t.Errorf("the anchor does not hold exactly the certificate (%d bytes, want %d)", len(got), len(pemBytes))
			}
			fi, err := os.Stat(anchor)
			if err != nil {
				t.Fatal(err)
			}
			if perm := fi.Mode().Perm(); perm != 0o644 {
				t.Errorf("the anchor has mode %o, want 644", perm)
			}
		})
	}
}

// TestWriteAnchor_DoesNotFollowASymlink: a symlink planted under an anchor's
// name must not make keploy write through it.
func TestWriteAnchor_DoesNotFollowASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("not keploy's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	anchor := filepath.Join(dir, "keploy-mitm-"+testRunID+".crt")
	if err := os.Symlink(target, anchor); err != nil {
		t.Fatal(err)
	}
	if err := writeAnchor(anchor, testCertPEM(t)); err == nil {
		t.Error("wrote an anchor through a symlink")
	}
	if got, _ := os.ReadFile(target); string(got) != "not keploy's\n" {
		t.Error("the symlink's target was changed")
	}
}
