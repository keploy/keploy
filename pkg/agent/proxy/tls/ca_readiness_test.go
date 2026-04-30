package tls

import (
	"errors"
	"testing"
)

// TestCAStatus_InitialState verifies the baseline after
// ResetCAReadyForTest: the CA is not ready and no failure is
// recorded. This is the default operators see during the boot
// window before SetupCA has completed.
func TestCAStatus_InitialState(t *testing.T) {
	ResetCAReadyForTest()

	ready, err := CAStatus()
	if ready {
		t.Fatalf("CAStatus.ready: got true, want false")
	}
	if err != nil {
		t.Fatalf("CAStatus.err: got %v, want nil", err)
	}
}

// TestCAStatus_AfterReady verifies that closing the channel (the
// production path via markCAReady) flips ready→true with no error.
func TestCAStatus_AfterReady(t *testing.T) {
	ResetCAReadyForTest()
	CloseCAReadyForTest()

	ready, err := CAStatus()
	if !ready {
		t.Fatalf("CAStatus.ready: got false, want true")
	}
	if err != nil {
		t.Fatalf("CAStatus.err: got %v, want nil", err)
	}
}

// TestCAStatus_AfterFailure verifies MarkCAFailed is observed by
// CAStatus without closing the readiness channel — this is the
// critical invariant that lets the /agent/ready handler surface a
// terminal error as a 503 body instead of polling forever.
func TestCAStatus_AfterFailure(t *testing.T) {
	ResetCAReadyForTest()
	want := errors.New("synthetic: CA store unwritable")
	MarkCAFailed(want)

	ready, err := CAStatus()
	if ready {
		t.Fatalf("CAStatus.ready: got true, want false (failure must NOT latch readiness)")
	}
	if !errors.Is(err, want) {
		t.Fatalf("CAStatus.err: got %v, want %v", err, want)
	}

	// ResetCAReadyForTest must clear the failure so subsequent
	// tests see a clean baseline.
	ResetCAReadyForTest()
	if _, err := CAStatus(); err != nil {
		t.Fatalf("after reset: err not cleared: %v", err)
	}
}

// TestMarkCAFailed_NilIsNoOp documents the contract that
// MarkCAFailed(nil) does nothing — callers can plumb the return of
// SetupCA unconditionally without a nil-check on the hot path.
func TestMarkCAFailed_NilIsNoOp(t *testing.T) {
	ResetCAReadyForTest()
	MarkCAFailed(nil)

	_, err := CAStatus()
	if err != nil {
		t.Fatalf("MarkCAFailed(nil) should be a no-op, got err=%v", err)
	}
}
