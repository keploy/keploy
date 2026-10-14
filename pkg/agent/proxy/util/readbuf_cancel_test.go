package util

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A read that is aborted because the proxy is SHUTTING DOWN must stay
// recognisable as a cancellation all the way up to the caller.
//
// utils.LogError deliberately swallows context.Canceled — that is how the agent
// avoids shouting about connections it tore down itself. That guard only works
// if the cancellation survives the return: a helper that logs the real error and
// then hands back a fresh errors.New() has destroyed the very fact the guard
// tests for, so the next LogError up the stack reports an ordinary shutdown at
// ERROR. That is not cosmetic — the node lanes fail a recording whenever the
// word ERROR appears in the log, so a routine stop turns a green run red.
func TestReadHelpersPreserveCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(context.Context, *zap.Logger, net.Conn) ([]byte, error)
	}{
		{"ReadInitialBuf", ReadInitialBuf},
		{"ReadHTTPHeadersUntilEnd", ReadHTTPHeadersUntilEnd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A pipe nobody ever writes to: the only way out is the context.
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			defer func() { _ = server.Close() }()

			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(20 * time.Millisecond)
				cancel()
				// Then close the peer, exactly as shutdown does. ReadBytes runs
				// its read in a goroutine and waits for it on the way out, so a
				// blocked Read is only released when the conn goes away; without
				// this the helper could never return and the test would only
				// fail at its 10s deadline.
				time.Sleep(10 * time.Millisecond)
				_ = client.Close()
			}()

			errCh := make(chan error, 1)
			go func() {
				_, callErr := tc.call(ctx, zap.NewNop(), server)
				errCh <- callErr
			}()

			var err error
			select {
			case err = <-errCh:
			case <-time.After(10 * time.Second):
				t.Fatal("helper did not return within 10s of the context being cancelled and the conn closed")
			}
			if err == nil {
				t.Fatal("expected an error when the context is cancelled mid-read")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was lost: errors.Is(err, context.Canceled) is false for %[1]T(%[1]q).\n"+
					"The caller can no longer tell a shutdown from a real read failure, so it logs it at ERROR.", err)
			}
		})
	}
}

// errConn is a net.Conn whose Read always fails with a specific, non-EOF error
// that has nothing to do with shutdown.
type errConn struct {
	net.Conn
	err error
}

func (c *errConn) Read([]byte) (int, error) { return 0, c.err }

// CONTROL for TestReadHelpersPreserveCancellation: the wrap must not turn every
// read failure into something LogError swallows. A read that fails for a reason
// unrelated to shutdown has to come back NOT matching context.Canceled, so the
// agent still reports it at ERROR.
//
// This drives a NON-EOF error on purpose. A closed pipe yields io.EOF, which
// ReadInitialBuf answers on an earlier branch — a control built that way never
// reaches the wrapped return at all and passes no matter what it wraps
// (confirmed: a mutant wrapping context.Canceled unconditionally survived it).
func TestReadHelpersStillSurfaceRealFailures(t *testing.T) {
	boom := errors.New("connection reset by peer (not a shutdown)")
	for _, tc := range []struct {
		name string
		call func(context.Context, *zap.Logger, net.Conn) ([]byte, error)
	}{
		{"ReadInitialBuf", ReadInitialBuf},
		{"ReadHTTPHeadersUntilEnd", ReadHTTPHeadersUntilEnd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			defer func() { _ = server.Close() }()
			conn := &errConn{Conn: server, err: boom}

			errCh := make(chan error, 1)
			go func() {
				_, callErr := tc.call(context.Background(), zap.NewNop(), conn)
				errCh <- callErr
			}()

			var err error
			select {
			case err = <-errCh:
			case <-time.After(10 * time.Second):
				t.Fatal("helper did not return on a failing conn")
			}
			if err == nil {
				t.Fatal("expected an error from a failing read")
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("a non-shutdown failure was reported as a cancellation (%v); "+
					"LogError would swallow it and the failure would go unreported", err)
			}
			if !errors.Is(err, boom) {
				t.Fatalf("the underlying cause was dropped: %v", err)
			}
		})
	}
}

// stepConn hands back an INCOMPLETE header block on the first read and the
// injected error on every read after it. That is the only shape that reaches
// the continuation read inside ReadHTTPHeadersUntilEnd: both other tests fail
// on the FIRST ReadBytes and return before the loop is ever entered, so without
// this the continuation site is covered by nothing — a mutant reverting just
// that site to a bare readErr survives the rest of this file (verified).
//
// The injected error must be non-EOF. The EOF branch inside the loop breaks the
// select rather than the loop, so injecting EOF here spins instead of returning.
type stepConn struct {
	net.Conn
	mu   sync.Mutex
	step int
	err  error
}

func (c *stepConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step++
	if c.step == 1 {
		return copy(p, []byte("GET / HTTP/1.1\r\nHost: x\r\n")), nil
	}
	return 0, c.err
}

func readHeadersContinuation(t *testing.T, inject error) error {
	t.Helper()
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	errCh := make(chan error, 1)
	go func() {
		_, callErr := ReadHTTPHeadersUntilEnd(context.Background(), zap.NewNop(), &stepConn{Conn: server, err: inject})
		errCh <- callErr
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the continuation read never returned")
		return nil
	}
}

func TestReadHTTPHeadersContinuationPreservesCancellation(t *testing.T) {
	err := readHeadersContinuation(t, context.Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the continuation read lost the cancellation: %[1]T(%[1]q)", err)
	}
}

func TestReadHTTPHeadersContinuationSurfacesRealFailures(t *testing.T) {
	boom := errors.New("connection reset by peer (not a shutdown)")
	err := readHeadersContinuation(t, boom)
	if errors.Is(err, context.Canceled) {
		t.Fatalf("a non-shutdown failure was reported as a cancellation: %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("the underlying cause was dropped: %v", err)
	}
}
