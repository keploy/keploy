//go:build linux

package tls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// TestMain points the trust-store lock at a directory of the package run's
// own, so that no test, whether or not it asks for a lock of its own
// (trustStoreLockForTest), takes or waits for the host's /run/keploy one,
// which as root it would share with running agents.
func TestMain(m *testing.M) {
	if os.Getenv("KEPLOY_TEST_KEYSTORE_EDIT") != "" || os.Getenv("KEPLOY_TEST_TRUST_STATE_DIR_HOLD") != "" {
		// A helper process of a test, which is given its test's lock
		// directory and leaves by os.Exit.
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "keploy-tls-test-trust-state-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "no directory for the tests' trust-store lock:", err)
		os.Exit(2)
	}
	trustStateDir = dir
	// Never the host's bundles, nor its record of a rebuild owed.
	legacyCABundlePaths, legacyRebuildMarker = []string{filepath.Join(dir, "no-bundle")}, filepath.Join(dir, "no-marker")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// trustStoreLockForTest points the trust-store lock at a directory of the
// test's own until it ends, so that no test takes, or waits for, the host's.
func trustStoreLockForTest(t *testing.T) {
	t.Helper()
	old := trustStateDir
	trustStateDir = t.TempDir()
	t.Cleanup(func() { trustStateDir = old })
}

// breakTrustStoreLockForTest makes the trust-store lock one that cannot be
// taken, for the rest of the test: its directory cannot be made, a file
// stands where it goes.
func breakTrustStoreLockForTest(t *testing.T) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := trustStateDir
	trustStateDir = filepath.Join(f, "keploy")
	t.Cleanup(func() { trustStateDir = old })
}

// The helper process of the lock's tests: it takes the lock, says so on
// stdout, and holds it until it is killed or its stdin ends (its test is
// gone, however it went). It does nothing unless a test started it.
func TestTrustStoreLockHelperProcess(t *testing.T) {
	dir := os.Getenv("KEPLOY_TEST_TRUST_STATE_DIR_HOLD")
	if dir == "" {
		return
	}
	trustStateDir = dir
	if _, _, err := lockTrustStore(context.Background(), zap.NewNop()); err != nil {
		os.Exit(1)
	}
	_, _ = os.Stdout.WriteString("held\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// holdTrustStoreLock starts a process of its own that takes the trust-store
// lock (in the test's lock directory) and holds it, and returns the function
// that kills it.
func holdTrustStoreLock(t *testing.T) (kill func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTrustStoreLockHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "KEPLOY_TEST_TRUST_STATE_DIR_HOLD="+trustStateDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Its stdin stays open until the helper is killed: its end is how a
	// helper whose test died goes too.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	killed := false
	kill = func() {
		if !killed {
			killed = true
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			_ = stdin.Close()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(kill)
	buf := make([]byte, 5)
	if n, _ := out.Read(buf); !strings.HasPrefix(string(buf[:n]), "held") {
		t.Fatalf("the lock's helper process did not take the lock (%q)", buf[:n])
	}
	return kill
}

// One keploy process at a time holds the lock, another process's holder keeps
// it out until that holder is gone (killed, here: the kernel drops a flock with
// its last holder), and a wait whose context ends gives up without it.
func TestTrustStoreLockKeepsOtherProcessesOut(t *testing.T) {
	trustStoreLockForTest(t)
	kill := holdTrustStoreLock(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := lockTrustStore(ctx, zap.NewNop()); err == nil {
		t.Fatal("took the lock another process holds")
	}

	kill()
	_, unlock, err := lockTrustStore(context.Background(), zap.NewNop())
	if err != nil {
		t.Fatalf("the lock of a killed holder was not free: %v", err)
	}
	unlock()
}

// Work under a context lockTrustStore returned does not take the lock again,
// which it would wait for for ever (holdsTrustStoreLock).
func TestTrustStoreLockHeldIsSeenOnItsContext(t *testing.T) {
	trustStoreLockForTest(t)
	work, unlock, err := lockTrustStore(context.Background(), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if !holdsTrustStoreLock(work) || holdsTrustStoreLock(context.Background()) {
		t.Fatal("holdsTrustStoreLock does not tell the lock's context from another")
	}
	// Work under the lock's context does not take the lock again: the lock is
	// not reentrant, so a second take would wait for this very holder (here,
	// until the bound below).
	bounded, cancel := context.WithTimeout(work, 5*time.Second)
	defer cancel()
	ran := false
	if err := withTrustStoreLock(bounded, zap.NewNop(), func(locked context.Context) error {
		ran = holdsTrustStoreLock(locked)
		return nil
	}); err != nil || !ran {
		t.Fatalf("work under the lock's own context waited for the lock (err %v, ran under it %v)", err, ran)
	}
}

// setTestXattr sets name=value on path and reports whether it could (a
// filesystem without user extended attributes cannot).
func setTestXattr(t *testing.T, path, name, value string) bool {
	t.Helper()
	if err := unix.Setxattr(path, name, []byte(value), 0); err != nil {
		t.Logf("no %s on %s (%v): its copy is not checked", name, path, err)
		return false
	}
	return true
}

// testXattr reads name of path, "" when it has none.
func testXattr(t *testing.T, path, name string) string {
	t.Helper()
	v, err := xattrValue(path, name)
	if err != nil {
		return ""
	}
	return string(v)
}

// A wait for the lock whose context ends gives up without the lock, and when
// its turn comes gives the lock straight back, so an abandoned wait (a stop
// during a start queued for the lock) never holds up the processes queued
// behind it.
func TestAnAbandonedWaitGivesTheLockBack(t *testing.T) {
	trustStoreLockForTest(t)
	returned := make(chan struct{})
	hook := func() { close(returned) }
	abandonedWaitReturned.Store(&hook)
	t.Cleanup(func() { abandonedWaitReturned.Store(nil) })

	kill := holdTrustStoreLock(t)
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	var once sync.Once
	core, _ := observer.New(zapcore.DebugLevel)
	logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if e.Message == msgTrustStoreBusy {
			once.Do(func() { close(waiting) })
		}
		return nil
	}))
	got := make(chan error, 1)
	go func() {
		_, _, err := lockTrustStore(ctx, logger)
		got <- err
	}()
	select {
	case <-waiting:
	case err := <-got:
		t.Fatalf("took the lock another process holds (err %v)", err)
	}
	cancel()
	if err := <-got; err == nil {
		t.Fatal("a wait whose context ended took the lock")
	}
	kill() // the holder's turn ends; the abandoned wait's comes, and is given back
	<-returned
	// A done context takes a free lock and does not wait for a busy one.
	done, stop := context.WithCancel(context.Background())
	stop()
	_, unlock, err := lockTrustStore(done, zap.NewNop())
	if err != nil {
		t.Fatalf("the lock an abandoned wait was given is not free: %v", err)
	}
	unlock()
}

// beatenWait is a wait for the trust-store lock (lockTrustStore, from a
// goroutine) whose beats the test gives (trustStoreWaitBeats), which says each
// beat it has counted, with its count of beats without a step
// (trustStoreWaitObserved), and each step of the queue it has seen
// (trustStoreQueueMoved). The test drives it beat by beat, and never waits
// for one thing without watching for the wait to have ended instead.
//
// The wait ends with its test, however the test ends: its cleanup stops it
// (its context) and waits for it before the variables it reads are put back,
// so a test that fails part way leaves no wait reading them into the next
// test. (A wait stopped before its turn leaves lockTrustStore's give-back of
// the lock to come when the holder lets go; that touches only its lock file
// and abandonedWaitReturned.)
type beatenWait struct {
	t       *testing.T
	beats   chan time.Time
	counted chan int
	moved   chan struct{}
	got     chan error
	stop    chan struct{} // closed at the test's end: no hook of the wait blocks after it
}

// startBeatenWait starts a wait for the lock, which another process holds,
// and returns once it is queued (msgTrustStoreBusy).
func startBeatenWait(t *testing.T) *beatenWait {
	t.Helper()
	w := &beatenWait{t: t, beats: make(chan time.Time), counted: make(chan int, 1), moved: make(chan struct{}, 64), got: make(chan error, 1), stop: make(chan struct{})}
	oldBeats, oldObserved, oldMoved := trustStoreWaitBeats, trustStoreWaitObserved, trustStoreQueueMoved
	trustStoreWaitBeats = func() (<-chan time.Time, func()) { return w.beats, func() {} }
	trustStoreWaitObserved = func(quiet int) {
		select {
		case w.counted <- quiet:
		case <-w.stop:
		}
	}
	trustStoreQueueMoved = func() {
		select {
		case w.moved <- struct{}{}:
		case <-w.stop:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	t.Cleanup(func() {
		close(w.stop)
		cancel()
		<-ended
		trustStoreWaitBeats, trustStoreWaitObserved, trustStoreQueueMoved = oldBeats, oldObserved, oldMoved
		stalledHolderRecord.Store(nil)
	})
	waiting := make(chan struct{})
	var once sync.Once
	core, _ := observer.New(zapcore.DebugLevel)
	logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if e.Message == msgTrustStoreBusy {
			once.Do(func() { close(waiting) })
		}
		return nil
	}))
	go func() {
		defer close(ended)
		_, unlock, err := lockTrustStore(ctx, logger)
		if err == nil {
			unlock()
		}
		w.got <- err
	}()
	select {
	case <-waiting:
	case err := <-w.got:
		t.Fatalf("took the lock another process holds (err %v)", err)
	}
	return w
}

// beat gives the wait a beat and returns its count of beats without a step
// then, and whether it saw a step of the queue at that beat.
func (w *beatenWait) beat() (quiet int, moved bool) {
	w.t.Helper()
	select {
	case w.beats <- time.Now():
	case err := <-w.got:
		w.t.Fatalf("the wait ended (err %v) before it was given a beat", err)
	}
	select {
	case quiet = <-w.counted:
	case err := <-w.got:
		// The wait counts a beat before it ends on it: both can be ready.
		select {
		case quiet = <-w.counted:
			w.got <- err
		default:
			w.t.Fatalf("the wait ended (err %v) at a beat instead of counting it", err)
		}
	}
	select {
	case <-w.moved:
		moved = true
	default:
	}
	return quiet, moved
}

// stepOfTheHolder writes a step into the holder's record, as the holder
// does at each step of its change (noteTrustStoreProgress).
func stepOfTheHolder(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile(trustStoreLockPath(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	noteTrustStoreProgress(context.WithValue(context.Background(), storeLockKey{}, f))
	_ = f.Close()
}

// A process waiting for the lock sees the queue move: each step the holder
// takes rewrites its record in the lock file (noteTrustStoreProgress), which
// the wait reads at each beat, and is a step of the wait, which the CLI's
// judge of a start or a stop counts; a beat with no step is not one.
func TestAWaitForTheLockSeesTheQueueMove(t *testing.T) {
	trustStoreLockForTest(t)
	kill := holdTrustStoreLock(t)
	w := startBeatenWait(t)
	if quiet, moved := w.beat(); moved || quiet != 1 {
		t.Fatalf("a beat with no step of the holder's: counted %d, step seen %v", quiet, moved)
	}
	stepOfTheHolder(t)
	if quiet, moved := w.beat(); !moved || quiet != 0 {
		t.Fatalf("a beat after a step of the holder's: counted %d, step seen %v", quiet, moved)
	}
	kill()
	if err := <-w.got; err != nil {
		t.Fatalf("the lock of a killed holder was not given to the waiter: %v", err)
	}
}

// A wait for the lock gives up when the queue stops moving: trustStoreStall-
// Beats beats in a row with no step of the holder's. Without that, an agent
// stopping with no CLI to watch it (its client killed, an operator's kill)
// waited for ever behind a holder that was wedged. A step resets the count.
// And a later wait behind the same holder, whose record has not changed since,
// gives it one beat rather than the whole count again.
func TestAWaitGivesUpWhenTheQueueStopsMoving(t *testing.T) {
	trustStoreLockForTest(t)
	oldStall := trustStoreStallBeats
	trustStoreStallBeats = 3
	t.Cleanup(func() { trustStoreStallBeats = oldStall })
	kill := holdTrustStoreLock(t) // a holder that takes no step
	w := startBeatenWait(t)
	for i, want := range []int{1, 2} {
		if quiet, _ := w.beat(); quiet != want {
			t.Fatalf("beat %d: counted %d beats without a step, want %d", i+1, quiet, want)
		}
	}
	stepOfTheHolder(t)
	for i, want := range []int{0, 1, 2} {
		if quiet, _ := w.beat(); quiet != want {
			t.Fatalf("beat %d after the step: counted %d, want %d", i+1, quiet, want)
		}
	}
	select {
	case err := <-w.got:
		t.Fatalf("the wait ended (err %v) with the queue still within its count", err)
	default:
	}
	if quiet, _ := w.beat(); quiet != 3 {
		t.Fatalf("the third beat in a row without a step: counted %d", quiet)
	}
	if err := <-w.got; !errors.Is(err, errTrustStoreQueueStalled) {
		t.Fatalf("the wait ended with %v, want it to give up on the stalled queue", err)
	}

	// The next wait, behind the same unchanged holder, which still runs: one
	// beat.
	w2 := startBeatenWait(t)
	if quiet, _ := w2.beat(); quiet != 3 {
		t.Fatalf("a wait behind a holder already given up on counted %d at its first beat, want 3", quiet)
	}
	if err := <-w2.got; !errors.Is(err, errTrustStoreQueueStalled) {
		t.Fatalf("the second wait ended with %v, want it to give up at once", err)
	}
	kill()
}

// A process queued for the lock sees the progress of a long command the holder
// runs under it (Debian's update-ca-certificates, whose hook starts a JVM),
// while it runs: the holder looks at the command each beat and rewrites its
// record when the command was scheduled (noteCommandProgress). Marked only at
// the command's end, a queued agent's start or stop took no step for as long
// as the command ran, and the CLI gave it up as stalled. Here the command is a
// fake rebuild that copies the test's lines back one at a time, and the test
// gives both their beats, the holder's and then the waiter's: each beat of
// the waiter after a look of the holder at a command that copied a line sees a
// step. (With the holder's beats on a clock, a waiter beaten faster than the
// holder's clock ran out its count between two of the holder's looks, on a
// loaded host, and gave up.)
func TestAWaiterSeesALongStoreCommandMakeProgress(t *testing.T) {
	update := nativeTrustStoreForTest(t)
	holderBeats, looked := make(chan time.Time), make(chan struct{}, 1)
	oldBeats, oldLooked := storeCommandBeats, storeCommandLooked
	storeCommandBeats = func() (<-chan time.Time, func()) { return holderBeats, func() {} }
	storeCommandLooked = func() { looked <- struct{}{} }
	t.Cleanup(func() { storeCommandBeats, storeCommandLooked = oldBeats, oldLooked })

	fifos := newKeytoolHold(t, "", "", false) // its two FIFOs: the command's acks, and the test's lines
	script := `#!/bin/sh
exec 8< '` + fifos.release + `'
exec 7<> '` + fifos.reached + `'
echo started >&7
while read l <&8; do
  [ "$l" = done ] && exit 0
  echo "$l" >&7
done
`
	if err := os.WriteFile(filepath.Join(update, "keploy-test-update-ca"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", update+string(os.PathListSeparator)+os.Getenv("PATH"))
	rebuilt, rebuildEnded := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(rebuildEnded)
		rebuilt <- updateCaStore(context.Background(), zap.NewNop())
	}()
	// However the test ends, the rebuild is let go to its end (the end of
	// its lines: the test is the release FIFO's only writer) and waited for,
	// before the variables it reads are put back.
	t.Cleanup(func() {
		_ = fifos.releaseF.Close()
		<-rebuildEnded
	})
	fifos.waitReached(t, rebuilt)

	// holderLook gives the holder a beat, and returns once it has looked at
	// the command. The bound only turns a holder that never looks (no look
	// at the command at all, the bug this test is for) into a failure, not a
	// hang of the package.
	holderLook := func() {
		t.Helper()
		select {
		case holderBeats <- time.Now():
		case err := <-rebuilt:
			t.Fatalf("the rebuild ended (err %v) before the holder looked at it", err)
		case <-time.After(time.Minute):
			t.Fatal("the holder took no beat in a minute: nothing looks at the command it runs under the lock")
		}
		select {
		case <-looked:
		case <-time.After(time.Minute):
			t.Fatal("the holder took a beat but did not look at the command in a minute")
		}
	}
	holderLook() // its first look, which counts as progress whatever the command did: before the waiter queues
	w := startBeatenWait(t)
	for line := 1; line <= 3; line++ {
		fifos.letGo(t) // the command wakes, copies the line back, and waits for the next
		fifos.waitReached(t, rebuilt)
		holderLook()
		if quiet, seen := w.beat(); !seen || quiet != 0 {
			t.Fatalf("line %d: the waiter's beat after the holder looked at the command that copied it counted %d beats without a step, step seen %v; want a step: to the CLI it waited without progress", line, quiet, seen)
		}
	}
	if _, err := fifos.releaseF.WriteString("done\n"); err != nil {
		t.Fatal(err)
	}
	if err := <-rebuilt; err != nil {
		t.Fatalf("updateCaStore: %v", err)
	}
	if err := <-w.got; err != nil {
		t.Fatal(err)
	}
}

// relabelForTest gives path, a file of the test's own, the SELinux type typ
// (chcon) and returns its label then, "" where that cannot be done (no
// SELinux, no chcon): its copy is then not checked.
func relabelForTest(t *testing.T, path, typ string) string {
	t.Helper()
	chcon, err := exec.LookPath("chcon")
	if err != nil {
		return ""
	}
	if out, err := exec.Command(chcon, "-t", typ, path).CombinedOutput(); err != nil {
		t.Logf("no SELinux label set on %s (%v: %s): its copy is not checked", path, err, strings.TrimSpace(string(out)))
		return ""
	}
	label := testXattr(t, path, "security.selinux")
	t.Logf("%s is labelled %s", path, label)
	return label
}

// A rebuild that runs past the ceiling is left running, never killed: it
// writes the live store (Fedora's update-ca-trust empties /etc/pki/tls/certs
// before it fills it again), and it keeps the trust-store lock it inherited
// until it ends, so that no other agent changes the store beside it. The wait
// for it ends with an error naming it, and the rebuild still gets to its end.
// A later wait of this process behind it (the teardown's, after a start whose
// rebuild was left running) gives it one beat, not the whole count.
func TestARebuildPastTheCeilingIsLeftRunningAndKeepsTheLock(t *testing.T) {
	update := nativeTrustStoreForTest(t)
	oldStall := trustStoreStallBeats
	trustStoreStallBeats = 3
	t.Cleanup(func() { trustStoreStallBeats = oldStall })
	// The ceiling comes when the test says, once the rebuild has started.
	ceiling := make(chan time.Time, 1)
	oldTimer := storeCommandCeilingTimer
	storeCommandCeilingTimer = func(time.Duration) (<-chan time.Time, func()) { return ceiling, func() {} }
	t.Cleanup(func() { storeCommandCeilingTimer = oldTimer })
	fifos := newKeytoolHold(t, "", "", false)
	finished := filepath.Join(t.TempDir(), "finished")
	script := `#!/bin/sh
exec 8< '` + fifos.release + `'
exec 7<> '` + fifos.reached + `'; echo started >&7; exec 7>&-
read _ <&8
echo done > '` + finished + `'
`
	if err := os.WriteFile(filepath.Join(update, "keploy-test-update-ca"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", update+string(os.PathListSeparator)+os.Getenv("PATH"))

	rebuilt := make(chan error, 1)
	go func() { rebuilt <- updateCaStore(context.Background(), zap.NewNop()) }()
	fifos.waitReached(t, rebuilt)
	ceiling <- time.Now()
	err := <-rebuilt
	if err == nil || !strings.Contains(err.Error(), "left running") {
		t.Fatalf("updateCaStore past its ceiling = %v; want it to leave the rebuild running", err)
	}
	// The rebuild runs on, holding the lock.
	done, stop := context.WithCancel(context.Background())
	stop()
	if _, _, err := lockTrustStore(done, zap.NewNop()); err == nil {
		t.Fatal("the lock was free while the rebuild left running still ran")
	}
	w := startBeatenWait(t)
	if quiet, _ := w.beat(); quiet != trustStoreStallBeats {
		t.Fatalf("a wait behind this process's own rebuild left running counted %d at its first beat, want %d", quiet, trustStoreStallBeats)
	}
	if err := <-w.got; !errors.Is(err, errTrustStoreQueueStalled) {
		t.Fatalf("the wait behind this process's own rebuild left running ended with %v, want it to give up at its first beat", err)
	}
	fifos.letGo(t) // the rebuild ends, and the lock with it
	if _, unlock, err := lockTrustStore(context.Background(), zap.NewNop()); err != nil {
		t.Fatalf("the lock was not free once the rebuild ended: %v", err)
	} else {
		unlock()
	}
	// The lock came free when the rebuild ended, not when it was killed: it
	// made the whole of its change.
	if _, err := os.Stat(finished); err != nil {
		t.Fatalf("the rebuild left running did not get to its end (killed at the ceiling?): %v", err)
	}
}
