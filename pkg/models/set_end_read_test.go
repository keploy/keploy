package models

import (
	"context"
	"testing"
)

// A context is a set's end read only once marked, and a context derived from
// a marked one stays marked.
func TestSetEndReadIsOnlyAMarkedContext(t *testing.T) {
	type key struct{}
	if SetEndRead(context.Background()) {
		t.Fatal("an unmarked context reads as a set's end")
	}
	marked := WithSetEndRead(context.Background())
	if !SetEndRead(marked) {
		t.Fatal("a marked context does not read as a set's end")
	}
	if !SetEndRead(context.WithValue(marked, key{}, 1)) {
		t.Fatal("a context derived from a marked one lost the mark")
	}
}
