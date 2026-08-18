//go:build unix

package cli

import (
	"context"
	"math"
	"os"
	"testing"

	"go.uber.org/zap"
)

// TestParentAlive verifies the liveness primitive the parent-death watchdog
// relies on: a running PID reads as "alive"; an unallocated PID reads as "dead".
func TestParentAlive(t *testing.T) {
	// A live process (ourselves) reads as alive.
	if !parentAlive(os.Getpid()) {
		t.Fatal("parentAlive(self) = false, want true")
	}
	// A PID that can never be allocated reads as dead — deterministically. We
	// deliberately do NOT spawn a real process and reap it to get a "dead" PID:
	// a freshly-reaped PID can be recycled to an unrelated process on a busy
	// host between the reap and the check, so kill(pid,0) would succeed and flake
	// this assertion. pid_max is at most 2^22 on Linux and far lower on
	// macOS/BSD, so a PID at the 32-bit ceiling has no task; kill(pid,0) returns
	// ESRCH with no reuse race and no dependency on an external helper binary.
	const neverAllocatedPID = math.MaxInt32
	if parentAlive(neverAllocatedPID) {
		t.Fatalf("parentAlive(%d) = true for an unallocated pid, want false", neverAllocatedPID)
	}
}

// TestWatchParentProcessNoopOnInvalidPID ensures the watchdog is a safe no-op
// for a non-positive client PID (it must not self-signal).
func TestWatchParentProcessNoopOnInvalidPID(t *testing.T) {
	// Should return immediately without spawning a watcher or panicking.
	watchParentProcess(context.Background(), zap.NewNop(), 0)
	watchParentProcess(context.Background(), zap.NewNop(), -1)
}
