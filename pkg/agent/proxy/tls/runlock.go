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
// keploy-mitm-<run id>.crt anchors and its NSS nickname. Two CI jobs on one
// runner host otherwise each removed the other's live entries, and the other
// job's TLS calls then failed.
//
// Each run holds an exclusive lock on <caRunsDir>/<run id>.<pid>.lock from its
// CA install until its teardown; the kernel releases the lock when the process
// dies, SIGKILL included. A sweep removes another run's entries only when that
// run holds no lock (caRunLive). The pid is in the name as well as the run id
// because a persisted Windows CA keeps its run id across that user's runs: two
// of them at once must each hold a lock of their own, not wait on one file.
//
// The lock of a run's trust-file directory (ca_rundir.go) cannot tell a sweep
// this: it lies in the temp directory of the user who started the run and is
// named by no CA run id, while a sweep runs as root over the host's stores and
// knows a run only by the id its entry carries.
//
// Nothing removes another run's lock file: a run removes its own at teardown.
// The files of runs that died stay, and so does the file of a run that did
// not get its lock (on Linux under /run, emptied at boot; on Windows for
// good, with those a teardown could not remove while another process had them
// open). An empty file is all each costs, and removing them would race a run
// that has created its file but not yet locked it.

// writesSweptEntries reports whether the native install on goos writes
// trust-store entries, and so takes this run's lock before it writes them
// (setupNativeForApp). Only the macOS install writes none
// (setupNativeDarwin): it takes no run lock (its legacy sweep takes the store
// lock: caRunsDirOn).
func writesSweptEntries(goos string) bool { return goos != "darwin" }

// caRunsDir is the directory of the runs' lock files: one for every keploy run
// on this host, whatever its environment, so that a sweep sees each live run's
// lock (defaultCARunsDir). A variable so tests can use their own.
var caRunsDir = defaultCARunsDir()

var caRunLock struct {
	mu   sync.Mutex
	id   string
	path string
	f    *os.File
}

// caRunLockWait bounds how long a run waits for its own lock file, and
// caRunLockPoll how often it tries it meanwhile. The only holder a run expects
// is another run's sweep testing the file, for microseconds; but whoever can
// open the file can hold it (on Windows every user of the host reads these
// files), and a sweep stopped in its test (SIGSTOP, a paused container) holds
// it until it goes on. Past the bound the run goes on without its lock, as
// every run did before these locks. Variables so tests can shorten them.
var (
	caRunLockWait = 5 * time.Second
	caRunLockPoll = 10 * time.Millisecond
)

// caRunLockWarned keeps the warning that a run took no lock to one per process.
var caRunLockWarned atomic.Bool

// caRunLockTry is tryLockFile; a variable so tests can make a try fail.
var caRunLockTry = tryLockFile

// errCARunLockHeld is waitForLock's error when its file stayed held through
// caRunLockWait.
var errCARunLockHeld = errors.New("still held")

// warnNoCARunLock says, once per process, that this run took no lock, why, what
// that risks (the failure the lock prevents can then come back), and how to fix
// it (fix).
func warnNoCARunLock(logger *zap.Logger, why, fix string, fields ...zap.Field) {
	if caRunLockWarned.CompareAndSwap(false, true) {
		logger.Warn("keploy could not take this run's lock ("+why+"), so another keploy run on this host may remove this run's CA from the trust stores while it runs, and the app's TLS calls then fail. "+fix, fields...)
	}
}

// caRunIDFix says how to fix an id that cannot name a lock file. Only a CA
// keploy reuses from its CA key directory (KEPLOY_CA_KEY_DIR on Kubernetes, the
// user's on Windows) can carry one: the id is the end of its common name, and
// a CA keploy makes has a hex id.
func caRunIDFix() string {
	return fmt.Sprintf("To fix it, remove the CA keploy reuses (%s and %s in its CA key directory) so that keploy makes its own, or give that CA a common name of %q followed by letters, digits, '-' and '_' only.",
		caKeyFileName, caCertFileName, caCNPrefix)
}

// caRunLockHeldFix says how to fix a run's lock file that another process
// kept locked: that process (holder, as storeLockHolder names it) goes on and
// lets go, or ends.
func caRunLockHeldFix(path, holder string) string {
	return fmt.Sprintf("To fix it, let the process that holds the lock on %s go on, or end it: %s.", path, holder)
}

// caRunLockName is the name id's lock files start with: id in lower case (a JDK
// keystore lowers the case of the aliases it lists, and the id is read back
// from them), when it is a name a lock file can have. ok is false for an id
// with any other character: no run holds a lock under such a name. (The empty
// id of a CA whose name carries none names ".<pid>.lock": every such run is
// one run here, as its entries share one name too.)
func caRunLockName(id string) (name string, ok bool) {
	name = strings.ToLower(id)
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return "", false
		}
	}
	return name, true
}

// holdCARunLock takes this run's lock under id and keeps it until releaseCARunLock
// or the end of the process. The CA install takes it before it writes any
// trust-store entry, so no sweep sees an entry of this run without its lock.
// Taking a lock for another id releases the one held. When no lock can be
// taken (the id cannot name a lock file, the directory cannot be made or
// used, the file cannot be opened, or it stays held past caRunLockWait or cannot be
// locked), the run goes on unprotected, as every run did before these locks,
// and says so once, with how to fix it (warnNoCARunLock); when ctx ends first,
// the run is stopping, and it says so only at Debug.
func holdCARunLock(ctx context.Context, logger *zap.Logger, id string) {
	caRunLock.mu.Lock()
	defer caRunLock.mu.Unlock()
	if caRunLock.f != nil && caRunLock.id == id {
		return
	}
	releaseCARunLockLocked()
	name, ok := caRunLockName(id)
	if !ok {
		warnNoCARunLock(logger, "its id cannot name a lock file", caRunIDFix(), zap.String("run_id", id))
		return
	}
	path := filepath.Join(caRunsDir, fmt.Sprintf("%s.%d.lock", name, os.Getpid()))
	// As the store lock's (lockStores): outside Windows, a directory no other
	// user than this run's and root can change, and a file opened through no
	// link that this run's user or root owns.
	if err := makeCARunsDir(caRunsDir); err != nil {
		warnNoCARunLock(logger, "the directory of keploy runs' locks cannot be made or used", lockFix(path, "read and write"), zap.String("dir", caRunsDir), zap.Error(err))
		return
	}
	// 0600 (openLockFile): on Linux only root, whose runs sweep, opens it (a
	// sweep that cannot open a lock file counts its run as live). On Windows
	// the mode only decides the read-only attribute, which 0600 leaves unset;
	// the directory's ACL lets every user read the file.
	f, err := openLockFile(path, os.O_RDWR)
	if err != nil {
		warnNoCARunLock(logger, "its lock file cannot be opened", lockFix(path, "read and write"), zap.String("path", path), zap.Error(err))
		return
	}
	if err := waitForLock(ctx, f); err != nil {
		// Who holds it, read while the file is open (storeLockHolder reads
		// the kernel's table of locks by the open file's id).
		holder := ""
		if errors.Is(err, errCARunLockHeld) {
			holder = storeLockHolder(f)
		}
		// The file stays (on Windows its holder has it open).
		_ = f.Close()
		if ctx.Err() != nil {
			// The run is stopping before its install is done: its TLS no
			// longer matters, and its teardown removes whatever the install
			// still writes.
			logger.Debug("this run stopped before it had its lock", zap.String("path", path), zap.Error(err))
			return
		}
		if errors.Is(err, errCARunLockHeld) {
			warnNoCARunLock(logger, "its lock file stayed held by another process", caRunLockHeldFix(path, holder), zap.String("path", path), zap.Error(err))
			return
		}
		warnNoCARunLock(logger, "its lock file could not be locked", lockFix(path, "read and write"), zap.String("path", path), zap.Error(err))
		return
	}
	caRunLock.id, caRunLock.path, caRunLock.f = id, path, f
}

// waitForLock takes f's lock, trying every caRunLockPoll for at most
// caRunLockWait, or until ctx ends. A try that fails for another reason than
// the lock being held ends the wait at once, with that error.
func waitForLock(ctx context.Context, f *os.File) error {
	deadline := time.Now().Add(caRunLockWait)
	for {
		locked, err := caRunLockTry(f)
		if err != nil {
			return err
		}
		if locked {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w after %s", errCARunLockHeld, caRunLockWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(caRunLockPoll):
		}
	}
}

// releaseCARunLock gives up this run's lock and removes its file. The teardown
// calls it once it has removed the run's entries.
func releaseCARunLock() {
	caRunLock.mu.Lock()
	defer caRunLock.mu.Unlock()
	releaseCARunLockLocked()
}

func releaseCARunLockLocked() {
	if caRunLock.f == nil {
		return
	}
	// Closed before the removal: Windows does not remove an open file.
	_ = caRunLock.f.Close()
	_ = os.Remove(caRunLock.path)
	caRunLock.id, caRunLock.path, caRunLock.f = "", "", nil
}

// caRunLive reports whether a keploy run with this id is alive on this host: one
// of its lock files is locked. A run with no lock file is not: it is from
// before these locks, or its teardown is done. Nor is one whose every lock
// file this process can lock: the kernel released them when its processes
// died. A run that could take no lock (holdCARunLock) is not live either,
// though a file it left reads as live while whoever kept it from the run
// still holds it. A lock file this process cannot open, or cannot test,
// counts as live, so that a sweep leaves those entries rather than remove a
// live run's; so does one it does not trust (openRunLockToTest: outside
// Windows, a link, or a file not this run's user's alone). (One removed since the listing counts as live too: its run just
// ended, and its teardown removed its entries before its lock.)
func caRunLive(id string) bool {
	name, ok := caRunLockName(id)
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
		f, err := openRunLockToTest(filepath.Join(caRunsDir, e.Name()))
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
	return caRunLive(strings.TrimSuffix(strings.TrimPrefix(name, "keploy-mitm-"), ".crt"))
}
