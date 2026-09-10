package recorder

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

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
func TestRelayRawPassthroughForwardsTheFIN_MySQL(t *testing.T) {
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

	go relayRawPassthrough(srcProxy, dstProxy)

	request := []byte("request-then-eof\n")
	reply := []byte("reply-after-eof\n")

	srvDone := make(chan error, 1)
	go func() {
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

	select {
	case err := <-srvDone:
		if err != nil {
			t.Fatalf("service: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the service never saw EOF: the FIN was not forwarded through the SafeConn " +
			"wrapper, so an EOF-delimited peer waits for bytes that will never come while " +
			"the client waits for a reply that will never be sent")
	}

	_ = clientApp.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(clientApp, got); err != nil {
		t.Fatalf("client never received the reply: %v", err)
	}
}
