package manager

import (
	"context"
	"testing"
)

// TestFromContextOrGlobal verifies the resolution seam used by parser
// emit sites (O2): a manager carried on the context is returned, and the
// absence of one falls back to the package global — keeping single-session
// behaviour unchanged.
func TestFromContextOrGlobal(t *testing.T) {
	t.Parallel()

	// No manager on the context → global default.
	if got := FromContextOrGlobal(context.Background()); got != Get() {
		t.Fatal("FromContextOrGlobal should fall back to the global when ctx carries no manager")
	}

	// nil context → nil from FromContext, global from FromContextOrGlobal.
	if FromContext(nil) != nil { //nolint:staticcheck // explicitly testing the nil-ctx guard
		t.Fatal("FromContext(nil) must be nil")
	}

	// A per-session manager on the context is returned verbatim.
	m := New(nil)
	ctx := NewContext(context.Background(), m)
	if got := FromContext(ctx); got != m {
		t.Fatal("FromContext did not return the carried manager")
	}
	if got := FromContextOrGlobal(ctx); got != m {
		t.Fatal("FromContextOrGlobal did not prefer the carried manager")
	}
	if m == Get() {
		t.Fatal("carried manager must not be the global instance")
	}
}
