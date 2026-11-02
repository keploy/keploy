package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
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
// for its session's summary, and the session's first to each app port is said
// at WARN, the next at Debug; the next session's first to a port is said at
// WARN again. Before, the ingress took the ClientHello for an HTTP/1
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
				// starts a new recording session first, as its incoming
				// stream opens (routes/record.go)
				session bool
			}{{8443, "/user/1", false}, {8443, "/user/2", false}, {9443, "/user/3", false}, {8443, "/user/4", true}} {
				if call.session {
					if before := rec.StartTLSRelayedSession(); before != 3 {
						t.Fatalf("the second session started at %d TLS connections relayed, want the first's 3", before)
					}
				}
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
			if got := rec.TLSRelayedConns(); got != 4 {
				t.Fatalf("the recording's manager counted %d TLS connections relayed unread, want all 4 (each session's summary says its own)", got)
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
			want := []line{{zapcore.WarnLevel, uint16(8443)}, {zapcore.DebugLevel, uint16(8443)}, {zapcore.WarnLevel, uint16(9443)}, {zapcore.WarnLevel, uint16(8443)}}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("the relays were said as %v, want %v: at WARN each session's first to each app port, then at Debug", got, want)
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

// In OSS no context carries a recording's manager (nothing calls
// syncMock.NewContext), so a TLS connection relayed there is counted on the
// process's manager, the one the session's summary reads (routes/record.go).
// And it is counted while it is still open, as its relay starts: most HTTPS
// clients keep their connections alive, and one kept past its recording's end
// is that recording's, whose summary would otherwise say 0. The app's
// ServerHello comes through the relay, so the client's handshake completes
// only after the relay started, and after the count.
func TestAnOpenTLSConnectionRelayedInOSSIsCountedOnTheProcesssManager(t *testing.T) {
	srv := startTLSApp(t)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	before := syncMock.Get().TLSRelayedConns()
	app, proxied := loopbackPair(t)
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.handleConnection(context.Background(), proxied, srv.Listener.Addr().String(), pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8443)
	}()
	_ = app.SetDeadline(time.Now().Add(time.Minute)) // only turns a hang into a failure
	if err := tls.Client(app, clientTLSConfig(srv)).Handshake(); err != nil {
		t.Fatalf("the TLS handshake with the app through the ingress: %v", err)
	}
	got := syncMock.Get().TLSRelayedConns() - before
	_ = app.Close()
	<-served
	if got != 1 {
		t.Fatalf("an open TLS connection relayed with no manager on its context counted %d on the process's manager, want 1", got)
	}
}

// While the recording stops, its client stops the app on purpose: a TLS
// connection then to an app that refuses it is closed, and the refusal is
// said at DEBUG, as for any call then (logAppDialFailure). Nothing is relayed
// or counted, and the agent does not panic. Before the stop, the same
// refusal is an app that has not listened yet, and the connection waits for
// it (the next row).
func TestATLSConnectionTheStoppedAppRefusesIsClosed(t *testing.T) {
	refused := testnet.RefusedAddr(t, "tcp")
	srv := startTLSApp(t) // only for its client's trust
	core, logs := observer.New(zapcore.DebugLevel)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.New(core), nil, cfg)
	pm.SetStopping(true)
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
		t.Fatal("the TLS handshake with an app the stop took succeeded")
	}
	<-served
	// Closed: its end (EOF), or a reset, since the ingress closed it with
	// the rest of the ClientHello unread. Not a timeout: still open.
	var ne net.Error
	if n, err := app.Read(make([]byte, 1)); n != 0 || err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("the client's connection read %d bytes (%v) after the failed dial, want it closed", n, err)
	}
	failed := logs.FilterMessage("Failed to connect to the application to relay a TLS connection to it. Verify that the application is listening on the resolved address and port.").All()
	if len(failed) != 1 || failed[0].Level != zapcore.DebugLevel || failed[0].ContextMap()["final_app_addr"] != refused {
		t.Fatalf("the refused dial was said %v, want once at DEBUG naming %s", failed, refused)
	}
	if n := logs.FilterMessage(tlsRelayedWarning).Len(); n != 0 {
		t.Fatalf("a connection that never reached the app was said relayed %d times", n)
	}
	if got := rec.TLSRelayedConns(); got != 0 {
		t.Fatalf("a connection that never reached the app was counted relayed %d times", got)
	}
}

// A TLS connection to an app that has not listened yet waits for its
// listener, as every call to the app does (dialApp), and is relayed to the
// app's TLS once it listens: its client is shown the app's own certificate
// and gets its answer, and the connection is counted once. Nothing is said
// at ERROR.
func TestATLSConnectionToAnAppThatHasNotListenedYetIsRelayedOnceItListens(t *testing.T) {
	addr := sharedAppAddr(t)
	// Every httptest TLS server has the same certificate (the package's
	// localhost one), so this server's trust is the late app's too; the
	// call checks the app's own certificate all the same.
	trust := startTLSApp(t)
	core, logs := observer.New(zapcore.DebugLevel)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.New(core), nil, cfg)
	rec := syncMock.New(zap.NewNop())
	ctx := syncMock.NewContext(context.Background(), rec)
	client, proxied := loopbackPair(t)
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.handleConnection(ctx, proxied, addr, pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8443)
	}()
	called := make(chan error, 1)
	go func() { called <- tlsCall(client, clientTLSConfig(trust), trust.Certificate(), "/late") }()
	// The ClientHello came, and its dial waits for the app.
	awaitWaiters(t, pm, addr, 1)
	app := startTLSAppOn(t, testnet.ListenShared(t, "tcp4", addr))
	select {
	case err := <-called:
		if err != nil {
			t.Fatalf("the TLS call that waited for the app: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the TLS call that waited for the app did not end")
	}
	if certs := app.Certificate(); certs == nil || !certs.Equal(trust.Certificate()) {
		t.Fatal("the late app's certificate is not the one the client trusted")
	}
	_ = client.Close()
	<-served
	if got := rec.TLSRelayedConns(); got != 1 {
		t.Fatalf("the connection was counted relayed %d times, want once", got)
	}
	if e := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(e) != 0 {
		t.Fatalf("logged at ERROR: %v", e)
	}
}

// startTLSApp is an app that serves HTTPS, answering "tls-pong <path>",
// closed at the test's end.
func startTLSApp(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(tlsPong)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// startTLSAppOn is startTLSApp on ln: an app that listens late, at a port
// the test holds.
func startTLSAppOn(t *testing.T, ln net.Listener) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(tlsPong)
	srv.Listener = ln
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// tlsPong is a TLS app's answer: "tls-pong <path>".
var tlsPong = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "tls-pong "+r.URL.Path)
})

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
	if err := tlsCall(conn, clientTLSConfig(srv), srv.Certificate(), path); err != nil {
		t.Fatalf("call %d: %v", i, err)
	}
}

// tlsCall makes one HTTPS request for path over conn, trusting only what cfg
// trusts: it fails unless the client is shown the app's own certificate,
// want, and gets the app's answer, then its close and nothing else. It
// returns, not fails, so it can run beside the test.
func tlsCall(conn net.Conn, cfg *tls.Config, want *x509.Certificate, path string) error {
	_ = conn.SetDeadline(time.Now().Add(time.Minute)) // only turns a hang into a failure
	c := tls.Client(conn, cfg)
	if err := c.Handshake(); err != nil {
		return fmt.Errorf("the TLS handshake with the app through the ingress: %w", err)
	}
	if certs := c.ConnectionState().PeerCertificates; len(certs) == 0 || !certs[0].Equal(want) {
		return errors.New("the client was not shown the app's own certificate")
	}
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"); err != nil {
		return fmt.Errorf("the request: %w", err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fmt.Errorf("the app's answer: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "tls-pong "+path {
		return fmt.Errorf("the app's answer: %d %q (%v), want 200 %q", resp.StatusCode, body, err, "tls-pong "+path)
	}
	if n, err := br.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		return fmt.Errorf("after the app's answer the client read %d bytes (%v), want the app's close (EOF) and nothing else", n, err)
	}
	return nil
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
