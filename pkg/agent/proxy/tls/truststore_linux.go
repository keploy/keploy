//go:build linux

package tls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Several keploy agents can run on one machine at the same time — parallel CI
// jobs on a shared runner, two services of a monorepo recorded side by side —
// and each of them edits the same JDK keystore: its start imports its run's CA,
// its teardown deletes it, the retired CA's sweep deletes that. keytool reads
// the whole keystore, makes its one change and writes the whole keystore back,
// so two keytools at once lose one's change. What keeps them apart is a
// flock(2) on one file under /run/keploy, which the kernel drops the instant
// every process holding it is gone, however it went: no pid to be reused, no
// age to guess at. A lock that can be taken is, by that fact alone, a lock
// nobody holds.
//
// /run/keploy is root-owned, as the agent is, and the lock file is 0600, so no
// unprivileged process can open it and so hold up an agent. /run is a tmpfs:
// nothing outlives a reboot.
//
// The lock file holds a record of its holder: its pid and a count of the steps
// of its change, which the holder rewrites at each step (writeHolderRecord).
// A process waiting for the lock reads it at each beat; a record that changed
// is the queue moving, one that has not changed for trustStoreStallBeats beats
// a holder that has stopped (lockTrustStore).
//
// This is draft keploy#4755's lock, with the holder's record added (see
// truststore.go).

// trustStateDir holds the trust-store lock. A variable so tests can point it
// at a directory of their own.
var trustStateDir = "/run/keploy"

// trustStoreLockIsMachineWide says lockTrustStore is a lock every keploy
// process on this machine takes: one held keeps every other keploy edit of a
// JDK keystore out (editJavaKeystore relies on it to clear what a dead edit
// left).
const trustStoreLockIsMachineWide = true

// trustStoreLockPath is the file every keploy agent on this machine locks
// while it changes the trust store.
func trustStoreLockPath() string { return filepath.Join(trustStateDir, "ca-trust.lock") }

// lockTrustStore blocks until this process holds the machine-wide trust-store
// lock. It returns the context to change the store under and the function that
// releases the lock.
//
// The context returned is not cancelled with ctx: work that has started under
// the lock runs to its end (a trust-store rebuild killed half-way leaves its
// JDK keystore hook writing alone). It carries the lock to every command
// started under it with storeCommand, which then keeps the lock until it
// exits, and work under it does not take the lock again (holdsTrustStoreLock).
//
// The wait has no time limit of its own. It ends when the lock is free (a
// holder finishes, or dies and the kernel releases its lock); when ctx ends;
// or when the queue stops moving: trustStoreStallBeats beats in a row in which
// the holder's record did not change, the CLI's own rule for an agent that
// takes no step, doubled. So a holder must take a step (noteTrustStoreProgress,
// or run its commands through runStoreCommand or combinedOutputNoting, which
// mark one each beat they make progress) at least that often. A lock that is
// free is taken even when ctx is already done: ctx bounds the wait, not the
// work. A lock that is busy is not waited for once ctx is done, and behind a
// holder an earlier wait gave up on, whose record has not changed since, for
// one beat only; a wait that ends before its turn comes gives the lock
// straight back when it does come.
//
// The lock is not reentrant: a caller that already holds it and asks again
// waits for itself. Take it once, around the whole change.
func lockTrustStore(ctx context.Context, logger *zap.Logger) (context.Context, func(), error) {
	path := trustStoreLockPath()
	// Explicit modes: keploy runs with a process-wide umask of 0.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, fmt.Errorf("failed to create the directory of the trust-store lock %s: %w%s", path, err, rootHint(err))
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open the trust-store lock %s: %w%s", path, err, rootHint(err))
	}
	held := func() (context.Context, func(), error) {
		// Name the holder, and start its count of steps, for whoever queues
		// behind it.
		writeHolderRecord(f)
		work := context.WithValue(context.WithoutCancel(ctx), storeLockKey{}, f)
		return work, func() { _ = f.Close() }, nil
	}

	ok, err := tryFlock(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("failed to lock %s: %w", path, err)
	}
	if ok {
		return held()
	}

	if ctx.Err() != nil {
		// The caller has already given up: there is nothing to wait for.
		_ = f.Close()
		return nil, nil, fmt.Errorf("did not wait for the trust-store lock %s: %w", path, context.Cause(ctx))
	}
	last := readHolderRecord(f)
	quiet := 0
	if stalled := stalledHolderRecord.Load(); stalled != nil && last != "" && *stalled == last {
		// An earlier wait gave up on this holder, and its record has not
		// changed since: give it one beat, not the whole count again. (Every
		// new holder writes its record as it takes the lock, so an unchanged
		// record is the same holder, or a command it left holding the lock;
		// the beat covers the instant between a new holder's lock and its
		// record.)
		quiet = trustStoreStallBeats - 1
	}
	logger.Info(msgTrustStoreBusy, zap.String("held_by", holderOf(last)), zap.String("lock", path),
		zap.String("next_step", "to see which processes hold it: sudo fuser -v "+path))
	start := time.Now()

	// flock(2) cannot be interrupted from here, so the blocking call gets a
	// goroutine of its own. It owns f until it returns.
	granted := make(chan error, 1)
	go func() { granted <- waitFlock(f) }()

	// moved reads the holder's record and reports whether it changed: the
	// queue took a step, which is a step of this wait too.
	moved := func() bool {
		now := readHolderRecord(f)
		if now == last {
			return false
		}
		last = now
		trustStoreQueueMoved()
		return true
	}
	beats, stopBeats := trustStoreWaitBeats()
	defer stopBeats()
	var why error
wait:
	for {
		select {
		case err := <-granted:
			if err != nil {
				_ = f.Close()
				return nil, nil, fmt.Errorf("failed to lock %s: %w", path, err)
			}
			moved() // the steps the last holder took since the last beat
			if ctx.Err() == nil {
				logger.Debug("took the trust-store lock after waiting for another keploy agent", zap.Duration("waited", time.Since(start)))
				return held()
			}
			// The turn came, but after the caller had given up: give it back
			// below, like a turn that is still to come.
			granted <- nil
			why = context.Cause(ctx)
			break wait
		case <-ctx.Done():
			why = context.Cause(ctx)
			break wait
		case <-beats:
			if moved() {
				quiet = 0
			} else {
				quiet++
			}
			trustStoreWaitObserved(quiet)
			if quiet >= trustStoreStallBeats && len(granted) == 0 { // a turn that came with the beat is taken
				stalled := last
				stalledHolderRecord.Store(&stalled)
				why = fmt.Errorf("%w for %d beats", errTrustStoreQueueStalled, quiet)
				logger.Warn("gave up waiting for the trust-store lock: the process that holds it has made no progress",
					zap.String("held_by", holderOf(last)), zap.String("lock", path), zap.Int("beats", quiet),
					zap.String("next_step", "find the process holding it with: sudo fuser -v "+path+" ; and look at why it is stuck"))
				break wait
			}
		}
	}
	// The blocked call gets the lock when its turn comes. Give it straight
	// back, so an abandoned wait never holds up the agents queued behind it.
	go func() {
		<-granted
		_ = f.Close()
		if hook := abandonedWaitReturned.Load(); hook != nil {
			(*hook)()
		}
	}()
	return nil, nil, fmt.Errorf("stopped waiting for the trust-store lock %s: %w", path, why)
}

// rootHint says what to do about a lock keploy may not open: it is root's
// alone, so that no unprivileged process can hold up an agent.
func rootHint(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return " (the lock is root's: run keploy as root, with sudo -E to keep JAVA_HOME)"
	}
	return ""
}

// errTrustStoreQueueStalled is why a wait for the trust-store lock gave up:
// its holder made no progress (lockTrustStore).
var errTrustStoreQueueStalled = errors.New("the holder of the trust-store lock made no progress")

// stalledHolderRecord is the record of the holder the last wait of this
// process gave up on: a later wait behind the same unchanged record gives it
// one beat, rather than wait out the count again for each step of a start or
// a stop.
var stalledHolderRecord atomic.Pointer[string]

// trustStoreWaitBeats returns the beats a wait for the lock is judged by, the
// CLI's (utils.TeardownBeat), and the function that stops them. A variable so
// that tests can give the beats themselves.
var trustStoreWaitBeats = func() (<-chan time.Time, func()) {
	t := time.NewTicker(utils.TeardownBeat)
	return t.C, t.Stop
}

// trustStoreStallBeats is how many beats in a row without a step of the queue
// end a wait: the CLI's count (utils.TeardownStallChecks), doubled, so that
// where a CLI watches, its own judgement of the holder (and of this waiter,
// whose count started no later) comes first, and where none does, this wait
// does not give up just before a wedged holder watched by its own CLI is
// killed and the lock comes free. A variable for tests.
var trustStoreStallBeats = 2 * utils.TeardownStallChecks

// trustStoreWaitObserved is called, in tests, each time a wait has counted a
// beat: its count of beats without a step since. It lets a test know the wait
// has seen what the test made happen.
var trustStoreWaitObserved = func(quiet int) {}

// trustStoreQueueMoved is called while this process waits for the trust-store
// lock, each time it finds the holder's record changed: the holder took a step
// of its change (a keytool run's end: noteTrustStoreProgress; a beat in which
// a command it runs under the lock was scheduled: noteCommandProgress), or a
// new holder took the lock. The CLI that starts or stops an agent gives it up
// as stalled once it takes no step for its count of beats (utils.BootStep,
// utils.TeardownStep); an agent that waits its turn behind other agents'
// changes is not stalled while the queue moves, so their steps are steps of
// its wait. A variable so tests can see the calls.
var trustStoreQueueMoved = func() {
	utils.BootStep("another keploy agent's change of the trust store took a step")
	utils.TeardownStep()
}

// holderRecordSize is the size of the holder's record at the start of the lock
// file. It is always written whole, at offset 0, and the file never shrunk:
// on a full /run, a write that needs no new block still lands.
const holderRecordSize = 48

// holderSteps counts the steps this process has written into a holder record.
// A process holds the lock once at a time, so one count serves every hold.
var holderSteps atomic.Uint64

// writeHolderRecord writes this process's record into the lock file f: its pid
// and its count of steps, one more than the last. A record it cannot write
// (it is only ever read) changes nothing but what waiters see.
func writeHolderRecord(f *os.File) {
	rec := fmt.Sprintf("%d %d", os.Getpid(), holderSteps.Add(1))
	b := []byte(rec + strings.Repeat(" ", holderRecordSize-1-len(rec)) + "\n")
	_, _ = f.WriteAt(b, 0)
}

// readHolderRecord reads the holder's record from the lock file f ("" when
// there is none).
func readHolderRecord(f *os.File) string {
	b := make([]byte, holderRecordSize)
	n, _ := f.ReadAt(b, 0)
	return string(bytes.TrimSpace(b[:n]))
}

// holderOf names the holder a record is of, for a log line, and says when
// that process is gone, or is this one: a command it started under the lock
// (a trust-store rebuild, a keytool run) holds it then, until it exits.
func holderOf(record string) string {
	pid, _, _ := strings.Cut(record, " ")
	if pid == "" {
		return "unknown"
	}
	if pid == strconv.Itoa(os.Getpid()) {
		return "pid " + pid + ", this keploy process: a store command it left running past its ceiling holds it"
	}
	if _, err := os.Stat("/proc/" + pid); err != nil {
		return "pid " + pid + ", which is gone: a command it started under the lock holds it"
	}
	return "pid " + pid
}

// storeCommandBeats returns the beats at which the holder of the trust-store
// lock looks at a command it runs under it (noteCommandProgress), the CLI's
// (utils.TeardownBeat), and the function that stops them. A variable so that
// tests can give the beats themselves.
var storeCommandBeats = func() (<-chan time.Time, func()) {
	t := time.NewTicker(utils.TeardownBeat)
	return t.C, t.Stop
}

// storeCommandLooked is called, in tests, after each look noteCommandProgress
// takes at a command: it lets a test know the holder has looked.
var storeCommandLooked = func() {}

// noteCommandProgress marks, at each beat while cmd (started, under the lock)
// runs, a step of the change for the processes queued behind it, if cmd or a
// process it runs has made progress since the beat before (utils.
// ProcessProgress): scheduled, or runnable. A command wedged in a wait marks
// nothing, so the agents queued behind it are judged by the CLI as it would
// judge the holder. It returns the function that stops it. A command that does
// not hold the lock marks nothing.
func noteCommandProgress(cmd *exec.Cmd) (stop func()) {
	var lock *os.File
	if len(cmd.ExtraFiles) == 1 {
		lock = cmd.ExtraFiles[0]
	}
	if lock == nil || cmd.Process == nil {
		return func() {}
	}
	moved := utils.ProcessProgress(cmd.Process.Pid)
	beats, stopBeats := storeCommandBeats()
	looked := storeCommandLooked
	done, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		defer stopBeats()
		for {
			select {
			case <-done:
				return
			case <-beats:
				if moved() {
					writeHolderRecord(lock)
				}
				looked()
			}
		}
	}()
	return func() {
		close(done)
		<-ended
	}
}

// abandonedStoreCommand is called once this process has stopped waiting for
// cmd, a store command left running past its ceiling (runStoreCommandWithin),
// whose progress is noted no more. cmd still holds the lock, so a later wait
// of this process for it would wait on its own command, for the whole count,
// as for another agent's: the holder's record as it now stands is the one an
// earlier wait gave up on (stalledHolderRecord), so a later wait gives it one
// beat.
func abandonedStoreCommand(cmd *exec.Cmd) {
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] == nil {
		return
	}
	rec := readHolderRecord(cmd.ExtraFiles[0])
	stalledHolderRecord.Store(&rec)
}

// noteTrustStoreProgress marks a step of the change made under ctx, a context
// lockTrustStore returned, for the processes that wait for the lock: it
// rewrites the holder's record. A ctx that does not hold the lock marks
// nothing.
func noteTrustStoreProgress(ctx context.Context) {
	if f, ok := ctx.Value(storeLockKey{}).(*os.File); ok && f != nil {
		writeHolderRecord(f)
	}
}

// abandonedWaitReturned, when set, is called once a wait for the trust-store
// lock that its caller gave up on has been granted the lock and given it back.
// It is for tests, which cannot otherwise know when that has happened.
var abandonedWaitReturned atomic.Pointer[func()]

// tryFlock takes an exclusive flock on f without waiting and reports whether it
// got it. false with a nil error means another open file description — in this
// process or any other — holds the lock.
func tryFlock(f *os.File) (bool, error) {
	defer runtime.KeepAlive(f)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		case errors.Is(err, syscall.EINTR):
			continue
		default:
			return false, err
		}
	}
}

// waitFlock takes an exclusive flock on f, blocking until it is free.
func waitFlock(f *os.File) error {
	defer runtime.KeepAlive(f)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
