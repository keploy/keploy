package conn

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// trackCloser wraps a response body and records whether Close was called.
type trackCloser struct {
	io.Reader
	closed bool
}

func (tc *trackCloser) Close() error { tc.closed = true; return nil }

// gzipBomb returns a gzip stream that inflates to MaxTestCaseSize+1 bytes.
func gzipBomb(t *testing.T) []byte {
	t.Helper()
	data, err := pkg.Compress(zap.NewNop(), "gzip", make([]byte, MaxTestCaseSize+1))
	if err != nil {
		t.Fatalf("compress bomb: %v", err)
	}
	return data
}

// TestCapture_DecompressOverLimit pins the behavior when a body inflates
// past MaxTestCaseSize during capture (#3867): the exchange is dropped with
// the regular size-limit message (not a corrupt-stream error), no testcase
// is emitted, and resp.Body is closed even on the request-side early return.
func TestCapture_DecompressOverLimit(t *testing.T) {
	newCapture := func(t *testing.T, reqBody []byte, reqEncoding string, respBody []byte, respEncoding string) (*observer.ObservedLogs, *trackCloser, chan *models.TestCase) {
		t.Helper()
		core, logs := observer.New(zap.ErrorLevel)
		logger := zap.New(core)

		req, err := http.NewRequest(http.MethodPost, "http://localhost:8080/upload", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if reqEncoding != "" {
			req.Header.Set("Content-Encoding", reqEncoding)
		}

		body := &trackCloser{Reader: bytes.NewReader(respBody)}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       body,
		}
		if respEncoding != "" {
			resp.Header.Set("Content-Encoding", respEncoding)
		}

		tcChan := make(chan *models.TestCase, 1)
		Capture(context.Background(), logger, tcChan, req, resp, time.Now(), time.Now(), models.IncomingOptions{}, true, false, 8080)
		return logs, body, tcChan
	}

	assertDropped := func(t *testing.T, logs *observer.ObservedLogs, body *trackCloser, tcChan chan *models.TestCase) {
		t.Helper()
		select {
		case tc := <-tcChan:
			t.Fatalf("expected capture to be dropped, got testcase %q", tc.Name)
		default:
		}
		if !body.closed {
			t.Error("resp.Body must be closed when capture returns early")
		}
		// Exactly ONE Error-level log — the size-limit drop. Anything else
		// (Capture's decode-failure messages, or Decompress's internal
		// "failed to read the ... compressed data") is the oversized body
		// being misreported as a corrupt stream.
		if n := logs.Len(); n != 1 {
			t.Errorf("want exactly 1 error log (the size-limit drop), got %d: %v", n, logs.All())
		}
		if n := logs.FilterMessageSnippet("exceeds 5MB limit").Len(); n != 1 {
			t.Errorf("want 1 size-limit log, got %d (all: %v)", n, logs.All())
		}
	}

	t.Run("RequestBody", func(t *testing.T) {
		logs, body, tcChan := newCapture(t, gzipBomb(t), "gzip", []byte("ok"), "")
		assertDropped(t, logs, body, tcChan)
	})

	t.Run("ResponseBody", func(t *testing.T) {
		logs, body, tcChan := newCapture(t, []byte("hi"), "", gzipBomb(t), "gzip")
		assertDropped(t, logs, body, tcChan)
	})
}

func TestIsFiltered_FilterPolicy(t *testing.T) {
	logger := zap.NewNop()

	tests := []struct {
		name     string
		filters  []models.Filter
		method   string
		path     string
		expected bool
	}{
		{
			name: "Only Exclude - Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/health"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/health",
			expected: true,
		},
		{
			name: "Only Exclude - No Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/health"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/api/data",
			expected: false,
		},
		{
			name: "Only Include - Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
			},
			method:   "GET",
			path:     "/api/users",
			expected: false, // NOT filtered
		},
		{
			name: "Only Include - No Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
			},
			method:   "GET",
			path:     "/health",
			expected: true, // Filtered because it's not in the whitelist
		},
		{
			name: "Mixed - Include Match and Exclude Match (Exclude Wins)",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
				{
					BypassRule:   models.BypassRule{Path: "/api/admin"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/api/admin",
			expected: true, // Filtered because Exclude takes priority
		},
		{
			name: "Mixed - Include Match and Exclude No Match",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/.*"},
					FilterPolicy: models.Include,
				},
				{
					BypassRule:   models.BypassRule{Path: "/api/admin"},
					FilterPolicy: models.Exclude,
				},
			},
			method:   "GET",
			path:     "/api/users",
			expected: false, // Not filtered
		},
		{
			name: "Multiple Includes - One Matches",
			filters: []models.Filter{
				{
					BypassRule:   models.BypassRule{Path: "/api/v1/.*"},
					FilterPolicy: models.Include,
				},
				{
					BypassRule:   models.BypassRule{Path: "/api/v2/.*"},
					FilterPolicy: models.Include,
				},
			},
			method:   "GET",
			path:     "/api/v2/users",
			expected: false, // Not filtered
		},
		{
			name: "Backward Compatibility - Default to Exclude",
			filters: []models.Filter{
				{
					BypassRule: models.BypassRule{Path: "/health"},
					// FilterPolicy is missing (defaults to empty string, which is Exclude logic)
				},
			},
			method:   "GET",
			path:     "/health",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, "http://localhost"+tt.path, nil)
			opts := models.IncomingOptions{
				Filters: tt.filters,
			}
			got := IsFiltered(logger, req, opts)
			if got != tt.expected {
				t.Errorf("IsFiltered() = %v, want %v", got, tt.expected)
			}
		})
	}
}
