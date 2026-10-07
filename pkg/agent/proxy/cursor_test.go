package proxy

import (
	"testing"

	"go.uber.org/zap"
)

// TestMockCursorPeekAdvanceSaturateReset exercises the real MockManager
// stateful-cursor primitive: peek returns the record-ordered position without
// advancing, advance moves one past (saturating at the last recorded index so a
// hot repeated request never grows the cursor unbounded), and
// ResetStatefulCursors sends it back to the start (the per-test-set boundary).
func TestMockCursorPeekAdvanceSaturateReset(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	const key, n = "GET /counter\x00h", 3

	// Peek is idempotent (no advance); advance moves the cursor on.
	for want := 0; want <= 2; want++ {
		if got := mm.MockCursorIndex(key, n); got != want {
			t.Fatalf("peek #%d = %d, want %d", want, got, want)
		}
		if got := mm.MockCursorIndex(key, n); got != want {
			t.Fatalf("peek is not idempotent: second read = %d, want %d", got, want)
		}
		mm.AdvanceMockCursor(key, n)
	}
	// Past the end: saturate on the last index, and the stored cursor stays
	// bounded (does not grow) across repeated over-reads.
	for i := 0; i < 5; i++ {
		if got := mm.MockCursorIndex(key, n); got != n-1 {
			t.Fatalf("saturate read %d = %d, want %d", i, got, n-1)
		}
		mm.AdvanceMockCursor(key, n)
	}

	// Reset (per-test-set boundary) sends the sequence back to the start.
	mm.ResetStatefulCursors()
	if got := mm.MockCursorIndex(key, n); got != 0 {
		t.Fatalf("after reset, peek = %d, want 0", got)
	}

	// A single recording is always index 0 and advance is a no-op (legacy reuse).
	if got := mm.MockCursorIndex("solo", 1); got != 0 {
		t.Fatalf("n=1 peek = %d, want 0", got)
	}
	mm.AdvanceMockCursor("solo", 1)
	if got := mm.MockCursorIndex("solo", 1); got != 0 {
		t.Fatalf("n=1 after advance = %d, want 0 (single recording never advances)", got)
	}
}
