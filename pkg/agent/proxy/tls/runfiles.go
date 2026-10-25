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
// after it creates the file until it removes it, and a later setup of the
// same user with the same temp dir removes the run files whose lock it can
// take. The system drops a process's locks
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
// The run files live in a dir of this user's own in the temp dir
// (runFilesDir: keploy-<euid> on Unix), never in the temp dir itself. The temp
// dir (/tmp) is writable by every local user and the names a run makes, locks
// and sweeps are predictable, so there any user could race a root run for one:
// plant a file at a name a sweep has just freed, or a link, or a device, and
// have root lock, read or hand its app what they put there. Nobody else can
// create anything in a dir only this user can write, so nobody can plant, link
// or race, and the sweep runs only there. The dir and the CA files in it stay
// readable by everyone (0755 and 0644: a CA certificate is public), so an app
// run as another user still reads them.
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

// runFilesDir is the dir a run makes its files in.
type runFilesDir struct {
	path string
	// own: a new dir of this run's alone, made because the dir at the
	// private dir's name was not this user's private dir; the run removes it
	// with its files.
	own bool
}

// openRunFilesDir returns the dir this run's files go in: the private dir
// keploy-<id> in the temp dir (privateDirName), made if missing, and used
// only if it is a directory (not a link), owned by this user and writable by
// nobody else (checkPrivateDir). A dir at that name that is not is never
// used: the run says why, and makes a new dir of its own in the temp dir,
// removed when the run ends (a killed run leaves that one, and no sweep takes
// it, since its name is not the private dir's).
//
// The check holds while nobody else can rename the dir away, which the
// sticky bit of /tmp guarantees; a TMPDIR every user may write without the
// sticky bit is open to that, as it is for anything put there.
func openRunFilesDir(logger *zap.Logger) (*runFilesDir, error) {
	tmp := os.TempDir()
	name, err := privateDirName()
	if err != nil {
		return nil, fmt.Errorf("naming this user's dir for keploy's trust files: %w", err)
	}
	path := filepath.Join(tmp, name)
	why := makePrivateDir(path)
	if why == nil {
		return &runFilesDir{path: path}, nil
	}
	own, err := os.MkdirTemp(tmp, name+"-")
	if err != nil {
		return nil, fmt.Errorf("%s is not usable (%v), and making a dir of this run's own failed: %w", path, why, err)
	}
	if err := setPrivateDirMode(own); err != nil {
		_ = os.Remove(own)
		return nil, err
	}
	if err := checkPrivateDir(own); err != nil {
		_ = os.Remove(own)
		return nil, fmt.Errorf("the dir %s this run made for its trust files: %w", own, err)
	}
	logger.Warn("keploy does not put its trust files in the dir at its own name in the temp dir, because that is not a dir only this user can write; this run uses a new dir of its own, and a run killed before its cleanup leaves that one behind",
		zap.String("dir", path), zap.String("why", why.Error()), zap.String("run_dir", own),
		zap.String("next_step", "remove that path (another user, or an older tool, made it), so the next run makes it again as this user's; if this run is killed before its cleanup, remove run_dir too"))
	return &runFilesDir{path: own, own: true}, nil
}

// removeIfOwn removes the dir if the run made it for itself alone (own),
// once its files are gone. Safe on a nil dir and on one removed already.
func (d *runFilesDir) removeIfOwn() error {
	if d == nil || !d.own {
		return nil
	}
	if err := os.Remove(d.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// makePrivateDir makes this user's private dir at path, if nothing is there,
// and returns why what is at path is not a usable private dir, or nil.
func makePrivateDir(path string) error {
	err := os.Mkdir(path, 0o755)
	switch {
	case err == nil:
		// The umask may have taken the bits an app of another user needs.
		if err := setPrivateDirMode(path); err != nil {
			return fmt.Errorf("setting its mode: %w", err)
		}
	case !errors.Is(err, os.ErrExist):
		return err
	}
	return checkPrivateDir(path)
}

// checkPrivateDir returns why the entry at path is not a dir only this user
// can write, or nil: a symlink or other reparse point, not a dir, or one of
// another user's, one that another user can write, or (Unix) a mount point,
// whose owner and mode are whatever its filesystem says
// (checkPrivateDirOwner).
func checkPrivateDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("it is a link (%s), not a dir", fi.Mode().Type())
	}
	if !fi.IsDir() {
		return fmt.Errorf("it is not a dir (%s)", fi.Mode().Type())
	}
	return checkPrivateDirOwner(path, fi)
}

// privateRunFilesDir returns this user's private dir in the temp dir, if it
// is there and is one (checkPrivateDir), without making it: what `keploy ca
// clean` sweeps.
func privateRunFilesDir() (string, error) {
	name, err := privateDirName()
	if err != nil {
		return "", err
	}
	path := filepath.Join(os.TempDir(), name)
	if err := checkPrivateDir(path); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return path, nil
}

// runFile is a temp file of this run's, under the run's lock while it exists.
type runFile struct {
	path string
	// lock is the handle holding the run's lock; nil for a file the run
	// could not lock, which has an unlocked name.
	lock *os.File
}

// Seams for the tests of a new run file's window before its lock, of the
// instant a run file's lock is dropped, and of a temp dir that refuses locks.
var (
	beforeRunFileOpen  = func(string) {}
	beforeRunFileLock  = func(string) {}
	afterRunFileUnlock = func(string) {}
	tryLockRunFileFn   = tryLockRunFile
)

// warnUnlockedOnce says once per process that a run's files go unlocked.
var warnUnlockedOnce sync.Once

// createRunFile creates a new file in dir (the run's openRunFilesDir), named
// after pattern as os.CreateTemp names it, and takes the run's lock on it. It
// returns the file open for writing, which the caller closes, and the run
// file, which the caller removes with remove. Where it cannot lock, it creates
// the file under unlockedPattern instead: the run still works, only a kill
// leaves the file.
func createRunFile(logger *zap.Logger, dir, pattern string) (*os.File, *runFile, error) {
	if !runFileLocking {
		return createUnlockedRunFile(logger, dir, pattern, errors.ErrUnsupported)
	}
	for attempt := 0; attempt < maxRunFileAttempts; attempt++ {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, nil, err
		}
		rf := &runFile{path: f.Name()}
		lock, err := lockNewRunFile(rf.path)
		if err == nil && !sameFile(f, lock) {
			// The name was freed by a sweep and taken by another file before
			// the lock's open: the lock holds that file, not the one f
			// writes, and the run would hand its app what someone else put
			// there.
			_ = lock.Close()
			err = errRunFileTaken
		}
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
			return createUnlockedRunFile(logger, dir, pattern, err)
		}
	}
	return nil, nil, fmt.Errorf("creating a %s run file: %w, %d times", pattern, errRunFileTaken, maxRunFileAttempts)
}

// createUnlockedRunFile creates a run file the run could not lock (why), under
// the name no sweep takes.
func createUnlockedRunFile(logger *zap.Logger, dir, pattern string, why error) (*os.File, *runFile, error) {
	warnUnlockedOnce.Do(func() {
		logger.Warn("keploy cannot lock its trust files in the temp dir, so a run killed before its cleanup leaves them there and no later run removes them",
			zap.String("temp_dir", os.TempDir()), zap.String("dir", dir), zap.Error(why),
			zap.String("next_step", "point TMPDIR at a local filesystem; while no keploy runs, remove the keploy-ca-*.crt and keploy-java-* files a killed run leaves in dir: `keploy ca clean` does for this user's own dir in temp_dir, with TMPDIR set to temp_dir (sudo resets TMPDIR: sudo env TMPDIR=<temp_dir> keploy ca clean), but not for a dir of a run's own (keploy-<id>-<N>), which is removed by hand"))
	})
	f, err := os.CreateTemp(dir, unlockedPattern(pattern))
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

// sameFile reports whether a and b have the same file open.
func sameFile(a, b *os.File) bool {
	ai, err := a.Stat()
	if err != nil {
		return false
	}
	bi, err := b.Stat()
	return err == nil && os.SameFile(ai, bi)
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
		afterRunFileUnlock(path)
	}
	return err
}

// olderTrustFilePatterns are the names of the trust files that keploy
// versions before the lock wrote, and that a run which cannot lock still
// writes: keploy-ca-<N>.crt, keploy-java-truststore-<N>.jks (an older
// keploy's per-uid store, N its uid) and keploy-java-cas-<N>.pem. No sweep
// takes them; only the explicit `keploy ca clean` does (removeLegacyTempFiles).
var olderTrustFilePatterns = []string{unlockedPattern(runCAFilePattern), unlockedPattern(runTrustStorePattern), unlockedPattern(runMergedPEMPattern)}

// isRunFileName reports whether name is one os.CreateTemp gives a run-file
// pattern: its prefix, a decimal number, its suffix, and nothing else.
func isRunFileName(name string) bool {
	return isCreateTempName(name, runFilePatterns)
}

// isOlderTrustFileName reports whether name is a trust file under an older
// keploy name: one of olderTrustFilePatterns, the one shared truststore of the
// oldest versions, or an older per-uid one where os.Geteuid is -1 (Windows).
func isOlderTrustFileName(name string) bool {
	switch name {
	case "keploy-java-truststore.jks", "keploy-java-truststore--1.jks":
		return true
	}
	return isCreateTempName(name, olderTrustFilePatterns)
}

// isCreateTempName reports whether name is one os.CreateTemp gives one of
// patterns: its prefix, a decimal number, its suffix, and nothing else.
func isCreateTempName(name string, patterns []string) bool {
	for _, pattern := range patterns {
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

// sweepDeadRunFiles removes from dir, this user's private dir
// (openRunFilesDir), the run files of runs that have ended without removing
// them: those whose lock it can take. It leaves everything else alone: a live
// run's run files, the files of older keploy versions, and any other file.
// It never sweeps the temp dir itself, where another user could put things.
// Best-effort: a setup never fails over it.
//
// Where keploy has no file lock (runFileLocking false) it sweeps nothing,
// since it could not tell a live run's file from a dead one's; createRunFile
// says so.
func sweepDeadRunFiles(logger *zap.Logger, dir string) {
	if !runFileLocking {
		return
	}
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
