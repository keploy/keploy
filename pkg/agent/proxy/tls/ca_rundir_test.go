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

	"go.keploy.io/server/v3/pkg/kubeconfig"
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

// endedDirsOf lists the directories run directory dir was renamed to when
// its run ended.
func endedDirsOf(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	var ended []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(dir)+runEndedMark) {
			ended = append(ended, filepath.Join(filepath.Dir(dir), e.Name()))
		}
	}
	return ended
}

// liveRunOf returns this process's run of directory dir, which must be live.
func liveRunOf(t *testing.T, dir string) *runTrustDir {
	t.Helper()
	var run *runTrustDir
	liveRunDirs.Range(func(k, _ any) bool {
		if r := k.(*runTrustDir); r.dir == dir {
			run = r
		}
		return run == nil
	})
	if run == nil {
		t.Fatalf("no live run holds %s", dir)
	}
	return run
}

// waitRunGone waits for run's removal to finish, and checks that it left
// nothing. It waits on liveRunDirs, not on the files: the removal forgets the
// run when it is done, and a Load that sees that orders the whole removal
// before what the test does next. A removal seen done only through the file
// system may still race a seam the next test sets.
func waitRunGone(t *testing.T, run *runTrustDir) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, live := liveRunDirs.Load(run); !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run of %s was not removed", run.dir)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Lstat(run.dir); !os.IsNotExist(err) {
		t.Fatalf("the removed run's directory %s is still there (%v)", run.dir, err)
	}
	if ended := endedDirsOf(t, run.dir); len(ended) != 0 {
		t.Fatalf("the removed run's directory is still there, renamed: %q", ended)
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
	run := liveRunOf(t, dirs[0])
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
	waitRunGone(t, run)
	if left := runDirsIn(t, tmp); len(left) != 0 {
		t.Fatalf("the run left %q behind", left)
	}
}

// A directory whose lock no process holds is an ended run's, and the next
// run removes it; one whose lock is held, the next run leaves alone, as it
// does a directory with no lock file (a run between making it and locking)
// and, under its run's name or the ended one, a directory with another user's
// name or that another user owns.
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
	// The same under the ended name a removal cut short leaves.
	otherEnded := other + runEndedMark + "x"
	foreignEnded := filepath.Join(tmp, runDirPattern()+"y"+runEndedMark+"z")
	for _, d := range []string{otherEnded, foreignEnded} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	orig := ownedByThisUserFn
	ownedByThisUserFn = func(dir string) bool { return dir != foreign.dir && dir != foreignEnded && orig(dir) }
	t.Cleanup(func() { ownedByThisUserFn = orig })

	reapEndedRunDirs(zap.NewNop())

	if _, err := os.Lstat(dead.dir); !os.IsNotExist(err) {
		t.Fatalf("the ended run's directory is still there (%v)", err)
	}
	if ended := endedDirsOf(t, dead.dir); len(ended) != 0 {
		t.Fatalf("the ended run's directory is still there, renamed: %q", ended)
	}
	if _, err := os.Stat(filepath.Join(live.dir, runCAFile)); err != nil {
		t.Fatalf("the live run's CA file was touched: %v", err)
	}
	for _, d := range []string{unlocked, other, foreign.dir, otherEnded, foreignEnded} {
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

// errCutShort is what a stubbed removal returns where it stops.
var errCutShort = errors.New("the removal was cut short")

// cutRemovalShort makes the deletion of an ended run's directory delete only
// gone, in that order, and stop there, as a process that dies, or meets a file
// it cannot delete, stops partway. restore puts the real deletion back.
func cutRemovalShort(t *testing.T, gone []string) (restore func()) {
	t.Helper()
	orig := removeAllFn
	removeAllFn = func(dir string) error {
		for _, name := range gone {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Errorf("control: the cut-short removal could not delete %s: %v", name, err)
			}
		}
		return errCutShort
	}
	restore = func() { removeAllFn = orig }
	t.Cleanup(restore)
	return restore
}

// A removal cut short, the run's own or the reaper's, leaves nothing the next
// run misses: under the run's own name its directory is gone or still holds
// its lock, so it is never taken for a run starting, and the next run removes
// whatever is left, the bundle included.
func TestReapEndedRunDirs_TakesWhatACutShortRemovalLeft(t *testing.T) {
	for _, tc := range []struct {
		name string
		gone []string // what the removal deleted before it stopped
	}{
		{"before it deleted anything", nil},
		{"after the lock", []string{runLockName}},
		{"after the lock and the bundle", []string{runLockName, runBundleFile}},
		{"after every file", []string{runBundleFile, runCAFile, runLockName}},
	} {
		for _, by := range []string{"the run", "the reaper"} {
			t.Run(by+" stopped "+tc.name, func(t *testing.T) {
				tmp := useTempDir(t)
				r, err := newRunTrustDir()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.remove() })
				for _, name := range []string{runCAFile, runBundleFile} {
					if _, err := r.write(name, testCACertPEM()); err != nil {
						t.Fatal(err)
					}
				}
				restore := cutRemovalShort(t, tc.gone)
				if by == "the run" {
					if err := r.remove(); !errors.Is(err, errCutShort) {
						t.Fatalf("control: the run's removal was not cut short (%v)", err)
					}
				} else {
					_ = r.lock.Close() // what the system does when its process dies
					reapEndedRunDirs(zap.NewNop())
				}
				if len(runDirsIn(t, tmp)) == 0 {
					t.Fatal("control: the removal was not cut short: nothing is left")
				}
				if _, err := os.Lstat(r.dir); err == nil {
					if _, err := os.Lstat(filepath.Join(r.dir, runLockName)); err != nil {
						t.Fatalf("the run's own name holds its directory without its lock, as a run starting would (%v)", err)
					}
				}
				restore()
				reapEndedRunDirs(zap.NewNop())
				if left := runDirsIn(t, tmp); len(left) != 0 {
					t.Fatalf("the next run left %q", left)
				}
			})
		}
	}
}

// A run lets its lock go before it renames its directory, so another run's
// reaper can take the directory first. The run's removal then has nothing
// left to do, and is no error.
func TestReapEndedRunDirs_ARunsRemovalAfterAReaperTookItIsNoError(t *testing.T) {
	tmp := useTempDir(t)
	r, err := newRunTrustDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.remove() })
	_ = r.lock.Close() // the run has let its lock go
	reapEndedRunDirs(zap.NewNop())
	if left := runDirsIn(t, tmp); len(left) != 0 {
		t.Fatalf("control: the reaper left %q", left)
	}
	if err := r.remove(); err != nil {
		t.Fatalf("the run's removal after a reaper took its directory: %v", err)
	}
}

// envRunDirChild makes the test binary, in the child process of
// TestReapEndedRunDirs_AKilledRunsDirectoryGoesAtTheNextStart, a native run
// that sets up its trust files, says where, and waits to be killed.
const envRunDirChild = "KEPLOY_TEST_RUNDIR_CHILD"

// A run killed outright (SIGKILL on Unix) leaves its directory, which a run
// started meanwhile leaves alone while it lives and the next one removes,
// once it has written back what the killed run's test changed in its
// kubeconfig.
func TestReapEndedRunDirs_AKilledRunsDirectoryGoesAtTheNextStart(t *testing.T) {
	if os.Getenv(envRunDirChild) != "" {
		if err := SetupCaCertEnv(context.Background(), zap.NewNop(), testCACertPEM()); err != nil {
			println("SetupCaCertEnv:", err.Error())
			os.Exit(2)
		}
		if _, err := os.Stdout.WriteString("kubeconfig=" + os.Getenv(kubeconfig.EnvVar) + "\ndir=" + filepath.Dir(os.Getenv("NODE_EXTRA_CA_CERTS")) + "\n"); err != nil {
			println("write the run's directory:", err.Error())
			os.Exit(4)
		}
		time.Sleep(2 * time.Minute)
		os.Exit(3)
	}
	isolateCertEnv(t)
	tmp := useTempDir(t)
	withSystemSearch(t, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReapEndedRunDirs_AKilledRunsDirectoryGoesAtTheNextStart$", "-test.count=1")
	user := filepath.Join(t.TempDir(), "config")
	src := "apiVersion: v1\nkind: Config\ncontexts:\n- name: a\n  context:\n    cluster: a\n"
	if err := os.WriteFile(user, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), envRunDirChild+"=1", EnvJavaToolOptions+"=", kubeconfig.EnvVar+"="+user)
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
	var dir, cp string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if c, ok := strings.CutPrefix(sc.Text(), "kubeconfig="); ok {
			cp = c
		}
		if d, ok := strings.CutPrefix(sc.Text(), "dir="); ok {
			dir = d
			break
		}
	}
	if dir == "" || filepath.Dir(dir) != tmp || filepath.Dir(cp) != dir {
		_ = killProcessGroup(cmd)
		t.Fatalf("the child said no run directory in %q, or no copy of the kubeconfig in it (got %q, %q; %v)", tmp, dir, cp, sc.Err())
	}
	// The killed run's test sets its context's namespace (kubectl config
	// set-context --current --namespace=web).
	if err := os.WriteFile(cp, []byte(src+"    namespace: web\n"), 0o600); err != nil {
		_ = killProcessGroup(cmd)
		t.Fatal(err)
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
	t.Cleanup(cancelNext)
	t.Setenv(EnvJavaToolOptions, "")
	if err := SetupCaCertEnv(next, zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	// The next run ends with the test, its directory gone before the temp
	// directory's cleanup.
	nextDir := filepath.Dir(os.Getenv("NODE_EXTRA_CA_CERTS"))
	nextRun := liveRunOf(t, nextDir)
	t.Cleanup(func() {
		cancelNext()
		waitRunGone(t, nextRun)
	})
	if left := runDirsIn(t, tmp); len(left) != 1 || left[0] != nextDir {
		t.Fatalf("after the next run's start the temp directory holds %q; want only its own, %q", left, nextDir)
	}
	if got, err := os.ReadFile(user); err != nil || !strings.Contains(string(got), "namespace: web") {
		t.Fatalf("the killed run's test set its context's namespace; the kubeconfig after the next run's start holds\n%s\n(%v)", got, err)
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
