package tls

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// runFilesTempDir points os.TempDir at a directory of the test's own, with
// none of the machine's trust settings, and removes what SetupCaCertEnv wrote
// when the test ends.
func runFilesTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// TMPDIR is os.TempDir's on Unix, TMP and TEMP on Windows.
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, dir)
	}
	// Not a skip, which go test does not show without -v: the Windows job
	// runs these rows and must not pass over them.
	if got := os.TempDir(); filepath.Clean(got) != filepath.Clean(dir) {
		t.Fatalf("os.TempDir is %s here whatever the environment says, not the test's %s (Windows as SYSTEM?)", got, dir)
	}
	// A JAVA_TOOL_OPTIONS naming a truststore of its own would have keploy
	// write none.
	for _, k := range append([]string{EnvJavaToolOptions}, caTrustEnvVars...) {
		t.Setenv(k, "")
	}
	orig := loadSystemCABundleFn
	loadSystemCABundleFn = func(*zap.Logger) ([]byte, string) { return testCACertPEM(), "test" }
	t.Cleanup(func() {
		_ = RemoveCaCertEnvFiles()
		loadSystemCABundleFn = orig
	})
	return dir
}

// runFilesIn lists the run files in dir.
func runFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	var all []string
	for _, pattern := range runFilePatterns {
		m, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, m...)
	}
	slices.Sort(all)
	return all
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// runFilesHelperEnv makes TestRunFilesOtherRun run as the other keploy run of
// TestRunFiles_ASetupTakesOnlyTheFilesOfRunsThatAreGone: the directory it
// says it is ready in.
const runFilesHelperEnv = "KEPLOY_TEST_RUN_FILES_MARKS"

// TestRunFiles_ASetupTakesOnlyTheFilesOfRunsThatAreGone: another keploy run
// on the machine has its app's trust files in the same temp dir. A setup while
// it runs leaves them alone. Killed (SIGKILL on Unix, TerminateProcess on
// Windows), it cannot remove them, and the next setup does; it leaves this
// run's own, which is still live.
func TestRunFiles_ASetupTakesOnlyTheFilesOfRunsThatAreGone(t *testing.T) {
	tmp := runFilesTempDir(t)
	marks := t.TempDir()

	other := exec.Command(os.Args[0], "-test.run=^TestRunFilesOtherRun$")
	other.Env = append(os.Environ(), runFilesHelperEnv+"="+marks)
	out := &bytes.Buffer{}
	other.Stdout, other.Stderr = out, out
	// It runs until this test kills it, or until its stdin ends: when this
	// test's process ends, so it never outlives it. No clock.
	stdin, err := other.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	var waitErr error
	exited := make(chan struct{})
	go func() { waitErr = other.Wait(); close(exited) }()
	t.Cleanup(func() {
		// It starts nothing of its own.
		_ = other.Process.Kill()
		<-exited
	})
	for !exists(t, filepath.Join(marks, "ready")) {
		select {
		case <-exited:
			t.Fatalf("the other run ended before it was ready: %v\n%s", waitErr, out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	theirs := runFilesIn(t, tmp)
	if len(theirs) != 2 {
		t.Fatalf("want the other run's CA file and Java truststore, have %v\n%s", theirs, out.String())
	}

	if err := SetupCaCertEnv(zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatalf("SetupCaCertEnv while the other run is live: %v", err)
	}
	for _, p := range theirs {
		if !exists(t, p) {
			t.Errorf("a setup took %s while the run that wrote it was live", p)
		}
	}
	var mine []string
	for _, p := range runFilesIn(t, tmp) {
		if !slices.Contains(theirs, p) {
			mine = append(mine, p)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("want this run's CA file and Java truststore beside the other run's, have %v", mine)
	}

	if err := other.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-exited

	if err := SetupCaCertEnv(zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatalf("SetupCaCertEnv after the other run was killed: %v", err)
	}
	// Linux and macOS drop a process's locks before its parent can reap it.
	// Windows drops them as it ends, but says the time it takes depends on
	// the system's resources, so there a later setup may be the one that
	// sweeps; it takes them once Windows has.
	for runtime.GOOS == "windows" && slices.ContainsFunc(theirs, func(p string) bool { return exists(t, p) }) {
		time.Sleep(10 * time.Millisecond)
		sweepDeadRunFiles(zap.NewNop())
	}
	for _, p := range theirs {
		if exists(t, p) {
			t.Errorf("%s of a killed run outlived the next setup", p)
		}
	}
	for _, p := range mine {
		if !exists(t, p) {
			t.Errorf("a setup took %s of its own run, which is live", p)
		}
	}
}

// TestRunFilesOtherRun is the other keploy run of
// TestRunFiles_ASetupTakesOnlyTheFilesOfRunsThatAreGone: it sets up its
// app's trust files, says so, and runs until it is killed.
func TestRunFilesOtherRun(t *testing.T) {
	marks := os.Getenv(runFilesHelperEnv)
	if marks == "" {
		t.Skip("only runs as the other run of TestRunFiles_ASetupTakesOnlyTheFilesOfRunsThatAreGone")
	}
	loadSystemCABundleFn = func(*zap.Logger) ([]byte, string) { return testCACertPEM(), "test" }
	if err := SetupCaCertEnv(zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(marks, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Until the test that runs it kills it, or ends without: then stdin ends.
	_, _ = io.Copy(io.Discard, os.Stdin)
	t.Fatal("the test that runs this one ended without killing it")
}

// TestRunFiles_ASweepLeavesEveryOtherFile: a sweep takes an unlocked run
// file and nothing else of the temp dir: no file of an older keploy (which
// holds no lock, so may be a live run's), no other name, and nothing that is
// not a regular file.
func TestRunFiles_ASweepLeavesEveryOtherFile(t *testing.T) {
	tmp := runFilesTempDir(t)
	dead := []string{"keploy-ca-run-4021.crt", "keploy-java-truststore-run-7.jks", "keploy-java-cas-run-0.pem"}
	foreign := []string{
		// Older keploy versions'.
		"keploy-ca-123.crt", "keploy-java-truststore-1000.jks", "keploy-java-truststore.jks", "keploy-java-cas-55.pem", "ca.crt",
		// Close, but no name CreateTemp gives a run-file pattern.
		"keploy-ca-run-.crt", "keploy-ca-run-12x.crt", "keploy-ca-run-1.crt.bak", "xkeploy-ca-run-1.crt",
		"keploy-ca-run-1.pem", "keploy-ca-run-1-2.crt", "keploy-java-truststore-run-1.pem",
	}
	for _, n := range append(slices.Clone(dead), foreign...) {
		if err := os.WriteFile(filepath.Join(tmp, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(tmp, "keploy-ca-run-2.crt"), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign = append(foreign, "keploy-ca-run-2.crt")
	target := filepath.Join(t.TempDir(), "not-keploys.crt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(target, filepath.Join(tmp, "keploy-ca-run-3.crt")); err != nil {
			t.Fatal(err)
		}
		foreign = append(foreign, "keploy-ca-run-3.crt")
	}

	sweepDeadRunFiles(zap.NewNop())

	for _, n := range dead {
		if exists(t, filepath.Join(tmp, n)) {
			t.Errorf("the sweep left %s, a run file nobody holds", n)
		}
	}
	for _, n := range foreign {
		if !exists(t, filepath.Join(tmp, n)) {
			t.Errorf("the sweep took %s, which is not an ended run's run file", n)
		}
	}
	if !exists(t, target) {
		t.Errorf("the sweep took %s through a symlink", target)
	}
}

// TestRunFiles_ASweepInTheWindowBeforeTheLockCostsOnlyAnotherFile: a new
// run file is unlocked for an instant, between its creation and its lock, and
// another run's sweep that lists it then takes it for an ended run's. The run
// that made it must notice and make another, and end with a file that is
// there and locked. On Windows the sweep cannot take it: the run's own write
// handle refuses delete until the file is locked, and the run keeps it.
func TestRunFiles_ASweepInTheWindowBeforeTheLockCostsOnlyAnotherFile(t *testing.T) {
	windows := runtime.GOOS == "windows"
	sweepTakes := func(t *testing.T, path string) func() {
		removed, err := removeIfDead(path)
		if err != nil || removed == windows {
			t.Errorf("the sweep of the unlocked new %s: removed %v, %v; want removed %v", path, removed, err, !windows)
		}
		return func() {}
	}
	for _, tc := range []struct {
		name string
		// seam is where in the window the other run's sweep comes.
		seam *func(string)
		// sweep is the other run's sweep of path; it returns what to do
		// once the run has its file.
		sweep func(t *testing.T, path string) func()
		// swept: the run loses its first file.
		swept bool
	}{
		{"swept before the open", &beforeRunFileOpen, sweepTakes, !windows},
		{"swept between the open and the lock", &beforeRunFileLock, sweepTakes, !windows},
		{"held by a sweep at the lock", &beforeRunFileLock, func(t *testing.T, path string) func() {
			lock, err := openRunFileLock(path)
			if err != nil {
				t.Fatal(err)
			}
			if held, err := tryLockRunFile(lock); err != nil || !held {
				t.Fatalf("the sweep could not lock the unlocked %s: %v, %v", path, held, err)
			}
			return func() { _ = lock.Close() }
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := runFilesTempDir(t)
			var first string
			release := func() {}
			orig := *tc.seam
			t.Cleanup(func() { *tc.seam = orig })
			*tc.seam = func(path string) {
				if first == "" {
					first = path
					release = tc.sweep(t, path)
				}
			}

			f, rf, err := createRunFile(zap.NewNop(), runCAFilePattern)
			release()
			if err != nil {
				t.Fatalf("createRunFile: %v", err)
			}
			t.Cleanup(func() { _ = rf.remove() })
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if first == "" {
				t.Fatal("the seam never ran")
			}
			if tc.swept {
				if rf.path == first {
					t.Fatalf("the run kept %s, which a sweep took", first)
				}
				if exists(t, first) {
					t.Errorf("the swept %s is still there", first)
				}
			} else if rf.path != first {
				t.Fatalf("the run made %s, though the sweep could not take its first %s", rf.path, first)
			}
			if rf.lock == nil || !exists(t, rf.path) {
				t.Fatalf("the run's file %s is not there under its lock", rf.path)
			}
			sweepDeadRunFiles(zap.NewNop())
			if !exists(t, rf.path) {
				t.Errorf("a sweep took %s, which its run holds", rf.path)
			}
			if left := runFilesIn(t, tmp); !slices.Equal(left, []string{rf.path}) {
				t.Errorf("want only the run's file in the temp dir, have %v", left)
			}
			if err := rf.remove(); err != nil || exists(t, rf.path) {
				t.Errorf("the run could not remove its own file: %v", err)
			}
		})
	}
}

// TestRunFiles_ATempDirThatRefusesLocksStillRuns: on a filesystem that will
// not lock (NFS without its lock service, say), a native run must still get
// its trust files, under the older names, which no sweep takes, and say once
// that a kill would leave them.
func TestRunFiles_ATempDirThatRefusesLocksStillRuns(t *testing.T) {
	tmp := runFilesTempDir(t)
	orig := tryLockRunFileFn
	t.Cleanup(func() { tryLockRunFileFn = orig })
	tryLockRunFileFn = func(*os.File) (bool, error) { return false, errors.New("no locks on this filesystem") }
	core, logs := observer.New(zap.WarnLevel)
	warnUnlockedOnce = sync.Once{}

	if err := SetupCaCertEnv(zap.New(core), testCACertPEM()); err != nil {
		t.Fatalf("SetupCaCertEnv where nothing locks: %v", err)
	}
	if left := runFilesIn(t, tmp); len(left) != 0 {
		t.Errorf("want no locked-run names for files that hold no lock, have %v", left)
	}
	var unlocked []string
	for _, pattern := range []string{"keploy-ca-*.crt", "keploy-java-truststore-*.jks"} {
		m, _ := filepath.Glob(filepath.Join(tmp, pattern))
		unlocked = append(unlocked, m...)
	}
	if len(unlocked) != 2 || os.Getenv("SSL_CERT_FILE") != unlocked[0] {
		t.Fatalf("want the app's CA file and Java truststore under the older names, the CA file in SSL_CERT_FILE; have %v, SSL_CERT_FILE=%q", unlocked, os.Getenv("SSL_CERT_FILE"))
	}
	if n := logs.FilterMessageSnippet("cannot lock its trust files").Len(); n != 1 {
		t.Errorf("want one warning that the files go unlocked, have %d: %v", n, logs.All())
	}
	tryLockRunFileFn = orig
	sweepDeadRunFiles(zap.NewNop())
	for _, p := range unlocked {
		if !exists(t, p) {
			t.Errorf("a sweep took %s, which holds no lock and may be a live run's", p)
		}
	}
	if err := RemoveCaCertEnvFiles(); err != nil {
		t.Fatal(err)
	}
	for _, p := range unlocked {
		if exists(t, p) {
			t.Errorf("%s outlived its run's cleanup", p)
		}
	}
}
