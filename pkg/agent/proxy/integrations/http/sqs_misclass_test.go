package http

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// TestHTTPMatchTypeRequestLineValidation validates that MatchType correctly
// identifies HTTP traffic and rejects non-HTTP binary data that happens to
// start with an HTTP method prefix.
func TestHTTPMatchTypeRequestLineValidation(t *testing.T) {

	h := &HTTP{Logger: zap.NewNop()}

	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		// Valid HTTP — should match
		{
			name:    "GET request",
			payload: []byte("GET /api/transfer HTTP/1.1\r\nHost: example.com\r\n\r\n"),
			want:    true,
		},
		{
			name:    "POST request",
			payload: []byte("POST /api/transfer HTTP/1.1\r\nHost: example.com\r\nContent-Type: application/json\r\n\r\n{}"),
			want:    true,
		},
		{
			name:    "HTTP response",
			payload: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nOK"),
			want:    true,
		},
		{
			name:    "CONNECT tunnel",
			payload: []byte("CONNECT api.wise-sandbox.com:443 HTTP/1.1\r\nHost: api.wise-sandbox.com:443\r\n\r\n"),
			want:    true,
		},
		{
			name: "AWS SQS SendMessage (valid HTTP POST)",
			payload: []byte("POST / HTTP/1.1\r\n" +
				"Host: sqs.us-east-1.amazonaws.com\r\n" +
				"X-Amz-Target: AmazonSQS.SendMessage\r\n\r\n{}"),
			want: true, // SQS IS HTTP — enterprise SQS parser at Priority 200 handles it
		},
		{
			name: "Wise API POST through CONNECT tunnel",
			payload: []byte("POST /v3/quotes HTTP/1.1\r\n" +
				"Host: api.wise-sandbox.com\r\n" +
				"Content-Type: application/json\r\n\r\n{}"),
			want: true,
		},

		// Non-HTTP — should NOT match
		{
			name:    "Postgres SSLRequest",
			payload: []byte("\x00\x00\x00\x08\x04\xd2\x16\x2f"),
			want:    false,
		},
		{
			name:    "TLS ClientHello",
			payload: []byte("\x16\x03\x01\x00\xf1\x01\x00\x00\xed\x03\x03"),
			want:    false,
		},
		{
			name:    "Binary data starting with POST but no HTTP version",
			payload: []byte("POST \x00\x01\x02binary-garbage-no-http-version"),
			want:    false,
		},
		{
			name:    "Binary data starting with GET but no HTTP version",
			payload: []byte("GET \xff\xfe\xfd\x00\x01"),
			want:    false,
		},
		{
			name:    "Method prefix only, truncated",
			payload: []byte("GET "),
			want:    false,
		},
		{
			name:    "Empty buffer",
			payload: nil,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := h.MatchType(context.Background(), tt.payload)
			if got != tt.want {
				t.Errorf("MatchType(%q [%d bytes]) = %v, want %v", tt.name, len(tt.payload), got, tt.want)
			}
		})
	}
}
