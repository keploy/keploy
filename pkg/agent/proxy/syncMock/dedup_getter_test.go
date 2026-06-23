package manager

import "testing"

// TestDedupQueueGetterPerInstanceVsGlobal pins the per-app dedup isolation
// contract that the multi-app consumer opts into via mgr.DedupQueue(): a New()
// manager exposes its own private queue (distinct per instance and from the
// global), the same instance across calls, while the package-global manager and
// a nil receiver fall back to the global queue. A future refactor that dropped
// the per-instance dedupQueue (the "half-wired" concern) would fail here.
func TestDedupQueueGetterPerInstanceVsGlobal(t *testing.T) {
	t.Parallel()

	a := New(nil)
	b := New(nil)

	if a.DedupQueue() == nil || a.DedupQueue() == GetDedupQueue() {
		t.Fatal("New() instance DedupQueue() must be a private queue, not the package-global")
	}
	if a.DedupQueue() == b.DedupQueue() {
		t.Fatal("two New() instances share a dedup queue")
	}
	if a.DedupQueue() != a.DedupQueue() {
		t.Fatal("DedupQueue() must return the same instance across calls")
	}

	if Get().DedupQueue() != GetDedupQueue() {
		t.Fatal("package-global manager DedupQueue() must fall back to the global queue")
	}

	var nilMgr *SyncMockManager
	if nilMgr.DedupQueue() != GetDedupQueue() {
		t.Fatal("nil manager DedupQueue() must fall back to the global queue")
	}
}
