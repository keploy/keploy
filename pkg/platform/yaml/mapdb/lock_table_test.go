package mapdb

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

// heldLocks is the set write locks in the table, by key, with their users.
func heldLocks() map[string]int {
	setWrites.mu.Lock()
	defer setWrites.mu.Unlock()
	out := make(map[string]int, len(setWrites.held))
	for k, w := range setWrites.held {
		out[k] = w.users
	}
	return out
}

// A set's write lock is in the table only while a write holds or waits for
// it: a write alone, two that met, and one that stopped waiting each leave
// none behind, so a process that records for days keeps no lock for the sets
// it wrote.
func TestASetsWriteLockIsInTheTableOnlyWhileAWriteHoldsOrWaitsForIt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seedSet(t, dir)
	if held := heldLocks(); len(held) != 0 {
		t.Fatalf("after writes alone: %v; want no lock", held)
	}

	recorder, other := New(zap.NewNop(), dir, "mappings"), New(zap.NewNop(), dir, "mappings")
	w := mapWriters[0]
	if e1, e2 := runMet(
		func(ctx context.Context) error { return recorderWrite.write(ctx, recorder) },
		func(ctx context.Context) error { return w.write(ctx, other) }); e1 != nil || e2 != nil {
		t.Fatalf("recorder: %v, %s: %v", e1, w.name, e2)
	}
	if held := heldLocks(); len(held) != 0 {
		t.Fatalf("after two writes that met: %v; want no lock", held)
	}

	held := &pausedCtx{Context: ctx, asked: make(chan struct{}), release: make(chan struct{})}
	holderErr := make(chan error, 1)
	go func() { holderErr <- recorderWrite.write(held, recorder) }()
	<-held.asked
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := w.write(cancelled, other); !errors.Is(err, context.Canceled) {
		t.Fatalf("the waiting write returned %v, want context.Canceled", err)
	}
	key := recorder.fileKey(lockedSetID)
	if got := heldLocks(); len(got) != 1 || got[key] != 1 {
		t.Fatalf("while the holder holds and the cancelled write has left: %v; want %s held by 1", got, key)
	}
	close(held.release)
	if err := <-holderErr; err != nil {
		t.Fatal(err)
	}
	if got := heldLocks(); len(got) != 0 {
		t.Fatalf("after the holder released: %v; want no lock", got)
	}
}
