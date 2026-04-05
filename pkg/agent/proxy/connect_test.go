package proxy

import (
	"bufio"
	"net"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
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

	result, err := handleConnectTunnel(testLogger(), server, nil, true)
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

	result, err := handleConnectTunnel(testLogger(), appServer, proxyServer, false)
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
		_, err := handleConnectTunnel(testLogger(), appServer, proxyServer, false)
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

	result, err := handleConnectTunnel(testLogger(), server, nil, true)
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

	_, err := handleConnectTunnel(testLogger(), server, nil, true)
	if err == nil {
		t.Fatal("expected error for port 0, got nil")
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
