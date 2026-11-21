package pkg

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The replay target's host name in keploy's log is its labels' shape beside
// its tag (models.HostLogName), never the name: a host name is traffic, and a
// program that embeds the replay (k8s-proxy) writes these lines into its own
// log. Each row drives one place that logs the target's host with a made-up
// name and fails if any line holds it, in any form, in its message or in any
// field of any type or key (logleak.Check). The error a test's send returns
// keeps the host whole: the test's report shows it.

// canaryHost is a made-up host name for the replay target: under .invalid
// (RFC 6761), so no lookup of it answers, and holding logleak.Canary.
const canaryHost = "app-" + logleak.Canary + ".invalid"

// canaryRecordedHost is a made-up host name a test was recorded with.
const canaryRecordedHost = "rec-" + logleak.Canary + ".invalid"

func TestNoLogLineNamesTheReplayTargetsHostWhole(t *testing.T) {
	target := net.JoinHostPort(canaryHost, "8080")
	httpTest := func() *models.TestCase {
		return &models.TestCase{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{
			Method: "GET", URL: "http://" + target + "/orders",
			Header: map[string]string{"Host": net.JoinHostPort(canaryRecordedHost, "8080")},
		}}
	}
	for _, row := range []struct {
		name string
		run  func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs)
	}{
		{"a test request to an app whose name does not resolve", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			_, err := SimulateHTTP(context.Background(), httpTest(), "test-set-0", logger, SimulationConfig{APITimeout: 5})
			said(t, logs, "overriding host header")
			said(t, logs, "failed to send testcase request to app")
			if err == nil || !strings.Contains(err.Error(), canaryHost) {
				t.Errorf("the error returned for the test's report does not name the host whole: %v", err)
			}
		}},
		{"a streamed test request to an app whose name does not resolve", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			_, err := SimulateHTTPStreaming(context.Background(), httpTest(), "test-set-0", logger, SimulationConfig{APITimeout: 5})
			said(t, logs, "failed to send testcase request to app")
			if err == nil || !strings.Contains(err.Error(), canaryHost) {
				t.Errorf("the error returned for the test's report does not name the host whole: %v", err)
			}
		}},
		{"a gRPC test to an app whose name does not resolve", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			tc := &models.TestCase{Name: "test-1", Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{
				":authority": net.JoinHostPort(canaryHost, "9090"), ":path": "/orders.Orders/Get", ":method": "POST", ":scheme": "http",
			}}}}
			if _, err := SimulateGRPC(context.Background(), tc, "test-set-0", logger, SimulationConfig{APITimeout: 5}); err == nil || !strings.Contains(err.Error(), canaryHost) {
				t.Errorf("the error returned for the test's report does not name the host whole: %v", err)
			}
			said(t, logs, "Final resolved target")
		}},
		{"a replaceWith rule that names hosts", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			for _, rule := range []map[string]string{
				{canaryRecordedHost: canaryHost},
				{"http://" + canaryRecordedHost + ":8080": "http://" + target + "/api"},
			} {
				if _, err := ResolveTestTarget("http://"+canaryRecordedHost+":8080/orders", rule, nil, "", 8080, 0, true, logger); err != nil {
					t.Fatal(err)
				}
			}
			if n := logs.FilterMessageSnippet("Applied replaceWith substitution").Len(); n != 2 {
				t.Fatalf("precondition: the rule was applied %d times, want 2: %v", n, logs.All())
			}
		}},
		{"test.host put in a test's URL", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			if _, err := ResolveTestTarget("http://127.0.0.1:8080/orders", nil, nil, canaryHost, 8080, 0, true, logger); err != nil {
				t.Fatal(err)
			}
			said(t, logs, "Replaced host with config host")
		}},
		{"a refused test request waits for the app to accept", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			var w reSendWait
			_ = waitToReSend(ctx, logger, "test request", target, errors.New("connect: connection refused"), &w)
			w.end()
			said(t, logs, "the test request was refused at")
		}},
		{"a refused test request is sent again with probing the address disabled", func(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs) {
			ctx, cancel := context.WithTimeout(WithoutAddressProbe(context.Background()), 300*time.Millisecond)
			defer cancel()
			var w reSendWait
			_ = waitToReSend(ctx, logger, "test request", target, errors.New("connect: connection refused"), &w)
			w.end()
			said(t, logs, "probing the application's address is disabled by configuration")
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			logger, logs := logleak.Observe()
			row.run(t, logger, logs)
			logleak.Check(t, logs, logleak.Canary, row.name)
		})
	}
}

// said fails t unless logs holds a line whose message holds snippet: the row
// drove the place it names.
func said(t *testing.T, logs *observer.ObservedLogs, snippet string) {
	t.Helper()
	if logs.FilterMessageSnippet(snippet).Len() == 0 {
		t.Fatalf("precondition: no line said %q: %v", snippet, logs.All())
	}
}
