package util

import "testing"

func TestIsHTTPReq(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		want bool
	}{
		{"GET request", []byte("GET / HTTP/1.1\r\n"), true},
		{"POST request", []byte("POST /api HTTP/1.1\r\n"), true},
		{"HTTP response (not a request)", []byte("HTTP/1.1 200 OK\r\n"), false},
		{"CONNECT tunnel", []byte("CONNECT host:443 HTTP/1.1\r\n"), true},
		{"binary with POST prefix", []byte("POST \x00\x01binary"), false},
		{"binary with GET prefix", []byte("GET \xff\xfe\x00"), false},
		{"truncated method", []byte("GET "), false},
		{"TLS ClientHello", []byte("\x16\x03\x01\x00\xf1"), false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsHTTPReq(tt.buf); got != tt.want {
				t.Errorf("IsHTTPReq(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
