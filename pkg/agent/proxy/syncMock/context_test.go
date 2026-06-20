package manager

import (
	"context"
	"testing"
)

// TestGetForContext_PerSessionIsolation verifies the DaemonSet multi-app path:
// distinct session keys get distinct managers, the same key is stable, and the
// no-key path is the global instance (sidecar behaviour unchanged).
func TestGetForContext_PerSessionIsolation(t *testing.T) {
	// No key on ctx -> global instance, identical to Get().
	if GetForContext(context.Background()) != Get() {
		t.Fatal("no-key GetForContext should return the global instance")
	}
	if GetForContext(nil) != Get() { //nolint:staticcheck // explicitly testing nil ctx
		t.Fatal("nil ctx GetForContext should return the global instance")
	}

	ctxA := WithSessionKey(context.Background(), "ns-a/app-a/test-set-0")
	ctxB := WithSessionKey(context.Background(), "ns-b/app-b/test-set-1")

	a1 := GetForContext(ctxA)
	a2 := GetForContext(ctxA)
	b1 := GetForContext(ctxB)

	if a1 == nil || b1 == nil {
		t.Fatal("per-session managers must be non-nil")
	}
	if a1 != a2 {
		t.Error("same session key must return the same manager")
	}
	if a1 == b1 {
		t.Error("different session keys must return different managers")
	}
	if a1 == Get() || b1 == Get() {
		t.Error("per-session managers must not be the global instance")
	}

	// Teardown removes the per-session manager; a later lookup makes a fresh one.
	DeleteForContext(ctxA)
	if GetForContext(ctxA) == a1 {
		t.Error("after DeleteForContext, a new manager should be created")
	}

	// WithSessionKey("") is a no-op -> global instance.
	if GetForContext(WithSessionKey(context.Background(), "")) != Get() {
		t.Error("empty key must fall back to the global instance")
	}
}
