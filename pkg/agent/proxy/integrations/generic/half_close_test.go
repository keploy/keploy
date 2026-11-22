package generic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The legacy generic path is no longer reachable from the dispatcher —
// the KEPLOY_NEW_RELAY rollback knob that routed to it has been removed.
// It is still covered here because the code exists and is broken
// differently from the relay rather than identically: it waits for BOTH
// copy directions instead of tearing the second down, so it never lost
// the reply — but it never forwarded the FIN either, so an EOF-delimited
// peer never learned the request had ended and the exchange deadlocked
// until external teardown.
//
// CRITICAL to how these tests are written: the conns must be wrapped the
// way proxy.buildRecordSession wraps them. util.SafeConn holds its conn
// as an unexported, NON-EMBEDDED field, so nothing is promoted — a
// CloseWrite forwarded through a raw *net.TCPConn proves nothing about
// production, where a SafeConn is what actually arrives. An earlier
// version of this file used raw TCP conns, passed, and certified a fix
// that was a guaranteed no-op on every real code path.

// prodPair returns a connected pair where the PROXY side is wrapped
// exactly as buildRecordSession wraps it, and the peer side is raw (it
// stands in for the real client or the real service).
func prodPair(t *testing.T, withReader bool) (peer net.Conn, proxySide net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	d, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatalf("accept: %v", got.err)
	}
	t.Cleanup(func() { _ = d.Close(); _ = got.c.Close() })

	raw := got.c
	if withReader {
		return d, proxyutil.NewSafeConnWithReader(raw, raw, zap.NewNop())
	}
	return d, proxyutil.NewSafeConn(raw, zap.NewNop())
}

// halfCloseWaitDeadline derives a deadline solely from the test binary's
// deadline (leaving three quarters of the remaining time for cleanup and
// subsequent tests, and ensuring a broken half-close fails with its own
// diagnostic before testing's binary-wide timeout panic). When the test has no
// deadline (e.g. go test -timeout 0), ok is false and waits have no wall-clock
// bound.
func halfCloseWaitDeadline(t *testing.T) (time.Time, bool) {
	t.Helper()
	dl, ok := t.Deadline()
	if !ok {
		return time.Time{}, false
	}
	rem := time.Until(dl) / 4
	if rem < 0 {
		rem = 0
	}
	return time.Now().Add(rem), true
}

// beforeHalfCloseDeadline returns a timer channel that fires at
// halfCloseWaitDeadline(t) along with a stop function, or (nil, noop) when the
// test has no deadline.
func beforeHalfCloseDeadline(t *testing.T) (<-chan time.Time, func()) {
	t.Helper()
	bound, ok := halfCloseWaitDeadline(t)
	if !ok {
		return nil, func() {}
	}
	timer := time.NewTimer(time.Until(bound))
	return timer.C, func() { timer.Stop() }
}

func setReadBound(t *testing.T, conn net.Conn) {
	t.Helper()
	if bound, ok := halfCloseWaitDeadline(t); ok {
		if err := conn.SetReadDeadline(bound); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}
}

func setWriteBound(t *testing.T, conn net.Conn) {
	t.Helper()
	if bound, ok := halfCloseWaitDeadline(t); ok {
		if err := conn.SetWriteDeadline(bound); err != nil {
			t.Fatalf("set write deadline: %v", err)
		}
		return
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("clear write deadline: %v", err)
	}
}

// halfCloseExchange drives one request/EOF/response exchange through
// `run` and reports whether the reply came back.
func halfCloseExchange(t *testing.T, run func(clientConn, destConn net.Conn)) {
	t.Helper()
	var waits []func()
	// Registered before prodPair so LIFO cleanup closes the raw TCP sockets
	// first, unblocking both goroutines before we wait for them to exit.
	t.Cleanup(func() {
		for _, w := range waits {
			w()
		}
	})

	clientApp, srcProxy := prodPair(t, true) // Ingress: NewSafeConnWithReader
	destSvc, dstProxy := prodPair(t, false)  // Egress: NewSafeConn

	runDone := make(chan struct{})
	waits = append(waits, func() { <-runDone })
	go func() {
		defer close(runDone)
		run(srcProxy, dstProxy)
	}()

	request := []byte("matrix-generic\n")
	reply := []byte("fixture-ack:matrix-generic\n")

	srvDone := make(chan error, 1)
	srvExited := make(chan struct{})
	waits = append(waits, func() { <-srvExited })
	go func() {
		defer close(srvExited)
		got, err := io.ReadAll(destSvc) // returns only once the FIN arrives
		if err != nil {
			srvDone <- err
			return
		}
		if !bytes.Equal(got, request) {
			srvDone <- io.ErrUnexpectedEOF
			return
		}
		_, err = destSvc.Write(reply)
		srvDone <- err
	}()

	if _, err := clientApp.Write(request); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := clientApp.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	expired, stop := beforeHalfCloseDeadline(t)
	defer stop()
	select {
	case err := <-srvDone:
		if err != nil {
			t.Fatalf("service: %v", err)
		}
	case <-expired:
		t.Fatal("the service never saw EOF: the FIN was not forwarded through the SafeConn " +
			"wrapper, so an EOF-delimited peer never learns the request ended. It waits for " +
			"bytes that will not come while the client waits for a reply it will not send.")
	}

	setReadBound(t, clientApp)
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(clientApp, got); err != nil {
		t.Fatalf("client never received the reply after half-closing: %v", err)
	}
	if !bytes.Equal(got, reply) {
		t.Fatalf("client got % x, want % x", got, reply)
	}
}

// encodeGeneric is the path that actually runs during recording.
func TestEncodeGeneric_ForwardsTheFIN(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mocks := make(chan *models.Mock, 8)
	halfCloseExchange(t, func(clientConn, destConn net.Conn) {
		_ = encodeGeneric(ctx, zap.NewNop(), nil, clientConn, destConn,
			mocks, models.OutgoingOptions{})
	})
}

// forwardBidirectional is the memoryguard-paused passthrough.
func TestForwardBidirectional_ForwardsTheFIN(t *testing.T) {
	halfCloseExchange(t, func(clientConn, destConn net.Conn) {
		_ = forwardBidirectional(clientConn, destConn)
	})
}

// SafeConn must actually implement the capability, or every call above
// silently degrades to a no-op and the deadlock returns.
func TestSafeConnSupportsHalfClose(t *testing.T) {
	_, proxySide := prodPair(t, false)
	if _, ok := proxySide.(interface{ CloseWrite() error }); !ok {
		t.Fatal("util.SafeConn does not implement CloseWrite: it holds its conn as an " +
			"unexported non-embedded field, so nothing is promoted and every half-close " +
			"through it is silently dropped")
	}
}

// A copy that ended in an ERROR must not be reported to the peer as a
// completed request: generic is the catch-all for mongo wire and
// arbitrary binary RPC, where acting on a truncated message can mean
// committing a partial operation.
func TestForwardFIN_SkipsWhenTheCopyFailed(t *testing.T) {
	peer, proxySide := prodPair(t, false)

	forwardFIN(proxySide, io.ErrUnexpectedEOF)

	// Prove synchronously without a wall-clock sleep that the write half is
	// still open: if forwardFIN called CloseWrite on the underlying TCP socket,
	// a follow-up write fails immediately and TCP ordering guarantees peer
	// reads io.EOF before any subsequent bytes.
	sentinel := []byte("still-open")
	if _, err := proxySide.Write(sentinel); err != nil {
		t.Fatalf("a FIN was forwarded for a copy that ended in an error (write after forwardFIN failed: %v): "+
			"the peer will read that as a complete request and may act on a truncated message", err)
	}

	setReadBound(t, peer)
	got := make([]byte, len(sentinel))
	if _, err := io.ReadFull(peer, got); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal("a FIN was forwarded for a copy that ended in an error: the peer will read " +
				"that as a complete request and may act on a truncated message")
		}
		t.Fatalf("peer read after failed-copy forwardFIN: %v", err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("peer got %q, want %q", got, sentinel)
	}
}
