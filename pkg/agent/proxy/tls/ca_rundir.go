package tls

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// A native run's trust files for its app (SetupCaCertEnv) live in a directory
// of the run's own, <temp>/keploy-ca-<uid>-<random>, beside a lock file the
// run holds open while it lives (holdRunLock). The run removes the directory
// when its context ends. A run whose process dies first leaves it, and the
// next run's SetupCaCertEnv removes each such directory of this user whose
// lock no process holds (reapEndedRunDirs). Only the process holding a lock
// lets it go, at its exit however it ends, so a live run's directory is never
// taken and a dead run's goes at the next start.
const (
	runDirPrefix  = "keploy-ca-"
	runLockName   = "run.lock"
	runCAFile     = "keploy-ca.crt"
	runBundleFile = "keploy-ca-bundle.crt"
)

// runDirPattern is the name of this user's run directories up to the random
// part. The uid keeps one user's run from judging another's, and a root-owned
// directory from a `sudo keploy` run out of the user's way.
func runDirPattern() string { return fmt.Sprintf("%s%d-", runDirPrefix, os.Geteuid()) }

// runTrustDir is one run's directory and the lock that says the run lives.
type runTrustDir struct {
	dir  string
	lock *os.File
	once sync.Once
}

// liveRunDirs holds every run directory this process has made and not yet
// removed. Its lock file must stay open as long as the run lives, and an
// *os.File nothing refers to is closed by the garbage collector: a run whose
// context can never end (context.Background) would otherwise let its lock go
// at the next collection, and the next run would take its files.
var liveRunDirs sync.Map // *runTrustDir -> struct{}

// newRunTrustDir makes this run's directory and takes its lock.
func newRunTrustDir() (*runTrustDir, error) {
	dir, err := os.MkdirTemp(os.TempDir(), runDirPattern())
	if err != nil {
		return nil, err
	}
	// 0755: the app may run as another user than keploy (under sudo), and
	// the directory holds public certificates only.
	if err := os.Chmod(dir, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	lock, err := holdRunLock(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	r := &runTrustDir{dir: dir, lock: lock}
	liveRunDirs.Store(r, struct{}{})
	return r, nil
}

// write writes data to name in the run's directory, mode 0644 (public
// certificates only), and returns its path.
func (r *runTrustDir) write(name string, data []byte) (string, error) {
	p := filepath.Join(r.dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return "", err
	}
	// keploy runs with umask 0, a test with the user's: the mode is set, not
	// left to the umask.
	return p, os.Chmod(p, 0o644)
}

// removeWhenDone removes the run's directory when ctx ends.
func (r *runTrustDir) removeWhenDone(ctx context.Context, logger *zap.Logger) {
	context.AfterFunc(ctx, func() {
		if err := r.remove(); err != nil {
			logger.Debug("could not remove this run's CA files", zap.String("dir", r.dir), zap.Error(err))
		}
	})
}

// remove lets the lock go and removes the run's directory, once.
func (r *runTrustDir) remove() error {
	var err error
	r.once.Do(func() {
		// First: on Windows a file open here cannot be removed.
		_ = r.lock.Close()
		err = os.RemoveAll(r.dir)
		liveRunDirs.Delete(r)
	})
	return err
}

// ownedByThisUserFn is ownedByThisUser; a test stubs it, as it cannot make a
// directory another user owns.
var ownedByThisUserFn = ownedByThisUser

// reapEndedRunDirs removes this user's run directories in the temp directory
// whose run has ended: their lock file is there and no process holds it. A
// directory without a lock file is a run between making it and taking its
// lock, and is left alone.
func reapEndedRunDirs(logger *zap.Logger) {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	prefix := runDirPattern()
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		dir := filepath.Join(tmp, e.Name())
		if !ownedByThisUserFn(dir) {
			continue
		}
		release, ended := runEnded(dir)
		if !ended {
			continue
		}
		release()
		if err := os.RemoveAll(dir); err != nil {
			logger.Debug("could not remove an ended run's CA files", zap.String("dir", dir), zap.Error(err))
			continue
		}
		logger.Debug("removed an ended run's CA files", zap.String("dir", dir))
	}
}
