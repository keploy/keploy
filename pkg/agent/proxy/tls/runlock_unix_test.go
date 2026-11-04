//go:build !windows

package tls

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"go.uber.org/zap"
)

// startLockHolder starts another process (this test binary, see TestMain)
// that takes the run lock for id in caRunsDir and holds it. The returned kill
// SIGKILLs its process group and waits for it; t.Cleanup does too.
func startLockHolder(t *testing.T, id string) (kill func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "KEPLOY_TEST_HOLD_RUN_LOCK="+id, "KEPLOY_TEST_RUN_LOCK_DIR="+caRunsDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	kill = func() {
		once.Do(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			_ = stdin.Close()
		})
	}
	t.Cleanup(kill)
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("the lock holder said %q (%v), want \"held\"", line, err)
	}
	return kill
}

// Another process's held lock makes its run live; once that process is
// SIGKILLed, the kernel has released the lock and the run is not live, though
// its lock file is still there.
func TestRunLockOfAnotherProcess(t *testing.T) {
	useRunsDir(t)
	kill := startLockHolder(t, "c0ffee01")
	if !runLive("c0ffee01") {
		t.Fatal("a run whose process holds its lock is not live")
	}
	// Only its owner (root, for a real run) can open it, and so hold it.
	files, _ := filepath.Glob(filepath.Join(caRunsDir, "c0ffee01.*.lock"))
	if len(files) != 1 {
		t.Fatalf("the holder's lock files: %q, want one", files)
	}
	if info, err := os.Stat(files[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the lock file: %v (%v), want mode 0600", info, err)
	}
	kill()
	files, _ = filepath.Glob(filepath.Join(caRunsDir, "c0ffee01.*.lock"))
	if len(files) != 1 {
		t.Fatalf("the killed run's lock files: %q, want its one file left", files)
	}
	if runLive("c0ffee01") {
		t.Fatal("a run whose process was killed is still live")
	}
}

// A lock file this process cannot open counts as live: the sweep then leaves
// that run's entries rather than risk removing a live one's.
func TestRunLockThatCannotBeOpenedCountsAsLive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file whatever its mode")
	}
	useRunsDir(t)
	p := filepath.Join(caRunsDir, "5eed5eed.1.lock")
	if err := os.WriteFile(p, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if !runLive("5eed5eed") {
		t.Fatal("a run whose lock file cannot be opened is not live")
	}
	// Only lock files count: another file under the run's name is not one.
	if err := os.WriteFile(filepath.Join(caRunsDir, "0ff1ce01.1.part"), nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if runLive("0ff1ce01") {
		t.Fatal("a file that is not a lock file made its run live")
	}
}

// A lock directory this process cannot read may hold a live run's lock: every
// run then counts as live.
func TestRunLockDirectoryThatCannotBeReadCountsAsLive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	useRunsDir(t)
	caRunsDir = filepath.Join(caRunsDir, "unreadable")
	if err := os.Mkdir(caRunsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	unreadable := caRunsDir
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o700) })
	if !runLive("0a1b2c") {
		t.Fatal("with a lock directory that cannot be read, a run is not live")
	}
}

// TestSweepsLeaveALiveRunsEntries: this host's trust stores hold the entries
// (a .crt anchor, an NSS nickname, a JDK alias) of four other runs and of this
// one. One other run is alive (another process holds its lock), one was
// SIGKILLed (its lock file stays, unlocked), one left a lock file nobody holds,
// and one has no lock file (a run from before these locks). Each sweep removes
// the entries of the three dead runs, and leaves the live run's and this
// run's.
func TestSweepsLeaveALiveRunsEntries(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the .crt anchors and the NSS database are swept on Linux")
	}
	const live, killed, stale, missing, current = "a11ce001", "b0b0b002", "c1a1c003", "d0d0d004", "e1e1e005"
	useRunsDir(t)
	startLockHolder(t, live)
	startLockHolder(t, killed)()
	if err := os.WriteFile(filepath.Join(caRunsDir, stale+".1.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ids := []string{live, killed, stale, missing, current}
	dead := []string{killed, stale, missing}

	t.Run("crt anchors", func(t *testing.T) {
		store := t.TempDir()
		old := caStorePath
		caStorePath = []string{store + "/"}
		t.Cleanup(func() { caStorePath = old })
		for _, id := range ids {
			if err := os.WriteFile(filepath.Join(store, "keploy-mitm-"+id+".crt"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		sweepRunLeftovers(zap.NewNop(), current)
		var left []string
		entries, _ := os.ReadDir(store)
		for _, e := range entries {
			left = append(left, e.Name())
		}
		want := []string{"keploy-mitm-" + live + ".crt", "keploy-mitm-" + current + ".crt"}
		if strings.Join(left, "|") != strings.Join(want, "|") {
			t.Fatalf("anchors after the sweep: %q, want %q", left, want)
		}
	})

	t.Run("NSS nicknames", func(t *testing.T) {
		dir := nssTestDir(t)
		list := "\nCertificate Nickname                                         Trust Attributes\n\n"
		for _, id := range ids {
			list += "keploy-mitm-" + id + "                                        C,,\n"
		}
		calls := fakeCertutil(t, list, 0)
		installNSSCA(context.Background(), zap.NewNop(), dir, "/ca.crt", "keploy-mitm-"+current)
		var want []string
		for _, id := range dead {
			want = append(want, "-D -d sql:"+dir+" -n keploy-mitm-"+id)
		}
		var deleted []string
		for _, c := range certutilCalls(t, calls) {
			if strings.HasPrefix(c, "-D") {
				deleted = append(deleted, c)
			}
		}
		if strings.Join(deleted, "|") != strings.Join(want, "|") {
			t.Fatalf("NSS nicknames removed: %q, want %q", deleted, want)
		}
	})

	t.Run("JDK aliases", func(t *testing.T) {
		dir := t.TempDir()
		listing := ""
		for _, id := range ids {
			listing += "keploy-mitm-" + id + ", Nov 3, 2026, trustedCertEntry,\n"
		}
		home, log := fakeJDKListing(t, dir, "jdk", "lib/security/cacerts", listing)
		ks := javaKeystore{keytool: filepath.Join(home, "bin", "keytool"), cacerts: filepath.Join(home, "lib", "security", "cacerts")}
		present, listed := sweepJavaLeftovers(context.Background(), zap.NewNop(), ks, "keploy-mitm-"+current)
		if !present || !listed {
			t.Fatalf("sweepJavaLeftovers = present %v, listed %v; want true, true", present, listed)
		}
		var want []string
		for _, id := range dead {
			want = append(want, "-delete -alias keploy-mitm-"+id+" -keystore "+ks.cacerts+" -storepass changeit -noprompt")
		}
		if dels := withPrefix(keytoolCalls(t, log), "-delete "); strings.Join(dels, "|") != strings.Join(want, "|") {
			t.Fatalf("JDK aliases removed: %q, want %q", dels, want)
		}
	})
}

// The native install holds this run's lock by the time it updates the store
// (after it wrote the anchors and imported into the JDK keystore: the update
// finds the lock file), and its teardown gives the lock up, removing its file,
// once the entries are gone. TestNativeInstallLocksBeforeItsFirstAnchor pins
// the lock's place before the first anchor.
func TestNativeInstallHoldsTheRunsLockUntilTeardown(t *testing.T) {
	useRunsDir(t)
	dir := t.TempDir()
	app, _ := fakeJDK(t, dir, "app-jdk", "lib/security/cacerts")
	other, _ := fakeJDK(t, dir, "other-jdk", "lib/security/cacerts")
	stores := fakeNativeLinux(t, 1, other)
	// The store's update lists the lock files there are as it runs.
	probe, seen := t.TempDir(), filepath.Join(t.TempDir(), "locks-at-update")
	script := "#!/bin/sh\nfor f in '" + caRunsDir + "'/*.lock; do [ -e \"$f\" ] && echo \"${f##*/}\" >> '" + seen + "'; done\nexit 0\n"
	if err := os.WriteFile(filepath.Join(probe, "keploy-test-update-ca"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", probe+string(os.PathListSeparator)+os.Getenv("PATH"))
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, nil, 0, app); err != nil {
		t.Fatal(err)
	}
	// The update runs holding the store lock too (storelock.go), whose file
	// sorts after this run's hex id.
	if b, _ := os.ReadFile(seen); strings.TrimSpace(string(b)) != filepath.Base(ownLockFile(ca.runID))+"\n"+storeLockName {
		t.Fatalf("lock files when the store was updated: %q, want this run's %s and %s", b, filepath.Base(ownLockFile(ca.runID)), storeLockName)
	}
	if !runLive(ca.runID) {
		t.Fatal("after the install, this run is not live: other runs' sweeps would remove its entries")
	}
	TeardownNativeCA(zap.NewNop())
	if runLive(ca.runID) {
		t.Fatal("after the teardown, this run is still live")
	}
	if files, _ := filepath.Glob(filepath.Join(caRunsDir, ca.runID+".*.lock")); files != nil {
		t.Fatalf("the teardown left this run's lock files %q", files)
	}
	if _, err := os.Stat(filepath.Join(stores[0], runCertFileName(ca))); !os.IsNotExist(err) {
		t.Fatalf("the teardown left this run's anchor (stat: %v)", err)
	}
}

// The native install takes this run's lock before it writes its first entry:
// with no java home given, it asks the PATH java for one before it writes the
// first anchor, and that java finds the run's lock file and no anchor yet.
// (The test leans on that order of the java and the anchors too: moving the
// java's question below the anchors fails it for another reason.)
func TestNativeInstallLocksBeforeItsFirstAnchor(t *testing.T) {
	useRunsDir(t)
	dir := t.TempDir()
	other, _ := fakeJDK(t, dir, "other-jdk", "lib/security/cacerts")
	stores := fakeNativeLinux(t, 1, other)
	seen := filepath.Join(t.TempDir(), "at-java")
	probe := "#!/bin/sh\n" +
		"for f in '" + caRunsDir + "'/*.lock; do [ -e \"$f\" ] && echo \"${f##*/}\" >> '" + seen + "'; done\n" +
		"for f in '" + stores[0] + "'/*.crt; do [ -e \"$f\" ] && echo \"anchor ${f##*/}\" >> '" + seen + "'; done\n" +
		"echo '    java.home = " + other + "' >&2\nexit 0\n"
	if err := os.WriteFile(filepath.Join(other, "bin", "java"), []byte(probe), 0o755); err != nil {
		t.Fatal(err)
	}
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the install did not ask the PATH java for its home: %v", err)
	}
	if got, want := strings.TrimSpace(string(b)), filepath.Base(ownLockFile(ca.runID)); got != want {
		t.Fatalf("what the PATH java found: %q, want only this run's lock file %s", got, want)
	}
}
