package pkg

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestWaitForPort_Success(t *testing.T) {
	// Start a listener after a short delay to simulate app startup.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	_, port, _ := net.SplitHostPort(ln.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := WaitForPort(ctx, "127.0.0.1", port, 5*time.Second, nil); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestWaitForPort_DelayedStart(t *testing.T) {
	// Pick an available port but don't listen yet.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close() // free the port

	// Start listening after a short delay.
	go func() {
		time.Sleep(2 * time.Second)
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
		if err != nil {
			return
		}
		defer l.Close()
		// Keep listener open until test finishes.
		time.Sleep(10 * time.Second)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := WaitForPort(ctx, "127.0.0.1", port, 0, nil); err != nil {
		t.Fatalf("expected nil error after delayed start, got: %v", err)
	}
}

func TestWaitForPort_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately to test fast exit.
	cancel()

	err := WaitForPort(ctx, "127.0.0.1", "19999", 0, nil)
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestWaitForPort_Timeout(t *testing.T) {
	err := WaitForPort(context.Background(), "127.0.0.1", "19998", 2*time.Second, nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}
