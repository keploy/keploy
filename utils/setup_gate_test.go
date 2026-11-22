package utils

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// An interrupt during the setup cancels the agent's lifetime, and Up then says
// so; one after Up cancels nothing. Racing, exactly one of the two happens:
// never an agent that is up and cancelled, nor an interrupted setup that Up
// reports as up.
func TestSetupGateDecidesTheInterruptAndUpTogether(t *testing.T) {
	t.Run("an interrupt during the setup", func(t *testing.T) {
		ctx, interrupt := context.WithCancel(context.Background())
		var cancelled atomic.Bool
		done := make(chan struct{})
		g := NewSetupGate(ctx, func() { cancelled.Store(true); close(done) })
		interrupt()
		<-done
		if g.Up() {
			t.Fatal("Up reported an agent up whose setup the interrupt cancelled")
		}
	})
	t.Run("an interrupt once the agent is up", func(t *testing.T) {
		ctx, interrupt := context.WithCancel(context.Background())
		var cancelled atomic.Bool
		g := NewSetupGate(ctx, func() { cancelled.Store(true) })
		if !g.Up() {
			t.Fatal("Up reported an interrupt that had not come")
		}
		if g.unwatch() {
			t.Fatal("the gate still watches the context once the agent is up")
		}
		interrupt()
		g.mu.Lock() // the interrupt's decision has run once the gate is free
		g.mu.Unlock()
		if cancelled.Load() {
			t.Fatal("an interrupt after the agent was up cancelled its lifetime")
		}
	})
	t.Run("racing", func(t *testing.T) {
		for i := 0; i < 2000; i++ {
			ctx, interrupt := context.WithCancel(context.Background())
			var cancelled atomic.Bool
			var decided sync.WaitGroup
			decided.Add(1)
			g := NewSetupGate(ctx, func() { cancelled.Store(true) })
			var up bool
			go func() { defer decided.Done(); up = g.Up() }()
			interrupt()
			decided.Wait()
			g.mu.Lock()
			g.mu.Unlock()
			if up == cancelled.Load() {
				t.Fatalf("iteration %d: up=%v and cancelled=%v: the interrupt and Up were not decided together", i, up, cancelled.Load())
			}
		}
	})
}
