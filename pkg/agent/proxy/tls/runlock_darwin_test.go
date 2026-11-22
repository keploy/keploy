//go:build darwin

package tls

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
