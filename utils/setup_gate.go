package utils

import (
	"context"
	"sync"
)

// SetupGate is a recording's rule for its agent's lifetime around an
// interrupt: an interrupt (ctx done) while the agent is still being set up
// cancels the agent's lifetime (cancel) at once; once the agent is up (Up), an
// interrupt leaves it running, and the teardown stops it after the stop's
// drain has taken what it captured. The interrupt and Up are decided under one
// lock, so an interrupt that lands as the setup finishes either cancelled the
// setup, and Up says so, or cancels nothing. Decided by which of two ready
// channels a select took, an interrupt could cancel an agent that was already
// up, after the setup had returned, and the run went on with an agent that
// was stopping.
type SetupGate struct {
	mu     sync.Mutex
	up     bool
	cut    bool
	cancel context.CancelFunc
	// unwatch stops watching ctx once the agent is up, so a gate on a
	// long-lived context leaves nothing registered on it.
	unwatch func() bool
}

// NewSetupGate watches ctx for the interrupt, and calls cancel for it while
// the agent is being set up.
func NewSetupGate(ctx context.Context, cancel context.CancelFunc) *SetupGate {
	g := &SetupGate{cancel: cancel}
	g.unwatch = context.AfterFunc(ctx, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if !g.up {
			g.cut = true
			g.cancel()
		}
	})
	return g
}

// Up says the agent is up: an interrupt from now on leaves it to the
// teardown. It reports false when an interrupt cancelled the setup first: the
// agent is stopping, and the run ends as an interrupted setup does.
func (g *SetupGate) Up() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cut {
		return false
	}
	g.up = true
	g.unwatch()
	return true
}
