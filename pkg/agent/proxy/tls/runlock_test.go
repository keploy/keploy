package tls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestMain gives the package's tests a lock directory of their own, so no test
// touches the host's (/run/keploy-ca-runs). Its name does not start with
// runDirPrefix: a child run of the run-directory rows makes one in the temp
// directory those rows list run directories in. Started with
// KEPLOY_TEST_HOLD_RUN_LOCK=<id>, the test binary is instead a lock holder for
// the cross-process tests (startLockHolder): it takes the run lock for id in
// KEPLOY_TEST_RUN_LOCK_DIR, says "held", and keeps it until it is killed or its
// stdin closes. Started with KEPLOY_TEST_PROBE_STORE_LOCK=<dir>, it only says
// whether <dir>/store.lock is held (probeStoreLock: the fake commands of the
// store-lock rows run it). Started with KEPLOY_TEST_IMPORT=<id>, it is another
// run that imports its CA (importAsAnotherRun).
func TestMain(m *testing.M) {
	if dir := os.Getenv("KEPLOY_TEST_PROBE_STORE_LOCK"); dir != "" {
		probeStoreLock(dir)
		os.Exit(0)
	}
	if id := os.Getenv("KEPLOY_TEST_IMPORT"); id != "" {
		importAsAnotherRun(id)
		os.Exit(0)
	}
	if id := os.Getenv("KEPLOY_TEST_HOLD_RUN_LOCK"); id != "" {
		caRunsDir = os.Getenv("KEPLOY_TEST_RUN_LOCK_DIR")
		holdCARunLock(context.Background(), zap.NewNop(), id)
		if caRunLock.f == nil {
			fmt.Println("no lock")
			os.Exit(1)
		}
		fmt.Println("held")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "tls-test-ca-runs-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	caRunsDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// useRunsDir gives the test a lock directory of its own (and gives up the
// test process's lock at its end): the lock files a test leaves stay, as dead
// runs' do.
func useRunsDir(t *testing.T) {
	t.Helper()
	old := caRunsDir
	caRunsDir = t.TempDir()
	t.Cleanup(func() {
		releaseCARunLock()
		caRunsDir = old
	})
}

// installCtx bounds an install row's install: a wait for the run's lock that
// does not end then fails the row at its own bound, by name, instead of
// holding the package up until go test's timeout.
func installCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ownLockFile is this process's lock file for id.
func ownLockFile(id string) string {
	return filepath.Join(caRunsDir, id+"."+strconv.Itoa(os.Getpid())+".lock")
}

// A run's lock makes it live until it is released, and the release removes its
// file. A second hold of the same id keeps the lock it has; a hold for another
// id gives the first one up.
func TestCARunLockHeldUntilReleased(t *testing.T) {
	useRunsDir(t)
	if caRunLive("0a1b2c") {
		t.Fatal("a run that never took a lock is live")
	}
	holdCARunLock(context.Background(), zap.NewNop(), "0a1b2c")
	if !caRunLive("0a1b2c") {
		t.Fatal("a run holding its lock is not live")
	}
	held := caRunLock.f
	holdCARunLock(context.Background(), zap.NewNop(), "0a1b2c") // the same id again: kept, not given up and taken anew
	if caRunLock.f != held || !caRunLive("0a1b2c") {
		t.Fatal("holding the same id twice did not keep the lock it had")
	}
	holdCARunLock(context.Background(), zap.NewNop(), "3d4e5f")
	if caRunLive("0a1b2c") || !caRunLive("3d4e5f") {
		t.Fatalf("after a hold for another id: first live %v, second live %v; want false, true", caRunLive("0a1b2c"), caRunLive("3d4e5f"))
	}
	if _, err := os.Stat(ownLockFile("0a1b2c")); !os.IsNotExist(err) {
		t.Fatalf("the given-up lock's file is still there (stat: %v)", err)
	}
	second := caRunLock.f
	releaseCARunLock()
	if caRunLive("3d4e5f") {
		t.Fatal("a released run is still live")
	}
	// (A closed file's Stat fails: on Windows with an error of its own, not
	// os.ErrClosed.)
	if _, err := second.Stat(); err == nil {
		t.Fatal("the release kept its lock file open")
	}
	if _, err := os.Stat(ownLockFile("3d4e5f")); !os.IsNotExist(err) {
		t.Fatalf("the release left its lock file (stat: %v)", err)
	}
}

// An id with upper-case letters is the same run as its lower-case form (a JDK
// keystore lists aliases in lower case), and a prefix of a live id is another
// id. An id that cannot name a lock file takes none and has none: one with a
// dot would read as another id's ("abc.def" as "abc"'s), one with a separator
// would put its file outside the directory. A lock file nobody holds is a dead
// run's.
func TestCARunLockNames(t *testing.T) {
	useRunsDir(t)
	holdCARunLock(context.Background(), zap.NewNop(), "ABCdef")
	if !caRunLive("abcdef") {
		t.Fatal("the lower-case form of a held id is not live")
	}
	if caRunLive("abc") {
		t.Fatal("a prefix of a held id is live")
	}
	holdCARunLock(context.Background(), zap.NewNop(), "run-id_1")
	if !caRunLive("run-id_1") {
		t.Fatal("an id with a dash and an underscore took no lock")
	}
	holdCARunLock(context.Background(), zap.NewNop(), "abc.def")
	if caRunLive("abc") || caRunLive("abc.def") {
		t.Fatalf("an id with a dot took a lock: %q live %v, %q live %v", "abc", caRunLive("abc"), "abc.def", caRunLive("abc.def"))
	}
	escaped := filepath.Join(filepath.Dir(caRunsDir), "escaped."+strconv.Itoa(os.Getpid())+".lock")
	t.Cleanup(func() { _ = os.Remove(escaped) })
	holdCARunLock(context.Background(), zap.NewNop(), "../escaped")
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Fatalf("an id with a separator put a lock file outside the directory (stat: %v)", err)
	}
	if err := os.WriteFile(filepath.Join(caRunsDir, "feed01.1.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if caRunLive("feed01") {
		t.Fatal("a run whose lock file nobody holds is live")
	}
}

// With no lock directory no run holds a lock: no run on this host has made one.
func TestCARunLockWithoutTheDirectory(t *testing.T) {
	useRunsDir(t)
	caRunsDir = filepath.Join(caRunsDir, "never-made")
	if caRunLive("0a1b2c") {
		t.Fatal("a run is live although there is no lock directory")
	}
}

// A run that can take no lock says why, and how to fix it: its id names no
// lock file (the CA keploy reuses to remove, or its common name to change), the
// lock directory cannot be made (a file holds a name on its path), or its lock
// file cannot be opened (a directory holds its name): the lock's directory, the
// directory above it and the lock's file, with the access each needs.
func TestCARunLockWarnsWhyItTookNone(t *testing.T) {
	useRunsDir(t)
	t.Cleanup(func() { caRunLockWarned.Store(false) })
	runs := caRunsDir
	notDir := filepath.Join(runs, "a-file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ownLockFile("0a1b2c"), 0o700); err != nil {
		t.Fatal(err)
	}
	// pathFix is what the warning says for a lock file in dir.
	pathFix := func(dir string) []string {
		path := filepath.Join(dir, "0a1b2c."+strconv.Itoa(os.Getpid())+".lock")
		return []string{
			"make the directory " + dir + " (write access to " + filepath.Dir(dir) + ")",
			"create the file " + path + " (write access to " + dir + ")",
			"or create that file with read and write access for that user",
			"keep " + dir + " on a file system that supports file locks",
		}
	}
	for _, row := range []struct {
		dir, id, why string
		fix          []string
	}{
		{runs, "abc.def", "its id cannot name a lock file", []string{
			"remove the CA keploy reuses (ca.key and ca.crt in its CA key directory) so that keploy makes its own",
			`or give that CA a common name of "Keploy MITM CA " followed by letters, digits, '-' and '_' only`,
		}},
		{filepath.Join(notDir, "runs"), "0a1b2c", "the directory of keploy runs' locks cannot be made", pathFix(filepath.Join(notDir, "runs"))},
		{runs, "0a1b2c", "its lock file cannot be opened", pathFix(runs)},
	} {
		caRunsDir = row.dir
		caRunLockWarned.Store(false)
		core, logs := observer.New(zapcore.WarnLevel)
		holdCARunLock(context.Background(), zap.New(core), row.id)
		if caRunLock.f != nil {
			t.Fatalf("id %q in %s: a lock was held", row.id, row.dir)
		}
		if logs.Len() != 1 || logs.FilterMessageSnippet("("+row.why+")").Len() != 1 {
			t.Fatalf("id %q in %s: warnings %v, want one saying %q", row.id, row.dir, logs.All(), row.why)
		}
		msg := logs.All()[0].Message
		for _, want := range append([]string{"the app's TLS calls then fail. To fix it, "}, row.fix...) {
			if !strings.Contains(msg, want) {
				t.Errorf("id %q in %s: the warning %q does not say %q", row.id, row.dir, msg, want)
			}
		}
	}
}

// A run that can take no lock says so, once per process.
func TestCARunLockWarnsOnceWhenItCannotLock(t *testing.T) {
	useRunsDir(t)
	caRunLockWarned.Store(false)
	t.Cleanup(func() { caRunLockWarned.Store(false) })
	core, logs := observer.New(zapcore.WarnLevel)
	holdCARunLock(context.Background(), zap.New(core), "abc.def")
	holdCARunLock(context.Background(), zap.New(core), "ghi.jkl")
	if n := logs.Len(); n != 1 {
		t.Fatalf("%d warnings for two runs' locks that could not be taken, want one", n)
	}
	if caRunLock.f != nil {
		t.Fatal("a lock was held for an id that cannot name a lock file")
	}
}

// lockOwnFileElsewhere opens this process's lock file for id through a handle
// of its own and locks it, as another holder would (flock and LockFileEx
// locks are per open file, so the run's own open conflicts with it).
func lockOwnFileElsewhere(t *testing.T, id string) *os.File {
	t.Helper()
	if err := os.MkdirAll(caRunsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	other, err := os.OpenFile(ownLockFile(id), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if locked, err := tryLockFile(other); err != nil || !locked {
		t.Fatalf("could not take the run's lock file first: %v %v", locked, err)
	}
	return other
}

// A run whose own lock file another holder keeps locked waits for it: when
// the holder lets go, the run takes its lock; when the holder keeps it past
// caRunLockWait the run goes on without it, saying so; when the run's context
// has ended it does not wait, and says nothing (the run is stopping).
func TestCARunLockWaitsForAFileHeldByAnother(t *testing.T) {
	useRunsDir(t)
	caRunLockWarned.Store(false)
	oldWait := caRunLockWait
	t.Cleanup(func() { caRunLockWarned.Store(false); caRunLockWait = oldWait })
	const warn = "keploy could not take this run's lock (its lock file stayed held by another process)"

	// Each row's bound: generous where the run must take its lock (a slow
	// runner only makes it wait longer), and short where it must give up.
	// Where the run must not wait at all, the bound is 30 s, so a wait there
	// ends with the bound's error, which fails the row.
	t.Run("the holder lets go", func(t *testing.T) {
		other := lockOwnFileElsewhere(t, "0a1b2c")
		caRunLockWait = 30 * time.Second
		unlocked := make(chan struct{})
		time.AfterFunc(20*time.Millisecond, func() { unlockFile(other); close(unlocked) })
		core, logs := observer.New(zapcore.WarnLevel)
		holdCARunLock(context.Background(), zap.New(core), "0a1b2c")
		<-unlocked // other is closed at the subtest's end, after this
		if caRunLock.f == nil || logs.Len() != 0 {
			t.Fatalf("the run did not take its lock once the holder let go (held %v, %d warnings)", caRunLock.f != nil, logs.Len())
		}
		releaseCARunLock()
	})

	t.Run("the holder keeps it", func(t *testing.T) {
		lockOwnFileElsewhere(t, "0a1b2c")
		caRunLockWarned.Store(false) // a failure above must not show up here
		caRunLockWait = 50 * time.Millisecond
		// The row's own bound: a wait with no bound of its own ends when this
		// context does, with no warning, which fails the row.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		core, logs := observer.New(zapcore.WarnLevel)
		start := time.Now()
		holdCARunLock(ctx, zap.New(core), "0a1b2c")
		if waited := time.Since(start); waited < caRunLockWait {
			t.Fatalf("the run gave up after %s, before its bound %s", waited, caRunLockWait)
		}
		if caRunLock.f != nil {
			t.Fatal("the run holds a lock another holder keeps")
		}
		if n := logs.FilterMessageSnippet(warn).Len(); n != 1 || logs.Len() != 1 {
			t.Fatalf("warnings %v, want the one saying the file stayed held", logs.All())
		}
		// It names the fix: the process that holds the file, which on Linux
		// the kernel's table of locks names (here this process, through
		// its other handle). Windows does not tell who holds a lock.
		msg := logs.All()[0].Message
		want := "To fix it, let the process that holds the lock on " + ownLockFile("0a1b2c") + " go on, or end it: "
		if runtime.GOOS == "linux" {
			want += "process " + strconv.Itoa(os.Getpid()) + " ("
		} else {
			want += storeLockHolderUnknown + "."
		}
		if !strings.Contains(msg, want) {
			t.Fatalf("the warning %q does not say %q", msg, want)
		}
	})

	t.Run("a try fails", func(t *testing.T) {
		caRunLockWarned.Store(false)
		caRunLockWait = 30 * time.Second
		old := caRunLockTry
		caRunLockTry = func(*os.File) (bool, error) { return false, errors.New("injected") }
		t.Cleanup(func() { caRunLockTry = old })
		core, logs := observer.New(zapcore.WarnLevel)
		holdCARunLock(context.Background(), zap.New(core), "0a1b2c")
		if caRunLock.f != nil {
			t.Fatal("the run holds a lock whose try failed")
		}
		const failed = "keploy could not take this run's lock (its lock file could not be locked)"
		if n := logs.FilterMessageSnippet(failed).Len(); n != 1 || logs.Len() != 1 {
			t.Fatalf("warnings %v, want the one saying the file could not be locked", logs.All())
		}
		msg := logs.All()[0].Message
		for _, want := range []string{
			"or create that file with read and write access for that user",
			"keep " + caRunsDir + " on a file system that supports file locks",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("the warning %q does not say %q", msg, want)
			}
		}
	})

	t.Run("the run's context has ended", func(t *testing.T) {
		lockOwnFileElsewhere(t, "0a1b2c")
		caRunLockWarned.Store(false)
		caRunLockWait = 30 * time.Second
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// The wait itself ends with the context's error, not the bound's.
		own, err := os.OpenFile(ownLockFile("0a1b2c"), os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = own.Close() }()
		if err := waitForLock(ctx, own); !errors.Is(err, context.Canceled) {
			t.Fatalf("the wait of a stopping run ended with %v, want the context's error", err)
		}
		core, logs := observer.New(zapcore.WarnLevel)
		holdCARunLock(ctx, zap.New(core), "0a1b2c")
		if caRunLock.f != nil || logs.Len() != 0 {
			t.Fatalf("a stopping run: held %v, warnings %v; want neither", caRunLock.f != nil, logs.All())
		}
	})
}

// A try that fails for another reason than the lock being held ends the wait
// at once, with that error (here, a closed file). The run's bound is an hour
// and the row's context ends after 30 s: a wait that went on ends with the
// context's error instead, which fails the row.
func TestCARunLockWaitEndsOnALockError(t *testing.T) {
	useRunsDir(t)
	oldWait := caRunLockWait
	caRunLockWait = time.Hour
	t.Cleanup(func() { caRunLockWait = oldWait })
	f, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = waitForLock(ctx, f)
	if err == nil {
		t.Fatal("a closed file was locked")
	}
	if ctx.Err() != nil {
		t.Fatalf("the wait went on after the lock error, until its context ended (%v)", err)
	}
}

// Every native install but macOS's writes trust-store entries, and so takes
// this run's lock; macOS's writes none.
func TestCARunLockIsTakenWhereTheInstallWritesEntries(t *testing.T) {
	for goos, want := range map[string]bool{"linux": true, "windows": true, "freebsd": true, "darwin": false} {
		if got := writesSweptEntries(goos); got != want {
			t.Errorf("writesSweptEntries(%q) = %v, want %v", goos, got, want)
		}
	}
}
