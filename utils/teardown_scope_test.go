package utils

import (
	"context"
	"testing"
)

// A run's teardown scope is its own: a movement of its stops moves it, and
// the run's part (a test set) moves it through its own; another run's scope,
// in the same process (a long-lived keploy mock, a run started again), is
// never moved by it. A stop outside any run's scope moves none.
func TestATeardownScopeIsItsRunsOwn(t *testing.T) {
	ctx, run := NewTeardownScope(context.Background())
	if TeardownScopeOf(ctx) != run {
		t.Fatal("the run's context does not carry its scope")
	}
	setCtx, set := NewTeardownScope(ctx)
	if TeardownScopeOf(setCtx) != set || set == run {
		t.Fatal("the test set's context does not carry a scope of its own")
	}
	_, other := NewTeardownScope(context.Background())

	TeardownScopeOf(setCtx).Note()
	if set.Moves() != 1 || run.Moves() != 1 {
		t.Fatalf("a movement of the set's teardown: set %d, run %d; want 1 and 1", set.Moves(), run.Moves())
	}
	run.Note()
	if set.Moves() != 1 || run.Moves() != 2 {
		t.Fatalf("a movement of the run's teardown: set %d, run %d; want 1 and 2", set.Moves(), run.Moves())
	}
	if other.Moves() != 0 {
		t.Fatalf("another run's scope moved %d times with this run's teardown", other.Moves())
	}

	// The next run starts with a scope of its own: what this one's teardown
	// did is not counted for its drains.
	_, next := NewTeardownScope(context.Background())
	if next.Moves() != 0 {
		t.Fatalf("a new run's scope starts at %d movements", next.Moves())
	}

	var none *TeardownScope
	none.Note() // a stop outside any run: nil-safe
	if none.Moves() != 0 || TeardownScopeOf(context.Background()) != nil {
		t.Fatal("a context with no run carries a scope")
	}
}
