//go:build !windows

package tls

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// defaultCARunsDir is this host's runs directory (caRunsDirOn).
func defaultCARunsDir() string { return caRunsDirOn(runtime.GOOS, os.Geteuid(), os.TempDir()) }

// caRunsDirOn is the runs directory on goos for a run of the user euid whose
// temp directory is tempDir.
//
// On Linux it is under /run: only root writes there (the native Linux install
// that writes trust-store entries runs as root), no tmp cleaner removes a live
// run's file, and it is emptied at boot, when no run is alive.
//
// macOS has no /run and its root volume is sealed, so no run could make a
// directory there, and the store lock its legacy sweep's keytool delete takes
// (SweepLegacyJavaAlias, withStoreLock) was never taken. There root's runs use
// /var/run, which macOS keeps as Linux keeps /run (only root writes there, and
// it is emptied at boot); a run of another user cannot write /var/run, so it
// uses a directory in its own temp directory (os.TempDir: macOS gives each
// user one, which only that user writes, so no other user can plant a link
// there or hold its lock). Root's runs and another user's do not share the
// lock, and need not: the macOS install writes no trust-store entry
// (setupNativeDarwin), so the one change its runs make is that delete, the
// same in every run, and two runs that make it at once do not undo each
// other's.
func caRunsDirOn(goos string, euid int, tempDir string) string {
	if goos != "darwin" {
		return "/run/keploy-ca-runs"
	}
	if euid == 0 {
		return "/var/run/keploy-ca-runs"
	}
	return filepath.Join(tempDir, "keploy-ca-runs")
}

// tryLockFile takes an exclusive flock on f if no one holds one: true when it
// did, false when someone holds it, and an error when flock failed otherwise.
func tryLockFile(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func unlockFile(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
