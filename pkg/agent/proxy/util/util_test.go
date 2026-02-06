package util

import (
	"bytes"
	"testing"
)

// TestIsHTTPReq verifies that IsHTTPReq correctly identifies HTTP/1.x requests
// and does not misidentify HTTP/2 prefaces or binary data.
func TestIsHTTPReq(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		want bool
	}{
		{
			name: "HTTP GET",
			buf:  []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"),
			want: true,
		},
		{
			name: "HTTP POST",
			buf:  []byte("POST /api/v1/resource HTTP/1.1\r\nContent-Type: application/json\r\n\r\n"),
			want: true,
		},
		{
			name: "HTTP CONNECT",
			buf:  []byte("CONNECT store.xyz.dev:443 HTTP/1.1\r\nHost: store.xyz.dev\r\n\r\n"),
			want: true,
		},
		{
			name: "HTTP/2 Preface",
			buf:  []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"),
			want: false,
		},
		{
			name: "Binary gRPC Data",
			buf:  []byte{0x00, 0x00, 0x00, 0x00, 0x05, 0x0a, 0x03, 0x66, 0x6f, 0x6f},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsHTTPReq(tt.buf); got != tt.want {
				t.Errorf("IsHTTPReq() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ShouldAllowH2 determines if H2 should be offered during TLS ALPN negotiation.
// This mirrors the logic in proxy.go's handleConnection.
func ShouldAllowH2(initialBuf []byte) bool {
	return !IsHTTPReq(initialBuf) || bytes.HasPrefix(initialBuf, []byte("CONNECT "))
}

// TestShouldAllowH2 verifies the ALPN selection logic used in proxy.go
// to ensure H2 is correctly offered for gRPC and CONNECT tunneling scenarios.
func TestShouldAllowH2(t *testing.T) {
	tests := []struct {
		name   string
		buf    []byte
		wantH2 bool
	}{
		{
			name:   "Standard HTTP GET - H1 only",
			buf:    []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"),
			wantH2: false,
		},
		{
			name:   "Standard HTTP POST - H1 only",
			buf:    []byte("POST /api HTTP/1.1\r\nContent-Type: application/json\r\n\r\n"),
			wantH2: false,
		},
		{
			name:   "CONNECT tunnel - allow H2 for gRPC",
			buf:    []byte("CONNECT store.xyz.dev:443 HTTP/1.1\r\nHost: store.xyz.dev\r\n\r\n"),
			wantH2: true,
		},
		{
			name:   "HTTP/2 Preface - allow H2",
			buf:    []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"),
			wantH2: true,
		},
		{
			name:   "Binary gRPC frames - allow H2",
			buf:    []byte{0x00, 0x00, 0x00, 0x00, 0x05, 0x0a, 0x03, 0x66, 0x6f, 0x6f},
			wantH2: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldAllowH2(tt.buf); got != tt.wantH2 {
				t.Errorf("ShouldAllowH2() = %v, want %v", got, tt.wantH2)
			}
		})
	}
}
