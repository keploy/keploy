package http

import (
	"bytes"
	"io"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testdeadline"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

func passthroughHalfCloseExchange(t *testing.T, run func(clientConn, destConn net.Conn)) {
	t.Helper()
	type cleanupWait struct {
		name string
		done <-chan struct{}
	}
	var waits []cleanupWait
	// Registered before pair so LIFO cleanup closes the raw TCP sockets first,
	// unblocking both goroutines before we wait for them to exit.
	t.Cleanup(func() {
		for _, w := range waits {
			testdeadline.Join(t, w.name, w.done)
		}
	})

	pair := func(t *testing.T) (peer, proxySide net.Conn) {
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
		go func() { c, err := ln.Accept(); ch <- res{c, err} }()
		d, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		got := <-ch
		if got.err != nil {
			t.Fatalf("accept: %v", got.err)
		}
		t.Cleanup(func() { _ = d.Close(); _ = got.c.Close() })
		return d, proxyutil.NewSafeConn(got.c, zap.NewNop())
	}

	clientApp, srcProxy := pair(t)
	destSvc, dstProxy := pair(t)

	runDone := make(chan struct{})
	waits = append(waits, cleanupWait{name: "passthroughHalfCloseExchange run goroutine", done: runDone})
	go func() {
		defer close(runDone)
		run(srcProxy, dstProxy)
	}()

	request := []byte("request-then-eof\n")
	reply := []byte("reply-after-eof\n")

	srvDone := make(chan error, 1)
	srvExited := make(chan struct{})
	waits = append(waits, cleanupWait{name: "passthroughHalfCloseExchange service goroutine", done: srvExited})
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

	expired, stop := testdeadline.Before(t)
	defer stop()
	select {
	case err := <-srvDone:
		if err != nil {
			t.Fatalf("service: %v", err)
		}
	case <-expired:
		t.Fatal("the service never saw EOF: the FIN was not forwarded through the SafeConn " +
			"wrapper, so an EOF-delimited peer waits for bytes that will never come while " +
			"the client waits for a reply that will never be sent")
	}

	testdeadline.SetRead(t, clientApp)
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(clientApp, got); err != nil {
		t.Fatalf("client never received the reply: %v", err)
	}
	if !bytes.Equal(got, reply) {
		t.Fatalf("client got % x, want % x", got, reply)
	}
}

// relayRawPassthrough is the memory-pressure fallback: nothing is
// captured, bytes are just relayed. It still has to forward each side's
// FIN, or an EOF-delimited peer never learns the request ended and the
// exchange deadlocks until something external tears it down.
//
// This was shipped uncovered once — the block was inline inside the
// memoryguard branch and memoryguard's paused flag is unexported, so no
// unit test could reach it. Extracting the loop was the fix; a test hook
// in production code was the alternative and a worse trade.
//
// The conns are wrapped as the proxy wraps them. util.SafeConn holds its
// conn as an unexported, NON-embedded field, so a FIN forwarded through
// a raw *net.TCPConn proves nothing about production — that is exactly
// how the same one-liner shipped as a silent no-op elsewhere.
func TestRelayRawPassthroughForwardsTheFIN_HTTP(t *testing.T) {
	passthroughHalfCloseExchange(t, relayRawPassthrough)
}
