package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// TestRecordViaSupervisor_HalfCloseReachesTheUpstream is the test that
// three separate half-close fixes needed and none of them had.
//
// Every earlier test drove the relay directly, over raw *net.TCPConn.
// Production does neither: handleConnection wraps the client side in
// *util.Conn before recordViaSupervisor ever sees it, and util.Conn
// embeds net.Conn as an INTERFACE — so Go promotes only net.Conn's
// method set, CloseWrite is not in it, and the relay's capability
// assertion silently fails. The fix was inert on every V2 connection
// while its own unit tests stayed green.
//
// So this drives recordViaSupervisor itself, with the client side
// wrapped exactly as handleConnection wraps it, over REAL sockets
// (net.Pipe has no notion of FIN and cannot express half-close at all).
// It is the lowest level at which "does a half-closing client get its
// reply through keploy" is a meaningful question.
//
// Note which direction each subtest exercises, because they do not
// share a failure mode. A CLIENT half-close sends its FIN to dst, which
// is a real socket and has always had CloseWrite — that path was never
// inert. A SERVER half-close sends its FIN to src, which is the
// *util.Conn, and that one WAS silently dropped. Both are covered
// deliberately: testing only the first is what let the second hide.
func TestRecordViaSupervisor_HalfCloseReachesTheUpstream(t *testing.T) {
	t.Parallel()

	tcpPair := func(t *testing.T) (dialed, accepted net.Conn) {
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
		return d, got.c
	}

	clientApp, rawSrc := tcpPair(t)
	dstConn, destSvc := tcpPair(t)

	// Exactly what handleConnection builds before dispatch.
	srcConn := &util.Conn{Conn: rawSrc, Reader: rawSrc, Logger: zap.NewNop()}

	p := &Proxy{
		recordBufferCap:        64 * 1024,
		recordBufferQueueSize:  64,
		recordBufferStallGrace: 2 * time.Second,
	}

	probe := newFeedProbe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = p.recordViaSupervisor(ctx, srcConn, dstConn, probe, "test",
			make(chan *models.Mock, 8), &errgroup.Group{}, zap.NewNop(), 1, 2,
			models.OutgoingOptions{})
	}()

	request := []byte("matrix-generic\n")
	reply := []byte("fixture-ack:matrix-generic\n")

	// The service answers only once it has seen EOF — the defining shape
	// of an EOF-delimited protocol, and of the deps-generic scenario that
	// exposed this in CI.
	var srvOnce sync.Once
	srvDone := make(chan error, 1)
	go func() {
		got, err := io.ReadAll(destSvc)
		if err != nil {
			srvOnce.Do(func() { srvDone <- err })
			return
		}
		if !bytes.Equal(got, request) {
			srvOnce.Do(func() { srvDone <- io.ErrUnexpectedEOF })
			return
		}
		_, err = destSvc.Write(reply)
		srvOnce.Do(func() { srvDone <- err })
	}()

	if _, err := clientApp.Write(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := clientApp.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	select {
	case err := <-srvDone:
		if err != nil {
			t.Fatalf("upstream: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream never saw EOF. recordViaSupervisor wraps the client side in " +
			"*util.Conn, which embeds net.Conn as an interface and so does not promote " +
			"CloseWrite — the FIN is dropped and an EOF-delimited peer never learns the " +
			"request ended. Unit tests on raw sockets cannot see this.")
	}

	_ = clientApp.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(clientApp, got); err != nil {
		t.Fatalf("client never received the reply after half-closing: %v", err)
	}
	if !bytes.Equal(got, reply) {
		t.Fatalf("client got % x, want % x", got, reply)
	}
}

// TestRecordViaSupervisor_ServerHalfCloseReachesTheClient covers the
// direction that the *util.Conn wrapper actually breaks.
//
// When the SERVER half-closes, the FIN has to travel to the client — and
// the client side is the wrapped conn. Without CloseWrite on util.Conn
// the relay cannot forward it, so an application waiting for EOF to know
// the response is complete (any EOF-delimited read, io.ReadAll, a
// Content-Length-less body) hangs on a response the server considers
// finished.
func TestRecordViaSupervisor_ServerHalfCloseReachesTheClient(t *testing.T) {
	t.Parallel()

	tcpPair := func(t *testing.T) (dialed, accepted net.Conn) {
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
		return d, got.c
	}

	clientApp, rawSrc := tcpPair(t)
	dstConn, destSvc := tcpPair(t)
	srcConn := &util.Conn{Conn: rawSrc, Reader: rawSrc, Logger: zap.NewNop()}

	p := &Proxy{
		recordBufferCap:        64 * 1024,
		recordBufferQueueSize:  64,
		recordBufferStallGrace: 2 * time.Second,
	}

	probe := newFeedProbe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = p.recordViaSupervisor(ctx, srcConn, dstConn, probe, "test",
			make(chan *models.Mock, 8), &errgroup.Group{}, zap.NewNop(), 1, 2,
			models.OutgoingOptions{})
	}()

	reply := []byte("response-body-with-no-length-prefix")

	if _, err := clientApp.Write([]byte("go\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The service answers and then half-closes: "that is the whole
	// response". The client learns that only from the FIN.
	go func() {
		buf := make([]byte, 64)
		_, _ = destSvc.Read(buf)
		_, _ = destSvc.Write(reply)
		if tc, ok := destSvc.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	_ = clientApp.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(clientApp)
	if err != nil {
		t.Fatalf("the client never saw EOF after the server half-closed: %v.\n"+
			"The FIN has to reach the client through *util.Conn, which embeds net.Conn as "+
			"an interface and so does not promote CloseWrite. An application that reads to "+
			"EOF to know the response ended hangs on a response the server considers "+
			"complete.", err)
	}
	if !bytes.Equal(got, reply) {
		t.Fatalf("client read % x, want % x", got, reply)
	}
}
