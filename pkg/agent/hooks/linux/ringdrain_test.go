//go:build linux

package linux

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

// waitBlocked spins, with no clock, until the drain has a waiter, failing if
// the wait returned before the watch got through its flush.
func waitBlocked(t *testing.T, d *reportDrain, done <-chan error) {
	t.Helper()
	for {
		select {
		case err := <-done:
			t.Fatalf("the wait returned (%v) before the watch got through its flush", err)
		default:
		}
		d.mu.Lock()
		w := d.waiting
		d.mu.Unlock()
		if w > 0 {
			return
		}
		runtime.Gosched()
	}
}

// A drain flushes the reader, and returns once the watch got through the
// flush: not before.
func TestADrainWaitsForTheWatchToGetThroughItsFlush(t *testing.T) {
	d := newReportDrain()
	flushes := 0
	done := make(chan error, 1)
	go func() { done <- d.wait(context.Background(), func() error { flushes++; return nil }) }()
	waitBlocked(t, d, done)
	d.flushed()
	if err := <-done; err != nil {
		t.Fatalf("wait = %v, want nil once the watch got through the flush", err)
	}
	if flushes != 1 {
		t.Fatalf("%d flushes, want 1", flushes)
	}
}

// A watch that ended leaves a drain unanswered: an error, never a count as
// if every report were in. ctx ends a drain too.
func TestADrainEndsWithTheWatchOrItsContext(t *testing.T) {
	d := newReportDrain()
	done := make(chan error, 1)
	go func() { done <- d.wait(context.Background(), func() error { return nil }) }()
	waitBlocked(t, d, done)
	broken := errors.New("ring broken")
	d.stop(broken)
	if err := <-done; !errors.Is(err, broken) {
		t.Fatalf("wait after the watch ended = %v, want %v", err, broken)
	}
	if err := d.wait(context.Background(), func() error { return nil }); !errors.Is(err, broken) {
		t.Fatalf("a later wait = %v, want %v", err, broken)
	}
	if err := d.failure(); !errors.Is(err, broken) {
		t.Fatalf("failure = %v, want %v", err, broken)
	}

	closed := newReportDrain()
	closed.stop(errReportClosed)
	if err := closed.failure(); err != nil {
		t.Fatalf("a watch keploy closed: failure = %v, want nil", err)
	}

	d = newReportDrain()
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- d.wait(ctx, func() error { return nil }) }()
	waitBlocked(t, d, done)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("wait after ctx ended = %v, want context.Canceled", err)
	}

	flushErr := errors.New("closed")
	if err := newReportDrain().wait(context.Background(), func() error { return flushErr }); !errors.Is(err, flushErr) {
		t.Fatalf("wait with a flush that failed = %v, want %v", err, flushErr)
	}
}
