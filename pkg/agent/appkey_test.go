package agent

import (
	"context"
	"testing"
)

func TestAppKeyContextRoundTrip(t *testing.T) {
	ctx := WithAppKey(context.Background(), AppKey("ns/dep/ts-1"))
	got, ok := AppKeyFromContext(ctx)
	if !ok {
		t.Fatalf("expected key present on ctx")
	}
	if got != AppKey("ns/dep/ts-1") {
		t.Fatalf("got %q, want %q", got, "ns/dep/ts-1")
	}
	if AppKeyOrDefault(ctx) != AppKey("ns/dep/ts-1") {
		t.Fatalf("AppKeyOrDefault mismatch")
	}
}

func TestAppKeyDefaultWhenAbsent(t *testing.T) {
	if got, ok := AppKeyFromContext(context.Background()); ok || got != DefaultAppKey {
		t.Fatalf("expected (DefaultAppKey, false), got (%q, %v)", got, ok)
	}
	if AppKeyOrDefault(context.Background()) != DefaultAppKey {
		t.Fatalf("expected DefaultAppKey from empty ctx")
	}
	if got, ok := AppKeyFromContext(nil); ok || got != DefaultAppKey { //nolint:staticcheck // nil ctx is a guarded edge
		t.Fatalf("expected (DefaultAppKey, false) for nil ctx, got (%q, %v)", got, ok)
	}
}
