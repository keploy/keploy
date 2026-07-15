package routes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// grabPort binds an ephemeral port and returns the listener (still holding the
// port), the numeric port, and the ":port" address StartAgentServer would use.
func grabPort(t *testing.T) (net.Listener, int, string) {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen ephemeral: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return ln, port, fmt.Sprintf(":%d", port)
}

// TestListenWithRetry_RecoversWhenPortFrees reproduces the couchbase-node flake:
// a previous session's agent still holds the published host port when this agent
// binds. The bind must not hard-fail — once the departing owner releases the
// port the retry acquires it.
func TestListenWithRetry_RecoversWhenPortFrees(t *testing.T) {
	holder, _, addr := grabPort(t)

	// Release the port shortly, mimicking the previous agent finishing teardown.
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = holder.Close()
	}()

	ln, err := listenWithRetry(context.Background(), zap.NewNop(), addr, 5*time.Second, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("expected bind to recover after the port freed, got: %v", err)
	}
	_ = ln.Close()
}

// TestListenWithRetry_GivesUpAfterBudget ensures the retry is bounded: a port
// that never frees fails after the budget rather than hanging forever.
func TestListenWithRetry_GivesUpAfterBudget(t *testing.T) {
	holder, _, addr := grabPort(t)
	defer func() { _ = holder.Close() }()

	start := time.Now()
	_, err := listenWithRetry(context.Background(), zap.NewNop(), addr, 300*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the port stays occupied past the budget")
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("gave up before the budget elapsed: %v", elapsed)
	}
}

// TestListenWithRetry_HonorsContextCancel ensures a cancelled context unblocks
// the retry loop promptly instead of waiting out the budget.
func TestListenWithRetry_HonorsContextCancel(t *testing.T) {
	holder, _, addr := grabPort(t)
	defer func() { _ = holder.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := listenWithRetry(ctx, zap.NewNop(), addr, 10*time.Second, 50*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("did not honor cancel promptly: %v", elapsed)
	}
}

// TestListenWithRetry_ReturnsImmediatelyOnNonAddrInUse ensures the retry is
// strictly for "address already in use": any other listen error (here, an
// out-of-range port) surfaces at once and is never retried — the retry must not
// mask a genuine misconfiguration.
func TestListenWithRetry_ReturnsImmediatelyOnNonAddrInUse(t *testing.T) {
	start := time.Now()
	_, err := listenWithRetry(context.Background(), zap.NewNop(), "127.0.0.1:99999", 10*time.Second, time.Second)
	if err == nil {
		t.Fatal("expected an error for an invalid port")
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("an out-of-range port must not be classified as address-in-use: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a non-address-in-use error must not be retried: took %v", elapsed)
	}
}

// TestStartAgentServer_ServesAfterTransientPortConflict is the end-to-end proof:
// the agent HTTP server starts while the port is still held, and begins serving
// once it frees — exactly the previous-session-teardown race from CI.
func TestStartAgentServer_ServesAfterTransientPortConflict(t *testing.T) {
	holder, port, _ := grabPort(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Release the port after the server has already started retrying.
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = holder.Close()
	}()

	go StartAgentServer(ctx, zap.NewNop(), port, handler)

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return // server recovered and is serving
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent HTTP server never started serving after the port freed (last err: %v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
