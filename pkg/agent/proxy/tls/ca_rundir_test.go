package tls

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// useTempDir points os.TempDir at a directory of the test's own.
func useTempDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, tmp)
	}
	return tmp
}

// runDirsIn lists the run directories in tmp.
func runDirsIn(t *testing.T, tmp string) []string {
	t.Helper()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), runDirPrefix) {
			dirs = append(dirs, filepath.Join(tmp, e.Name()))
		}
	}
	return dirs
}

// waitGone waits for path to be removed.
func waitGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is still there", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A native run's trust files are in a directory of its own, and the run
// leaves nothing of it behind when its context ends.
func TestSetupCaCertEnv_RunLeavesNoFileBehind(t *testing.T) {
	isolateCertEnv(t)
	tmp := useTempDir(t)
	withSystemSearch(t, nil, nil)
	t.Setenv(EnvJavaToolOptions, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := SetupCaCertEnv(ctx, zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	dirs := runDirsIn(t, tmp)
	if len(dirs) != 1 {
		t.Fatalf("the temp directory holds %d run directories; want this run's one: %q", len(dirs), dirs)
	}
	for _, v := range []string{"NODE_EXTRA_CA_CERTS", envSSLCertFile, "REQUESTS_CA_BUNDLE", "CARGO_HTTP_CAINFO"} {
		if p := os.Getenv(v); filepath.Dir(p) != dirs[0] {
			t.Fatalf("%s=%q is not in the run's directory %q", v, p, dirs[0])
		}
		if _, err := os.Stat(os.Getenv(v)); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
	}
	if runtime.GOOS != "windows" {
		// The app may run as another user (under sudo): public certificates only.
		for p, want := range map[string]os.FileMode{dirs[0]: 0o755, os.Getenv(envSSLCertFile): 0o644, os.Getenv("NODE_EXTRA_CA_CERTS"): 0o644} {
			if st, err := os.Stat(p); err != nil || st.Mode().Perm() != want {
				t.Fatalf("%s: mode %v (%v); want %v", p, st.Mode().Perm(), err, want)
			}
		}
	}
	cancel()
	waitGone(t, dirs[0])
	if left := runDirsIn(t, tmp); len(left) != 0 {
		t.Fatalf("the run left %q behind", left)
	}
}

// A directory whose lock no process holds is an ended run's, and the next
// run removes it; one whose lock is held, the next run leaves alone, as it
// does a directory with no lock file (a run between making it and locking).
func TestReapEndedRunDirs_TakesOnlyEndedRuns(t *testing.T) {
	tmp := useTempDir(t)
	live, err := newRunTrustDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.remove() })
	if _, err := live.write(runCAFile, testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	dead, err := newRunTrustDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dead.write(runCAFile, testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	_ = dead.lock.Close() // what the system does when its process dies
	unlocked, err := os.MkdirTemp(tmp, runDirPattern())
	if err != nil {
		t.Fatal(err)
	}
	// Another user's name, its lock free: not this user's to judge.
	other := filepath.Join(tmp, fmt.Sprintf("%s%d-x", runDirPrefix, os.Geteuid()+1))
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, runLockName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// An ended run's directory that another user made under this user's name.
	foreign, err := newRunTrustDir()
	if err != nil {
		t.Fatal(err)
	}
	_ = foreign.lock.Close()
	orig := ownedByThisUserFn
	ownedByThisUserFn = func(dir string) bool { return dir != foreign.dir && orig(dir) }
	t.Cleanup(func() { ownedByThisUserFn = orig })

	reapEndedRunDirs(zap.NewNop())

	if _, err := os.Lstat(dead.dir); !os.IsNotExist(err) {
		t.Fatalf("the ended run's directory is still there (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(live.dir, runCAFile)); err != nil {
		t.Fatalf("the live run's CA file was touched: %v", err)
	}
	for _, d := range []string{unlocked, other, foreign.dir} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s was removed: %v", d, err)
		}
	}
	// The live run still holds its lock: the reaper's look let it go.
	if _, ended := runEnded(live.dir); ended {
		t.Fatal("the live run's lock was free after the reaper looked at it")
	}
	// Removing a run lets its lock go and forgets it.
	if err := live.remove(); err != nil {
		t.Fatal(err)
	}
	if err := live.lock.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("the removed run's lock file is still open (%v)", err)
	}
	if _, held := liveRunDirs.Load(live); held {
		t.Fatal("the removed run is still among the live ones")
	}
}

// envRunDirChild makes the test binary, in the child process of
// TestReapEndedRunDirs_AKilledRunsDirectoryGoesAtTheNextStart, a native run
// that sets up its trust files, says where, and waits to be killed.
const envRunDirChild = "KEPLOY_TEST_RUNDIR_CHILD"

// A run killed outright (SIGKILL on Unix) leaves its directory, which a run
// started meanwhile leaves alone while it lives and the next one removes.
func TestReapEndedRunDirs_AKilledRunsDirectoryGoesAtTheNextStart(t *testing.T) {
	if os.Getenv(envRunDirChild) != "" {
		if err := SetupCaCertEnv(context.Background(), zap.NewNop(), testCACertPEM()); err != nil {
			println("SetupCaCertEnv:", err.Error())
			os.Exit(2)
		}
		os.Stdout.WriteString("dir=" + filepath.Dir(os.Getenv("NODE_EXTRA_CA_CERTS")) + "\n")
		time.Sleep(2 * time.Minute)
		os.Exit(3)
	}
	isolateCertEnv(t)
	tmp := useTempDir(t)
	withSystemSearch(t, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReapEndedRunDirs_AKilledRunsDirectoryGoesAtTheNextStart$", "-test.count=1")
	cmd.Env = append(os.Environ(), envRunDirChild+"=1", EnvJavaToolOptions+"=")
	setOwnProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 10 * time.Second
	t.Cleanup(func() { _ = killProcessGroup(cmd) })
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var dir string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "dir="); ok {
			dir = d
			break
		}
	}
	if dir == "" || filepath.Dir(dir) != tmp {
		_ = killProcessGroup(cmd)
		t.Fatalf("the child said no run directory in %q (got %q; %v)", tmp, dir, sc.Err())
	}

	reapEndedRunDirs(zap.NewNop())
	if _, err := os.Stat(filepath.Join(dir, runBundleFile)); err != nil {
		t.Fatalf("a live run's bundle was touched: %v", err)
	}

	if err := killProcessGroup(cmd); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("control: the killed run removed its directory itself (%v)", err)
	}
	// The next run removes it.
	next, cancelNext := context.WithCancel(context.Background())
	defer cancelNext()
	t.Setenv(EnvJavaToolOptions, "")
	if err := SetupCaCertEnv(next, zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("the killed run's directory is still there after the next run's start (%v)", err)
	}
}

// A run whose context never ends keeps its lock: the garbage collector does
// not close a lock file the run no longer refers to.
func TestSetupCaCertEnv_BackgroundRunKeepsItsLockThroughCollections(t *testing.T) {
	isolateCertEnv(t)
	tmp := useTempDir(t)
	withSystemSearch(t, nil, nil)
	t.Setenv(EnvJavaToolOptions, "")
	if err := SetupCaCertEnv(context.Background(), zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	dirs := runDirsIn(t, tmp)
	if len(dirs) != 1 {
		t.Fatalf("want one run directory, got %q", dirs)
	}
	t.Cleanup(func() {
		liveRunDirs.Range(func(k, _ any) bool {
			if r := k.(*runTrustDir); r.dir == dirs[0] {
				_ = r.remove()
			}
			return true
		})
	})
	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond) // finalizers run on their own goroutine
	}
	if _, ended := runEnded(dirs[0]); ended {
		t.Fatal("the live run's lock was let go by a garbage collection")
	}
}
