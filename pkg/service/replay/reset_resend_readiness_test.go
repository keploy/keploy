package replay

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// TestResolveProbeTarget verifies the reset-resend readiness gate resolves its
// probe target from the ACTUAL dial target — mirroring the simulation's
// ResolveTestTarget (ConfigHost/--host override, port precedence) — not the raw
// recorded URL. The first case is the couchbase-java regression: a recorded
// 127.0.0.1 with the default ConfigHost "localhost" must probe localhost (→ the
// IPv6 path that resets), NOT the recorded IPv4 127.0.0.1 (which never resets and
// would make the fix inert).
func TestResolveProbeTarget(t *testing.T) {
	httpTC := func(url string) *models.TestCase {
		return &models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{URL: url}}
	}
	cases := []struct {
		name                           string
		cfg                            config.Test
		tc                             *models.TestCase
		wantScheme, wantHost, wantPort string
		wantOK                         bool
	}{
		{
			name:       "configHost rewrites recorded 127.0.0.1 to localhost (couchbase-java repro)",
			cfg:        config.Test{Host: "localhost"},
			tc:         httpTC("http://127.0.0.1:8080/health"),
			wantScheme: "http", wantHost: "localhost", wantPort: "8080", wantOK: true,
		},
		{
			name:       "empty configHost defaults to localhost",
			cfg:        config.Test{},
			tc:         httpTC("http://127.0.0.1:8080/health"),
			wantScheme: "http", wantHost: "localhost", wantPort: "8080", wantOK: true,
		},
		{
			name:       "configHost can point the other way (localhost -> 127.0.0.1)",
			cfg:        config.Test{Host: "127.0.0.1"},
			tc:         httpTC("http://localhost:9000/x"),
			wantScheme: "http", wantHost: "127.0.0.1", wantPort: "9000", wantOK: true,
		},
		{
			name:       "config port override wins over the recorded port",
			cfg:        config.Test{Host: "localhost", Port: 7777},
			tc:         httpTC("http://127.0.0.1:8080/x"),
			wantScheme: "http", wantHost: "localhost", wantPort: "7777", wantOK: true,
		},
		{
			name:       "https default port",
			cfg:        config.Test{Host: "example.com"},
			tc:         httpTC("https://recorded-host/health"),
			wantScheme: "https", wantHost: "example.com", wantPort: "443", wantOK: true,
		},
		{
			name:   "non-http test case is not probeable",
			cfg:    config.Test{Host: "localhost"},
			tc:     &models.TestCase{Kind: models.GRPC_EXPORT},
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scheme, host, port, ok := resolveProbeTarget(c.cfg, c.tc, "test-set-0", zap.NewNop())
			if ok != c.wantOK || scheme != c.wantScheme || host != c.wantHost || port != c.wantPort {
				t.Fatalf("got (%q,%q,%q,%v), want (%q,%q,%q,%v)",
					scheme, host, port, ok, c.wantScheme, c.wantHost, c.wantPort, c.wantOK)
			}
		})
	}
	if _, _, _, ok := resolveProbeTarget(config.Test{Host: "localhost"}, nil, "test-set-0", zap.NewNop()); ok {
		t.Fatal("a nil test case must not be probeable")
	}
}

// resettingThenServing starts a TCP server that RST-closes the first resetCount
// accepted connections (mimicking docker's userland-proxy resetting freshly
// accepted host-port conns under load) and then answers subsequent connections
// with a valid HTTP response. It returns the listen address and a stop func.
func resettingThenServing(t *testing.T, resetCount int) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	accepted := 0
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			n := accepted
			accepted++
			mu.Unlock()

			if n < resetCount {
				// Force an RST rather than a graceful FIN: SetLinger(0) makes
				// Close send a reset, reproducing "connection reset by peer".
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0)
				}
				_ = conn.Close()
				continue
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				buf := make([]byte, 1024)
				_, _ = c.Read(buf) // consume the request line/headers
				_, _ = c.Write([]byte("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// TestWaitForHTTPServing_WaitsThroughResetsUntilServing is the couchbase-java
// repro: a bare TCP-accept gate would pass immediately (the handshake completes
// before the app resets), but waitForHTTPServing keeps probing through the reset
// burst and only returns once the app answers a real HTTP response.
func TestWaitForHTTPServing_WaitsThroughResetsUntilServing(t *testing.T) {
	addr, stop := resettingThenServing(t, 3) // reset the first 3 connections
	defer stop()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	// A plain TCP dial succeeds even while the server is in its reset phase —
	// this is exactly why the old TCP-accept gate was insufficient.
	if c, derr := net.DialTimeout("tcp", addr, time.Second); derr == nil {
		_ = c.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitForHTTPServing(ctx, "http", host, port); err != nil {
		t.Fatalf("expected app to become ready after the reset burst, got: %v", err)
	}
}

// TestWaitForHTTPServing_TimesOutWhenNeverServing ensures the probe stays bounded
// by ctx when the app never serves — the caller then proceeds with its bounded
// re-send attempts rather than blocking forever.
func TestWaitForHTTPServing_TimesOutWhenNeverServing(t *testing.T) {
	addr, stop := resettingThenServing(t, 1<<30) // always reset
	defer stop()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := waitForHTTPServing(ctx, "http", host, port); err == nil {
		t.Fatal("expected a timeout error when the app never serves an HTTP response")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("probe overran its context deadline: %v", elapsed)
	}
}
