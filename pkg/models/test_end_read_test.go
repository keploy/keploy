package models

import (
	"context"
	"testing"
)

// A context names the test whose end it reads only once marked with a name,
// and a context derived from a marked one keeps the name.
func TestTestEndReadIsOnlyAContextMarkedWithAName(t *testing.T) {
	type key struct{}
	if test, ok := TestEndRead(context.Background()); ok || test != "" {
		t.Fatalf("an unmarked context reads as test %q's end", test)
	}
	if test, ok := TestEndRead(WithTestEndRead(context.Background(), "")); ok || test != "" {
		t.Fatalf("a context marked with no name reads as test %q's end", test)
	}
	marked := WithTestEndRead(context.Background(), "test-2")
	if test, ok := TestEndRead(marked); !ok || test != "test-2" {
		t.Fatalf("a marked context reads as %q (%v), want test-2's end", test, ok)
	}
	if test, ok := TestEndRead(context.WithValue(marked, key{}, 1)); !ok || test != "test-2" {
		t.Fatalf("a context derived from a marked one reads as %q (%v), want test-2's end", test, ok)
	}
	if SetEndRead(marked) {
		t.Fatal("a test's end read reads as the set's end")
	}
}
