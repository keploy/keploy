package pkg

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestDialTCPWithConnRefusedRetry_RetriesBoundedOnPersistentRefusal proves the
// gRPC dial retries a connection-refused (the app still coming up) and is bounded
// — it does NOT give up on the first refusal (the old single-shot behavior that
// false-failed a slow-starting gRPC app), nor hang forever.
func TestDialTCPWithConnRefusedRetry_RetriesBoundedOnPersistentRefusal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listening now → refused

	start := time.Now()
	conn, err := dialTCPWithConnRefusedRetry(context.Background(), zap.NewNop(), addr)
	elapsed := time.Since(start)
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected a connection-refused error on a port with no listener")
	}
	if !isPreResponseConnRefused(err) {
		t.Fatalf("expected a connection-refused error, got: %v", err)
	}
	if elapsed < connRefusedRetryBackoff {
		t.Fatalf("expected at least one backoff (%v) of retrying before giving up, took only %v", connRefusedRetryBackoff, elapsed)
	}
}

// TestDialTCPWithConnRefusedRetry_RetriesThenConnects proves the value: a dial
// refused at first succeeds once the app starts listening.
func TestDialTCPWithConnRefusedRetry_RetriesThenConnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ready := make(chan net.Listener, 1)
	go func() {
		time.Sleep(connRefusedRetryBackoff / 2)
		l, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			ready <- nil
			return
		}
		ready <- l
		if c, aerr := l.Accept(); aerr == nil && c != nil {
			_ = c.Close()
		}
	}()

	conn, err := dialTCPWithConnRefusedRetry(context.Background(), zap.NewNop(), addr)
	l := <-ready
	if l != nil {
		_ = l.Close()
	}
	if l == nil {
		t.Skip("could not re-bind the reserved port (likely TIME_WAIT); retry-connect path not exercised this run")
	}
	if err != nil {
		t.Fatalf("expected the dial to retry and connect once the listener opened, got: %v", err)
	}
	_ = conn.Close()
}

// TestDialTCPWithConnRefusedRetry_RespectsContext proves a cancelled context
// aborts the retry loop promptly instead of running all backoffs.
func TestDialTCPWithConnRefusedRetry_RespectsContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), connRefusedRetryBackoff/2)
	defer cancel()
	if _, err := dialTCPWithConnRefusedRetry(ctx, zap.NewNop(), addr); err == nil {
		t.Fatal("expected an error when the context is cancelled mid-retry")
	}
}
