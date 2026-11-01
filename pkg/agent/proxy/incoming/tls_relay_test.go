package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A TLS connection to the app's port (an app that serves HTTPS) reaches the
// app's TLS through the ingress (the record forwarder's, the park's and the
// live handler's handleConnection), relayed unread: its client is shown the
// app's own certificate and gets the app's answer, and then the app's close,
// nothing more, since the relay owns the connection to its end. Nothing on it
// is recorded: each such connection is counted on the recording's manager,
// for its session's summary, and the first to each app port is said at WARN,
// the next at Debug. Before, the ingress took the ClientHello for an HTTP/1
// request and answered it with a plain-text 400 (or, synchronous, closed it),
// and the client failed with "http: server gave HTTP response to HTTPS
// client".
func TestATLSConnectionToTheAppIsRelayedToTheAppsTLS(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync bool
	}{{"zero-copy", false}, {"synchronous", true}} {
		t.Run(tc.name, func(t *testing.T) {
			// Two apps that serve HTTPS, on two app ports.
			apps := map[uint16]*httptest.Server{8443: startTLSApp(t), 9443: startTLSApp(t)}
			core, logs := observer.New(zapcore.DebugLevel)
			cfg := &config.Config{}
			cfg.Agent.RecordRequests = true
			cfg.Agent.Synchronous = tc.sync
			pm := New(zap.New(core), nil, cfg)
			tcs := make(chan *models.TestCase, 4)
			rec := syncMock.New(zap.NewNop())
			ctx := syncMock.NewContext(context.Background(), rec)

			for i, call := range []struct {
				port uint16
				path string
			}{{8443, "/user/1"}, {8443, "/user/2"}, {9443, "/user/3"}} {
				srv := apps[call.port]
				app, proxied := loopbackPair(t)
				served := make(chan struct{})
				go func() {
					defer close(served)
					pm.handleConnection(ctx, proxied, srv.Listener.Addr().String(), pm.logger, tcs, make(chan struct{}, 1), call.port)
				}()
				callOverTLS(t, i, srv, app, call.path)
				_ = app.Close()
				<-served
			}
			if len(tcs) != 0 {
				t.Fatalf("%d test cases recorded from TLS connections, want none", len(tcs))
			}
			if got := rec.TLSRelayedConns(); got != 3 {
				t.Fatalf("the recording's manager counted %d TLS connections relayed unread, want all 3 (its session's summary says them)", got)
			}
			said := logs.FilterMessage(tlsRelayedWarning).All()
			type line struct {
				level zapcore.Level
				port  any
			}
			got := make([]line, len(said))
			for i, e := range said {
				got[i] = line{e.Level, e.ContextMap()["port"]}
			}
			want := []line{{zapcore.WarnLevel, uint16(8443)}, {zapcore.DebugLevel, uint16(8443)}, {zapcore.WarnLevel, uint16(9443)}}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("the relays were said as %v, want %v: at WARN the first to each app port, then at Debug", got, want)
			}
			// The relay owns the connection to its end: nothing after it
			// reads the spent connection as HTTP/1.
			for _, msg := range []string{"Failed to parse downstream HTTP request", "Failed to read client request; ignoring this connection. Verify the client is sending valid HTTP if this persists."} {
				if n := logs.FilterMessage(msg).Len(); n != 0 {
					t.Fatalf("after the relay, the HTTP/1 handler read the connection: %q logged %d times", msg, n)
				}
			}
		})
	}
}

// A TLS connection to an app port nothing answers on (the app is not
// listening) is closed, and the failed dial is said; nothing is relayed or
// counted, and the agent does not panic.
func TestATLSConnectionTheAppDoesNotAnswerIsClosed(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := ln.Addr().String()
	_ = ln.Close()
	srv := startTLSApp(t) // only for its client's trust
	core, logs := observer.New(zapcore.DebugLevel)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.New(core), nil, cfg)
	rec := syncMock.New(zap.NewNop())
	ctx := syncMock.NewContext(context.Background(), rec)
	app, proxied := loopbackPair(t)
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.handleConnection(ctx, proxied, refused, pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8443)
	}()
	_ = app.SetDeadline(time.Now().Add(time.Minute)) // only turns a hang into a failure
	if err := tls.Client(app, clientTLSConfig(srv)).Handshake(); err == nil {
		t.Fatal("the TLS handshake with an app that is not listening succeeded")
	}
	<-served
	// Closed: its end (EOF), or a reset, since the ingress closed it with
	// the rest of the ClientHello unread. Not a timeout: still open.
	var ne net.Error
	if n, err := app.Read(make([]byte, 1)); n != 0 || err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("the client's connection read %d bytes (%v) after the failed dial, want it closed", n, err)
	}
	failed := logs.FilterMessage("Failed to connect to the application to relay a TLS connection to it. Verify that the application is listening on the resolved address and port.").All()
	if len(failed) != 1 || failed[0].Level != zapcore.ErrorLevel || failed[0].ContextMap()["final_app_addr"] != refused {
		t.Fatalf("the failed dial was said %v, want once at ERROR naming %s", failed, refused)
	}
	if n := logs.FilterMessage(tlsRelayedWarning).Len(); n != 0 {
		t.Fatalf("a connection that never reached the app was said relayed %d times", n)
	}
	if got := rec.TLSRelayedConns(); got != 0 {
		t.Fatalf("a connection that never reached the app was counted relayed %d times", got)
	}
}

// startTLSApp is an app that serves HTTPS, answering "tls-pong <path>",
// closed at the test's end.
func startTLSApp(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tls-pong "+r.URL.Path)
	}))
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// clientTLSConfig trusts srv's certificate, and only it.
func clientTLSConfig(srv *httptest.Server) *tls.Config {
	cfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg.ServerName = "127.0.0.1"
	return cfg
}

// callOverTLS makes call i, one HTTPS request for path, over conn to srv: the
// client must be shown srv's own certificate and get srv's answer, and then
// srv's close and nothing else.
func callOverTLS(t *testing.T, i int, srv *httptest.Server, conn net.Conn, path string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(time.Minute)) // only turns a hang into a failure
	c := tls.Client(conn, clientTLSConfig(srv))
	if err := c.Handshake(); err != nil {
		t.Fatalf("call %d, the TLS handshake with the app through the ingress: %v", i, err)
	}
	if certs := c.ConnectionState().PeerCertificates; len(certs) == 0 || !certs[0].Equal(srv.Certificate()) {
		t.Fatalf("call %d: the client was not shown the app's own certificate", i)
	}
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("call %d, the request: %v", i, err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("call %d, the app's answer: %v", i, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "tls-pong "+path {
		t.Fatalf("call %d, the app's answer: %d %q (%v), want 200 %q", i, resp.StatusCode, body, err, "tls-pong "+path)
	}
	if n, err := br.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("call %d: after the app's answer the client read %d bytes (%v), want the app's close (EOF) and nothing else", i, n, err)
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
