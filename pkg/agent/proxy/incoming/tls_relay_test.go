package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A TLS connection to the app's port (an app that serves HTTPS) reaches the
// app's TLS through the ingress (the record forwarder's, the park's and the
// live handler's handleConnection), relayed unread: its client is shown the
// app's own certificate and gets the app's answer, nothing on it is recorded,
// and the first such connection to the port is said at WARN, the next at
// Debug. Before, the ingress took the ClientHello for an HTTP/1 request and
// answered it with a plain-text 400 (or, synchronous, closed it), and the
// client failed with "http: server gave HTTP response to HTTPS client".
func TestATLSConnectionToTheAppIsRelayedToTheAppsTLS(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync bool
	}{{"zero-copy", false}, {"synchronous", true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "tls-pong "+r.URL.Path)
			}))
			srv.StartTLS()
			t.Cleanup(srv.Close)
			core, logs := observer.New(zapcore.DebugLevel)
			cfg := &config.Config{}
			cfg.Agent.RecordRequests = true
			cfg.Agent.Synchronous = tc.sync
			pm := New(zap.New(core), nil, cfg)
			tcs := make(chan *models.TestCase, 4)

			for i, path := range []string{"/user/1", "/user/2"} {
				app, proxied := loopbackPair(t)
				served := make(chan struct{})
				go func() {
					defer close(served)
					pm.handleConnection(context.Background(), proxied, srv.Listener.Addr().String(), pm.logger, tcs, make(chan struct{}, 1), 8443)
				}()
				resp, err := tlsClientOver(srv, app).Get(srv.URL + path)
				if err != nil {
					t.Fatalf("call %d, the TLS call to the app through the ingress: %v", i, err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusOK || string(body) != "tls-pong "+path {
					t.Fatalf("call %d, the app's answer: %d %q (%v), want 200 %q", i, resp.StatusCode, body, err, "tls-pong "+path)
				}
				if certs := resp.TLS.PeerCertificates; len(certs) == 0 || !certs[0].Equal(srv.Certificate()) {
					t.Fatalf("call %d: the client was not shown the app's own certificate", i)
				}
				_ = app.Close()
				<-served
			}
			if len(tcs) != 0 {
				t.Fatalf("%d test cases recorded from TLS connections, want none", len(tcs))
			}
			said := logs.FilterMessage(tlsRelayedWarning).All()
			if len(said) != 2 || said[0].Level != zapcore.WarnLevel || said[1].Level != zapcore.DebugLevel {
				levels := make([]zapcore.Level, len(said))
				for i, e := range said {
					levels[i] = e.Level
				}
				t.Fatalf("the relay was said at %v, want once at WARN, then at Debug", levels)
			}
			if port, ok := said[0].ContextMap()["port"]; !ok || port != uint16(8443) {
				t.Fatalf("the warning names port %v, want the app's 8443", port)
			}
		})
	}
}

// loopbackPair is a loopback TCP connection: the client's end, and the
// ingress's accepted end, both closed at the test's end.
func loopbackPair(t *testing.T) (client, accepted net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	got := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		got <- c
	}()
	client, err = net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted = <-got
	if accepted == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = client.Close(); _ = accepted.Close() })
	return client, accepted
}

// tlsClientOver is srv's own client (trusting srv's certificate, and only
// it), whose one connection is conn.
func tlsClientOver(srv *httptest.Server, conn net.Conn) *http.Client {
	tr := srv.Client().Transport.(*http.Transport).Clone()
	var dialed atomic.Bool
	tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
		if dialed.Swap(true) {
			return nil, errors.New("the call has one connection")
		}
		return conn, nil
	}
	tr.DisableKeepAlives = true
	return &http.Client{Transport: tr}
}
