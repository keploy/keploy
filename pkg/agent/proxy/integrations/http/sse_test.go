package http

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestIsStreamingResponse(t *testing.T) {
	tests := []struct {
		name     string
		resp     string
		expected streamType
	}{
		{
			name:     "SSE response",
			resp:     "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\n\r\n",
			expected: streamSSE,
		},
		{
			name:     "Chunked text/plain",
			resp:     "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n",
			expected: streamChunkedText,
		},
		{
			name:     "Regular JSON response",
			resp:     "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 42\r\n\r\n{\"status\":\"ok\"}",
			expected: streamNone,
		},
		{
			name:     "text/plain without chunked",
			resp:     "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\nhello",
			expected: streamNone,
		},
		{
			name:     "SSE with charset",
			resp:     "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream; charset=utf-8\r\n\r\n",
			expected: streamSSE,
		},
		{
			name:     "No headers end",
			resp:     "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream",
			expected: streamNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := isStreamingResponse([]byte(tc.resp))
			if result != tc.expected {
				t.Errorf("isStreamingResponse() = %v, want %v", result, tc.expected)
			}
		})
	}
}

func TestParseSSEFrames(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		expected []models.SSEFrame
	}{
		{
			name: "single data frame",
			raw:  "data: hello world\n\n",
			expected: []models.SSEFrame{
				{Data: "hello world"},
			},
		},
		{
			name: "frame with all fields",
			raw:  "id: 1\nevent: message\ndata: {\"price\": 42}\nretry: 3000\n\n",
			expected: []models.SSEFrame{
				{ID: "1", Event: "message", Data: "{\"price\": 42}", Retry: 3000},
			},
		},
		{
			name: "multi-line data",
			raw:  "data: line1\ndata: line2\ndata: line3\n\n",
			expected: []models.SSEFrame{
				{Data: "line1\nline2\nline3"},
			},
		},
		{
			name: "multiple frames",
			raw:  "data: first\n\ndata: second\n\n",
			expected: []models.SSEFrame{
				{Data: "first"},
				{Data: "second"},
			},
		},
		{
			name: "frame with event type",
			raw:  "event: TICKER\ndata: {\"price\": 42.5}\n\nevent: MARKET_CLOSE\ndata: {\"status\": \"closed\"}\n\n",
			expected: []models.SSEFrame{
				{Event: "TICKER", Data: "{\"price\": 42.5}"},
				{Event: "MARKET_CLOSE", Data: "{\"status\": \"closed\"}"},
			},
		},
		{
			name:     "comment lines ignored",
			raw:      ": this is a comment\ndata: actual data\n\n",
			expected: []models.SSEFrame{{Data: "actual data"}},
		},
		{
			name:     "empty input",
			raw:      "",
			expected: nil,
		},
		{
			name: "CRLF line endings",
			raw:  "data: hello\r\n\r\n",
			expected: []models.SSEFrame{
				{Data: "hello"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := parseSSEFrames([]byte(tc.raw))
			if !reflect.DeepEqual(result, tc.expected) {
				t.Errorf("parseSSEFrames() = %+v, want %+v", result, tc.expected)
			}
		})
	}
}

func TestFormatSSEFrame(t *testing.T) {
	tests := []struct {
		name     string
		frame    models.SSEFrame
		expected string
	}{
		{
			name:     "data only",
			frame:    models.SSEFrame{Data: "hello"},
			expected: "data: hello\n\n",
		},
		{
			name:     "all fields",
			frame:    models.SSEFrame{ID: "1", Event: "update", Data: "payload", Retry: 5000},
			expected: "id: 1\nevent: update\nretry: 5000\ndata: payload\n\n",
		},
		{
			name:     "multi-line data",
			frame:    models.SSEFrame{Data: "line1\nline2"},
			expected: "data: line1\ndata: line2\n\n",
		},
		{
			name:     "event only (no data)",
			frame:    models.SSEFrame{Event: "ping"},
			expected: "event: ping\ndata: \n\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := string(formatSSEFrame(tc.frame))
			if result != tc.expected {
				t.Errorf("formatSSEFrame() = %q, want %q", result, tc.expected)
			}
		})
	}
}

func TestParseFormatRoundTrip(t *testing.T) {
	// Parse → Format → Parse should produce the same frames
	original := "id: 42\nevent: update\ndata: {\"key\": \"value\"}\n\n"

	parsed := parseSSEFrames([]byte(original))
	if len(parsed) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(parsed))
	}

	formatted := formatSSEFrame(parsed[0])
	reparsed := parseSSEFrames(formatted)

	if len(reparsed) != 1 {
		t.Fatalf("expected 1 frame after round-trip, got %d", len(reparsed))
	}

	// DelayMs is not part of wire format, so clear it for comparison
	parsed[0].DelayMs = 0
	reparsed[0].DelayMs = 0

	if !reflect.DeepEqual(parsed[0], reparsed[0]) {
		t.Errorf("round-trip mismatch:\n  original: %+v\n  reparsed: %+v", parsed[0], reparsed[0])
	}
}

func TestSerializeDeserializeRoundTrip(t *testing.T) {
	// Verify serialize→deserialize round-trip for file-based storage
	original := []models.SSEFrame{
		{ID: "1", Event: "update", Data: "{\"price\": 42.5}", Retry: 3000, DelayMs: 100},
		{ID: "2", Event: "close", Data: "done", DelayMs: 500},
		{Data: "simple data", DelayMs: 0},
	}

	serialized, err := serializeSSEFrames(original)
	if err != nil {
		t.Fatalf("serializeSSEFrames() error = %v", err)
	}

	deserialized, err := deserializeSSEFrames(serialized)
	if err != nil {
		t.Fatalf("deserializeSSEFrames() error = %v", err)
	}

	if len(deserialized) != len(original) {
		t.Fatalf("expected %d frames, got %d", len(original), len(deserialized))
	}

	for i := range original {
		if !reflect.DeepEqual(original[i], deserialized[i]) {
			t.Errorf("frame %d mismatch:\n  original:     %+v\n  deserialized: %+v", i, original[i], deserialized[i])
		}
	}
}
