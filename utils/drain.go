package utils

import (
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
// On timeout it logs (so the offending goroutine is discoverable) and returns
// nil; the leaked goroutine is reaped when the process exits. In the normal
// case the goroutines drain in well under the timeout, so this is a no-op.
func DrainErrGroup(logger *zap.Logger, name string, g *errgroup.Group, timeout time.Duration) error {
	err, _ := DrainErrGroupStatus(logger, name, g, timeout)
	return err
}

// DrainErrGroupStatus is DrainErrGroup with the timeout made observable to the
// caller. It reports timedOut=true iff g.Wait() did NOT return within `timeout`
// — i.e. a goroutine is ignoring context cancellation and MAY STILL BE RUNNING
// (and writing) after this returns. On that path err is nil, preserving the
// "a timeout must never fail teardown" contract described above.
//
// So `!timedOut` is the "every goroutine in the group has returned" signal: a
// non-nil err still means the group finished (one goroutine returned an error),
// whereas timedOut means it did not join. A caller that then touches an artifact
// a wedged group goroutine could also be writing — e.g. a post-record pass that
// rewrites the mock file — must gate on `!timedOut`, not on `err == nil`.
func DrainErrGroupStatus(logger *zap.Logger, name string, g *errgroup.Group, timeout time.Duration) (err error, timedOut bool) {
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case e := <-done:
		return e, false
	case <-time.After(timeout):
		logger.Error("teardown drain timed out after cancellation; forcing shutdown so stop/SIGINT isn't swallowed — a goroutine is ignoring context cancellation",
			zap.String("group", name),
			zap.Duration("timeout", timeout))
		return nil, true
	}
}
