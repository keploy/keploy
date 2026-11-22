//go:build unix

package cli

import (
	"context"
	"math"
	"os"
	"sync"
	"testing"

	"go.keploy.io/server/v3/utils"
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

// A client that dies while the agent's stop is under way -- holding for the
// client's streams, which no repeated signal cuts short -- ends that stop
// (ExecCancel): the agent does not wait on the streams of a client that is
// gone. (Without a stop under way it signals itself SIGTERM, which this
// process cannot take in a test.)
func TestAGoneClientEndsAStopUnderWay(t *testing.T) {
	utils.MarkInterrupted() // the stop under way
	t.Cleanup(utils.ClearInterrupted)
	cancelled := make(chan struct{})
	var once sync.Once
	utils.SetCancel(func() { once.Do(func() { close(cancelled) }) })
	t.Cleanup(func() { utils.SetCancel(func() {}) })
	endForGoneClient()
	select {
	case <-cancelled:
	default:
		t.Fatal("a gone client did not end the stop under way")
	}
}

// TestWatchParentProcessNoopOnInvalidPID ensures the watchdog is a safe no-op
// for a non-positive client PID (it must not self-signal).
func TestWatchParentProcessNoopOnInvalidPID(t *testing.T) {
	// Should return immediately without spawning a watcher or panicking.
	watchParentProcess(context.Background(), zap.NewNop(), 0)
	watchParentProcess(context.Background(), zap.NewNop(), -1)
}
