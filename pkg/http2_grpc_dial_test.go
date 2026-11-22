package pkg

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/testnet"
	"go.uber.org/zap"
)

// TestDialTCPWithConnRefusedRetry_RetriesBoundedOnPersistentRefusal proves the
// gRPC dial retries a connection-refused (the app still coming up) and is bounded
// — it does NOT give up on the first refusal (the old single-shot behavior that
// false-failed a slow-starting gRPC app), nor hang forever.
func TestDialTCPWithConnRefusedRetry_RetriesBoundedOnPersistentRefusal(t *testing.T) {
	addr := testnet.RefusedAddr(t, "tcp")

	// The row's own stop ends a dial that waits with no bound (one that
	// took this context for an application keploy started), so the row
	// fails by its own check, not by the test binary's timeout.
	ctx, cancel := context.WithCancel(context.Background())
	type dialResult struct {
		conn net.Conn
		err  error
	}
	done := make(chan dialResult, 1)
	t.Cleanup(cancel)
	start := time.Now()
	go func() {
		conn, err := dialTCPWithConnRefusedRetry(ctx, zap.NewNop(), addr, nil, 0)
		done <- dialResult{conn, err}
	}()
	var conn net.Conn
	var err error
	select {
	case r := <-done:
		conn, err = r.conn, r.err
	// Four refused dials take about 9 s on Windows, under a second on Linux.
	case <-time.After(30 * time.Second):
		t.Fatal("the bounded re-dials did not end on a port with no listener")
	}
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
	// The port is kept for the test's life (testnet.SharedPort): a dial to
	// it is refused until the listener opens there, and no bind to port 0 is
	// given it meanwhile.
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))

	ready := make(chan net.Listener, 1)
	go func() {
		time.Sleep(connRefusedRetryBackoff / 2)
		l, lerr := testnet.OpenShared("tcp4", addr)
		if lerr != nil {
			ready <- nil
			return
		}
		ready <- l
		if c, aerr := l.Accept(); aerr == nil && c != nil {
			_ = c.Close()
		}
	}()

	conn, err := dialTCPWithConnRefusedRetry(context.Background(), zap.NewNop(), addr, nil, 0)
	l := <-ready
	if l != nil {
		_ = l.Close()
	}
	if l == nil {
		t.Fatal("the listener could not open at the held port")
	}
	if err != nil {
		t.Fatalf("expected the dial to retry and connect once the listener opened, got: %v", err)
	}
	_ = conn.Close()
}

// TestDialTCPWithConnRefusedRetry_RespectsContext proves a cancelled context
// aborts the retry loop promptly instead of running all backoffs.
func TestDialTCPWithConnRefusedRetry_RespectsContext(t *testing.T) {
	addr := testnet.RefusedAddr(t, "tcp")

	ctx, cancel := context.WithTimeout(context.Background(), connRefusedRetryBackoff/2)
	defer cancel()
	if _, err := dialTCPWithConnRefusedRetry(ctx, zap.NewNop(), addr, nil, 0); err == nil {
		t.Fatal("expected an error when the context is cancelled mid-retry")
	}
}
