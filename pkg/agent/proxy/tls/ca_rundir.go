package tls

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
//
// An ended run's directory is renamed to <name>.ended-<random> before anything
// in it is deleted (retireRunDir), so a removal cut short leaves nothing under
// the run's name without its lock, and the next run removes what is under the
// ended name whole. A run killed between making its directory and taking its
// lock leaves that directory, with no certificate in it: nothing tells it from
// a run starting, so it stays.
//
// A run that pointed its test at copies of the user's kubeconfig keeps beside
// each copy what writing the test's changes back takes (kubeconfigRecord),
// until its end has written them back. A run that ended without its cleanup
// (killed, crashed) leaves them, and the next run writes them back
// (writeBackEndedRun), holding the ended run's lock, before it removes the
// directory: the test's changes are never lost with it.
const (
	runDirPrefix  = "keploy-ca-"
	runLockName   = "run.lock"
	runCAFile     = "keploy-ca.crt"
	runBundleFile = "keploy-ca-bundle.crt"
	runEndedMark  = ".ended-"
)

// runDirPattern is the name of this user's run directories up to the random
// part. The uid keeps one user's run from judging another's, and a root-owned
// directory from a `sudo keploy` run out of the user's way.
func runDirPattern() string { return fmt.Sprintf("%s%d-", runDirPrefix, os.Geteuid()) }

// runTrustDir is one run's directory and the lock that says the run lives.
type runTrustDir struct {
	dir     string
	lock    *os.File
	started time.Time // when the run made it, for the run's records
	once    sync.Once

	mu    sync.Mutex
	atEnd []func() // run by remove, before the directory goes (atRemove)
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
	// 0755: the app may run as another user than keploy (under sudo). The
	// directory holds public certificates, and copies of the user's files
	// that only their owner reads (writePrivate).
	if err := os.Chmod(dir, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	lock, err := holdRunLock(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	r := &runTrustDir{dir: dir, lock: lock, started: time.Now()}
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

// writePrivate writes data to name in the run's directory, readable by its
// owner alone (mode 0600): a copy of a file that may hold secrets, such as a
// kubeconfig's client keys. It returns its path.
func (r *runTrustDir) writePrivate(name string, data []byte) (string, error) {
	p := filepath.Join(r.dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", err
	}
	return p, os.Chmod(p, 0o600)
}

// atRemove has f run when the run ends, before its directory is removed: f
// may read what the run's files hold then.
func (r *runTrustDir) atRemove(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.atEnd = append(r.atEnd, f)
}

// removeWhenDone removes the run's directory when ctx ends.
func (r *runTrustDir) removeWhenDone(ctx context.Context, logger *zap.Logger) {
	context.AfterFunc(ctx, func() {
		if err := r.remove(); err != nil {
			logger.Debug("could not remove this run's CA files", zap.String("dir", r.dir), zap.Error(err))
		}
	})
}

// remove runs what the run left for its end (atRemove), then lets the lock
// go and removes the run's directory, once.
func (r *runTrustDir) remove() error {
	var err error
	r.once.Do(func() {
		r.mu.Lock()
		atEnd := r.atEnd
		r.atEnd = nil
		r.mu.Unlock()
		for _, f := range atEnd {
			f()
		}
		// Then: Windows renames no directory while a file in it is open.
		_ = r.lock.Close()
		err = retireRunDir(r.dir)
		// Last: a test waits on this to know the whole removal is done.
		liveRunDirs.Delete(r)
	})
	return err
}

// removeAllFn is os.RemoveAll; a test stubs it to cut a removal short.
var removeAllFn = os.RemoveAll

// retireRunDir removes the directory of a run that has ended and whose lock
// no process holds. It renames the directory to an ended name first and only
// then deletes it. A removal cut short before the rename leaves the directory
// whole, its lock file free, and the next run takes it by its lock; one cut
// short after it leaves a directory under an ended name, which only an ended
// run's directory is given, and the next run takes it whole. So the run's own
// name never holds its files without its lock, and a directory under that name
// without a lock is only ever a run between making it and taking its lock.
// Deleting in place could not give that: in every order the last file goes
// before the directory, and an empty directory is what a run starting looks
// like. The ended name's random part keeps another user of a shared temp
// directory from making it first, which would fail the rename for good.
func retireRunDir(dir string) error {
	ended := dir + runEndedMark + rand.Text()
	if err := os.Rename(dir, ended); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // another run's reaper retired it first
		}
		return err
	}
	return removeAllFn(ended)
}

// ownedByThisUserFn is ownedByThisUser; a test stubs it, as it cannot make a
// directory another user owns.
var ownedByThisUserFn = ownedByThisUser

// reapEndedRunDirs removes this user's run directories in the temp directory
// whose run has ended: each under an ended name (a removal cut short), and
// each whose lock file is there and no process holds. A directory under its
// run's name without a lock file is a run between making it and taking its
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
		if strings.Contains(e.Name(), runEndedMark) {
			err = removeAllFn(dir)
		} else {
			release, ended := runEnded(dir)
			if !ended {
				continue
			}
			// Holding its lock, so no other run starting now writes it too.
			kept := !writeBackEndedRun(logger, dir)
			// Then: Windows renames no directory while a file in it is open.
			release()
			if kept {
				continue
			}
			err = retireRunDir(dir)
		}
		if err != nil {
			logger.Debug("could not remove an ended run's CA files", zap.String("dir", dir), zap.Error(err))
			continue
		}
		logger.Debug("removed an ended run's CA files", zap.String("dir", dir))
	}
}
