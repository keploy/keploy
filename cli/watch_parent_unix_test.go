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
	// Spawn a trivial process and reap it so its PID is definitively gone.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn helper process: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait() // reap, so the PID leaves the table
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
