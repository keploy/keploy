package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testdeadline"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

func opportunisticHalfCloseExchange(t *testing.T, wrapped bool, run func(ctx context.Context, clientConn, destConn net.Conn)) {
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

	pair := func(t *testing.T, wrap bool) (peer, proxySide net.Conn) {
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
		if wrap {
			return d, util.NewSafeConn(got.c, zap.NewNop())
		}
		return d, got.c
	}

	clientApp, srcProxy := pair(t, wrapped)
	destSvc, dstProxy := pair(t, wrapped)

	ctx, cancel := context.WithCancel(context.Background())
	// Deferred so cancel runs before t.Cleanup joins run.
	defer cancel()
	runDone := make(chan struct{})
	waits = append(waits, cleanupWait{name: "opportunisticHalfCloseExchange run goroutine", done: runDone})
	go func() {
		defer close(runDone)
		run(ctx, srcProxy, dstProxy)
	}()

	request := []byte("plaintext-then-eof\n")
	reply := []byte("plaintext-reply\n")

	srvDone := make(chan error, 1)
	srvExited := make(chan struct{})
	waits = append(waits, cleanupWait{name: "opportunisticHalfCloseExchange service goroutine", done: srvExited})
	go func() {
		defer close(srvExited)
		got, err := io.ReadAll(destSvc)
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
		t.Fatal("the service never saw EOF: closeWriteIfPossible did not forward the " +
			"FIN, so an EOF-delimited peer waits for bytes that never come")
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

// relayPlaintext is the in-repo precedent for half-close handling: copy
// both directions, CloseWrite each when its copy ends, wait for both. It
// is also the last caller of this package's closeWriteIfPossible, which
// is now a thin alias over util.CloseWriteIfPossible.
//
// That alias had no test. Making it a no-op left the whole suite green,
// which is how the same one-liner shipped inert twice elsewhere in this
// change — so pin the behaviour, not the delegation.
func TestRelayPlaintextForwardsTheFIN(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		name := "raw conns"
		if wrapped {
			name = "SafeConn-wrapped"
		}
		t.Run(name, func(t *testing.T) {
			opportunisticHalfCloseExchange(t, wrapped, func(ctx context.Context, clientConn, destConn net.Conn) {
				_ = relayPlaintext(ctx, clientConn, destConn)
			})
		})
	}
}
