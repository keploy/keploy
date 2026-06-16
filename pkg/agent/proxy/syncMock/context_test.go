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

// fakeStaticDeduper is a minimal StaticDeduper for the context round-trip.
type fakeStaticDeduper struct{}

func (fakeStaticDeduper) IsDuplicate(string) bool                          { return false }
func (fakeStaticDeduper) GetCustomFieldsForEndpoint(string, string, int) []string { return nil }

// TestStaticDeduperContext verifies the per-app static-deduper context
// carrier: a deduper set via WithStaticDeduper is returned, and absence
// yields nil (single-app fallback).
func TestStaticDeduperContext(t *testing.T) {
	t.Parallel()

	if StaticDeduperFromContext(context.Background()) != nil {
		t.Fatal("no deduper on ctx should yield nil")
	}
	if StaticDeduperFromContext(nil) != nil { //nolint:staticcheck // testing nil-ctx guard
		t.Fatal("nil ctx should yield nil")
	}
	d := fakeStaticDeduper{}
	ctx := WithStaticDeduper(context.Background(), d)
	if got := StaticDeduperFromContext(ctx); got != d {
		t.Fatal("StaticDeduperFromContext did not return the carried deduper")
	}
}
