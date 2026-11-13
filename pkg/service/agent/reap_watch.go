package agent

import (
	"context"
	"time"
)

// goneWatchEvery is how often, while a test is open, the agent looks for
// the process that began it having exited (watchGoneLocked).
var goneWatchEvery = 50 * time.Millisecond

// watchGoneLocked starts, as a test opens (with provenMu held), the watch
// that fails an open test as soon as its process is seen gone (reapGone),
// unless the watch runs already. Without it such a test stayed open until
// the next claim or the run's final read: what the processes it left
// running did after its process exited (a daemon on its way out as keploy
// stopped it, the application's poller while keploy waited for the series
// the run owes) was charged to the dead test, in its window.
func (a *Agent) watchGoneLocked() {
	if a.proven.watches > 0 {
		return
	}
	a.proven.watches++
	go func() {
		t := time.NewTicker(goneWatchEvery)
		defer t.Stop()
		var procs []claimant
		for range t.C {
			var open bool
			if procs, open = a.reapGone(procs); !open {
				return
			}
		}
	}()
}

// reapGone fails each open test whose process exited, the run's one test
// process's or a worker's (endGoneWorkers), as the next claim or the run's
// final read would: the same outcome and reason, as soon as the process is
// seen gone. It reports false, and the watch ends, once no test is open (the
// run's final read leaves none); the next test to open starts it again.
//
// It looks without the wire: a look that finds every process running takes
// nothing a claim, a begin or an end waits for. Only a process found gone
// takes the wire, as a claim does, and is looked at again under it: a claim
// or the final read that took the wire first may have failed the same test
// already, and only one of them does. procs is a buffer for the processes a
// look reads, handed back for the next look: each look reads them anew.
func (a *Agent) reapGone(procs []claimant) ([]claimant, bool) {
	procs = procs[:0]
	a.provenMu.Lock()
	if len(a.proven.open) == 0 {
		a.proven.watches--
		a.provenMu.Unlock()
		return procs, false
	}
	if a.proven.openOf(0) != "" {
		procs = append(procs, a.proven.claim)
	}
	for _, w := range a.proven.workers {
		procs = append(procs, w.claimant)
	}
	a.provenMu.Unlock()
	if !anyGone(procs) {
		return procs, true
	}
	// A free context: only a wait ends with the lock's error, and this one
	// waits for as long as the operation holding the wire takes.
	ctx := context.Background()
	_ = a.wire.lock(ctx)
	defer a.wire.unlock()
	a.endGoneWorkers(ctx)
	a.provenMu.Lock()
	stale := ""
	if c := a.proven.claim; !procAlive(c.pid, c.start) {
		stale = a.proven.openOf(0)
	}
	a.provenMu.Unlock()
	if stale != "" {
		a.failOpen(ctx, stale, "the test process exited while the test was open")
	}
	return procs, true
}

// anyGone reports whether any of procs has exited.
func anyGone(procs []claimant) bool {
	for _, p := range procs {
		if !procAlive(p.pid, p.start) {
			return true
		}
	}
	return false
}
