package tls

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// A run's lock tells the other keploy runs on this host that the run is alive,
// so that their start-up sweeps leave its trust-store entries alone: its
// keploy-mitm-<run id>.crt anchor, its NSS nickname and its JDK alias. Two CI
// jobs on one runner host otherwise each removed the other's live entries, and
// the other job's TLS calls then failed.
//
// Each run holds an exclusive lock on <caRunsDir>/<run id>.<pid>.lock from its
// CA install until its teardown; the kernel releases the lock when the process
// dies, SIGKILL included. A sweep removes another run's entries only when that
// run holds no lock (runLive). The pid is in the name as well as the run id
// because a persisted Windows CA keeps its run id across that user's runs: two
// of them at once must each hold a lock of their own, not wait on one file.
//
// Nothing removes another run's lock file: a run removes its own at teardown.
// The files of runs that died stay, and so does the file of a run that did
// not get its lock (on Linux under /run, emptied at boot; on Windows for
// good, with those a teardown could not remove while another process had them
// open). An empty file is all each costs, and removing them would race a run
// that has created its file but not yet locked it.

// caRunsDir is the directory of the runs' lock files: one for every keploy run
// on this host, whatever its environment, so that a sweep sees each live run's
// lock (defaultCARunsDir). A variable so tests can use their own.
var caRunsDir = defaultCARunsDir()

var runLock struct {
	mu   sync.Mutex
	id   string
	path string
	f    *os.File
}

// runLockWait bounds how long a run waits for its own lock file, and
// runLockPoll how often it tries it meanwhile. The only holder a run expects
// is another run's sweep testing the file, for microseconds; but whoever can
// open the file can hold it (on Windows every user of the host reads these
// files), and a sweep stopped in its test (SIGSTOP, a paused container) holds
// it until it goes on. Past the bound the run goes on without its lock, as
// every run did before these locks. Variables so tests can shorten them.
var (
	runLockWait = 5 * time.Second
	runLockPoll = 10 * time.Millisecond
)

// runLockWarned keeps the warning that a run took no lock to one per process.
var runLockWarned atomic.Bool

// warnNoRunLock says, once per process, that this run took no lock, and why:
// the failure the lock prevents can then come back.
func warnNoRunLock(logger *zap.Logger, why string, fields ...zap.Field) {
	if runLockWarned.CompareAndSwap(false, true) {
		logger.Warn("keploy could not take this run's lock ("+why+"), so another keploy run on this host may remove this run's CA from the trust stores while it runs, and the app's TLS calls then fail", fields...)
	}
}

// runLockName is the name id's lock files start with: id in lower case (a JDK
// keystore lowers the case of the aliases it lists, and the id is read back
// from them), when it is a name a lock file can have. ok is false for an id
// with any other character: no run holds a lock under such a name. (The empty
// id of a CA whose name carries none names ".<pid>.lock": every such run is
// one run here, as its entries share one name too.)
func runLockName(id string) (name string, ok bool) {
	name = strings.ToLower(id)
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return "", false
		}
	}
	return name, true
}

// holdRunLock takes this run's lock under id and keeps it until releaseRunLock
// or the end of the process. The CA install takes it before it writes any
// trust-store entry, so no sweep sees an entry of this run without its lock.
// Taking a lock for another id releases the one held. When no lock can be
// taken (the id cannot name a lock file, the directory cannot be made, the
// file cannot be opened, or it stays held past runLockWait or cannot be
// locked), the run goes on unprotected, as every run did before these locks,
// and says so once (warnNoRunLock); when ctx ends first, the run is stopping,
// and it says so only at Debug.
func holdRunLock(ctx context.Context, logger *zap.Logger, id string) {
	runLock.mu.Lock()
	defer runLock.mu.Unlock()
	if runLock.f != nil && runLock.id == id {
		return
	}
	releaseRunLockLocked()
	name, ok := runLockName(id)
	if !ok {
		warnNoRunLock(logger, "its id cannot name a lock file", zap.String("run_id", id))
		return
	}
	if err := os.MkdirAll(caRunsDir, 0o755); err != nil {
		warnNoRunLock(logger, "the directory of keploy runs' locks cannot be made", zap.String("dir", caRunsDir), zap.Error(err))
		return
	}
	path := filepath.Join(caRunsDir, fmt.Sprintf("%s.%d.lock", name, os.Getpid()))
	// 0600: on Linux only root, whose runs sweep, opens it (a sweep that
	// cannot open a lock file counts its run as live). On Windows the mode
	// only decides the read-only attribute, which 0600 leaves unset; the
	// directory's ACL lets every user read the file.
	// #nosec G304 -- the path is caRunsDir and a name runLockName checked.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		warnNoRunLock(logger, "its lock file cannot be opened", zap.String("path", path), zap.Error(err))
		return
	}
	if err := waitForLock(ctx, f); err != nil {
		// The file stays (on Windows its holder has it open).
		_ = f.Close()
		if ctx.Err() != nil {
			// The run is stopping before its install is done: its TLS no
			// longer matters, and its teardown removes whatever the install
			// still writes.
			logger.Debug("this run stopped before it had its lock", zap.String("path", path), zap.Error(err))
			return
		}
		warnNoRunLock(logger, "its lock file stayed held by another process, or could not be locked", zap.String("path", path), zap.Error(err))
		return
	}
	runLock.id, runLock.path, runLock.f = id, path, f
}

// waitForLock takes f's lock, trying every runLockPoll for at most
// runLockWait, or until ctx ends. A try that fails for another reason than
// the lock being held ends the wait at once, with that error.
func waitForLock(ctx context.Context, f *os.File) error {
	deadline := time.Now().Add(runLockWait)
	for {
		locked, err := tryLockFile(f)
		if err != nil {
			return err
		}
		if locked {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("still held after %s", runLockWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(runLockPoll):
		}
	}
}

// releaseRunLock gives up this run's lock and removes its file. The teardown
// calls it once it has removed the run's entries.
func releaseRunLock() {
	runLock.mu.Lock()
	defer runLock.mu.Unlock()
	releaseRunLockLocked()
}

func releaseRunLockLocked() {
	if runLock.f == nil {
		return
	}
	// Closed before the removal: Windows does not remove an open file.
	_ = runLock.f.Close()
	_ = os.Remove(runLock.path)
	runLock.id, runLock.path, runLock.f = "", "", nil
}

// runLive reports whether a keploy run with this id is alive on this host: one
// of its lock files is locked. A run with no lock file is not: it is from
// before these locks, or its teardown is done. Nor is one whose every lock
// file this process can lock: the kernel released them when its processes
// died. A run that could take no lock (holdRunLock) is not live either,
// though a file it left reads as live while whoever kept it from the run
// still holds it. A lock file this process cannot open, or cannot test,
// counts as live, so that a sweep leaves those entries rather than remove a
// live run's. (One removed since the listing counts as live too: its run just
// ended, and its teardown removed its entries before its lock.)
func runLive(id string) bool {
	name, ok := runLockName(id)
	if !ok {
		return false
	}
	entries, err := os.ReadDir(caRunsDir)
	if err != nil {
		// No directory, no lock: no run on this host has made one. One this
		// process cannot read may hold a live run's lock.
		return !errors.Is(err, fs.ErrNotExist)
	}
	for _, e := range entries {
		// The store lock (storelock.go) is no run's, though "store" would
		// name a run.
		if e.Name() == storeLockName || !strings.HasPrefix(e.Name(), name+".") || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		// #nosec G304 -- a lock file in caRunsDir.
		f, err := os.Open(filepath.Join(caRunsDir, e.Name()))
		if err != nil {
			return true
		}
		if locked, err := tryLockFile(f); err != nil || !locked {
			_ = f.Close()
			return true
		}
		unlockFile(f)
		_ = f.Close()
	}
	return false
}

// liveRunEntry reports whether the trust-store entry named name (a
// keploy-mitm-<run id> alias or nickname, or a keploy-mitm-<run id>.crt anchor)
// belongs to a live run, which a sweep must leave.
func liveRunEntry(name string) bool {
	return runLive(strings.TrimSuffix(strings.TrimPrefix(name, "keploy-mitm-"), ".crt"))
}
