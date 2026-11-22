//go:build !windows

package tls

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// anotherUser is a uid that does not own path, and is not root, for a row that
// checks path for another user: this test's user as itself, or, run as root
// (who owns every file a root test makes), a uid whose files path is made one
// of first.
func anotherUser(t *testing.T, path string) int {
	t.Helper()
	other := 4242
	if os.Geteuid() == other {
		other++
	}
	if os.Geteuid() == 0 {
		if err := os.Lchown(path, other+1, -1); err != nil {
			t.Fatal(err)
		}
	}
	return other
}

// rootsDir is a directory root owns that only root writes.
func rootsDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		// 0755: t.TempDir makes it 0777 less the umask.
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	return "/"
}

// What keploy trusts as a directory or file of its own (checkOwned): no link,
// of the kind it wants, with none of the permission bits it denies, owned by
// the run's user, or by root where root's is allowed. Each refusal names what
// it refused and why.
func TestCheckOwnedTrustsOnlyWhatTheRunsUserOrRootAloneControls(t *testing.T) {
	me := os.Geteuid()
	mk := func(mode os.FileMode) string {
		dir := filepath.Join(t.TempDir(), "d")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "l")
	if err := os.Symlink(mk(0o700), link); err != nil {
		t.Fatal(err)
	}
	others := mk(0o700)
	other := anotherUser(t, others)
	for _, row := range []struct {
		name   string
		path   string
		dir    bool
		euid   int
		orRoot bool
		want   string // "" for trusted
	}{
		{"the user's own directory", mk(0o755), true, me, false, ""},
		{"the user's own file", file, false, me, false, ""},
		{"a directory others can write", mk(0o777), true, me, false, "has mode 777; it must have none of the bits 22"},
		{"a directory its group can write", mk(0o775), true, me, false, "has mode 775; it must have none of the bits 22"},
		{"a link to the user's own directory", link, true, me, false, "is a symlink"},
		{"a file where a directory goes", file, true, me, false, "is not a directory"},
		{"a directory where a file goes", mk(0o700), false, me, false, "is not a regular file"},
		{"another user's directory", others, true, other, true, "is owned by uid"},
		{"root's directory, where root's is allowed", rootsDir(t), true, other, true, ""},
		{"root's directory, where only the user's is", rootsDir(t), true, other, false, "is owned by uid 0"},
	} {
		fi, err := os.Lstat(row.path)
		if err != nil {
			t.Fatal(err)
		}
		err = checkOwned("the row's path", row.path, fi, row.dir, 0o022, row.euid, row.orRoot)
		switch {
		case row.want == "" && err != nil:
			t.Errorf("%s: refused: %v", row.name, err)
		case row.want != "" && (err == nil || !strings.Contains(err.Error(), row.want) || !strings.Contains(err.Error(), "the row's path "+row.path)):
			t.Errorf("%s: error %v, want one naming %s and saying %q", row.name, err, row.path, row.want)
		}
	}
}

// A run on macOS of a user other than root makes its runs directory in its
// temp directory only while that temp directory is the user's alone: a real
// directory (no link) the user owns that no one else can write. /tmp, the
// temp directory with TMPDIR unset or empty, is not, nor is a world- or
// group-writable directory, a link, or another user's: the run makes nothing
// there and says to set TMPDIR. The runs directory itself must be a real
// directory that the user or root owns and no one else can write, on every
// OS; elsewhere than a macOS user's run its parent is not checked.
func TestRunsDirectoryOnlyWhereNoOtherUserCanChangeIt(t *testing.T) {
	me := os.Geteuid()
	user := me
	if user == 0 {
		// A macOS run of root makes no runs directory in its temp
		// directory: the rows of a user's run take another user's
		// uid, whose temp directory they make theirs.
		user = 4242
	}
	own := func(t *testing.T) string {
		// 0700: t.TempDir makes it 0777 less the umask.
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if me == 0 {
			if err := os.Chown(dir, user, -1); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	chmod := func(t *testing.T, dir string, mode os.FileMode) string {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	const tmpFix = "set TMPDIR to the one macOS gives this user (getconf DARWIN_USER_TEMP_DIR)"
	for _, row := range []struct {
		name string
		goos string
		tmp  func(t *testing.T) string
		euid func(t *testing.T, tmp string) int
		want []string // nil: made and trusted
	}{
		{"a macOS user's own temp directory", "darwin", own, nil, nil},
		{"/tmp, with TMPDIR unset or empty", "darwin", func(*testing.T) string { return "/tmp" }, nil, []string{"the temp directory /tmp ", tmpFix}},
		{"a world-writable temp directory", "darwin", func(t *testing.T) string { return chmod(t, own(t), 0o777) }, nil, []string{"has mode 777", tmpFix}},
		{"a group-writable temp directory", "darwin", func(t *testing.T) string { return chmod(t, own(t), 0o770) }, nil, []string{"has mode 770", tmpFix}},
		{"a link to the user's own temp directory", "darwin", func(t *testing.T) string {
			link := filepath.Join(t.TempDir(), "tmp")
			if err := os.Symlink(own(t), link); err != nil {
				t.Fatal(err)
			}
			return link
		}, nil, []string{"is a symlink", tmpFix}},
		{"another user's temp directory", "darwin", own, func(t *testing.T, tmp string) int { return anotherUser(t, tmp) }, []string{"the temp directory", "is owned by uid", tmpFix}},
		{"root's temp directory", "darwin", func(t *testing.T) string { return rootsDir(t) }, nil, []string{"the temp directory", "is owned by uid 0", tmpFix}},
		{"a Linux run's world-writable parent", "linux", func(t *testing.T) string { return chmod(t, t.TempDir(), 0o777) }, func(*testing.T, string) int { return me }, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			tmp := row.tmp(t)
			euid := user
			if row.euid != nil {
				euid = row.euid(t, tmp)
			}
			// A name of the row's own, so a row in /tmp or / makes nothing
			// any other process uses.
			dir := filepath.Join(tmp, "keploy-ca-runs-row-"+strconv.Itoa(os.Getpid()))
			err := makeCARunsDirFor(row.goos, euid, dir)
			_, statErr := os.Lstat(dir)
			if row.want == nil {
				if err != nil || statErr != nil {
					t.Fatalf("refused: %v (made: %v)", err, statErr)
				}
				_ = os.Remove(dir)
				return
			}
			if !errors.Is(statErr, fs.ErrNotExist) {
				_ = os.Remove(dir)
				t.Errorf("%s was made (%v): want nothing made in a temp directory the run does not use", dir, statErr)
			}
			if err == nil {
				t.Fatal("trusted, want refused")
			}
			for _, w := range row.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not say %q", err, w)
				}
			}
		})
	}
	// The runs directory itself, on Linux and for a macOS user's run (whose
	// temp directory, its parent here, is the user's own).
	for _, row := range []struct {
		name string
		oses []string
		dir  func(t *testing.T) string
		euid func(t *testing.T, dir string) int
		want string
	}{
		{"a world-writable runs directory", []string{"linux", "darwin"}, func(t *testing.T) string {
			dir := filepath.Join(own(t), "runs")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return chmod(t, dir, 0o777)
		}, nil, "has mode 777"},
		{"a group-writable runs directory", []string{"linux", "darwin"}, func(t *testing.T) string {
			dir := filepath.Join(own(t), "runs")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return chmod(t, dir, 0o770)
		}, nil, "has mode 770"},
		{"a link to a runs directory", []string{"linux", "darwin"}, func(t *testing.T) string {
			link := filepath.Join(own(t), "runs")
			if err := os.Symlink(own(t), link); err != nil {
				t.Fatal(err)
			}
			return link
		}, nil, "is a symlink"},
		// For a macOS user's run another user's temp directory is refused
		// first (above).
		{"another user's runs directory", []string{"linux"}, func(t *testing.T) string {
			dir := filepath.Join(own(t), "runs")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return dir
		}, func(t *testing.T, dir string) int { return anotherUser(t, dir) }, "is owned by uid"},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := row.dir(t)
			euid := user
			if row.euid != nil {
				euid = row.euid(t, dir)
			}
			for _, goos := range row.oses {
				err := makeCARunsDirFor(goos, euid, dir)
				if err == nil || !strings.Contains(err.Error(), "the directory of keploy runs' locks "+dir+" ") || !strings.Contains(err.Error(), row.want) {
					t.Errorf("%s: error %v, want one naming %s and saying %q", goos, err, dir, row.want)
				}
			}
		})
	}
}

// A run whose runs directory cannot be made in its temp directory (one the
// user cannot write) says so, by the mkdir's error, and makes nothing.
func TestRunsDirectoryThatCannotBeMadeSaysWhy(t *testing.T) {
	tmp := t.TempDir()
	user := os.Geteuid()
	if user == 0 {
		// A macOS run of root makes no runs directory in its temp
		// directory: the row takes another user's uid.
		user = 4242
		if err := os.Chown(tmp, user, -1); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(tmp, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o700) })
	dir := filepath.Join(tmp, "keploy-ca-runs")
	err := makeCARunsDirFor("darwin", user, dir)
	if os.Geteuid() == 0 {
		// Root writes a directory whatever its mode: it makes it, its
		// own, which a run of the user trusts as root's.
		if err != nil {
			t.Fatalf("as root: %v, want made", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "mkdir "+dir) {
		t.Errorf("error %v, want the mkdir's, naming %s", err, dir)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s was made: %v", dir, err)
	}
}

// A runs directory root owns, and only root writes, as Linux's
// /run/keploy-ca-runs, is one a run of another user uses too.
func TestRunsDirectoryRootOwnsIsTrusted(t *testing.T) {
	if err := makeCARunsDirFor("linux", 4242, rootsDir(t)); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

// A lock file is opened from its directory as opened, which must be one no
// other user than the run's and root can change (openCARunsDirFor), through no
// link, and only when it is a regular file that the run's user owns and that
// no other user can read or write (openLockFileFor, openLockFileAt).
func TestLockFileOnlyARegularFileOfTheRunsUserAlone(t *testing.T) {
	me := os.Geteuid()
	// 0700: t.TempDir makes it 0777 less the umask.
	ownDir := func(t *testing.T) string {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	dir := ownDir(t)
	path := filepath.Join(dir, "a.lock")
	f, err := openLockFileFor(me, path, os.O_RDONLY|os.O_CREATE)
	if err != nil {
		t.Fatalf("the run's own new lock file: %v", err)
	}
	_ = f.Close()
	if fi, err := os.Lstat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the lock file made: %v %v, want mode 600", fi, err)
	}
	// Another user's file, in a directory the run trusts.
	d, err := openCARunsDirFor(me, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if f, err := openLockFileAt(d, anotherUser(t, path), path, os.O_RDONLY); err == nil || !strings.Contains(err.Error(), "the lock file "+path+" is owned by uid") {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("another user's lock file: %v, want refused", err)
	}
	// The run's own file that others can read: anyone who can open it can
	// hold its lock.
	readable := filepath.Join(dir, "r.lock")
	if err := os.WriteFile(readable, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readable, 0o640); err != nil {
		t.Fatal(err)
	}
	if f, err := openLockFileFor(me, readable, os.O_RDONLY|os.O_CREATE); err == nil || !strings.Contains(err.Error(), "the lock file "+readable+" has mode 640") {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("a lock file its group can read: %v, want refused", err)
	}
	// Nor one root owns that every user can read, in a directory root owns
	// (one a run of another user than root trusts): /etc/passwd is there,
	// by its real path (macOS's /etc is a link to /private/etc, and a link
	// is no runs directory).
	passwd, err := filepath.EvalSymlinks("/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if f, err := openLockFileFor(4242, passwd, os.O_RDONLY); err == nil || !strings.Contains(err.Error(), "the lock file "+passwd+" has mode 644") {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("a lock file root owns that others can read: %v, want refused for its mode", err)
	}
	target := filepath.Join(t.TempDir(), "made-through-the-link")
	link := filepath.Join(dir, "b.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openLockFileFor(me, link, os.O_RDONLY|os.O_CREATE); err == nil {
		_ = f.Close()
		t.Errorf("a link at the lock file was opened")
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the link's target was made: %v", err)
	}
	sub := filepath.Join(dir, "c.lock")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if f, err := openLockFileFor(me, sub, os.O_RDONLY); err == nil {
		_ = f.Close()
		t.Errorf("a directory at the lock file was opened")
	}
	// The directory is checked as the file is opened, not only when a run
	// makes it: a lock file in a directory others can write, or through a
	// link to a directory, is not opened, and none is made.
	open := ownDir(t)
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	inOpen := filepath.Join(open, "a.lock")
	if f, err := openLockFileFor(me, inOpen, os.O_RDONLY|os.O_CREATE); err == nil || !strings.Contains(err.Error(), runsDirWhat+" "+open+" has mode 777") {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("a lock file in a directory others can write: %v, want refused", err)
	}
	if _, err := os.Lstat(inOpen); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a lock file was made in a directory others can write: %v", err)
	}
	dirLink := filepath.Join(ownDir(t), "runs")
	if err := os.Symlink(ownDir(t), dirLink); err != nil {
		t.Fatal(err)
	}
	if f, err := openLockFileFor(me, filepath.Join(dirLink, "a.lock"), os.O_RDONLY|os.O_CREATE); err == nil || !strings.Contains(err.Error(), runsDirWhat+" "+dirLink+" is a symlink") {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("a lock file through a link to a directory: %v, want refused", err)
	}
}

// Another run's lock file is opened to test its lock (caRunLive) as a run's
// own is (openRunLockToTest), never made: one it does not trust, a link or a
// file others can open, counts as live, so a sweep leaves that run's entries.
func TestCARunLockTestsOnlyALockFileItTrusts(t *testing.T) {
	useRunsDir(t)
	gone := filepath.Join(caRunsDir, "0ddba11.1.lock")
	if f, err := openRunLockToTest(gone); err == nil {
		_ = f.Close()
		t.Error("openRunLockToTest opened a lock file that is not there, want an error")
	}
	if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("openRunLockToTest made a lock file: %v", err)
	}
	// A link at a run's lock file to a file no one holds.
	unheld := filepath.Join(t.TempDir(), "unheld.lock")
	if err := os.WriteFile(unheld, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unheld, filepath.Join(caRunsDir, "11e1ed.1.lock")); err != nil {
		t.Fatal(err)
	}
	if !caRunLive("11e1ed") {
		t.Error("a run whose lock file is a link counts as dead, want live")
	}
	// A lock file no one holds is not live while it is one keploy makes
	// (0600), and is once others can read it, which keploy never makes.
	if err := os.WriteFile(filepath.Join(caRunsDir, "0bed.1.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if caRunLive("0bed") {
		t.Fatal("a run whose own lock file no one holds is live")
	}
	if err := os.Chmod(filepath.Join(caRunsDir, "0bed.1.lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !caRunLive("0bed") {
		t.Error("a run whose lock file others can read counts as dead, want live")
	}
}

// The store lock is not taken in a runs directory others can write, nor
// through a link at store.lock: the change runs without it, the run warns
// once, naming what it refused, and nothing is made through the link.
func TestStoreLockRefusesADirectoryOthersCanWriteAndALink(t *testing.T) {
	for _, row := range []struct {
		name  string
		setup func(t *testing.T) (want string, check func(t *testing.T))
	}{
		{"a world-writable runs directory", func(t *testing.T) (string, func(*testing.T)) {
			if err := os.Chmod(caRunsDir, 0o777); err != nil {
				t.Fatal(err)
			}
			return "has mode 777", func(t *testing.T) {
				if _, err := os.Lstat(filepath.Join(caRunsDir, storeLockName)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("store.lock was made in it: %v", err)
				}
			}
		}},
		{"a link at store.lock", func(t *testing.T) (string, func(*testing.T)) {
			target := filepath.Join(t.TempDir(), "made-through-the-link")
			if err := os.Symlink(target, filepath.Join(caRunsDir, storeLockName)); err != nil {
				t.Fatal(err)
			}
			return storeLockName, func(t *testing.T) {
				if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the link's target was made: %v", err)
				}
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			useRunsDir(t)
			storeLockWarned.Store(false)
			t.Cleanup(func() { storeLockWarned.Store(false) })
			want, check := row.setup(t)
			core, logs := observer.New(zapcore.WarnLevel)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			runs := 0
			for i := 0; i < 2; i++ {
				if err := withStoreLock(ctx, zap.New(core), func() error { runs++; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if runs != 2 {
				t.Fatalf("%d of 2 changes ran", runs)
			}
			if logs.FilterMessageSnippet(storeLockWarnMsg).Len() != 1 || logs.Len() != 1 {
				t.Fatalf("warnings %v, want one", logs.All())
			}
			if e, ok := logs.All()[0].ContextMap()["error"].(string); !ok || !strings.Contains(e, want) {
				t.Errorf("the warning's error %q does not say %q", e, want)
			}
			check(t)
		})
	}
}

// The CA key keploy keeps is trusted only when it is a regular file, no link,
// that this run's user owns and no one else can read or write
// (verifyKeyFileSecure); one root owns is not this user's.
func TestCAKeyFileOnlyTheUsersOwnAndPrivate(t *testing.T) {
	dir := t.TempDir()
	key := func(name string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("key"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if err := verifyKeyFileSecure(key("own", 0o600)); err != nil {
		t.Errorf("the user's own 0600 key: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(key("target", 0o600), link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		key("group-readable", 0o640): "has mode 640",
		key("other-readable", 0o604): "has mode 604",
		link:                         "is a symlink",
		filepath.Join(dir, "dir"):    "is not a regular file",
	} {
		if err := verifyKeyFileSecure(path); err == nil || !strings.Contains(err.Error(), "CA key "+path+" "+want) {
			t.Errorf("%s: %v, want it refused: %q", path, err, want)
		}
	}
	if os.Geteuid() != 0 {
		// A regular file root owns that no one else can read (procfs
		// lists pid 1's environment so), where /proc shows it.
		if fi, err := os.Lstat("/proc/1/environ"); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o077 == 0 {
			if err := verifyKeyFileSecure("/proc/1/environ"); err == nil || !strings.Contains(err.Error(), "is owned by uid 0") {
				t.Errorf("a key root owns: %v, want it refused", err)
			}
		}
	}
}
