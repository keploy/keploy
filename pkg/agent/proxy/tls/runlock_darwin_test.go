//go:build darwin

package tls

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// On macOS a run's own runs directory is one it can make, so the store lock
// its legacy sweep's keytool delete takes is taken: the change runs holding
// it, and nothing warns. macOS has no /run (its root volume is sealed): under
// /run/keploy-ca-runs every such change ran without the lock and warned with
// a fix no one could make. The row takes the lock in the host's real default
// directory for this test's user (its temp directory; /var/run for root), and
// removes that directory again when the row made it.
func TestMacOSRunsTakeTheStoreLockInADirectoryTheyCanMake(t *testing.T) {
	if os.Geteuid() != 0 {
		// The temp directory macOS gives this user, whatever TMPDIR the
		// test runs with.
		t.Setenv("TMPDIR", userTempDir(t))
	}
	dir := defaultCARunsDir()
	want := filepath.Join(os.TempDir(), "keploy-ca-runs")
	if os.Geteuid() == 0 {
		want = "/var/run/keploy-ca-runs"
	}
	if dir != want {
		t.Fatalf("the runs directory is %q, want %q", dir, want)
	}
	// The run can make the directory, also where an earlier run has made
	// it: it can make one beside it.
	probe, err := os.MkdirTemp(filepath.Dir(dir), "keploy-ca-runs-probe-")
	if err != nil {
		t.Fatalf("a run cannot make a directory in %s: %v", filepath.Dir(dir), err)
	}
	_ = os.Remove(probe)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	old := caRunsDir
	caRunsDir = dir
	t.Cleanup(func() { caRunsDir = old })
	storeLockWarned.Store(false)
	t.Cleanup(func() { storeLockWarned.Store(false) })
	core, logs := observer.New(zapcore.WarnLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	held := false
	if err := withStoreLock(ctx, zap.New(core), func() error { held = storeLockHeldHere(t); return nil }); err != nil {
		t.Fatal(err)
	}
	if !held || logs.Len() != 0 {
		t.Fatalf("store lock held while the change ran: %v; warnings %v; want true and none", held, logs.All())
	}
}

// userTempDir is the temp directory macOS gives this test's user, which only
// that user can write.
func userTempDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("getconf", "DARWIN_USER_TEMP_DIR").Output()
	if err != nil {
		t.Fatalf("getconf DARWIN_USER_TEMP_DIR: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// A macOS run of a user other than root with TMPDIR unset or empty (env -i,
// sudo -u, a launchd job) has /tmp for its temp directory, which every user
// can write: another user could have made the runs directory there, or put a
// link where its lock file goes. The run takes no lock there and makes
// nothing in /tmp: its change runs without the lock, and it warns once,
// saying to set TMPDIR to the user's own temp directory.
func TestMacOSRunWithTMPDIRUnsetTakesNoLockInTmp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root's runs use /var/run, whatever TMPDIR")
	}
	t.Setenv("TMPDIR", "")
	dir := defaultCARunsDir()
	if dir != "/tmp/keploy-ca-runs" {
		t.Fatalf("the runs directory is %q, want /tmp/keploy-ca-runs", dir)
	}
	_, before := os.Lstat(dir)
	old := caRunsDir
	caRunsDir = dir
	t.Cleanup(func() { caRunsDir = old })
	storeLockWarned.Store(false)
	t.Cleanup(func() { storeLockWarned.Store(false) })
	core, logs := observer.New(zapcore.WarnLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ran := false
	if err := withStoreLock(ctx, zap.New(core), func() error { ran = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !ran || logs.Len() != 1 {
		t.Fatalf("change ran: %v; warnings %v; want it ran, and one warning", ran, logs.All())
	}
	if e, _ := logs.All()[0].ContextMap()["error"].(string); !strings.Contains(e, "the temp directory /tmp ") || !strings.Contains(e, "set TMPDIR to the one macOS gives this user (getconf DARWIN_USER_TEMP_DIR)") {
		t.Errorf("the warning's error %q does not name /tmp and say to set TMPDIR", e)
	}
	if _, after := os.Lstat(dir); errors.Is(before, fs.ErrNotExist) && !errors.Is(after, fs.ErrNotExist) {
		_ = os.RemoveAll(dir)
		t.Errorf("%s was made: %v", dir, after)
	}
}
