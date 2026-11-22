package models

import (
	"reflect"
	"testing"
)

// A mock's request keys are what its kind's parser declared, none for a kind
// with none; unregistering puts back what was there.
func TestRequestKeysAreTheParsersDeclaration(t *testing.T) {
	m := &Mock{Name: "m", Kind: HTTP}
	if got := RequestKeys(m); got != nil {
		t.Fatalf("a kind with no declared keys gave %q", got)
	}
	undo := RegisterRequestKeys(HTTP, func(*Mock) []string { return []string{"GET /a", "GET #1"} })
	if got := RequestKeys(m); !reflect.DeepEqual(got, []string{"GET /a", "GET #1"}) {
		t.Fatalf("RequestKeys = %q, want the declared GET /a, GET #1", got)
	}
	if got := RequestKeys(&Mock{Kind: Mongo}); got != nil {
		t.Fatalf("another kind's mock gave %q", got)
	}
	undo()
	if got := RequestKeys(m); got != nil || RequestKeys(nil) != nil {
		t.Fatalf("after unregistering RequestKeys = %q", got)
	}
}
