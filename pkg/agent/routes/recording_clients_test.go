package routes

import (
	"testing"
	"time"
)

// A stop signal waits while a recording's client holds a frame stream open,
// and goes on once every one is closed, or at once when told to stop now.
func TestWaitForRecordingClients(t *testing.T) {
	stop := make(chan struct{})
	if !WaitForRecordingClients(stop) {
		t.Fatal("waited with no client attached")
	}
	a, b := attachRecordingClient(), attachRecordingClient()
	if n := RecordingClients(); n != 2 {
		t.Fatalf("%d clients, want 2", n)
	}
	done := make(chan bool, 1)
	go func() { done <- WaitForRecordingClients(stop) }()
	a()
	a() // idempotent
	select {
	case <-done:
		t.Fatal("the wait ended with a stream still open")
	default:
	}
	b()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the wait says it was cut short")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the wait did not end once every stream closed")
	}

	c := attachRecordingClient()
	defer c()
	go func() { done <- WaitForRecordingClients(stop) }()
	close(stop)
	select {
	case ok := <-done:
		if ok {
			t.Fatal("the wait says every stream closed")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("stopNow did not end the wait")
	}
}
