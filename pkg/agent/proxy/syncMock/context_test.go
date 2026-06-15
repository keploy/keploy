package manager

import (
	"context"
	"testing"
)

func TestNewReturnsIndependentManagers(t *testing.T) {
	a := New()
	b := New()
	if a == b {
		t.Fatalf("New must return distinct managers")
	}
	if a == instance || b == instance {
		t.Fatalf("New must not return the global instance")
	}
	if a.buffer == nil {
		t.Fatalf("New must initialise the buffer")
	}
}

func TestFromContextFallsBackToGlobal(t *testing.T) {
	if FromContext(context.Background()) != instance {
		t.Fatalf("FromContext with no manager must fall back to the global instance")
	}
	var nilCtx context.Context // exercise the guarded nil-ctx edge
	if FromContext(nilCtx) != instance {
		t.Fatalf("FromContext(nil) must fall back to the global instance")
	}
}

func TestFromContextReturnsStampedManager(t *testing.T) {
	m := New()
	ctx := NewContext(context.Background(), m)
	if FromContext(ctx) != m {
		t.Fatalf("FromContext must return the manager stamped by NewContext")
	}
	if FromContext(ctx) == instance {
		t.Fatalf("FromContext must not fall back when a manager is present")
	}
}
