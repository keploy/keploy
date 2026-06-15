package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestIsBenignIncompleteCapture pins the classification that decides whether a
// failed V2-HTTP-record build is logged quietly (Debug) or surfaced (Warn) —
// never Error, because the parser supervisor always recovers via passthrough.
func TestIsBenignIncompleteCapture(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain EOF", io.EOF, true},
		{"wrapped EOF", fmt.Errorf("dest stream: %w", io.EOF), true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		// The exact shape buildHTTPMock returns for a truncated body (e.g. an
		// SQS ReceiveMessage long-poll the peer closes at shutdown).
		{"read response body: unexpected EOF", fmt.Errorf("read response body: %w", io.ErrUnexpectedEOF), true},
		{"fakeconn closed", fakeconn.ErrClosed, true},
		{"wrapped fakeconn closed", fmt.Errorf("relay: %w", fakeconn.ErrClosed), true},
		{"context canceled", context.Canceled, true},
		{"genuine decode error", errors.New("malformed chunked encoding"), false},
		{"unrelated sentinel", io.ErrShortWrite, false},
	}
	for _, c := range cases {
		if got := isBenignIncompleteCapture(c.err); got != c.want {
			t.Errorf("isBenignIncompleteCapture(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRecordV2_TruncatedResponseNotLoggedAsError is the end-to-end guard for the
// reported issue: a response whose body is cut short by the peer closing early
// (the SQS long-poll shape) makes buildHTTPMock fail with io.ErrUnexpectedEOF.
// keploy recovers via passthrough, so this must NOT be logged at Error — it
// should leave a quiet Debug breadcrumb instead.
func TestRecordV2_TruncatedResponseNotLoggedAsError(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	obs := zap.New(core)
	h := &HTTP{Logger: obs}

	sess, sendReq, closeReq, sendResp, closeResp, _ := newTestSession(t)
	sess.Logger = obs // recordV2 prefers sess.Logger over h.Logger

	t0 := time.Unix(1_700_000_000, 0)
	sendReq(canonicalRequest, t0, t0.Add(time.Millisecond))
	// Declares Content-Length: 100 but only 5 body bytes arrive before close →
	// buildHTTPMock's body read returns io.ErrUnexpectedEOF.
	truncated := []byte(
		"HTTP/1.1 200 OK\r\n" +
			"Content-Type: text/plain\r\n" +
			"Content-Length: 100\r\n" +
			"\r\n" +
			"hello",
	)
	sendResp(truncated, t0.Add(2*time.Millisecond), t0.Add(3*time.Millisecond))
	closeReq()
	closeResp()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.recordV2(ctx, sess) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recordV2 did not exit within 2s")
	}

	for _, e := range logs.All() {
		if e.Level >= zapcore.ErrorLevel {
			t.Fatalf("recovered truncated capture logged at %s (want <= Warn): %q", e.Level, e.Message)
		}
	}
	if logs.FilterMessageSnippet("incomplete exchange").Len() == 0 {
		t.Errorf("expected a Debug breadcrumb for the incomplete exchange; entries=%v", logs.All())
	}
}
