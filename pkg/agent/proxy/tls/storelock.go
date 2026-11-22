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
// file (caRunLive leaves it out).
const storeLockName = "store.lock"

// storeLockPoll is how often a change waiting for the store lock tries it. It
// only spaces the tries: the change runs once the lock is free.
var storeLockPoll = 10 * time.Millisecond

// storeLockWait bounds how long a change waits for the store lock (in one CA
// install, a wait that ends at it makes the changes after it try the lock
// once, until one gets it: withStoreLockBudget), and storeLockSay is how
// often it says, while it waits, that it does. A run holds the lock for
// one command, a keytool or a store's update, which takes seconds. But
// whoever can open the file can hold it (on Windows any user of the host:
// lockStores), and a run stopped while it holds it (Ctrl+Z, SIGSTOP) holds it
// until it goes on. Past the bound the change does not run and fails with
// ErrStoreLockHeld: run without the lock, it could undo another run's change,
// or have its own undone. Variables so tests can shorten them.
var (
	storeLockWait = 2 * time.Minute
	storeLockSay  = 5 * time.Second
)

// ErrStoreLockHeld is the error of a change that did not run because the
// store lock stayed held past its wait's bound (storeLockBound). The agent's
// log of a CA install that failed with it says what to do instead of what a
// failed write needs (StoreLockHeldNextStep).
var ErrStoreLockHeld = errors.New("the trust stores' lock stayed held")

// StoreLockHeldNextStep says what to do about a CA install that failed with
// ErrStoreLockHeld, whose error names the lock's holder (storeLockHolder):
// resume it if it is stopped, or end it, then restart the agent. Access is
// not what failed: keploy did not change a store another process had
// locked. A holder that keeps the lock past the bound is most likely stopped
// mid-change (storeLockWait), so waiting for it is no remedy. Only the native
// Linux install fails on this error (its store update): elsewhere the lock
// guards JDK keystore changes at most, whose failure is a warning. The lock
// file is root's (0600): its holder opened it as root, or was given a handle
// by a process that did, so the commands run as root.
func StoreLockHeldNextStep() string {
	path := filepath.Join(caRunsDir, storeLockName)
	return "Another process holds keploy's trust-store lock " + path + " and has not let go; the error names it and its state." +
		" If it is stopped (its state is T), resume it as root (`kill -CONT PID`); otherwise end it (`kill PID`)." +
		" Where the error names no process, it says how to find it on the host, or why it cannot. Then restart the agent."
}

// storeLockHolderUnknown is who holds the store lock, for a holder the agent
// cannot name (storeLockHolder).
const storeLockHolderUnknown = "another keploy run on this host, or another process that opened it"

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

// storeLockBudget is one CA install's: whether one of its waits for the store
// lock ended at its bound, the lock held, and no try since has got the lock.
type storeLockBudget struct{ spent atomic.Bool }

type storeLockBudgetKey struct{}

// withStoreLockBudget gives the changes made under ctx one storeLockWait to
// wait out for the store lock, or keeps the one ctx has. A CA install makes
// several changes (JDK keystore deletes and an import, the store's update),
// and without it a holder that never lets go would make each of them wait
// out the whole bound in turn: minutes of a start that cannot succeed. With
// it, a wait that ends at the bound spends it: each change after it tries
// the lock once, and fails at once if it is still held. A wait that gets the
// lock spends none of it, and a try that gets it gives it back: waits for
// runs that take turns, each for seconds, add up to no bound.
func withStoreLockBudget(ctx context.Context) context.Context {
	if _, ok := ctx.Value(storeLockBudgetKey{}).(*storeLockBudget); ok {
		return ctx
	}
	return context.WithValue(ctx, storeLockBudgetKey{}, &storeLockBudget{})
}

// storeLockBound is how long a change under ctx may wait for the store lock:
// storeLockWait, or none when ctx's install has spent its budget
// (withStoreLockBudget); and the budget, nil without one.
func storeLockBound(ctx context.Context) (time.Duration, *storeLockBudget) {
	b, ok := ctx.Value(storeLockBudgetKey{}).(*storeLockBudget)
	if !ok {
		return storeLockWait, nil
	}
	if b.spent.Load() {
		return 0, b
	}
	return storeLockWait, b
}

// withStoreLock runs change, a command that rewrites a trust store, holding
// the host's store lock. While another process holds the lock it waits, says
// so at Info every storeLockSay, and tries again until it has the lock, ctx
// ends, or its bound has passed: storeLockWait, or none while ctx's CA
// install has its budget spent, until a change gets the lock (storeLockBound). When ctx ends first, change does not run
// (it runs under ctx, so it would only be killed) and ctx's error is returned;
// past the bound, change does not run and the error is ErrStoreLockHeld,
// naming the lock's file. When the lock cannot be taken at all (its directory
// cannot be made, or is one another user can change; its file cannot be
// opened, or is a link or another user's; or a try fails for another reason
// than the lock being held), change runs without it, as every change
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
// ErrStoreLockHeld, when it stayed held past its bound (storeLockBound).
func lockStores(ctx context.Context, logger *zap.Logger) (unlock func(), err error) {
	path := filepath.Join(caRunsDir, storeLockName)
	// Outside Windows, a directory no other user than this run's and root
	// can change (makeCARunsDir), and a file opened through no link that
	// this run's user or root owns (openLockFile): no other user makes the
	// run create a file elsewhere, or holds its lock.
	if err := makeCARunsDir(caRunsDir); err != nil {
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
	f, err := openLockFile(path, os.O_RDONLY)
	if err != nil {
		warnNoStoreLock(logger, path, err)
		return func() {}, nil
	}
	if err := waitForStoreLock(ctx, logger, f, path); err != nil {
		_ = f.Close()
		if ctx.Err() != nil {
			return func() {}, ctx.Err()
		}
		if errors.Is(err, ErrStoreLockHeld) {
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
// another process holds it, for at most its bound (storeLockBound), or until
// ctx ends; it tries once at least. While it waits it says so at Info, at the
// first try that finds the lock held and then every storeLockSay, each time a
// step of the agent's start too (storeLockStep). Past the bound it ends with
// ErrStoreLockHeld, naming path and, on Linux, the process that holds it
// (storeLockHolder); Windows does not tell who holds a LockFileEx lock. A try
// that fails for another reason than the lock being held ends the wait at
// once, with that error.
func waitForStoreLock(ctx context.Context, logger *zap.Logger, f *os.File, path string) error {
	start := time.Now()
	bound, budget := storeLockBound(ctx)
	install := ""
	if budget != nil {
		install = " in one CA install"
	}
	// When it last said it waits: before the first line, the zero time, which
	// time.Since puts past any storeLockSay.
	var said time.Time
	for {
		locked, err := storeLockTry(f)
		if err != nil {
			return err
		}
		if locked {
			if budget != nil {
				budget.spent.Store(false)
			}
			return nil
		}
		waited := time.Since(start)
		if waited >= bound {
			if budget != nil {
				budget.spent.Store(true)
			}
			return fmt.Errorf("%w: %s was held, through the %s keploy waits for it%s, by %s, and keploy changes a trust store only while it holds that lock (two changes of one store at once undo each other's)", ErrStoreLockHeld, path, storeLockWait, install, storeLockHolder(f))
		}
		if time.Since(said) >= storeLockSay {
			said = time.Now()
			logger.Info("waiting for another keploy run on this host to finish changing a trust store",
				zap.String("lock", path), zap.Duration("waited", waited.Round(time.Second)), zap.Duration("bound", bound))
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
// without the store lock, why (err), what that risks (keploy runs on this host
// that change one store at once race, and one may undo another's change), and
// how to give the runs the lock (lockFix).
func warnNoStoreLock(logger *zap.Logger, path string, err error) {
	if storeLockWarned.CompareAndSwap(false, true) {
		logger.Warn("keploy could not take the trust stores' lock, so it changes trust stores without it: keploy runs on this host that change the same trust store at the same time race, and one may undo another's change. "+lockFix(path, "read"),
			zap.String("lock", path), zap.Error(err))
	}
}

// lockFix says what the lock file at path needs so that a run can take its
// lock, as the store lock (lockStores) and a run's own lock (holdCARunLock)
// are taken: its directory, which a run makes when it is missing (write access
// to the directory above it), and its file, which a run creates in that
// directory (write access to it) or opens with access ("read" for the store
// lock, "read and write" for a run's own) when it is there, and then locks.
// Outside Windows the directory and the file are also that user's or root's,
// no link, and the directory is one only its owner writes (makeCARunsDir,
// openLockFile).
func lockFix(path, access string) string {
	dir := filepath.Dir(path)
	return fmt.Sprintf("To fix it, let the user keploy runs as make the directory %s (write access to %s), or make it for that user; let it create the file %s (write access to %s), or create that file with %s access for that user; and keep %s on a file system that supports file locks. Outside Windows, keploy uses that directory and that file only when each is that user's or root's and no link, and only its owner can write the directory.",
		dir, filepath.Dir(dir), path, dir, access, dir)
}
