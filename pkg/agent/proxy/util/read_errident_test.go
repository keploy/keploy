package util

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ReadInitialBuf and ReadHTTPHeadersUntilEnd used to return a bare
// errors.New(...), discarding the cause. Every caller that classifies the
// result with errors.Is — Proxy.isShutdownError, Proxy.isNetworkClosedErr —
// therefore saw an unrecognisable string and logged an ordinary connection
// teardown at ERROR. Since the proxy closes each connection's socket itself at
// teardown, keploy was reporting its own shutdown as a fault, twice per
// connection.
//
// These tests pin the property that actually matters: the CAUSE survives, so
// errors.Is can still classify it.

// dialPair returns a connected client/server pair over real TCP.
func dialPair(t *testing.T) (client net.Conn, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	r := <-ch
	require.NoError(t, r.err)
	t.Cleanup(func() { _ = client.Close(); _ = r.c.Close() })
	return client, r.c
}

// Reading a socket that this side has already closed is the teardown case the
// proxy hits on every connection. The cause must remain net.ErrClosed.
func TestReadInitialBuf_PreservesClosedConnIdentity(t *testing.T) {
	client, _ := dialPair(t)
	require.NoError(t, client.Close())

	_, err := ReadInitialBuf(context.Background(), zap.NewNop(), client)
	require.Error(t, err)
	require.True(t, errors.Is(err, net.ErrClosed),
		"cause was discarded; callers can no longer tell a teardown from a fault: %v", err)
}

// NOTE, established by running the obvious version of this test and watching it
// hang for 600s: ReadBytes CANNOT be interrupted by context cancellation alone
// on an idle socket. Its deferred g.Wait() (util.go:520-526) blocks on the read
// goroutine, and that goroutine is parked in reader.Read until the socket
// yields something. Cancellation only unblocks it because the proxy separately
// CLOSES the socket at teardown (Proxy.startConnCloser). So the teardown error
// that actually reaches these functions is net.ErrClosed — covered above — not
// context.Canceled, and there is no way to drive the latter here without a
// second goroutine racing the close. Recorded rather than asserted.

// A read deadline expiring must stay classifiable as a timeout. It is
// deliberately NOT treated as benign — a deadline mid-stream can mean a parser
// hung — so this only asserts the identity survives for the caller to decide.
func TestReadInitialBuf_PreservesTimeoutIdentity(t *testing.T) {
	client, _ := dialPair(t)
	require.NoError(t, client.SetReadDeadline(time.Now().Add(20*time.Millisecond)))

	_, err := ReadInitialBuf(context.Background(), zap.NewNop(), client)
	require.Error(t, err)
	var ne net.Error
	require.True(t, errors.As(err, &ne) && ne.Timeout(), "timeout identity was discarded: %v", err)
}

// A peer that opens a connection and closes it without writing yields a clean
// EOF with no bytes. That shape is returned as io.EOF itself and must remain
// distinguishable, because the accept loop suppresses exactly that.
func TestReadInitialBuf_EmptyCloseIsPlainEOF(t *testing.T) {
	client, server := dialPair(t)
	require.NoError(t, server.Close())

	_, err := ReadInitialBuf(context.Background(), zap.NewNop(), client)
	require.True(t, errors.Is(err, io.EOF), "expected io.EOF, got %v", err)
}

// Same property for the headers reader, which had the identical defect and,
// worse, returned its bare error even for a clean zero-byte EOF.
func TestReadHTTPHeadersUntilEnd_PreservesIdentity(t *testing.T) {
	client, _ := dialPair(t)
	require.NoError(t, client.Close())

	_, err := ReadHTTPHeadersUntilEnd(context.Background(), zap.NewNop(), client)
	require.Error(t, err)
	require.True(t, errors.Is(err, net.ErrClosed), "cause was discarded: %v", err)
}
