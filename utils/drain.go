package utils

import (
	"runtime"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// DrainErrGroup waits for g.Wait() to return, but gives up after `timeout`.
//
// Why this exists: keploy's record/replay teardown runs in a deferred function
// that is also the path SIGINT/SIGTERM takes (the signal cancels the root
// context, which triggers the defer). The teardown calls g.Wait() to drain the
// run/setup goroutines after cancel(). If any one of those goroutines does not
// observe context cancellation — e.g. an agent-in-docker bring-up or proxy
// setup that wedges while the host is under heavy CPU/IO contention — an
// unbounded g.Wait() blocks the teardown forever. Because that same teardown is
// what SIGINT triggers, the process then ignores SIGINT entirely and only dies
// when an outer `timeout`/CI sends SIGKILL minutes later (observed: a CI lane
// hung ~50 min despite a 15-min `timeout -s INT`).
//
// Bounding the drain guarantees the process exits promptly after cancellation.
// On timeout it dumps every goroutine's stack (so the goroutine that ignored
// cancellation is actually discoverable: a stack parked in a docker/daemon
// syscall or socket read points to host IO contention, while one parked on a Go
// channel/mutex/select with no IO points to a missing-cancel code bug) and
// returns nil; the leaked goroutine is reaped when the process exits. In the
// normal case the goroutines drain in well under the timeout, so this is a no-op.
func DrainErrGroup(logger *zap.Logger, name string, g *errgroup.Group, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		logger.Error("teardown drain timed out after cancellation; forcing shutdown so stop/SIGINT isn't swallowed — a goroutine is ignoring context cancellation",
			zap.String("group", name),
			zap.Duration("timeout", timeout),
			zap.ByteString("goroutine_dump", allGoroutineStacks()))
		return nil
	}
}

// allGoroutineStacks returns the stack traces of all goroutines, growing the
// buffer until the dump fits (capped at 16 MiB) so the offending goroutine is
// never truncated away.
func allGoroutineStacks() []byte {
	for size := 1 << 20; size <= 1<<24; size *= 2 {
		buf := make([]byte, size)
		if n := runtime.Stack(buf, true); n < size {
			return buf[:n]
		}
	}
	buf := make([]byte, 1<<24)
	return buf[:runtime.Stack(buf, true)]
}
