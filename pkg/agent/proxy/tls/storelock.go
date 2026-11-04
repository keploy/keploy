package tls

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Each trust store keploy changes is one file a command rewrites whole: a JDK
// keystore (keytool -import, -delete) and the system bundle
// (update-ca-certificates, update-ca-trust, which rebuild it from the anchors'
// directory). Each such command reads the store, changes it, and writes it all
// back. Two keploy runs on one host that changed one store at once each wrote
// back what it had read, so the one that wrote last dropped the other's
// change: that run's CA was missing from the store, or an entry removed came
// back. Every such command now runs holding the host's store lock,
// <caRunsDir>/store.lock (withStoreLock), so the runs take turns.

// storeLockName is the store lock's file in caRunsDir. It is no run's lock
// file (runLive leaves it out).
const storeLockName = "store.lock"

// storeLockPoll is how often a change waiting for the store lock tries it. It
// only spaces the tries: the change runs once the lock is free.
var storeLockPoll = 10 * time.Millisecond

// storeLockWait bounds how long a change waits for the store lock, and
// storeLockSay is how often it says, while it waits, that it does. A run holds
// the lock for one command, a keytool or a store's update, which takes
// seconds. But whoever can open the file can hold it (on Windows any user of
// the host: lockStores), and a run stopped while it holds it (Ctrl+Z, SIGSTOP,
// a paused container) holds it until it goes on. Past the bound the change
// does not run and fails with errStoreLockHeld: run without the lock, it could
// undo another run's change, or have its own undone. Variables so tests can
// shorten them.
var (
	storeLockWait = 2 * time.Minute
	storeLockSay  = 5 * time.Second
)

// errStoreLockHeld is the error of a change that did not run because the
// store lock stayed held past storeLockWait.
var errStoreLockHeld = errors.New("the trust stores' lock stayed held")

// storeLockTry is tryLockFile; a variable so tests can make a try fail.
var storeLockTry = tryLockFile

// storeLockStep is utils.BootStep, which each of the wait's "waiting" lines
// takes as well: an agent's start that waits for the lock, which ends by
// storeLockWait, has not stopped (the CLI that waits for the start judges by
// its steps). A variable so tests can see the steps.
var storeLockStep = utils.BootStep

// storeLockWarned keeps the warning that a change ran without the store lock
// to one per process.
var storeLockWarned atomic.Bool

// withStoreLock runs change, a command that rewrites a trust store, holding
// the host's store lock. While another process holds the lock it waits, says
// so at Info every storeLockSay, and tries again until it has the lock, ctx
// ends, or storeLockWait has passed. When ctx ends first, change does not run
// (it runs under ctx, so it would only be killed) and ctx's error is returned;
// past the bound, change does not run and the error is errStoreLockHeld,
// naming the lock's file. When the lock cannot be taken at all (its directory
// cannot be made, its file cannot be opened, or a try fails for another
// reason than the lock being held), change runs without it, as every change
// did before this lock, and the run says so once.
func withStoreLock(ctx context.Context, logger *zap.Logger, change func() error) error {
	unlock, err := lockStores(ctx, logger)
	if err != nil {
		return err
	}
	defer unlock()
	return change()
}

// lockStores takes the host's store lock for withStoreLock and returns how to
// give it up: one that does nothing when the change goes on without it. Its
// error is ctx's, when ctx ended while the lock was held by another, or
// errStoreLockHeld, when it stayed held past storeLockWait.
func lockStores(ctx context.Context, logger *zap.Logger) (unlock func(), err error) {
	path := filepath.Join(caRunsDir, storeLockName)
	if err := os.MkdirAll(caRunsDir, 0o755); err != nil {
		warnNoStoreLock(logger, path, err)
		return func() {}, nil
	}
	// Read-only: every run on this host opens this one file, also a run of
	// another user than the one who made it (on Windows, whose
	// %ProgramData% lets every user read the files in it); a read handle
	// takes both flock and LockFileEx. 0600: on Linux only root, whose runs
	// change the stores, opens it. On Windows a run that is not elevated
	// changes a JDK keystore too (one the user can write, as a JDK in the
	// user's profile), so the file stays one every user can open, and so
	// lock: one who holds it keeps the runs' changes waiting up to
	// storeLockWait, and then they fail, naming the file.
	// #nosec G304 -- the path is caRunsDir and a constant name.
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		warnNoStoreLock(logger, path, err)
		return func() {}, nil
	}
	if err := waitForStoreLock(ctx, logger, f, path); err != nil {
		_ = f.Close()
		if ctx.Err() != nil {
			return func() {}, ctx.Err()
		}
		if errors.Is(err, errStoreLockHeld) {
			return func() {}, err
		}
		warnNoStoreLock(logger, path, err)
		return func() {}, nil
	}
	return func() {
		unlockFile(f)
		_ = f.Close()
	}, nil
}

// waitForStoreLock takes f's lock, trying again every storeLockPoll while
// another process holds it, for at most storeLockWait, or until ctx ends.
// While it waits it says so at Info, at the first try that finds the lock held
// and then every storeLockSay, each time a step of the agent's start too
// (storeLockStep). Past the bound it ends with errStoreLockHeld, naming path,
// but no holder: Windows does not tell who holds a LockFileEx lock. A try
// that fails for another reason than the lock being held ends the wait at
// once, with that error.
func waitForStoreLock(ctx context.Context, logger *zap.Logger, f *os.File, path string) error {
	start := time.Now()
	// When it last said it waits: before the first line, the zero time, which
	// time.Since puts past any storeLockSay.
	var said time.Time
	for {
		locked, err := storeLockTry(f)
		if err != nil {
			return err
		}
		if locked {
			return nil
		}
		waited := time.Since(start)
		if waited >= storeLockWait {
			return fmt.Errorf("%w: %s was held for %s by another keploy run on this host, or by another process that opened it, and keploy changes a trust store only while it holds that lock (two changes of one store at once undo each other's)", errStoreLockHeld, path, storeLockWait)
		}
		if time.Since(said) >= storeLockSay {
			said = time.Now()
			logger.Info("waiting for another keploy run on this host to finish changing a trust store",
				zap.String("lock", path), zap.Duration("waited", waited.Round(time.Second)), zap.Duration("bound", storeLockWait))
			storeLockStep("waiting for the trust stores' lock " + path)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(storeLockPoll):
		}
	}
}

// warnNoStoreLock says, once per process, that a change of a trust store ran
// without the store lock, and why: another keploy run's change of the same
// store at the same time may then undo it.
func warnNoStoreLock(logger *zap.Logger, path string, err error) {
	if storeLockWarned.CompareAndSwap(false, true) {
		logger.Warn("keploy could not take the trust stores' lock, so another keploy run on this host that changes the same trust store at the same time may undo this run's change",
			zap.String("lock", path), zap.Error(err))
	}
}
