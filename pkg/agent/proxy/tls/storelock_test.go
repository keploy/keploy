package tls

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
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
	defer func() { _ = f.Close() }()
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
	defer func() { _ = f.Close() }()
	locked, err := tryLockFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if locked {
		unlockFile(f)
	}
	return !locked
}

// setStoreLockTimes sets, for the rest of the test, how long a change waits
// for the store lock at most (storeLockWait) and how often it says it waits
// (storeLockSay).
func setStoreLockTimes(t *testing.T, wait, say time.Duration) {
	t.Helper()
	oldWait, oldSay := storeLockWait, storeLockSay
	storeLockWait, storeLockSay = wait, say
	t.Cleanup(func() { storeLockWait, storeLockSay = oldWait, oldSay })
}

// recordStoreLockSteps records, for the rest of the test, the steps of the
// agent's start that a wait for the store lock takes (storeLockStep), and
// returns how to read them.
func recordStoreLockSteps(t *testing.T) (steps func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	old := storeLockStep
	storeLockStep = func(name string) { mu.Lock(); got = append(got, name); mu.Unlock() }
	t.Cleanup(func() { storeLockStep = old })
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
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

// changeInBackground runs withStoreLock(ctx, logger, change) on a goroutine
// and returns the channel its error comes on. The test's end cancels ctx and
// waits for it: a row that fails leaves no wait behind that still reads the
// lock's times, try or step when the rows after it set their own.
func changeInBackground(ctx context.Context, t *testing.T, cancel context.CancelFunc, logger *zap.Logger, change func() error) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); done <- withStoreLock(ctx, logger, change) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	return done
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := withStoreLock(ctx, zap.NewNop(), func() error { return io.EOF }); !errors.Is(err, io.EOF) {
		t.Fatalf("withStoreLock returned %v, not the change's own error", err)
	}
}

// While another holder keeps the store lock, a change waits, says so at Info
// and as a step of the agent's start, and runs once the holder lets go. The
// line is said again only every storeLockSay (here an hour), and the bound
// (an hour too) does not end the wait.
func TestStoreLockWaitsForAnotherHolder(t *testing.T) {
	useRunsDir(t)
	setStoreLockTimes(t, time.Hour, time.Hour)
	steps := recordStoreLockSteps(t)
	unlock := holdStoreLockElsewhere(t)
	// The holder lets go only after three tries: the Info must stay one.
	var tries atomic.Int32
	old := storeLockTry
	storeLockTry = func(f *os.File) (bool, error) { tries.Add(1); return old(f) }
	t.Cleanup(func() { storeLockTry = old })
	core, logs := observer.New(zapcore.InfoLevel)
	ran := make(chan bool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := changeInBackground(ctx, t, cancel, zap.New(core), func() error { ran <- true; return nil })
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
	if got := steps(); len(got) != 1 {
		t.Fatalf("steps of the start %q, want the one that it waited", got)
	}
}

// While it waits, a change says so again every storeLockSay (here at every
// try), each line naming the lock's file and each one a step of the agent's
// start too, which names the file: a start that waits for the lock is not one
// that has stopped.
func TestStoreLockWaitSaysSoAgain(t *testing.T) {
	useRunsDir(t)
	setStoreLockTimes(t, time.Hour, 0)
	steps := recordStoreLockSteps(t)
	unlock := holdStoreLockElsewhere(t)
	core, logs := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := changeInBackground(ctx, t, cancel, zap.New(core), func() error { return nil })
	for deadline := time.Now().Add(30 * time.Second); logs.FilterMessageSnippet(storeLockWaitMsg).Len() < 3; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d lines that it waits in 30 s, want 3: %v", logs.FilterMessageSnippet(storeLockWaitMsg).Len(), logs.All())
		}
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("the change ended with %v once the holder let go", err)
	}
	path := filepath.Join(caRunsDir, storeLockName)
	lines := logs.FilterMessageSnippet(storeLockWaitMsg).All()
	for _, l := range lines {
		if l.ContextMap()["lock"] != path {
			t.Fatalf("a line that it waits names %v, not %s", l.ContextMap()["lock"], path)
		}
	}
	got := steps()
	if len(got) != len(lines) {
		t.Fatalf("%d steps of the start for %d lines that it waits: %q", len(got), len(lines), got)
	}
	for _, s := range got {
		if !strings.Contains(s, path) {
			t.Fatalf("step %q does not name %s", s, path)
		}
	}
}

// storeLockSayDefault and storeLockPollDefault are storeLockSay and
// storeLockPoll as the package sets them, read before any row sets its own
// (setStoreLockTimes).
var (
	storeLockSayDefault  = storeLockSay
	storeLockPollDefault = storeLockPoll
)

// A wait for the store lock tries it every storeLockPoll, and says it waits,
// taking a step of the agent's start, every storeLockSay, at the first try
// after it: a gap of up to storeLockSay+storeLockPoll between steps. The CLI
// gives up a start at TeardownStallChecks beats (of BootBeat each) without a
// step. The agent counts them from the step itself, but a step does not move
// the beats: a beat due just before a step can be counted just after it, so
// a gap of G beats can count G+1 of them. A gap of at most
// TeardownStallChecks-2 beats stays under the count, with a beat to spare (on
// Windows, where the CLI sees no busy work, the steps are all it sees); past
// the count the CLI gives up a start that waits for the lock, as one that has
// stopped, before the wait's own bound. And the tries are spaced, at least
// 1 ms apart (closer is a busy loop of flock calls), and so are the lines,
// at most one a second: each line is an Info and a step, which makes the
// agent rewrite its progress report, and at a line every try, every 10 ms, a
// 2-minute wait writes 12,000 of each.
func TestStoreLockSaysWithinTheCLIsBound(t *testing.T) {
	bound := (utils.TeardownStallChecks - 2) * utils.BootBeat
	if storeLockPollDefault < time.Millisecond || storeLockSayDefault < time.Second || storeLockSayDefault+storeLockPollDefault > bound {
		t.Fatalf("storeLockSay is %s, storeLockPoll %s; want a poll of 1 ms at least, a line at most once a second, and the two together at most %s, under the CLI's bound for a start that takes no step", storeLockSayDefault, storeLockPollDefault, bound)
	}
}

// After one CA install's wait for the store lock ends at storeLockWait, its
// changes try the lock once (withStoreLockBudget): with a holder that never
// lets go, its first change waits out the bound, and the one after it, under
// the same budget (also one given again, which keeps the budget it has),
// fails at once, naming the install. It still tries the lock: once the
// holder lets go, a change under the spent budget runs, and that gives the
// budget back: a holder after it is waited for the whole bound again.
func TestStoreLockTriesOnceAfterTheInstallsBound(t *testing.T) {
	useRunsDir(t)
	setStoreLockTimes(t, time.Second, time.Hour)
	recordStoreLockSteps(t)
	unlock := holdStoreLockElsewhere(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	install := withStoreLockBudget(ctx)
	ran := false
	change := func() error { ran = true; return nil }

	start := time.Now()
	if err := withStoreLock(install, zap.NewNop(), change); !errors.Is(err, ErrStoreLockHeld) || time.Since(start) < time.Second {
		t.Fatalf("the install's first change ended with %v after %s, want ErrStoreLockHeld after its 1 s", err, time.Since(start))
	}
	start = time.Now()
	err := withStoreLock(withStoreLockBudget(install), zap.NewNop(), change)
	if !errors.Is(err, ErrStoreLockHeld) || time.Since(start) >= 500*time.Millisecond {
		t.Fatalf("the install's next change ended with %v after %s, want ErrStoreLockHeld at once", err, time.Since(start))
	}
	if !strings.Contains(err.Error(), "in one CA install") {
		t.Fatalf("the error %q does not say the bound is the install's", err)
	}
	if ran {
		t.Fatal("a change ran while another holder kept the store lock")
	}
	unlock()
	if err := withStoreLock(install, zap.NewNop(), change); err != nil || !ran {
		t.Fatalf("with the holder gone, a change under the spent budget ended with %v (ran %v), want it run", err, ran)
	}
	holdStoreLockElsewhere(t)
	start = time.Now()
	if err := withStoreLock(install, zap.NewNop(), change); !errors.Is(err, ErrStoreLockHeld) || time.Since(start) < time.Second {
		t.Fatalf("after a change that got the lock, a change ended with %v after %s, want ErrStoreLockHeld after a whole 1 s", err, time.Since(start))
	}
}

// Waits that get the store lock spend none of the install's budget: runs
// that take turns on the lock, each for less than the bound, can make one
// install's changes wait longer than the bound in all, and each of its
// changes still runs. Here two changes each wait 2 s of a 3 s bound for a
// holder that then lets go.
func TestStoreLockTurnsSpendNoBudget(t *testing.T) {
	useRunsDir(t)
	setStoreLockTimes(t, 3*time.Second, time.Hour)
	recordStoreLockSteps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	install := withStoreLockBudget(ctx)
	for i := 1; i <= 2; i++ {
		unlock := holdStoreLockElsewhere(t)
		ran := make(chan struct{}, 1)
		done := changeInBackground(install, t, cancel, zap.NewNop(), func() error { ran <- struct{}{}; return nil })
		time.Sleep(2 * time.Second)
		unlock()
		if err := <-done; err != nil || len(ran) != 1 {
			t.Fatalf("change %d, which waited 2 s for a holder that then let go, ended with %v (ran %v), want it run", i, err, len(ran) == 1)
		}
	}
}

// awaitBootProgress waits, until ctx ends, for the report of the agent's
// start's progress at path to be one that want takes, and returns it.
func awaitBootProgress(ctx context.Context, t *testing.T, path string, want func(utils.BootProgress) bool) utils.BootProgress {
	t.Helper()
	var last utils.BootProgress
	for {
		if b, err := os.ReadFile(path); err == nil {
			if p, err := utils.ParseBootProgress(b); err == nil {
				if want(p) {
					return p
				}
				last = p
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no such report of the start's progress in %s by the row's bound; the last: %+v", path, last)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A change that waits for the store lock takes a step of the agent's start
// through the real storeLockStep, utils.BootStep: the start's progress report,
// which the CLI that waits for the start reads (utils.ReportBootProgress),
// counts a step that names the lock's file while the change waits, so the CLI
// does not take the wait for a start that has stopped. The row's bound is
// 30 s: a wait that takes no such step fails it there.
func TestStoreLockWaitIsAStepOfTheStart(t *testing.T) {
	useRunsDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report := filepath.Join(t.TempDir(), utils.BootProgressFileName)
	reportCtx, stopReport := context.WithCancel(ctx)
	exited := utils.ReportBootProgress(reportCtx, zap.NewNop(), report)
	t.Cleanup(func() { stopReport(); <-exited })
	first := awaitBootProgress(ctx, t, report, func(utils.BootProgress) bool { return true })
	if first.Ready || first.Failed {
		t.Fatalf("the start's progress report says the start is over (%+v): the reporter writes no more steps", first)
	}
	unlock := holdStoreLockElsewhere(t)
	done := changeInBackground(ctx, t, cancel, zap.NewNop(), func() error { return nil })
	path := filepath.Join(caRunsDir, storeLockName)
	awaitBootProgress(ctx, t, report, func(p utils.BootProgress) bool {
		return p.Seq > first.Seq && strings.Contains(p.Step, path)
	})
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("the change ended with %v once the holder let go", err)
	}
}

// A CA install that failed because the store lock stayed held says what to
// do: the error names the lock's holder, and the next step names the lock's
// file and says to resume the holder, as root, if it is stopped, or end it.
func TestStoreLockHeldNextStep(t *testing.T) {
	useRunsDir(t)
	path := filepath.Join(caRunsDir, storeLockName)
	step := StoreLockHeldNextStep()
	for _, w := range []string{"trust-store lock " + path + " ", "the error names it and its state", "If it is stopped (its state is T)", "as root (`kill -CONT PID`)", "end it (`kill PID`)", "how to find it on the host, or why it cannot", "restart the agent"} {
		if !strings.Contains(step, w) {
			t.Errorf("the next step %q does not say %q", step, w)
		}
	}
}

// A holder that never lets go: the change waits until storeLockWait has
// passed (here 50 ms; its context's 30 s have not), does not run, and fails
// with an error that names the lock's file and who holds it: on Linux the
// holding process (here this one, through another handle), elsewhere another
// keploy run or a process that opened it. It warns of nothing: it did not go
// on without the lock.
func TestStoreLockHeldPastTheBound(t *testing.T) {
	useRunsDir(t)
	storeLockWarned.Store(false)
	t.Cleanup(func() { storeLockWarned.Store(false) })
	setStoreLockTimes(t, 50*time.Millisecond, time.Hour)
	holdStoreLockElsewhere(t)
	core, logs := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ran := false
	err := withStoreLock(ctx, zap.New(core), func() error { ran = true; return nil })
	if ran || !errors.Is(err, ErrStoreLockHeld) || ctx.Err() != nil {
		t.Fatalf("ran %v, error %v (context %v); want no change and the bound's error before the context ended", ran, err, ctx.Err())
	}
	path := filepath.Join(caRunsDir, storeLockName)
	holder := storeLockHolderUnknown
	if runtime.GOOS == "linux" {
		holder = "by process " + strconv.Itoa(os.Getpid()) + " ("
	}
	if msg := err.Error(); !strings.Contains(msg, path) || !strings.Contains(msg, holder) {
		t.Fatalf("the error %q does not name %s and who holds it (%q)", msg, path, holder)
	}
	if n := logs.FilterMessageSnippet(storeLockWarnMsg).Len(); n != 0 {
		t.Fatalf("it warned that it went on without the lock: %v", logs.All())
	}
	if n := logs.FilterMessageSnippet(storeLockWaitMsg).Len(); n != 1 {
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
	defer cancel()
	ran := false
	done := changeInBackground(ctx, t, cancel, zap.New(core), func() error { ran = true; return nil })
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
