//go:build !windows

package tls

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
func TestCARunLockOfAnotherProcess(t *testing.T) {
	useRunsDir(t)
	kill := startLockHolder(t, "c0ffee01")
	if !caRunLive("c0ffee01") {
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
	if caRunLive("c0ffee01") {
		t.Fatal("a run whose process was killed is still live")
	}
}

// A lock file this process cannot open counts as live: the sweep then leaves
// that run's entries rather than risk removing a live one's.
func TestCARunLockThatCannotBeOpenedCountsAsLive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file whatever its mode")
	}
	useRunsDir(t)
	p := filepath.Join(caRunsDir, "5eed5eed.1.lock")
	if err := os.WriteFile(p, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if !caRunLive("5eed5eed") {
		t.Fatal("a run whose lock file cannot be opened is not live")
	}
	// Only lock files count: another file under the run's name is not one.
	if err := os.WriteFile(filepath.Join(caRunsDir, "0ff1ce01.1.part"), nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if caRunLive("0ff1ce01") {
		t.Fatal("a file that is not a lock file made its run live")
	}
}

// A lock directory this process cannot read may hold a live run's lock: every
// run then counts as live.
func TestCARunLockDirectoryThatCannotBeReadCountsAsLive(t *testing.T) {
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
	if !caRunLive("0a1b2c") {
		t.Fatal("with a lock directory that cannot be read, a run is not live")
	}
}

// TestSweepsLeaveALiveRunsEntries: this host's trust stores hold the entries
// (a .crt anchor, an NSS nickname) of four other runs and of this one. One
// other run is alive (another process holds its lock), one was SIGKILLed (its
// lock file stays, unlocked), one left a lock file nobody holds, and one has
// no lock file (a run from before these locks). Each sweep removes the entries
// of the three dead runs, and leaves the live run's and this run's.
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
}

// nativeLinuxForLockRows points the native Linux install at stores trust
// store directories of the test's own, made under one temp directory, with
// keploy-test-update-ca (whose body is update) as the store's update, and no
// JDK or certutil on PATH. The install's record, the run's lock and the CA's
// readiness are undone at the test's end.
func nativeLinuxForLockRows(t *testing.T, stores int, update string) []string {
	t.Helper()
	dir := t.TempDir()
	bin, home := filepath.Join(dir, "bin"), filepath.Join(dir, "home")
	writeScript(t, filepath.Join(bin, "keploy-test-update-ca"), update)
	paths := []string{home}
	var storeDirs []string
	for i := 0; i < stores; i++ {
		storeDirs = append(storeDirs, filepath.Join(dir, "store"+strconv.Itoa(i)))
	}
	for _, d := range append(paths, storeDirs...) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("JAVA_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"} {
		t.Setenv(k, "")
	}
	oldPaths, oldCmds := caStorePath, caStoreUpdateCmd
	caStorePath, caStoreUpdateCmd = nil, []string{"keploy-test-update-ca"}
	for _, d := range storeDirs {
		caStorePath = append(caStorePath, d+"/")
	}
	t.Cleanup(func() {
		caStorePath, caStoreUpdateCmd = oldPaths, oldCmds
		recordNativeInstall(nil)
		releaseCARunLock()
		ResetCAReadyForTest()
	})
	return storeDirs
}

// The native install holds this run's lock by the time it updates the store
// (after it wrote the anchors: the update finds the lock file), and its
// teardown gives the lock up, removing its file, once the entries are gone.
// TestNativeInstallLocksBeforeItsFirstAnchor pins the lock's place before the
// first anchor.
func TestNativeInstallHoldsTheRunsLockUntilTeardown(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's")
	}
	useRunsDir(t)
	// The store's update lists the lock files there are as it runs, and says
	// whether the store lock is held then (probeStoreLock).
	seen := filepath.Join(t.TempDir(), "locks-at-update")
	stores := nativeLinuxForLockRows(t, 1, "for f in '"+caRunsDir+"'/*.lock; do [ -e \"$f\" ] && echo \"${f##*/}\" >> '"+seen+"'; done\n"+
		"p=$(KEPLOY_TEST_PROBE_STORE_LOCK='"+caRunsDir+"' '"+os.Args[0]+"')\necho \"store lock $p\" >> '"+seen+"'\nexit 0\n")
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(installCtx(t), zap.NewNop(), ca, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	// The update runs holding the store lock too (storelock.go), whose file
	// sorts after this run's hex id.
	if b, _ := os.ReadFile(seen); strings.TrimSpace(string(b)) != filepath.Base(ownLockFile(ca.runID))+"\n"+storeLockName+"\nstore lock held" {
		t.Fatalf("lock files when the store was updated: %q, want this run's %s and %s, and the store lock held", b, filepath.Base(ownLockFile(ca.runID)), storeLockName)
	}
	if !caRunLive(ca.runID) {
		t.Fatal("after the install, this run is not live: other runs' sweeps would remove its entries")
	}
	TeardownNativeCA(zap.NewNop())
	if caRunLive(ca.runID) {
		t.Fatal("after the teardown, this run is still live")
	}
	if files, _ := filepath.Glob(filepath.Join(caRunsDir, ca.runID+".*.lock")); files != nil {
		t.Fatalf("the teardown left this run's lock files %q", files)
	}
	if _, err := os.Stat(filepath.Join(stores[0], runCertFileName(ca))); !os.IsNotExist(err) {
		t.Fatalf("the teardown left this run's anchor (stat: %v)", err)
	}
}

// The native install takes this run's lock before it writes its first entry,
// the anchor in the first trust store directory: where that write fails (a
// directory holds the anchor's name), the install ends there, and the run
// holds its lock all the same. Its teardown then gives the lock up.
func TestNativeInstallLocksBeforeItsFirstAnchor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's")
	}
	useRunsDir(t)
	stores := nativeLinuxForLockRows(t, 2, "exit 0\n")
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(stores[0], runCertFileName(ca)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(installCtx(t), zap.NewNop(), ca, nil, 0, ""); err == nil {
		t.Fatal("the install wrote its first anchor over a directory")
	}
	if !caRunLive(ca.runID) {
		t.Fatal("the install's first anchor write was reached without this run's lock")
	}
	TeardownNativeCA(zap.NewNop())
	if caRunLive(ca.runID) {
		t.Fatal("after the teardown of a failed install, this run is still live")
	}
}

// The runs directory of each OS and user: /run on Linux for every run; on
// macOS, which has no /run, /var/run for root's runs and the run's own temp
// directory for another user's. The host's default is its own OS's.
func TestCARunsDirOfEachOSAndUser(t *testing.T) {
	const tmp = "/var/folders/9c/user/T/"
	for _, tc := range []struct {
		goos string
		euid int
		want string
	}{
		{"linux", 0, "/run/keploy-ca-runs"},
		{"linux", 1000, "/run/keploy-ca-runs"},
		{"darwin", 0, "/var/run/keploy-ca-runs"},
		{"darwin", 502, "/var/folders/9c/user/T/keploy-ca-runs"},
	} {
		if got := caRunsDirOn(tc.goos, tc.euid, tmp); got != tc.want {
			t.Errorf("caRunsDirOn(%s, euid %d) = %q, want %q", tc.goos, tc.euid, got, tc.want)
		}
	}
	if got, want := defaultCARunsDir(), caRunsDirOn(runtime.GOOS, os.Geteuid(), os.TempDir()); got != want {
		t.Errorf("defaultCARunsDir() = %q, want %q", got, want)
	}
}
