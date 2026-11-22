package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/logleak"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func testLogger() *zap.Logger {
	return zap.NewNop()
}

func TestHandleConnectTunnel_TestMode(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	var wg sync.WaitGroup
	var clientResponse []byte

	wg.Add(1)
	go func() {
		defer wg.Done()
		// App sends CONNECT request.
		_, _ = client.Write([]byte("CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n"))
		// Read the 200 response.
		buf := make([]byte, 256)
		n, _ := client.Read(buf)
		clientResponse = buf[:n]
	}()

	result, err := handleConnectTunnel(context.Background(), testLogger(), server, nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wg.Wait()

	if result.TargetHost != "api.example.com" {
		t.Errorf("TargetHost = %q, want %q", result.TargetHost, "api.example.com")
	}
	if result.TargetPort != "443" {
		t.Errorf("TargetPort = %q, want %q", result.TargetPort, "443")
	}
	if result.TargetAddr != "api.example.com:443" {
		t.Errorf("TargetAddr = %q, want %q", result.TargetAddr, "api.example.com:443")
	}

	want := "HTTP/1.1 200 Connection Established\r\n\r\n"
	if string(clientResponse) != want {
		t.Errorf("client received %q, want %q", clientResponse, want)
	}
}

func TestHandleConnectTunnel_RecordMode(t *testing.T) {
	appClient, appServer := net.Pipe()
	defer appClient.Close()
	defer appServer.Close()

	proxyClient, proxyServer := net.Pipe()
	defer proxyClient.Close()
	defer proxyServer.Close()

	var wg sync.WaitGroup
	var appResponse []byte

	// Simulate the corporate proxy.
	wg.Add(1)
	go func() {
		defer wg.Done()
		reader := bufio.NewReader(proxyClient)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		_, _ = proxyClient.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	}()

	// Simulate the app.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = appClient.Write([]byte("CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\nUser-Agent: test/1.0\r\n\r\n"))
		buf := make([]byte, 256)
		n, _ := appClient.Read(buf)
		appResponse = buf[:n]
	}()

	result, err := handleConnectTunnel(context.Background(), testLogger(), appServer, proxyServer, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wg.Wait()

	if result.TargetHost != "api.example.com" {
		t.Errorf("TargetHost = %q, want %q", result.TargetHost, "api.example.com")
	}

	want := "HTTP/1.1 200 Connection Established\r\n\r\n"
	if string(appResponse) != want {
		t.Errorf("app received %q, want %q", appResponse, want)
	}

	// Verify DstReader is set (tunnel connection metadata preserved).
	if result.DstReader == nil {
		t.Error("DstReader is nil; tunnel proxy reader not preserved")
	}
}

// A client must take any number of interim (1xx) responses before the final
// one (RFC 9110 15.2), and keploy is the corporate proxy's client: the tunnel
// is up at the 200 that follows them. net/http's ReadResponse returns the
// first interim response as if it were the answer, which was taken as the
// proxy turning the CONNECT down.
func TestHandleConnectTunnel_RecordModeTakesInterimResponses(t *testing.T) {
	appClient, appServer := net.Pipe()
	defer appClient.Close()
	defer appServer.Close()
	proxyClient, proxyServer := net.Pipe()
	defer proxyClient.Close()
	defer proxyServer.Close()

	var wg sync.WaitGroup
	var appResponse bytes.Buffer
	wg.Add(2)
	go func() { // the corporate proxy
		defer wg.Done()
		reader := bufio.NewReader(proxyClient)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		_, _ = proxyClient.Write([]byte("HTTP/1.1 102 Processing\r\n\r\nHTTP/1.1 200 Connection established\r\n\r\n"))
	}()
	go func() { // the app
		defer wg.Done()
		_, _ = appClient.Write([]byte("CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n"))
		_, _ = http.ReadResponse(bufio.NewReader(io.TeeReader(appClient, &appResponse)), nil)
	}()

	result, err := handleConnectTunnel(context.Background(), testLogger(), appServer, proxyServer, false)
	wg.Wait()
	if err != nil {
		t.Fatalf("the tunnel was refused: %v; the app got %q", err, appResponse.String())
	}
	if result.TargetAddr != "api.example.com:443" {
		t.Errorf("TargetAddr = %q, want %q", result.TargetAddr, "api.example.com:443")
	}
	if want := "HTTP/1.1 200 Connection Established\r\n\r\n"; appResponse.String() != want {
		t.Errorf("app received %q, want %q", appResponse.String(), want)
	}
}

func TestHandleConnectTunnel_ProxyRejectsWithError(t *testing.T) {
	appClient, appServer := net.Pipe()
	proxyClient, proxyServer := net.Pipe()

	errCh := make(chan error, 1)

	// Simulate proxy responding with 407.
	go func() {
		reader := bufio.NewReader(proxyClient)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		_, _ = proxyClient.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
	}()

	// Simulate the app: send CONNECT then continuously drain responses.
	go func() {
		_, _ = appClient.Write([]byte("CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n"))
		buf := make([]byte, 4096)
		for {
			if _, err := appClient.Read(buf); err != nil {
				break
			}
		}
	}()

	go func() {
		_, err := handleConnectTunnel(context.Background(), testLogger(), appServer, proxyServer, false)
		errCh <- err
		// Close connections to unblock goroutines.
		appServer.Close()
		appClient.Close()
		proxyServer.Close()
		proxyClient.Close()
	}()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error for 407 response, got nil")
	}
}

func TestIsConnectRequest(t *testing.T) {
	tests := []struct {
		name string
		peek []byte
		want bool
	}{
		{"CONNECT prefix", []byte("CONNE"), true},
		{"full CONNECT", []byte("CONNECT host:443 HTTP/1.1"), true},
		{"GET request", []byte("GET /"), false},
		{"POST request", []byte("POST "), false},
		{"too short", []byte("CON"), false},
		{"empty", []byte{}, false},
		{"TLS handshake", []byte{0x16, 0x03, 0x01, 0x00, 0x05}, false},
		// The method is CONNECT exactly: one that only begins CONNE, or runs
		// on past it, is none, and nor is a STOMP CONNECT frame.
		{"a method that only begins CONNE", []byte("CONNEkpLogCanary /x HTTP/1.1"), false},
		{"a method that runs on past CONNECT", []byte("CONNECTX / HTTP/1.1"), false},
		{"a STOMP CONNECT frame", []byte("CONNECT\naccept-version:1.2\n"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isConnectRequest(tt.peek)
			if got != tt.want {
				t.Errorf("isConnectRequest(%q) = %v, want %v", tt.peek, got, tt.want)
			}
		})
	}
}

// The proxy peeks 5 bytes of a connection; only where they may begin a
// CONNECT does it read the rest of the method, and it hands the connection
// to the CONNECT tunnel only for the method CONNECT exactly. A method that
// only began CONNE was handed there, and its error quoted it.
func TestConnectRequestAheadReadsTheWholeMethod(t *testing.T) {
	for _, c := range []struct {
		name, sent string
		want       bool
	}{
		{"a CONNECT", "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n", true},
		{"a method that only begins CONNE", "CONNE" + logleak.Canary + " /x HTTP/1.1\r\nHost: a:1\r\n\r\n", false},
		{"a method that runs on past CONNECT", "CONNECTX / HTTP/1.1\r\n\r\n", false},
		{"a STOMP CONNECT frame", "CONNECT\naccept-version:1.2\nhost:broker\n\n\x00", false},
		{"CONNE, then the end", "CONNE", false},
		{"a GET", "GET / HTTP/1.1\r\n\r\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(c.sent))
			peeked, _ := r.Peek(5)
			if got := connectRequestAhead(r, peeked); got != c.want {
				t.Errorf("connectRequestAhead(%q) = %v, want %v", c.sent, got, c.want)
			}
			// Nothing is consumed: the parser after it reads every byte.
			if rest, _ := io.ReadAll(r); string(rest) != c.sent {
				t.Errorf("the reader lost bytes: %q left of %q", rest, c.sent)
			}
		})
	}
}

// A request whose method only begins CONNE is no CONNECT: the proxy passes
// its connection on as any other, its bytes reaching their destination, and
// hands it to no CONNECT tunnel, which refused it with an ERROR naming the
// method. No line about the connection holds the client's text.
func TestAMethodThatOnlyBeginsCONNEIsNoTunnel(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- ""
			return
		}
		defer func() { _ = c.Close() }()
		line, _ := bufio.NewReader(c).ReadString('\n')
		got <- line
	}()
	logger, logs := logleak.Observe()
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(logger, fakeDestInfo{dest: ipv4Dest(t, ln.Addr().String())}, cfg)
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	line := "CONNE" + logleak.Canary + " /x HTTP/1.1\r\n"
	if _, err := io.WriteString(app, line+"Host: a:1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if first := awaitEvent(t, got, "the destination reading the request"); first != line {
		t.Fatalf("the destination read %q, want the request's first line %q: the request was not passed on", first, line)
	}
	_ = app.Close()
	_ = awaitEvent(t, done, "handleConnection returning after the client left")
	if n := len(logs.FilterMessageSnippet("failed to handle CONNECT tunnel").All()); n != 0 {
		t.Fatalf("the connection was handed to the CONNECT tunnel: %+v", logs.All())
	}
	logleak.Check(t, logs, logleak.Canary, "a method that only begins CONNE")
}

// In a replay too, a request whose method only begins CONNE is no CONNECT:
// the proxy hands it to its parsers, which find no mock for it and say so,
// and never to the CONNECT tunnel, which refused it with an ERROR. No line
// about the connection, and not its error, holds the client's text.
func TestAMethodThatOnlyBeginsCONNEIsNoTunnelInAReplay(t *testing.T) {
	logger, logs := logleak.Observe()
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := New(logger, fakeDestInfo{dest: ipv4Dest(t, "127.0.0.1:61613")}, cfg)
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	if err := p.Mock(context.Background(), models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true}); err != nil {
		t.Fatal(err)
	}
	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	if _, err := io.WriteString(app, "CONNE"+logleak.Canary+" /x HTTP/1.1\r\nHost: a:1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// The request's end: the parser reads on until it.
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	err := awaitEvent(t, done, "handleConnection returning after the client's end")
	_ = app.Close()
	if n := len(logs.FilterMessageSnippet("failed to handle CONNECT tunnel").All()); n != 0 {
		t.Fatalf("the connection was handed to the CONNECT tunnel: %+v", logs.All())
	}
	if logs.FilterMessage("failed to mock the outgoing message").Len() == 0 {
		t.Fatalf("no parser was asked for the request's mock: %+v", logs.All())
	}
	if err != nil && logleak.Holds(err.Error(), logleak.Canary) {
		t.Errorf("handleConnection's error, which the accept loop logs, holds the client's text: %q", err)
	}
	logleak.Check(t, logs, logleak.Canary, "a replay of a method that only begins CONNE")
}

// The CONNECT error, which proxy.go logs at ERROR, names no text the client
// chose: an IPv6 target's zone (an origin-form CONNECT's Host header, or an
// authority-form target's %25-escaped zone) is written {}, the port by its
// size, and a method that is not CONNECT by its size. Each was whole.
func TestHandleConnectTunnel_ErrorHoldsNoClientText(t *testing.T) {
	for _, c := range []struct{ name, head, want string }{
		{"origin-form Host zone, port 0", "CONNECT /x HTTP/1.1\r\nHost: [fe80::1%" + logleak.Canary + "]:0\r\n\r\n",
			"invalid port in CONNECT target [fe80::1%{}]:0: a 1-byte value"},
		{"authority-form %25 zone, port 0", "CONNECT [fe80::1%25" + logleak.Canary + "]:0 HTTP/1.1\r\nHost: x\r\n\r\n",
			"invalid port in CONNECT target [fe80::1%{}]:0: a 1-byte value"},
		{"origin-form Host zone, 99999", "CONNECT /x HTTP/1.1\r\nHost: [fe80::1%" + logleak.Canary + "]:99999\r\n\r\n",
			"invalid port in CONNECT target [fe80::1%{}]:99999: a 5-byte value"},
		{"method CONNE<text>", "CONNE" + logleak.Canary + " /x HTTP/1.1\r\nHost: a:1\r\n\r\n",
			fmt.Sprintf("got a %d-byte method that is not CONNECT", len("CONNE"+logleak.Canary))},
	} {
		t.Run(c.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			defer func() { _ = server.Close() }()
			go func() {
				_, _ = client.Write([]byte(c.head))
				buf := make([]byte, 256)
				_, _ = client.Read(buf)
			}()
			_, err := handleConnectTunnel(models.WithLogKeySession(context.Background(), models.NewLogKeySession()), testLogger(), server, nil, true)
			if err == nil {
				t.Fatal("no error")
			}
			if logleak.Holds(err.Error(), logleak.Canary) {
				t.Fatalf("the CONNECT error, logged at ERROR, holds client text: %q", err.Error())
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the CONNECT error = %q, want it to say %q", err.Error(), c.want)
			}
		})
	}
}

func TestHandleConnectTunnel_IPv6Target(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = client.Write([]byte("CONNECT [2001:db8::1]:443 HTTP/1.1\r\nHost: [2001:db8::1]:443\r\n\r\n"))
		buf := make([]byte, 256)
		client.Read(buf)
	}()

	result, err := handleConnectTunnel(context.Background(), testLogger(), server, nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wg.Wait()

	if result.TargetHost != "2001:db8::1" {
		t.Errorf("TargetHost = %q, want %q", result.TargetHost, "2001:db8::1")
	}
	if result.TargetPort != "443" {
		t.Errorf("TargetPort = %q, want %q", result.TargetPort, "443")
	}
}

func TestHandleConnectTunnel_InvalidPort(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		_, _ = client.Write([]byte("CONNECT example.com:0 HTTP/1.1\r\nHost: example.com:0\r\n\r\n"))
		buf := make([]byte, 256)
		client.Read(buf)
	}()

	_, err := handleConnectTunnel(context.Background(), testLogger(), server, nil, true)
	if err == nil {
		t.Fatal("expected error for port 0, got nil")
	}
}

func TestHandleConnectTunnel_NonNumericPort(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		_, _ = client.Write([]byte("CONNECT example.com:abc HTTP/1.1\r\nHost: example.com:abc\r\n\r\n"))
		buf := make([]byte, 256)
		client.Read(buf)
	}()

	_, err := handleConnectTunnel(context.Background(), testLogger(), server, nil, true)
	if err == nil {
		t.Fatal("expected error for non-numeric port, got nil")
	}
}

// A CONNECT whose target is origin-form takes its authority from the Host
// header, whose port Go's parser does not check, so the port is any text
// the client sent. The error, which proxy.go logs at ERROR, names the
// target by its shape (the port {}) and the port by its size, never the
// port's text: it quoted it whole.
func TestHandleConnectTunnel_HostHeaderPortTextIsNotQuoted(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	go func() {
		_, _ = client.Write([]byte("CONNECT /x HTTP/1.1\r\nHost: api.example.com:" + logleak.Canary + "\r\n\r\n"))
		buf := make([]byte, 256)
		_, _ = client.Read(buf)
	}()
	keys := models.NewLogKeySession()
	_, err := handleConnectTunnel(models.WithLogKeySession(context.Background(), keys), testLogger(), server, nil, true)
	if err == nil {
		t.Fatal("no error")
	}
	if logleak.Holds(err.Error(), logleak.Canary) {
		t.Fatalf("the CONNECT error, which proxy.go logs at ERROR, holds the Host header's port text: %q", err.Error())
	}
	if want := fmt.Sprintf("invalid port in CONNECT target {}.{}.{}:{} %s: a %d-byte value", keys.HostTag("api.example.com"), len(logleak.Canary)); !strings.Contains(err.Error(), want) {
		t.Fatalf("the CONNECT error = %q, want it to name the target by its shape and the port by its size, %q", err.Error(), want)
	}
}

func TestStripUtilConn(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	logger := testLogger()
	wrapped := &util.Conn{Conn: server, Reader: server, Logger: logger}
	stripped := stripUtilConn(wrapped)
	if stripped != server {
		t.Error("stripUtilConn did not unwrap util.Conn")
	}

	plain := stripUtilConn(server)
	if plain != server {
		t.Error("stripUtilConn modified a plain net.Conn")
	}
}

// TestHandleConnectTunnel_EmptyHost closes the last route by which an address
// with no host at all could reach the upstream TLS dial.
//
// http.ReadRequest accepts `CONNECT :443 HTTP/1.1` and reports Host==":443";
// net.SplitHostPort then succeeds with host=="". Before this check that value
// flowed on as a real target: it was stored as the SNI for the client's source
// port (so CertForClient minted a certificate with an empty CN), it overwrote
// dstAddr with ":443", and with upstream verification on it left
// resolveUpstreamServerName with no host to verify against — crypto/tls then
// rejects the config outright with "either ServerName or InsecureSkipVerify
// must be specified", which reads as keploy being broken rather than the
// target being unroutable. A CONNECT with no authority host names no
// destination; reject it exactly as the empty port is rejected.
//
// FAILS BEFORE THE FIX: handleConnectTunnel returned a result with
// TargetHost=="" and no error.
func TestHandleConnectTunnel_EmptyHost(t *testing.T) {
	for _, target := range []string{":443", "[]:443"} {
		t.Run(target, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()

			go func() {
				_, _ = client.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"))
				buf := make([]byte, 256)
				_, _ = client.Read(buf)
			}()

			res, err := handleConnectTunnel(context.Background(), testLogger(), server, nil, true)
			if err == nil {
				t.Fatalf("a host-less CONNECT target was accepted (TargetHost=%q, TargetAddr=%q)", res.TargetHost, res.TargetAddr)
			}
		})
	}
}
