package testdeadline

import (
	"net"
	"testing"
)

func TestBoundMatchesTestDeadline(t *testing.T) {
	bound, ok := Bound(t)
	dl, hasDL := t.Deadline()
	if ok != hasDL {
		t.Fatalf("Bound ok = %v, want %v (t.Deadline)", ok, hasDL)
	}
	if !hasDL {
		if !bound.IsZero() {
			t.Fatalf("Bound with no test deadline = %v, want zero", bound)
		}
		ch, stop := Before(t)
		defer stop()
		if ch != nil {
			t.Fatal("Before with no test deadline returned non-nil channel")
		}
		return
	}
	if !bound.Before(dl) {
		t.Fatalf("Bound %v is not before t.Deadline %v", bound, dl)
	}
	ch, stop := Before(t)
	defer stop()
	if ch == nil {
		t.Fatal("Before with a test deadline returned nil channel")
	}
}

func TestSetReadWriteAndJoin(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	SetWrite(t, a)
	SetRead(t, b)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Write([]byte("ok"))
	}()
	buf := make([]byte, 2)
	if _, err := b.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ok" {
		t.Fatalf("got %q, want %q", buf, "ok")
	}
	Join(t, "writer goroutine", done)
}
