package http

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func eofTCPPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()

	var dialer net.Dialer
	client, err = dialer.DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server = <-accepted
	if server == nil {
		_ = client.Close()
		t.Fatal("accept failed")
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// When a client closes its connection (io.EOF) mid-header or mid-chunked-body
// (for example after receiving a 502), the HTTP parser must log at Debug only
// and emit zero Error lines.
func TestClientEOFLogsDebugOnly(t *testing.T) {
	t.Run("chunked body EOF", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		h := &HTTP{Logger: zap.New(core)}

		client, server := eofTCPPair(t)
		_, _ = client.Write([]byte("POST /intake/v2/events HTTP/1.1\r\nHost: apm.internal\r\nTransfer-Encoding: chunked\r\n\r\n4\r\npa"))
		_ = client.Close()

		var reqBuf []byte
		err := h.HandleChunkedRequests(context.Background(), &reqBuf, server, nil)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected io.EOF, got %v", err)
		}
		if errs := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errs) != 0 {
			t.Errorf("expected 0 Error lines on client EOF, got %d: %v", len(errs), errs)
		}
		if dbgs := logs.FilterLevelExact(zapcore.DebugLevel).All(); len(dbgs) == 0 {
			t.Errorf("expected Debug log on client EOF, got none")
		}
	})

	t.Run("header EOF", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		h := &HTTP{Logger: zap.New(core)}

		client, server := eofTCPPair(t)
		_, _ = client.Write([]byte("POST /intake HTTP/1.1\r\nHost: apm"))
		_ = client.Close()

		var reqBuf []byte
		_, _, err := h.readRequestHead(context.Background(), &reqBuf, server, nil)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected io.EOF, got %v", err)
		}
		if errs := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errs) != 0 {
			t.Errorf("expected 0 Error lines on header client EOF, got %d: %v", len(errs), errs)
		}
	})
}
