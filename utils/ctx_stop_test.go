package utils

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A stop cancels its context with the instant it was made, which StoppedAt
// reads from the context and every one derived from it; a context cancelled
// otherwise says none.
func TestWithStopSaysTheInstantOfTheStop(t *testing.T) {
	ctx, stop := WithStop(context.Background())
	child, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	if _, ok := StoppedAt(ctx); ok {
		t.Fatal("a context not yet stopped says a stop")
	}
	before := time.Now()
	stop()
	after := time.Now()
	for name, c := range map[string]context.Context{"the stopped context": ctx, "a context derived from it": child} {
		at, ok := StoppedAt(c)
		if !ok || at.Before(before) || at.After(after) {
			t.Fatalf("%s says stopped at %v (%v), want between %v and %v", name, at, ok, before, after)
		}
	}
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatal("a stop's cause is not a cancel")
	}
	plain, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := StoppedAt(plain); ok {
		t.Fatal("a context cancelled without a stop says one")
	}
	var none context.Context
	if _, ok := StoppedAt(none); ok {
		t.Fatal("a nil context says a stop")
	}
}
