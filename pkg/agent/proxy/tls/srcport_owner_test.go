package tls

import "testing"

// TestReleaseSrcPortIfOwner_RecycleClobber pins the fix for the source-port
// recycle race: an older connection's deferred cleanup must not delete the
// SrcPortToDstURL mapping of a NEW connection that reused the same (recycled)
// source port. Under the previous unconditional `SrcPortToDstURL.Delete`, the
// final assertion (B's mapping survives A's cleanup) failed.
func TestReleaseSrcPortIfOwner_RecycleClobber(t *testing.T) {
	const port = 54321
	const tokenA int64 = 1001
	const tokenB int64 = 1002

	// Connection A owns the port and stores its dest.
	ClaimSrcPort(port, tokenA)
	SrcPortToDstURL.Store(port, "appA.svc:443")

	// The OS recycles the port to a new connection B (only possible after A's
	// socket closed) before A's deferred cleanup runs. B claims it and stores
	// its own dest, overwriting the ownership.
	ClaimSrcPort(port, tokenB)
	SrcPortToDstURL.Store(port, "appB.svc:443")

	// A's cleanup runs last (LIFO, after srcConn.Close freed the port). It must
	// be a no-op because A no longer owns the port.
	if ReleaseSrcPortIfOwner(port, tokenA) {
		t.Fatal("stale owner A deleted the recycled port's mapping")
	}
	if v, ok := SrcPortToDstURL.Load(port); !ok || v.(string) != "appB.svc:443" {
		t.Fatalf("B's mapping was clobbered by A's cleanup: got %v (ok=%v)", v, ok)
	}

	// B's own cleanup removes both its ownership and its mapping.
	if !ReleaseSrcPortIfOwner(port, tokenB) {
		t.Fatal("owner B failed to release its own port")
	}
	if _, ok := SrcPortToDstURL.Load(port); ok {
		t.Fatal("B's mapping not deleted by its own cleanup")
	}
	if _, ok := srcPortOwner.Load(port); ok {
		t.Fatal("owner entry leaked after release")
	}
}

// TestReleaseSrcPortIfOwner_NormalLifecycle covers the common single-owner
// case: claim, store, release deletes the mapping and the owner entry.
func TestReleaseSrcPortIfOwner_NormalLifecycle(t *testing.T) {
	const port = 54322
	const token int64 = 2001

	ClaimSrcPort(port, token)
	SrcPortToDstURL.Store(port, "x:443")

	if !ReleaseSrcPortIfOwner(port, token) {
		t.Fatal("owner failed to release its own port")
	}
	if _, ok := SrcPortToDstURL.Load(port); ok {
		t.Fatal("mapping not deleted by the owner's cleanup")
	}
	if _, ok := srcPortOwner.Load(port); ok {
		t.Fatal("owner entry leaked after release")
	}

	// A second release (e.g. a non-CONNECT connection with nothing stored) is a
	// harmless no-op.
	if ReleaseSrcPortIfOwner(port, token) {
		t.Fatal("release reported a delete for an already-released port")
	}
}
