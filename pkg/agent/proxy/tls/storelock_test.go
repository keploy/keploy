package tls

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// probeStoreLock prints whether dir's store lock is held: "held" when another
// handle has it locked, "free" when this one could lock it (and lets it go),
// "nofile" or "error" otherwise. The store-lock rows' fake commands run it.
func probeStoreLock(dir string) {
	f, err := os.Open(filepath.Join(dir, storeLockName))
	if err != nil {
		fmt.Println("nofile")
		return
	}
	defer f.Close()
	locked, err := tryLockFile(f)
	switch {
	case err != nil:
		fmt.Println("error")
	case locked:
		unlockFile(f)
		fmt.Println("free")
	default:
		fmt.Println("held")
	}
}

// importAsAnotherRun is another keploy run that imports its CA into the
// keystore KEPLOY_TEST_CACERTS names (with the keytool KEPLOY_TEST_KEYTOOL
// names) under keploy-mitm-<id>, holding its run lock for id in
// KEPLOY_TEST_RUN_LOCK_DIR, as a run does. It says "ready" once it holds the
// lock, imports once a line comes on stdin, says "imported <error>", and keeps
// its run lock until its stdin closes: a sweep of the other run must see it
// live.
func importAsAnotherRun(id string) {
	caRunsDir = os.Getenv("KEPLOY_TEST_RUN_LOCK_DIR")
	holdRunLock(context.Background(), zap.NewNop(), id)
	if runLock.f == nil {
		fmt.Println("no lock")
		return
	}
	fmt.Println("ready")
	in := bufio.NewReader(os.Stdin)
	if _, err := in.ReadString('\n'); err != nil {
		return
	}
	ks := javaKeystore{keytool: os.Getenv("KEPLOY_TEST_KEYTOOL"), cacerts: os.Getenv("KEPLOY_TEST_CACERTS")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := importJavaCA(ctx, zap.NewNop(), ks, os.Getenv("KEPLOY_TEST_CA_FILE"), "keploy-mitm-"+id)
	fmt.Printf("imported %v\n", err)
	_, _ = io.Copy(io.Discard, in)
}

// holdStoreLockElsewhere opens caRunsDir's store lock through a handle of its
// own and locks it, as another run would (flock and LockFileEx locks are per
// open file). unlock lets it go; the test's end closes the handle.
func holdStoreLockElsewhere(t *testing.T) (unlock func()) {
	t.Helper()
	if err := os.MkdirAll(caRunsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(caRunsDir, storeLockName), os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if locked, err := tryLockFile(f); err != nil || !locked {
		t.Fatalf("could not take the store lock first: %v %v", locked, err)
	}
	return func() { unlockFile(f) }
}

// storeLockHeldHere reports whether the store lock is held by a handle other
// than a new one of this process's: it is while a change runs.
func storeLockHeldHere(t *testing.T) bool {
	t.Helper()
	f, err := os.Open(filepath.Join(caRunsDir, storeLockName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	locked, err := tryLockFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if locked {
		unlockFile(f)
	}
	return !locked
}

const storeLockWaitMsg = "waiting for another keploy run on this host to finish changing a trust store"
const storeLockWarnMsg = "keploy could not take the trust stores' lock"

// waitForLog waits, up to the test's own bound, for logs to hold msg.
func waitForLog(t *testing.T, logs *observer.ObservedLogs, msg string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for logs.FilterMessageSnippet(msg).Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in the logs: %v", msg, logs.All())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A change holds the store lock while it runs and gives it up after: a second
// change then runs as well. Each change's bound is 30 s; a lock that was kept
// makes the second one end with that bound's error.
func TestStoreLockHeldWhileTheChangeRuns(t *testing.T) {
	useRunsDir(t)
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		held := false
		err := withStoreLock(ctx, zap.NewNop(), func() error { held = storeLockHeldHere(t); return nil })
		cancel()
		if err != nil || !held {
			t.Fatalf("change %d: error %v, store lock held while it ran %v; want nil, true", i+1, err, held)
		}
	}
	if err := withStoreLock(context.Background(), zap.NewNop(), func() error { return io.EOF }); !errors.Is(err, io.EOF) {
		t.Fatalf("withStoreLock returned %v, not the change's own error", err)
	}
}

// While another holder keeps the store lock, a change waits, says so once at
// Info, and runs once the holder lets go.
func TestStoreLockWaitsForAnotherHolder(t *testing.T) {
	useRunsDir(t)
	unlock := holdStoreLockElsewhere(t)
	// The holder lets go only after three tries: the Info must stay one.
	var tries atomic.Int32
	old := storeLockTry
	storeLockTry = func(f *os.File) (bool, error) { tries.Add(1); return old(f) }
	t.Cleanup(func() { storeLockTry = old })
	core, logs := observer.New(zapcore.InfoLevel)
	ran := make(chan bool, 1)
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		done <- withStoreLock(ctx, zap.New(core), func() error { ran <- true; return nil })
	}()
	waitForLog(t, logs, storeLockWaitMsg)
	for deadline := time.Now().Add(30 * time.Second); tries.Load() < 3; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d tries of the store lock in 30 s", tries.Load())
		}
	}
	select {
	case <-ran:
		t.Fatal("the change ran while another holder kept the store lock")
	default:
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("the change ended with %v once the holder let go", err)
	}
	if len(ran) != 1 {
		t.Fatal("the change did not run once the holder let go")
	}
	if n := logs.FilterMessageSnippet(storeLockWaitMsg).Len(); n != 1 || logs.Len() != 1 {
		t.Fatalf("logs %v, want the one Info that it waited", logs.All())
	}
}

// A change still waiting for the store lock when its context ends does not
// run, ends with the context's error, and warns of nothing: the run is
// stopping.
func TestStoreLockWaitEndsWithTheContext(t *testing.T) {
	useRunsDir(t)
	storeLockWarned.Store(false)
	t.Cleanup(func() { storeLockWarned.Store(false) })
	holdStoreLockElsewhere(t)
	core, logs := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ran := false
	go func() { done <- withStoreLock(ctx, zap.New(core), func() error { ran = true; return nil }) }()
	waitForLog(t, logs, storeLockWaitMsg)
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the wait went on 30 s after its context ended")
	}
	if !errors.Is(err, context.Canceled) || ran {
		t.Fatalf("error %v, ran %v; want context.Canceled and no change", err, ran)
	}
	if n := logs.FilterMessageSnippet(storeLockWarnMsg).Len(); n != 0 {
		t.Fatalf("a stopping run warned: %v", logs.All())
	}
}

// A try that fails for another reason than the lock being held (here, a
// closed file) ends the wait at once, with that error. The bound is 30 s: a
// wait that went on ends with the bound's error instead, which fails the row.
func TestStoreLockWaitEndsOnALockError(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = waitForStoreLock(ctx, zap.NewNop(), f, f.Name())
	if err == nil || ctx.Err() != nil {
		t.Fatalf("waiting on a closed file: %v (context %v), want the lock's own error at once", err, ctx.Err())
	}
}

// When the store lock cannot be taken at all, the change runs without it, as
// every change did before the lock, and the run warns once: its directory
// cannot be made (a file is in the way), its file cannot be opened (a
// directory is in its place), or a try fails for another reason than the
// lock being held.
func TestStoreLockThatCannotBeTaken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"no directory", func(t *testing.T) {
			file := filepath.Join(caRunsDir, "a-file")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			caRunsDir = filepath.Join(file, "runs")
		}},
		{"no file", func(t *testing.T) {
			if err := os.MkdirAll(filepath.Join(caRunsDir, storeLockName), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"a try fails", func(t *testing.T) {
			old := storeLockTry
			storeLockTry = func(*os.File) (bool, error) { return false, errors.New("injected") }
			t.Cleanup(func() { storeLockTry = old })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useRunsDir(t)
			storeLockWarned.Store(false)
			t.Cleanup(func() { storeLockWarned.Store(false) })
			tc.setup(t)
			core, logs := observer.New(zapcore.WarnLevel)
			// A bound, so that a change that waits instead fails the row.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			runs := 0
			for i := 0; i < 2; i++ {
				if err := withStoreLock(ctx, zap.New(core), func() error { runs++; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if runs != 2 {
				t.Fatalf("%d of 2 changes ran without the lock", runs)
			}
			if n := logs.FilterMessageSnippet(storeLockWarnMsg).Len(); n != 1 || logs.Len() != 1 {
				t.Fatalf("warnings %v, want one", logs.All())
			}
		})
	}
}

// The store lock is no run's lock: held, it does not make a run named "store"
// live.
func TestStoreLockIsNoRunsLock(t *testing.T) {
	useRunsDir(t)
	holdStoreLockElsewhere(t)
	if runLive("store") {
		t.Fatal("the held store lock made a run live")
	}
}
