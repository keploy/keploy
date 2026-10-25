package tls

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// Run files are the trust files a native run writes to the temp dir for its
// app: the CA file the trust variables name, the Java truststore and the PEM
// it is built from. A run removes its own when its command returns
// (RemoveCaCertEnvFiles), but a run killed before that (SIGKILL, an os.Exit,
// a crash, a CI job's timeout) cannot, and on a long-lived box those would
// pile up one set per killed run.
//
// So each run holds an advisory lock on every run file it creates, from just
// after it creates the file until it removes it, and a later setup that uses
// the same temp dir removes the run files whose lock it can take (and that it
// may remove: root's, or its own user's). The system drops a process's locks
// when it ends, however it ends, so a run file nobody holds is one whose
// keploy is gone, and a live keploy's files are never touched. An app that
// outlived its killed keploy may still read one, but that app's keploy, its
// proxy with it, is gone. There is no age rule: a run may take as long as it
// likes.
//
// The names carry "-run-" to tell them from the files keploy versions before
// the lock wrote ("keploy-ca-123.crt", "keploy-java-truststore-1000.jks"):
// those hold no lock, so an unlocked one may be a live run's of an older
// keploy on the same machine, and is never swept.
//
// The lock is only as good as the temp dir's filesystem: a TMPDIR on NFS
// shared by several hosts with host-local locks (nolock, local_lock) would let
// one host sweep another's live files. Where a run cannot lock at all (a
// filesystem that refuses locks, a platform keploy has no lock for), it writes
// its files under the older names, unlocked and never swept, and says so.
const (
	runCAFilePattern     = "keploy-ca-run-*.crt"
	runTrustStorePattern = "keploy-java-truststore-run-*.jks"
	runMergedPEMPattern  = "keploy-java-cas-run-*.pem"
)

var runFilePatterns = []string{runCAFilePattern, runTrustStorePattern, runMergedPEMPattern}

// unlockedPattern is the name a run gives a file it cannot lock: the older
// one, which no sweep takes.
func unlockedPattern(pattern string) string {
	return strings.Replace(pattern, "-run-", "-", 1)
}

// maxRunFileAttempts bounds createRunFile's retries. A sweep takes a new file
// only in the instant between its creation and its lock, and only a file it
// listed before, so a second attempt is already rare.
const maxRunFileAttempts = 8

var (
	// errRunFileTaken: the file was swept, or is being swept, before the run
	// that made it took its lock.
	errRunFileTaken = errors.New("the run file was taken by another run's sweep before its lock")
	// errNotRegular: something other than a regular file is at a run file's
	// name.
	errNotRegular = errors.New("not a regular file")
)

// runFile is a temp file of this run's, under the run's lock while it exists.
type runFile struct {
	path string
	// lock is the handle holding the run's lock; nil for a file the run
	// could not lock, which has an unlocked name.
	lock *os.File
}

// Seams for the tests of a new run file's window before its lock and of a
// temp dir that refuses locks.
var (
	beforeRunFileOpen = func(string) {}
	beforeRunFileLock = func(string) {}
	tryLockRunFileFn  = tryLockRunFile
)

// warnUnlockedOnce says once per process that a run's files go unlocked.
var warnUnlockedOnce sync.Once

// createRunFile creates a new file in the temp dir, named after pattern as
// os.CreateTemp names it, and takes the run's lock on it. It returns the file
// open for writing, which the caller closes, and the run file, which the
// caller removes with remove. Where it cannot lock, it creates the file under
// unlockedPattern instead: the run still works, only a kill leaves the file.
func createRunFile(logger *zap.Logger, pattern string) (*os.File, *runFile, error) {
	if !runFileLocking {
		return createUnlockedRunFile(logger, pattern, errors.ErrUnsupported)
	}
	for attempt := 0; attempt < maxRunFileAttempts; attempt++ {
		f, err := os.CreateTemp("", pattern)
		if err != nil {
			return nil, nil, err
		}
		rf := &runFile{path: f.Name()}
		lock, err := lockNewRunFile(rf.path)
		if err == nil {
			rf.lock = lock
			return f, rf, nil
		}
		// Removed only while the name is still this file (a sweep that took
		// it has freed the name), and once closed: Windows refuses to remove
		// a file this handle has open.
		mine := stillAt(f, rf.path)
		_ = f.Close()
		if mine {
			_ = os.Remove(rf.path)
		}
		if !errors.Is(err, errRunFileTaken) {
			return createUnlockedRunFile(logger, pattern, err)
		}
	}
	return nil, nil, fmt.Errorf("creating a %s run file: %w, %d times", pattern, errRunFileTaken, maxRunFileAttempts)
}

// createUnlockedRunFile creates a run file the run could not lock (why), under
// the name no sweep takes.
func createUnlockedRunFile(logger *zap.Logger, pattern string, why error) (*os.File, *runFile, error) {
	warnUnlockedOnce.Do(func() {
		logger.Warn("keploy cannot lock its trust files in the temp dir, so a run killed before its cleanup leaves them there and no later run removes them",
			zap.String("temp_dir", os.TempDir()), zap.Error(why),
			zap.String("next_step", "point TMPDIR at a local filesystem; keploy-ca-*.crt and keploy-java-* files in the temp dir while no keploy runs can be removed by hand"))
	})
	f, err := os.CreateTemp("", unlockedPattern(pattern))
	if err != nil {
		return nil, nil, err
	}
	return f, &runFile{path: f.Name()}, nil
}

// lockNewRunFile takes the run's lock on the file this run just created at
// path, and checks that path is still that file: another run's sweep that
// listed it before the lock may have removed it. errRunFileTaken when it did,
// or when it holds the lock now and is about to.
func lockNewRunFile(path string) (*os.File, error) {
	beforeRunFileOpen(path)
	lock, err := openRunFileLock(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errNotRegular) || isSymlinkRefused(err) {
			// Gone, or the name freed by a sweep and taken by something else.
			return nil, errRunFileTaken
		}
		return nil, err
	}
	beforeRunFileLock(path)
	held, err := tryLockRunFileFn(lock)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if !held || !stillAt(lock, path) {
		_ = lock.Close()
		return nil, errRunFileTaken
	}
	return lock, nil
}

// stillAt reports whether path names the file f has open.
func stillAt(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	now, err := os.Lstat(path)
	return err == nil && os.SameFile(held, now)
}

// remove removes the run file and then drops its lock. Safe on a nil run file
// and on one removed already.
func (rf *runFile) remove() error {
	if rf == nil {
		return nil
	}
	err := removeAndUnlock(rf.path, rf.lock)
	rf.lock = nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// removeAndUnlock removes the file at path, then drops the lock: in that
// order, so no other run's sweep sees the file unlocked while it is there.
// The lock handle lets the file be removed while open, on Windows too. The
// lock goes even when the remove fails: the next sweep then takes the file.
func removeAndUnlock(path string, lock *os.File) error {
	err := os.Remove(path)
	if lock != nil {
		_ = lock.Close()
	}
	return err
}

// isRunFileName reports whether name is one os.CreateTemp gives a run-file
// pattern: its prefix, a decimal number, its suffix, and nothing else.
func isRunFileName(name string) bool {
	for _, pattern := range runFilePatterns {
		prefix, suffix, _ := strings.Cut(pattern, "*")
		mid, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		if mid, ok = strings.CutSuffix(mid, suffix); !ok || mid == "" {
			continue
		}
		if strings.Trim(mid, "0123456789") == "" {
			return true
		}
	}
	return false
}

// sweepDeadRunFiles removes from the temp dir the run files of runs that have
// ended without removing them: those whose lock it can take. It leaves
// everything else alone: a live run's run files, the files of older keploy
// versions, any other file, and run files it may not remove (another user's,
// for a run that is not root). Best-effort: a setup never fails over it.
//
// Where keploy has no file lock (runFileLocking false) it sweeps nothing,
// since it could not tell a live run's file from a dead one's; createRunFile
// says so.
func sweepDeadRunFiles(logger *zap.Logger) {
	if !runFileLocking {
		return
	}
	dir := os.TempDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Debug("could not list the temp dir for the trust files of ended runs", zap.String("dir", dir), zap.Error(err))
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !isRunFileName(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		removed, err := removeIfDead(p)
		switch {
		case err != nil:
			logger.Debug("could not remove the trust file of an ended run", zap.String("path", p), zap.Error(err))
		case removed:
			logger.Debug("removed the trust file of an ended run", zap.String("path", p))
		}
	}
}

// removeIfDead removes the run file at path if no run holds its lock.
func removeIfDead(path string) (bool, error) {
	lock, err := openRunFileLock(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	held, err := tryLockRunFileFn(lock)
	if err != nil || !held {
		_ = lock.Close()
		return false, err
	}
	if !stillAt(lock, path) {
		_ = lock.Close()
		return false, nil
	}
	if err := removeAndUnlock(path, lock); err != nil {
		if errors.Is(err, os.ErrNotExist) || isInUse(err) {
			// Gone already, or (Windows) open in a handle that refuses
			// delete: the run that made it, in the instant before its lock.
			return false, nil
		}
		return false, err
	}
	return true, nil
}
