//go:build unix

package cli

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"go.uber.org/zap"
)

// TestParentAlive verifies the liveness primitive the parent-death watchdog
// relies on: a running PID is "alive"; a spawned-then-reaped PID is "dead".
func TestParentAlive(t *testing.T) {
	if !parentAlive(os.Getpid()) {
		t.Fatal("parentAlive(self) = false, want true")
	}
	// Spawn a short-lived process and reap it so its PID is definitively gone.
	// Re-exec THIS test binary (os.Args[0], always present) with a run filter
	// that matches no tests, so it exits immediately — rather than depending on
	// an external `true` being on PATH, which isn't guaranteed on every Unix CI
	// image. The exit code is irrelevant; we only need the PID reaped.
	helper := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // re-exec of the test binary itself
	if err := helper.Start(); err != nil {
		t.Skipf("cannot spawn helper process: %v", err)
	}
	pid := helper.Process.Pid
	_ = helper.Wait() // reap, so the PID leaves the table
	if parentAlive(pid) {
		t.Fatalf("parentAlive(%d) = true after the process exited and was reaped, want false", pid)
	}
}

// TestWatchParentProcessNoopOnInvalidPID ensures the watchdog is a safe no-op
// for a non-positive client PID (it must not self-signal).
func TestWatchParentProcessNoopOnInvalidPID(t *testing.T) {
	// Should return immediately without spawning a watcher or panicking.
	watchParentProcess(context.Background(), zap.NewNop(), 0)
	watchParentProcess(context.Background(), zap.NewNop(), -1)
}
