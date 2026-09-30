package models

import (
	"context"
	"testing"
)

// HandMockDoc reaches the receiver WithMockDocReceiver installed, through the
// contexts derived from it, naming the mock; without one, or with a nil one,
// it does nothing.
func TestHandMockDoc(t *testing.T) {
	mock := &Mock{Name: "mock-1"}
	var got []string
	ctx := WithMockDocReceiver(context.Background(), func(m *Mock, doc []byte, format string) {
		got = append(got, m.Name+":"+format+":"+string(doc))
	})
	derived, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	HandMockDoc(derived, mock, []byte("a: 1\n"), "yaml")
	if len(got) != 1 || got[0] != "mock-1:yaml:a: 1\n" {
		t.Fatalf("receiver got %q, want the one document handed", got)
	}

	HandMockDoc(context.Background(), mock, []byte("b"), "json")
	if nilCtx := WithMockDocReceiver(context.Background(), nil); nilCtx != context.Background() {
		t.Fatal("a nil receiver was installed")
	}
	if len(got) != 1 {
		t.Fatalf("a document handed without this receiver reached it: %q", got)
	}
}
