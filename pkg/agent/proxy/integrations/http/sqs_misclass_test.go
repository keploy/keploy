package http

import (
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
		payload string
		want    bool
	}{
		// Valid HTTP — should match
		{
			name:    "GET request",
			payload: "GET /api/transfer HTTP/1.1\r\nHost: example.com\r\n\r\n",
			want:    true,
		},
		{
			name:    "POST request",
			payload: "POST /api/transfer HTTP/1.1\r\nHost: example.com\r\nContent-Type: application/json\r\n\r\n{}",
			want:    true,
		},
		{
			name:    "HTTP response",
			payload: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nOK",
			want:    true,
		},
		{
			name:    "CONNECT tunnel",
			payload: "CONNECT api.wise-sandbox.com:443 HTTP/1.1\r\nHost: api.wise-sandbox.com:443\r\n\r\n",
			want:    true,
		},
		{
			name: "AWS SQS SendMessage (valid HTTP POST)",
			payload: "POST / HTTP/1.1\r\n" +
				"Host: sqs.us-east-1.amazonaws.com\r\n" +
				"X-Amz-Target: AmazonSQS.SendMessage\r\n\r\n{}",
			want: true, // SQS IS HTTP — enterprise SQS parser at Priority 200 handles it
		},
		{
			name: "Wise API POST through CONNECT tunnel",
			payload: "POST /v3/quotes HTTP/1.1\r\n" +
				"Host: api.wise-sandbox.com\r\n" +
				"Content-Type: application/json\r\n\r\n{}",
			want: true,
		},

		// Non-HTTP — should NOT match
		{
			name:    "Postgres SSLRequest",
			payload: "\x00\x00\x00\x08\x04\xd2\x16\x2f",
			want:    false,
		},
		{
			name:    "TLS ClientHello",
			payload: "\x16\x03\x01\x00\xf1\x01\x00\x00\xed\x03\x03",
			want:    false,
		},
		{
			name:    "Binary data starting with POST but no HTTP version",
			payload: "POST \x00\x01\x02binary-garbage-no-http-version",
			want:    false,
		},
		{
			name:    "Binary data starting with GET but no HTTP version",
			payload: "GET \xff\xfe\xfd\x00\x01",
			want:    false,
		},
		{
			name:    "Method prefix only, truncated",
			payload: "GET ",
			want:    false,
		},
		{
			name:    "Empty buffer",
			payload: "",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := h.MatchType(nil, []byte(tt.payload))
			if got != tt.want {
				t.Errorf("MatchType(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
