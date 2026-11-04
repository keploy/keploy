package tls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestMain gives the package's tests a lock directory of their own, so no test
// touches the host's (/run/keploy-ca-runs). Started with
// KEPLOY_TEST_PROBE_STORE_LOCK=<dir>, the test binary only says whether
// <dir>/store.lock is held (probeStoreLock: the fake commands of the store-lock
// rows run it). Started with KEPLOY_TEST_IMPORT=<id>, it is another run that
// imports its CA (importAsAnotherRun). Started with
// KEPLOY_TEST_HOLD_RUN_LOCK=<id>, the test binary is instead a lock holder for
// the cross-process tests (startLockHolder): it takes the run lock for id in
// KEPLOY_TEST_RUN_LOCK_DIR, says "held", and keeps it until it is killed or its
// stdin closes.
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
		holdRunLock(context.Background(), zap.NewNop(), id)
		if runLock.f == nil {
			fmt.Println("no lock")
			os.Exit(1)
		}
		fmt.Println("held")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "keploy-ca-runs-test-")
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
		releaseRunLock()
		caRunsDir = old
	})
}

// installCtx bounds an install row's install and import: a store-lock wait
// that does not end (a lock kept, or a held one read as taken) then fails the
// row at its own bound, by name, instead of holding the package up until go
// test's timeout.
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
func TestRunLockHeldUntilReleased(t *testing.T) {
	useRunsDir(t)
	if runLive("0a1b2c") {
		t.Fatal("a run that never took a lock is live")
	}
	holdRunLock(context.Background(), zap.NewNop(), "0a1b2c")
	if !runLive("0a1b2c") {
		t.Fatal("a run holding its lock is not live")
	}
	held := runLock.f
	holdRunLock(context.Background(), zap.NewNop(), "0a1b2c") // the same id again: kept, not given up and taken anew
	if runLock.f != held || !runLive("0a1b2c") {
		t.Fatal("holding the same id twice did not keep the lock it had")
	}
	holdRunLock(context.Background(), zap.NewNop(), "3d4e5f")
	if runLive("0a1b2c") || !runLive("3d4e5f") {
		t.Fatalf("after a hold for another id: first live %v, second live %v; want false, true", runLive("0a1b2c"), runLive("3d4e5f"))
	}
	if _, err := os.Stat(ownLockFile("0a1b2c")); !os.IsNotExist(err) {
		t.Fatalf("the given-up lock's file is still there (stat: %v)", err)
	}
	second := runLock.f
	releaseRunLock()
	if runLive("3d4e5f") {
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
func TestRunLockNames(t *testing.T) {
	useRunsDir(t)
	holdRunLock(context.Background(), zap.NewNop(), "ABCdef")
	if !runLive("abcdef") {
		t.Fatal("the lower-case form of a held id is not live")
	}
	if runLive("abc") {
		t.Fatal("a prefix of a held id is live")
	}
	holdRunLock(context.Background(), zap.NewNop(), "abc.def")
	if runLive("abc") || runLive("abc.def") {
		t.Fatalf("an id with a dot took a lock: %q live %v, %q live %v", "abc", runLive("abc"), "abc.def", runLive("abc.def"))
	}
	escaped := filepath.Join(filepath.Dir(caRunsDir), "escaped."+strconv.Itoa(os.Getpid())+".lock")
	t.Cleanup(func() { _ = os.Remove(escaped) })
	holdRunLock(context.Background(), zap.NewNop(), "../escaped")
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Fatalf("an id with a separator put a lock file outside the directory (stat: %v)", err)
	}
	if err := os.WriteFile(filepath.Join(caRunsDir, "feed01.1.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if runLive("feed01") {
		t.Fatal("a run whose lock file nobody holds is live")
	}
}

// With no lock directory no run holds a lock: no run on this host has made one.
func TestRunLockWithoutTheDirectory(t *testing.T) {
	useRunsDir(t)
	caRunsDir = filepath.Join(caRunsDir, "never-made")
	if runLive("0a1b2c") {
		t.Fatal("a run is live although there is no lock directory")
	}
}

// A run that can take no lock says so, once per process.
func TestRunLockWarnsOnceWhenItCannotLock(t *testing.T) {
	useRunsDir(t)
	runLockWarned.Store(false)
	t.Cleanup(func() { runLockWarned.Store(false) })
	core, logs := observer.New(zapcore.WarnLevel)
	holdRunLock(context.Background(), zap.New(core), "abc.def")
	holdRunLock(context.Background(), zap.New(core), "ghi.jkl")
	if n := logs.Len(); n != 1 {
		t.Fatalf("%d warnings for two runs' locks that could not be taken, want one", n)
	}
	if runLock.f != nil {
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
// runLockWait the run goes on without it, saying so; when the run's context
// has ended it does not wait, and says nothing (the run is stopping).
func TestRunLockWaitsForAFileHeldByAnother(t *testing.T) {
	useRunsDir(t)
	runLockWarned.Store(false)
	oldWait := runLockWait
	t.Cleanup(func() { runLockWarned.Store(false); runLockWait = oldWait })
	const warn = "keploy could not take this run's lock (its lock file stayed held by another process, or could not be locked)"

	// Each row's bound: generous where the run must take its lock (a slow
	// runner only makes it wait longer), and short where it must give up.
	// Where the run must not wait at all, the bound is 30 s, so a wait there
	// ends with the bound's error, which fails the row.
	t.Run("the holder lets go", func(t *testing.T) {
		other := lockOwnFileElsewhere(t, "0a1b2c")
		runLockWait = 30 * time.Second
		unlocked := make(chan struct{})
		time.AfterFunc(20*time.Millisecond, func() { unlockFile(other); close(unlocked) })
		core, logs := observer.New(zapcore.WarnLevel)
		holdRunLock(context.Background(), zap.New(core), "0a1b2c")
		<-unlocked // other is closed at the subtest's end, after this
		if runLock.f == nil || logs.Len() != 0 {
			t.Fatalf("the run did not take its lock once the holder let go (held %v, %d warnings)", runLock.f != nil, logs.Len())
		}
		releaseRunLock()
	})

	t.Run("the holder keeps it", func(t *testing.T) {
		lockOwnFileElsewhere(t, "0a1b2c")
		runLockWarned.Store(false) // a failure above must not show up here
		runLockWait = 50 * time.Millisecond
		// The row's own bound: a wait with no bound of its own ends when this
		// context does, with no warning, which fails the row.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		core, logs := observer.New(zapcore.WarnLevel)
		start := time.Now()
		holdRunLock(ctx, zap.New(core), "0a1b2c")
		if waited := time.Since(start); waited < runLockWait {
			t.Fatalf("the run gave up after %s, before its bound %s", waited, runLockWait)
		}
		if runLock.f != nil {
			t.Fatal("the run holds a lock another holder keeps")
		}
		if n := logs.FilterMessageSnippet(warn).Len(); n != 1 || logs.Len() != 1 {
			t.Fatalf("warnings %v, want the one saying the file stayed held", logs.All())
		}
	})

	t.Run("the run's context has ended", func(t *testing.T) {
		lockOwnFileElsewhere(t, "0a1b2c")
		runLockWarned.Store(false)
		runLockWait = 30 * time.Second
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
		holdRunLock(ctx, zap.New(core), "0a1b2c")
		if runLock.f != nil || logs.Len() != 0 {
			t.Fatalf("a stopping run: held %v, warnings %v; want neither", runLock.f != nil, logs.All())
		}
	})
}

// A try that fails for another reason than the lock being held ends the wait
// at once, with that error (here, a closed file). The run's bound is an hour
// and the row's context ends after 30 s: a wait that went on ends with the
// context's error instead, which fails the row.
func TestRunLockWaitEndsOnALockError(t *testing.T) {
	useRunsDir(t)
	oldWait := runLockWait
	runLockWait = time.Hour
	t.Cleanup(func() { runLockWait = oldWait })
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
