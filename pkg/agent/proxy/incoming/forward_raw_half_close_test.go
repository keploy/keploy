package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// forwardRawTCP is the sampling-bypass byte pump: no capture, no HTTP
// parsing. It still has to forward each side's FIN, and it does so
// UNCONDITIONALLY — unlike the record paths, which gate on a clean copy
// so a truncated request is never reported to the peer as complete.
//
// The difference is deliberate and worth pinning. This pump joins on both
// copies, so the FIN is what unblocks the peer io.Copy. Gating it on a
// clean exit would leave a client-side error with nothing to wake the
// upstream side — two goroutines and two sockets held until the upstream
// closes on its own, which on an idle WebSocket is minutes. An earlier
// version of this change added that gate; this test exists so it is not
// added again.
//
// The FIN also used to be forwarded via a concrete *net.TCPConn
// assertion, which silently skipped every *tls.Conn and every wrapped
// conn. So one case here drives a SafeConn, the wrapper shape that
// assertion could not see through.
func TestForwardRawTCPForwardsTheFIN(t *testing.T) {
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
			return d, proxyutil.NewSafeConn(got.c, zap.NewNop())
		}
		return d, got.c
	}

	for _, wrapped := range []bool{false, true} {
		name := "raw conns"
		if wrapped {
			name = "SafeConn-wrapped, the shape a concrete type assertion misses"
		}
		t.Run(name, func(t *testing.T) {
			clientApp, srcProxy := pair(t, wrapped)
			upSvc, upProxy := pair(t, wrapped)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go forwardRawTCP(ctx, srcProxy, upProxy)

			request := []byte("upgrade-then-eof\n")
			reply := []byte("upstream-reply\n")

			srvDone := make(chan error, 1)
			go func() {
				got, err := io.ReadAll(upSvc)
				if err != nil {
					srvDone <- err
					return
				}
				if !bytes.Equal(got, request) {
					srvDone <- io.ErrUnexpectedEOF
					return
				}
				_, err = upSvc.Write(reply)
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
					t.Fatalf("upstream: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the upstream never saw EOF: the FIN was not forwarded, so an " +
					"EOF-delimited peer waits for bytes that will never come")
			}

			_ = clientApp.SetReadDeadline(time.Now().Add(3 * time.Second))
			got := make([]byte, len(reply))
			if _, err := io.ReadFull(clientApp, got); err != nil {
				t.Fatalf("client never received the reply: %v", err)
			}
		})
	}
}
