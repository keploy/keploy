package http

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestMockOutgoingSaysMockMissOnlyOnAMiss holds MockOutgoing's "mock miss"
// line, which users grep keploy's output for, to a miss: a request no mock
// matches logs it once, and a request the parser cannot read, which is an
// error but no miss, logs no line with the phrase. The row has no reporter in
// its context and the proven switch off (TestMain), so the miss comes back to
// MockOutgoing, which logs it.
func TestMockOutgoingSaysMockMissOnlyOnAMiss(t *testing.T) {
	const line = "mock miss — 502 already sent to client"
	cases := []struct {
		name, request string
		miss          bool
	}{
		{name: "no mock matches", request: "GET /orders HTTP/1.1\r\nHost: localhost:50001\r\n\r\n", miss: true},
		{name: "unreadable request", request: "NOT AN HTTP REQUEST\r\n\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			h := &HTTP{Logger: zap.New(core)}
			app, prox := net.Pipe()
			t.Cleanup(func() {
				_ = app.Close()
				_ = prox.Close()
			})
			done := make(chan error, 1)
			go func() {
				done <- h.MockOutgoing(context.Background(), prox, &models.ConditionalDstCfg{Addr: "127.0.0.1:50001"}, &mockMemDb{}, models.OutgoingOptions{})
			}()
			if _, err := io.WriteString(app, tc.request); err != nil {
				t.Fatal(err)
			}
			// The app reads what it is sent (the 502 on a miss), so the
			// parser's write never blocks on the pipe.
			go func() { _, _ = io.Copy(io.Discard, app) }()
			err := <-done
			if got := errors.Is(err, ErrMockNotMatched); got != tc.miss {
				t.Fatalf("MockOutgoing returned %v: a miss is %v, want %v", err, got, tc.miss)
			}
			if err == nil {
				t.Fatal("MockOutgoing returned no error; the row needs one on each path")
			}
			if n := logs.FilterMessage(line).Len(); tc.miss && n != 1 {
				t.Errorf("a miss logged %q %d times, want once", line, n)
			}
			for _, e := range logs.All() {
				if !tc.miss && strings.Contains(strings.ToLower(e.Message), "mock miss") {
					t.Errorf("not a miss, yet logged %q", e.Message)
				}
			}
		})
	}
}
