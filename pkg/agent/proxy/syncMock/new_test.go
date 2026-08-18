package manager

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestNewReturnsIndependentInstances verifies that New() yields managers
// that share no mutable state with each other or with the package global.
// This is the foundation of the multi-app capability (O1): one process
// can run N independent capture sessions without cross-contamination.
func TestNewReturnsIndependentInstances(t *testing.T) {
	t.Parallel()

	a := New(nil)
	b := New(nil)

	if a == nil || b == nil {
		t.Fatal("New returned nil")
	}
	if a == b {
		t.Fatal("New returned the same instance for two calls")
	}
	if a == Get() || b == Get() {
		t.Fatal("New aliased the package-global instance returned by Get()")
	}

	// firstReqSeen must not be shared.
	a.firstReqSeen = true
	if b.firstReqSeen {
		t.Fatal("firstReqSeen is shared between independent New() instances")
	}

	// buffers must be distinct backing arrays.
	a.buffer = append(a.buffer, &models.Mock{})
	if len(b.buffer) != 0 {
		t.Fatalf("buffer is shared: b has %d mocks after appending to a", len(b.buffer))
	}
	if len(Get().buffer) != 0 {
		t.Fatalf("global buffer mutated by New() instance: %d mocks", len(Get().buffer))
	}
}

// TestNewDedupQueueIndependent verifies per-session dedup queues (O1b)
// share no state, so one session's FIFO ordering can't bleed into another.
func TestNewDedupQueueIndependent(t *testing.T) {
	t.Parallel()

	qa := NewDedupQueue()
	qb := NewDedupQueue()

	if qa == qb || qa == GetDedupQueue() || qb == GetDedupQueue() {
		t.Fatal("NewDedupQueue aliased another queue or the global")
	}

	qa.Enqueue(time.Now())
	if len(qb.queue) != 0 {
		t.Fatalf("dedup queue shared: qb has %d jobs after enqueue on qa", len(qb.queue))
	}
	if len(GetDedupQueue().queue) != 0 {
		t.Fatalf("global dedup queue mutated by NewDedupQueue instance: %d jobs", len(GetDedupQueue().queue))
	}
}

// TestNextTestIDPerInstance verifies per-session test-ID numbering (O4):
// each manager numbers from 1 independently, so concurrent sessions don't
// share a counter.
func TestNextTestIDPerInstance(t *testing.T) {
	t.Parallel()

	a := New(nil)
	b := New(nil)

	if got := a.NextTestID(); got != 1 {
		t.Fatalf("first NextTestID on a = %d, want 1", got)
	}
	if got := a.NextTestID(); got != 2 {
		t.Fatalf("second NextTestID on a = %d, want 2", got)
	}
	if got := b.NextTestID(); got != 1 {
		t.Fatalf("first NextTestID on b = %d, want 1 (counter leaked from a)", got)
	}
}

// TestManagerDedupQueueOwnership verifies New() managers own a private dedup
// queue while the package-global instance falls back to globalDedupQueue —
// so callers can switch GetDedupQueue() → mgr.DedupQueue() for per-session
// dedup without changing single-session behaviour.
func TestManagerDedupQueueOwnership(t *testing.T) {
	t.Parallel()

	a := New(nil)
	b := New(nil)
	if a.DedupQueue() == b.DedupQueue() {
		t.Fatal("two New() managers share a dedup queue")
	}
	if a.DedupQueue() == GetDedupQueue() {
		t.Fatal("a New() manager's queue aliases the global queue")
	}
	if Get().DedupQueue() != GetDedupQueue() {
		t.Fatal("the global manager's DedupQueue() must be the package-global queue")
	}
}
