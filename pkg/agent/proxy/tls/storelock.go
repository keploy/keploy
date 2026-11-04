package tls

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

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

// storeLockTry is tryLockFile; a variable so tests can make a try fail.
var storeLockTry = tryLockFile

// storeLockWarned keeps the warning that a change ran without the store lock
// to one per process.
var storeLockWarned atomic.Bool

// withStoreLock runs change, a command that rewrites a trust store, holding
// the host's store lock. While another process holds the lock it waits, says
// so once at Info, and tries again until it has the lock or ctx ends; then
// change does not run (it runs under ctx, so it would only be killed) and
// ctx's error is returned. When the lock cannot be taken at all (its directory
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
// error is ctx's, when ctx ended while the lock was held by another.
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
	// change the stores, opens it.
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
		warnNoStoreLock(logger, path, err)
		return func() {}, nil
	}
	return func() {
		unlockFile(f)
		_ = f.Close()
	}, nil
}

// waitForStoreLock takes f's lock, trying again every storeLockPoll while
// another process holds it, until ctx ends. The first try that finds it held
// says so at Info: a run that holds it and does not go on (one stopped with
// Ctrl+Z) keeps this one waiting until it is stopped too. A try that fails for
// another reason ends the wait at once, with that error.
func waitForStoreLock(ctx context.Context, logger *zap.Logger, f *os.File, path string) error {
	said := false
	for {
		locked, err := storeLockTry(f)
		if err != nil {
			return err
		}
		if locked {
			return nil
		}
		if !said {
			said = true
			logger.Info("waiting for another keploy run on this host to finish changing a trust store", zap.String("lock", path))
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
