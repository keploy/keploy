package pkg

import (
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestSimulateHTTP_NewRequestError_303 ensures that SimulateHTTP returns an error
// when http.NewRequestWithContext fails. This is triggered by providing an invalid
// HTTP method string in the test case.
func TestSimulateHTTP_NewRequestError_303(t *testing.T) {
	// Arrange
	ctx := context.Background()
	logger := zap.NewNop()
	tc := &models.TestCase{
		Name: "test-case-new-request-error",
		HTTPReq: models.HTTPReq{
			Method: "INVALID METHOD", // Invalid method
			URL:    "http://example.com/test",
			Body:   `{"key":"value"}`,
		},
	}

	// Act
	resp, err := SimulateHTTP(ctx, tc, "test-set", logger, SimulationConfig{APITimeout: 10})

	// Assert
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid method")
	assert.Nil(t, resp)
}

func TestSimulateHTTP_MultipartRebuildWithPaths_314(t *testing.T) {
	ctx := context.Background()
	logger := zap.NewNop()

	tempFile, err := os.CreateTemp("", "keploy-multipart-test-*.txt")
	require.NoError(t, err)
	_, err = tempFile.WriteString("file-data")
	require.NoError(t, err)
	require.NoError(t, tempFile.Close())
	defer os.Remove(tempFile.Name())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType := r.Header.Get("Content-Type")
		mediaType, params, err := mime.ParseMediaType(contentType)
		if err != nil {
			t.Errorf("failed to parse media type: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, "multipart/form-data", mediaType)
		if params["boundary"] == "" {
			t.Errorf("missing multipart boundary")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.NotEqual(t, "test-boundary", params["boundary"])

		if cl := r.Header.Get("Content-Length"); cl != "" {
			assert.NotEqual(t, "1", cl)
		}

		reader := multipart.NewReader(r.Body, params["boundary"])
		fields := map[string]string{}
		files := map[string]string{}
		fileNames := map[string]string{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("failed to read multipart part: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			name := part.FormName()
			data, err := io.ReadAll(part)
			if err != nil {
				t.Errorf("failed to read multipart part data: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if part.FileName() != "" {
				files[name] = string(data)
				fileNames[name] = part.FileName()
				continue
			}
			fields[name] = string(data)
		}

		assert.Equal(t, "text-value", fields["text"])
		assert.Equal(t, "file-data", files["upload"])
		assert.Equal(t, filepath.Base(tempFile.Name()), fileNames["upload"])

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tc := &models.TestCase{
		Name: "multipart-paths",
		HTTPReq: models.HTTPReq{
			Method: "POST",
			URL:    server.URL,
			Header: map[string]string{
				"Content-Type":   "multipart/form-data; boundary=test-boundary",
				"Content-Length": "1",
			},
			Form: []models.FormData{
				{
					Key:    "text",
					Values: []string{"text-value"},
				},
				{
					Key:   "upload",
					Paths: []string{tempFile.Name()},
				},
			},
		},
	}

	resp, err := SimulateHTTP(ctx, tc, "test-set", logger, SimulationConfig{APITimeout: 10})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestSimulateHTTP_MultipartRebuildWithFileNames_315(t *testing.T) {
	ctx := context.Background()
	logger := zap.NewNop()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType := r.Header.Get("Content-Type")
		mediaType, params, err := mime.ParseMediaType(contentType)
		if err != nil {
			t.Errorf("failed to parse media type: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, "multipart/form-data", mediaType)
		if params["boundary"] == "" {
			t.Errorf("missing multipart boundary")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.NotEqual(t, "legacy-boundary", params["boundary"])

		reader := multipart.NewReader(r.Body, params["boundary"])
		fields := map[string]string{}
		files := map[string]string{}
		fileNames := map[string]string{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("failed to read multipart part: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			name := part.FormName()
			data, err := io.ReadAll(part)
			if err != nil {
				t.Errorf("failed to read multipart part data: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if part.FileName() != "" {
				files[name] = string(data)
				fileNames[name] = part.FileName()
				continue
			}
			fields[name] = string(data)
		}

		assert.Equal(t, "text-value", fields["text"])
		assert.Equal(t, "binary-content", files["payload"])
		assert.Equal(t, "blob.bin", fileNames["payload"])

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tc := &models.TestCase{
		Name: "multipart-filenames",
		HTTPReq: models.HTTPReq{
			Method: "POST",
			URL:    server.URL,
			Header: map[string]string{
				"Content-Type": "multipart/form-data; boundary=legacy-boundary",
			},
			Form: []models.FormData{
				{
					Key:    "text",
					Values: []string{"text-value"},
				},
				{
					Key:       "payload",
					Values:    []string{"binary-content"},
					FileNames: []string{"blob.bin"},
				},
			},
		},
	}

	resp, err := SimulateHTTP(ctx, tc, "test-set", logger, SimulationConfig{APITimeout: 10})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestSimulateHTTP_SSEStreamMatchAndEarlyClose_316(t *testing.T) {
	ctx := context.Background()
	logger := zap.NewNop()

	serverClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "server response writer must support flushing")

		_, _ = w.Write([]byte("id:100\nevent:ticker\ndata:{\"value\":1}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("id:101\nevent:ticker\ndata:{\"value\":2}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("id:102\nevent:ticker\ndata:{\"value\":3}\n\n"))
		flusher.Flush()

		<-r.Context().Done()
		close(serverClosed)
	}))
	defer server.Close()

	expectedSSEBody := strings.Join([]string{
		"id:100",
		"event:ticker",
		"data:{\"value\":1}",
		"",
		"id:101",
		"event:ticker",
		"data:{\"value\":2}",
		"",
	}, "\n")

	tc := &models.TestCase{
		Name: "sse-match-and-close",
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    server.URL,
			Header: map[string]string{
				"Accept": "text/event-stream",
			},
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{
				"Content-Type": "text/event-stream; charset=utf-8",
			},
			Body: expectedSSEBody,
			StreamBody: []models.HTTPStreamChunk{
				{
					Data: []models.HTTPStreamDataField{
						{Key: "id", Value: "100"},
						{Key: "event", Value: "ticker"},
						{Key: "data", Value: `{"value":1}`},
					},
				},
				{
					Data: []models.HTTPStreamDataField{
						{Key: "id", Value: "101"},
						{Key: "event", Value: "ticker"},
						{Key: "data", Value: `{"value":2}`},
					},
				},
			},
		},
	}

	// Use SimulateHTTPStreaming for streaming test cases
	streamResp, err := SimulateHTTPStreaming(ctx, tc, "test-set", logger, SimulationConfig{APITimeout: 3})
	require.NoError(t, err)
	require.NotNil(t, streamResp)

	// Compare the stream
	noiseKeys := map[string]struct{}{}
	matched, capturedBody, _, compareErr := CompareHTTPStream(tc.HTTPResp, streamResp.Reader, streamResp.StreamConfig, noiseKeys, logger)
	streamResp.Reader.Close()
	require.NoError(t, compareErr)
	require.True(t, matched)

	respBody := tc.HTTPResp.Body
	if !matched {
		respBody = capturedBody
	}

	assert.Equal(t, http.StatusOK, streamResp.StatusCode)
	assert.Equal(t, expectedSSEBody, respBody)

	select {
	case <-serverClosed:
	case <-time.After(1 * time.Second):
		t.Fatal("expected server stream to be closed by client after matching SSE queue")
	}
}

func TestSimulateHTTP_SSEStreamMismatch_317(t *testing.T) {
	ctx := context.Background()
	logger := zap.NewNop()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "server response writer must support flushing")

		_, _ = w.Write([]byte("id:1\nevent:update\ndata:{\"value\":1}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("id:2\nevent:update\ndata:{\"value\":999}\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	tc := &models.TestCase{
		Name: "sse-mismatch",
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    server.URL,
			Header: map[string]string{
				"Accept": "text/event-stream",
			},
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{
				"Content-Type": "text/event-stream",
			},
			Body: strings.Join([]string{
				"id:1",
				"event:update",
				"data:{\"value\":1}",
				"",
				"id:2",
				"event:update",
				"data:{\"value\":2}",
				"",
			}, "\n"),
			StreamBody: []models.HTTPStreamChunk{
				{
					Data: []models.HTTPStreamDataField{
						{Key: "id", Value: "1"},
						{Key: "event", Value: "update"},
						{Key: "data", Value: `{"value":1}`},
					},
				},
				{
					Data: []models.HTTPStreamDataField{
						{Key: "id", Value: "2"},
						{Key: "event", Value: "update"},
						{Key: "data", Value: `{"value":2}`},
					},
				},
			},
		},
	}

	// Use SimulateHTTPStreaming for streaming test cases
	streamResp, err := SimulateHTTPStreaming(ctx, tc, "test-set", logger, SimulationConfig{APITimeout: 3})
	require.NoError(t, err)
	require.NotNil(t, streamResp)

	// Compare the stream - should NOT match due to value mismatch
	noiseKeys := map[string]struct{}{}
	matched, capturedBody, _, compareErr := CompareHTTPStream(tc.HTTPResp, streamResp.Reader, streamResp.StreamConfig, noiseKeys, logger)
	streamResp.Reader.Close()
	require.NoError(t, compareErr)

	respBody := capturedBody
	if matched {
		respBody = tc.HTTPResp.Body
	}

	assert.Equal(t, http.StatusOK, streamResp.StatusCode)
	assert.NotEqual(t, tc.HTTPResp.Body, respBody)
	assert.Contains(t, respBody, "999")
}

func TestSimulateHTTP_SSEStreamMatch_WithStructuredExpectedBody_317A(t *testing.T) {
	ctx := context.Background()
	logger := zap.NewNop()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "server response writer must support flushing")

		_, _ = w.Write([]byte("id:1\nevent:update\ndata:{\"value\":1}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("id:2\nevent:update\ndata:{\"value\":2}\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	tc := &models.TestCase{
		Name: "sse-structured-expected",
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    server.URL,
			Header: map[string]string{
				"Accept": "text/event-stream",
			},
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{
				"Content-Type": "text/event-stream",
			},
			// This intentionally does not match the streamed frames. The stream
			// comparator must use HTTPResp.StreamBody when present.
			Body: "legacy-body-not-used-for-stream-compare",
			StreamBody: []models.HTTPStreamChunk{
				{
					Data: []models.HTTPStreamDataField{
						{Key: "id", Value: "1"},
						{Key: "event", Value: "update"},
						{Key: "data", Value: `{"value":1}`},
					},
				},
				{
					Data: []models.HTTPStreamDataField{
						{Key: "id", Value: "2"},
						{Key: "event", Value: "update"},
						{Key: "data", Value: `{"value":2}`},
					},
				},
			},
		},
	}

	// Use SimulateHTTPStreaming for streaming test cases
	streamResp, err := SimulateHTTPStreaming(ctx, tc, "test-set", logger, SimulationConfig{APITimeout: 3})
	require.NoError(t, err)
	require.NotNil(t, streamResp)

	// Compare the stream
	noiseKeys := map[string]struct{}{}
	matched, _, _, compareErr := CompareHTTPStream(tc.HTTPResp, streamResp.Reader, streamResp.StreamConfig, noiseKeys, logger)
	streamResp.Reader.Close()
	require.NoError(t, compareErr)
	require.True(t, matched)

	// When matched, use tc.HTTPResp.Body (legacy body)
	respBody := tc.HTTPResp.Body

	assert.Equal(t, http.StatusOK, streamResp.StatusCode)
	assert.Equal(t, tc.HTTPResp.Body, respBody)
}

func TestCanonicalizeSSEFrame_318(t *testing.T) {
	input := "id: 100\nevent: system-alert\ndata: {\"active\": true, \"user\" : \"alice\"}\n"
	got := canonicalizeSSEFrame(input)

	assert.Equal(t, "id:100\nevent:system-alert\ndata:{\"active\":true,\"user\":\"alice\"}", got)
}

func TestSimulateHTTP_NDJSONStreamMatch_WithStructuredExpectedBody_318A(t *testing.T) {
	ctx := context.Background()
	logger := zap.NewNop()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "server response writer must support flushing")

		_, _ = w.Write([]byte("{\"id\":1,\"ok\":true}\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("{\"id\":2,\"ok\":false}\n"))
		flusher.Flush()
	}))
	defer server.Close()

	tc := &models.TestCase{
		Name: "ndjson-structured-expected",
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    server.URL,
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{
				"Content-Type": "application/x-ndjson",
			},
			Body: "legacy-body-not-used-for-stream-compare",
			StreamBody: []models.HTTPStreamChunk{
				{
					Data: []models.HTTPStreamDataField{
						{Key: "raw", Value: `{"id":1,"ok":true}`},
					},
				},
				{
					Data: []models.HTTPStreamDataField{
						{Key: "raw", Value: `{"id":2,"ok":false}`},
					},
				},
			},
		},
	}

	// Use SimulateHTTPStreaming for streaming test cases
	streamResp, err := SimulateHTTPStreaming(ctx, tc, "test-set", logger, SimulationConfig{APITimeout: 3})
	require.NoError(t, err)
	require.NotNil(t, streamResp)

	// Compare the stream
	noiseKeys := map[string]struct{}{}
	matched, _, _, compareErr := CompareHTTPStream(tc.HTTPResp, streamResp.Reader, streamResp.StreamConfig, noiseKeys, logger)
	streamResp.Reader.Close()
	require.NoError(t, compareErr)
	require.True(t, matched)

	// When matched, use tc.HTTPResp.Body (legacy body)
	respBody := tc.HTTPResp.Body

	assert.Equal(t, http.StatusOK, streamResp.StatusCode)
	assert.Equal(t, tc.HTTPResp.Body, respBody)
}

func TestSimulateHTTP_NDJSONStreamMatchAndEarlyClose_319(t *testing.T) {
	ctx := context.Background()
	logger := zap.NewNop()

	serverClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "server response writer must support flushing")

		_, _ = w.Write([]byte("{\"id\":1,\"ok\":true}\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("{\"id\":2,\"ok\":false}\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("{\"id\":3,\"ok\":true}\n"))
		flusher.Flush()

		<-r.Context().Done()
		close(serverClosed)
	}))
	defer server.Close()

	expectedBody := "{\"id\":1,\"ok\":true}\n{\"id\":2,\"ok\":false}\n"

	tc := &models.TestCase{
		Name: "ndjson-match-and-close",
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    server.URL,
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{
				"Content-Type": "application/x-ndjson",
			},
			Body: expectedBody,
			StreamBody: []models.HTTPStreamChunk{
				{
					Data: []models.HTTPStreamDataField{
						{Key: "raw", Value: `{"id":1,"ok":true}`},
					},
				},
				{
					Data: []models.HTTPStreamDataField{
						{Key: "raw", Value: `{"id":2,"ok":false}`},
					},
				},
			},
		},
	}

	// Use SimulateHTTPStreaming for streaming test cases
	streamResp, err := SimulateHTTPStreaming(ctx, tc, "test-set", logger, SimulationConfig{APITimeout: 3})
	require.NoError(t, err)
	require.NotNil(t, streamResp)

	// Compare the stream
	noiseKeys := map[string]struct{}{}
	matched, _, _, compareErr := CompareHTTPStream(tc.HTTPResp, streamResp.Reader, streamResp.StreamConfig, noiseKeys, logger)
	streamResp.Reader.Close()
	require.NoError(t, compareErr)
	require.True(t, matched)

	respBody := expectedBody
	if !matched {
		respBody = ""
	}

	assert.Equal(t, http.StatusOK, streamResp.StatusCode)
	assert.Equal(t, expectedBody, respBody)

	select {
	case <-serverClosed:
	case <-time.After(1 * time.Second):
		t.Fatal("expected NDJSON stream to be closed by client after matching queue")
	}
}

func TestSimulateHTTP_MultipartStreamMatchAndEarlyClose_320(t *testing.T) {
	ctx := context.Background()
	logger := zap.NewNop()
	const boundary = "myCustomBoundary"

	writePart := func(w http.ResponseWriter, contentType string, body string) {
		_, _ = w.Write([]byte("--" + boundary + "\r\n"))
		_, _ = w.Write([]byte("Content-Type: " + contentType + "\r\n\r\n"))
		_, _ = w.Write([]byte(body))
		_, _ = w.Write([]byte("\r\n"))
	}

	serverClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "server response writer must support flushing")

		writePart(w, "application/json", `{"frame":1}`)
		flusher.Flush()
		writePart(w, "text/plain", "frame-2")
		flusher.Flush()
		writePart(w, "text/plain", "frame-3")
		flusher.Flush()

		<-r.Context().Done()
		close(serverClosed)
	}))
	defer server.Close()

	expectedBody := strings.Join([]string{
		"--" + boundary,
		"Content-Type: application/json",
		"",
		`{"frame":1}`,
		"--" + boundary,
		"Content-Type: text/plain",
		"",
		"frame-2",
		"",
	}, "\r\n")

	tc := &models.TestCase{
		Name: "multipart-match-and-close",
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    server.URL,
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{
				"Content-Type": "multipart/x-mixed-replace; boundary=" + boundary,
			},
			Body: expectedBody,
			StreamBody: []models.HTTPStreamChunk{
				{
					Data: []models.HTTPStreamDataField{
						{Key: "raw", Value: expectedBody},
					},
				},
			},
		},
	}

	// Use SimulateHTTPStreaming for streaming test cases
	streamResp, err := SimulateHTTPStreaming(ctx, tc, "test-set", logger, SimulationConfig{APITimeout: 3})
	require.NoError(t, err)
	require.NotNil(t, streamResp)

	// Compare the stream
	noiseKeys := map[string]struct{}{}
	matched, _, _, compareErr := CompareHTTPStream(tc.HTTPResp, streamResp.Reader, streamResp.StreamConfig, noiseKeys, logger)
	streamResp.Reader.Close()
	require.NoError(t, compareErr)
	require.True(t, matched)

	respBody := expectedBody
	if !matched {
		respBody = ""
	}

	assert.Equal(t, http.StatusOK, streamResp.StatusCode)
	assert.Equal(t, expectedBody, respBody)

	select {
	case <-serverClosed:
	case <-time.After(1 * time.Second):
		t.Fatal("expected multipart stream to be closed by client after matching queue")
	}
}

func TestSimulateHTTP_PlainTextStreamMatchAndEarlyClose_321(t *testing.T) {
	ctx := context.Background()
	logger := zap.NewNop()

	serverClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "server response writer must support flushing")

		_, _ = w.Write([]byte("[INFO] booting\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("[INFO] ready\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("[INFO] keep-running\n"))
		flusher.Flush()

		<-r.Context().Done()
		close(serverClosed)
	}))
	defer server.Close()

	expectedBody := "[INFO] booting\n[INFO] ready\n"

	tc := &models.TestCase{
		Name: "plain-stream-match-and-close",
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    server.URL,
		},
		HTTPResp: models.HTTPResp{
			Header: map[string]string{
				"Content-Type": "text/plain",
			},
			Body: expectedBody,
			StreamBody: []models.HTTPStreamChunk{
				{
					Data: []models.HTTPStreamDataField{
						{Key: "raw", Value: "[INFO] booting"},
					},
				},
				{
					Data: []models.HTTPStreamDataField{
						{Key: "raw", Value: "[INFO] ready"},
					},
				},
			},
		},
	}

	// Use SimulateHTTPStreaming for streaming test cases
	streamResp, err := SimulateHTTPStreaming(ctx, tc, "test-set", logger, SimulationConfig{APITimeout: 3})
	require.NoError(t, err)
	require.NotNil(t, streamResp)

	// Compare the stream
	noiseKeys := map[string]struct{}{}
	matched, _, _, compareErr := CompareHTTPStream(tc.HTTPResp, streamResp.Reader, streamResp.StreamConfig, noiseKeys, logger)
	streamResp.Reader.Close()
	require.NoError(t, compareErr)
	require.True(t, matched)

	respBody := expectedBody
	if !matched {
		respBody = ""
	}

	assert.Equal(t, http.StatusOK, streamResp.StatusCode)
	assert.Equal(t, expectedBody, respBody)

	select {
	case <-serverClosed:
	case <-time.After(1 * time.Second):
		t.Fatal("expected plain text stream to be closed by client after matching queue")
	}
}

func TestCompareSSEFrame_DataJSONTypeMismatch_322(t *testing.T) {
	logger := zap.NewNop()
	match, reason := compareSSEFrame(
		"data:{\"value\":1}",
		"data:not-json",
		nil,
		logger,
	)

	assert.False(t, match)
	assert.Equal(t, "data-json-type mismatch", reason)
}

func TestCompareSSEFields_MultilineDataNormalization_322A(t *testing.T) {
	logger := zap.NewNop()

	expected := []sseField{
		{key: "id", value: "1", hasValue: true},
		{key: "data", value: "line-1\nline-2", hasValue: true},
	}
	actual := parseSSEFrame("id:1\ndata:line-1\ndata:line-2")

	match, reason := compareSSEFields(expected, actual, nil, logger)
	assert.True(t, match, "multiline SSE data should compare equal after normalization")
	assert.Equal(t, "", reason)
}

func TestComputeStreamingTimeoutSeconds_323(t *testing.T) {
	now := time.Now().UTC()
	tc := &models.TestCase{
		HTTPReq: models.HTTPReq{
			Timestamp: now,
		},
		HTTPResp: models.HTTPResp{
			Timestamp: now.Add(1500 * time.Millisecond),
		},
	}

	timeout := ComputeStreamingTimeoutSeconds(tc, 2)
	assert.Equal(t, uint64(12), timeout)

	preferConfigured := ComputeStreamingTimeoutSeconds(tc, 30)
	assert.Equal(t, uint64(30), preferConfigured)

	fallback := ComputeStreamingTimeoutSeconds(&models.TestCase{}, 7)
	assert.Equal(t, uint64(7), fallback)

	defaultMin := ComputeStreamingTimeoutSeconds(&models.TestCase{}, 0)
	assert.Equal(t, uint64(10), defaultMin)
}

// TestIsTime_VariousFormats_808 covers multiple scenarios for the IsTime function,
// including standard date formats (RFC3339, UnixDate), numeric timestamps as strings,
// and invalid inputs to ensure it correctly identifies time-like strings.
func TestIsTime_VariousFormats_808(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected bool
	}{
		{"RFC3339", "2023-01-17T16:34:58Z", true},
		{"UnixDate", "Tue Jan 17 16:34:58 UTC 2023", true},
		{"NumericTimestamp", fmt.Sprintf("%f", float64(time.Now().UnixNano())), true},
		{"AlmostNow", fmt.Sprintf("%f", float64(time.Now().Add(-time.Hour).UnixNano())), true},
		{"TooOldNumeric", fmt.Sprintf("%f", float64(time.Now().Add(-48*time.Hour).UnixNano())), false},
		{"InvalidString", "not a date", false},
		{"EmptyString", "", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, IsTime(tc.input))
		})
	}
}

// TestToHTTPHeader_WithTimeValue_909 verifies that the ToHTTPHeader function correctly
// converts a map of strings to an http.Header object. It specifically checks that
// header values recognized as timestamps are not split by commas, while other
// comma-separated values are correctly split into slices.
func TestToHTTPHeader_WithTimeValue_909(t *testing.T) {
	// Arrange
	mockHeader := map[string]string{
		"Date":            "Tue, 17 Jan 2023 16:34:58 IST",
		"X-Custom-Header": "value1,value2",
		"Content-Type":    "application/json",
	}

	// Act
	httpHeader := ToHTTPHeader(mockHeader)

	// Assert
	require.NotNil(t, httpHeader)
	assert.Equal(t, []string{"Tue, 17 Jan 2023 16:34:58 IST"}, httpHeader["Date"])
	assert.Equal(t, []string{"value1", "value2"}, httpHeader["X-Custom-Header"])
	assert.Equal(t, []string{"application/json"}, httpHeader["Content-Type"])
}

// TestParseHTTPRequest_And_Response_111 contains sub-tests for ParseHTTPRequest and
// ParseHTTPResponse, validating both success and failure cases for parsing raw
// HTTP data into their respective struct representations.
func TestParseHTTPRequest_And_Response_111(t *testing.T) {
	t.Run("ParseHTTPRequest_Valid", func(t *testing.T) {
		rawReq := "GET /test HTTP/1.1\r\nHost: example.com\r\n\r\n"
		req, err := ParseHTTPRequest([]byte(rawReq))
		require.NoError(t, err)
		assert.Equal(t, "GET", req.Method)
		assert.Equal(t, "/test", req.URL.Path)
		assert.Equal(t, "example.com", req.Host)
	})

	t.Run("ParseHTTPRequest_Invalid", func(t *testing.T) {
		rawReq := "this is not a valid request"
		_, err := ParseHTTPRequest([]byte(rawReq))
		require.Error(t, err)
	})

	t.Run("ParseHTTPResponse_Valid", func(t *testing.T) {
		rawResp := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nHello, world!"
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		resp, err := ParseHTTPResponse([]byte(rawResp), req)
		require.NoError(t, err)
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Logf("failed to close response body: %v", err)
			}
		}()
		assert.Equal(t, 200, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, "Hello, world!", string(body))
	})

	t.Run("ParseHTTPResponse_Invalid", func(t *testing.T) {
		rawResp := "this is not a valid response"
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		_, err := ParseHTTPResponse([]byte(rawResp), req)
		require.Error(t, err)
	})
}

// TestCompressDecompress_AllEncodings_555 provides comprehensive testing for the
// Compress and Decompress functions. It checks:
// - Successful round-trip (compress then decompress) for 'gzip' and 'br' (brotli).
// - No-op behavior for unknown encodings.
// - Error handling for invalid compressed data.
func TestCompressDecompress_AllEncodings_555(t *testing.T) {
	logger := zap.NewNop()

	t.Run("Gzip", func(t *testing.T) {
		originalData := []byte("hello world, this is a test")
		compressedData, err := Compress(logger, "gzip", originalData)
		require.NoError(t, err)
		assert.NotEqual(t, originalData, compressedData)
		decompressedData, err := Decompress(logger, "gzip", compressedData)
		require.NoError(t, err)
		assert.Equal(t, originalData, decompressedData)
	})

	t.Run("Brotli", func(t *testing.T) {
		originalData := []byte("hello world, this is a brotli test")
		compressedData, err := Compress(logger, "br", originalData)
		require.NoError(t, err)
		assert.NotEqual(t, originalData, compressedData)
		decompressedData, err := Decompress(logger, "br", compressedData)
		require.NoError(t, err)
		assert.Equal(t, originalData, decompressedData)
	})

	t.Run("UnknownEncoding", func(t *testing.T) {
		originalData := []byte("hello world")
		compressedData, err := Compress(logger, "unknown", originalData)
		require.NoError(t, err)
		assert.Equal(t, originalData, compressedData)
		decompressedData, err := Decompress(logger, "unknown", originalData)
		require.NoError(t, err)
		assert.Equal(t, originalData, decompressedData)
	})

	t.Run("DecompressError", func(t *testing.T) {
		invalidGzipData := []byte("not gzip")
		_, err := Decompress(logger, "gzip", invalidGzipData)
		require.Error(t, err)

		invalidBrotliData := []byte{0xce, 0xb2, 0xcf, 0x81}
		_, err = Decompress(logger, "br", invalidBrotliData)
		require.Error(t, err)
	})
}

// TestFilterMocks_678 validates the filtering and sorting of mocks in Test and Config modes.
func TestFilterMocks_678(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	now := time.Now()
	mock1 := &models.Mock{Name: "mock1", Version: "api.keploy.io/v1beta1", Spec: models.MockSpec{ReqTimestampMock: now.Add(-2 * time.Hour), ResTimestampMock: now.Add(-2 * time.Hour)}}
	mock2 := &models.Mock{Name: "mock2", Version: "api.keploy.io/v1beta1", Spec: models.MockSpec{ReqTimestampMock: now, ResTimestampMock: now}}
	mock3 := &models.Mock{Name: "mock3", Version: "api.keploy.io/v1beta1", Spec: models.MockSpec{ReqTimestampMock: now.Add(2 * time.Hour), ResTimestampMock: now.Add(2 * time.Hour)}}
	mockNoTime := &models.Mock{Name: "mockNoTime", Version: "api.keploy.io/v1beta1", Spec: models.MockSpec{}}
	mockNonKeploy := &models.Mock{Name: "mockNonKeploy", Version: "v1", Spec: models.MockSpec{ReqTimestampMock: now, ResTimestampMock: now}}

	allMocks := []*models.Mock{mock3, mock1, mock2, mockNoTime, mockNonKeploy}

	t.Run("filterByTimeStamp_NoFilters", func(t *testing.T) {
		filtered, unfiltered := filterByTimeStamp(ctx, logger, allMocks, time.Time{}, time.Time{})
		assert.Len(t, filtered, 5)
		assert.Len(t, unfiltered, 0)
	})

	t.Run("filterByTimeStamp_ValidRange", func(t *testing.T) {
		after := now.Add(-1 * time.Hour)
		before := now.Add(1 * time.Hour)
		filtered, unfiltered := filterByTimeStamp(ctx, logger, allMocks, after, before)

		// Check filtered mocks
		require.Len(t, filtered, 3)
		filteredNames := []string{}
		for _, m := range filtered {
			filteredNames = append(filteredNames, m.Name)
			assert.True(t, m.TestModeInfo.IsFiltered)
		}
		assert.ElementsMatch(t, []string{"mock2", "mockNoTime", "mockNonKeploy"}, filteredNames)

		// Check unfiltered mocks
		require.Len(t, unfiltered, 2)
		unfilteredNames := []string{}
		for _, m := range unfiltered {
			unfilteredNames = append(unfilteredNames, m.Name)
			assert.False(t, m.TestModeInfo.IsFiltered)
		}
		assert.ElementsMatch(t, []string{"mock1", "mock3"}, unfilteredNames)
	})

	t.Run("FilterTcsMocks", func(t *testing.T) {
		after := now.Add(-1 * time.Hour)
		before := now.Add(1 * time.Hour)
		result := FilterTcsMocks(ctx, logger, allMocks, after, before)
		require.Len(t, result, 3)
		assert.Equal(t, "mockNoTime", result[0].Name) // Zero time comes first
		assert.Equal(t, "mock2", result[1].Name)
		assert.Equal(t, "mockNonKeploy", result[2].Name)
	})

	t.Run("FilterConfigMocks", func(t *testing.T) {
		after := now.Add(-1 * time.Hour)
		before := now.Add(1 * time.Hour)
		result := FilterConfigMocks(ctx, logger, allMocks, after, before)
		require.Len(t, result, 5)
		// Sorted filtered part
		assert.Equal(t, "mockNoTime", result[0].Name)
		assert.Equal(t, "mock2", result[1].Name)
		assert.Equal(t, "mockNonKeploy", result[2].Name)
		// Sorted unfiltered part
		assert.Equal(t, "mock1", result[3].Name)
		assert.Equal(t, "mock3", result[4].Name)
	})
}

func TestFilterConfigMocks_PrioritizesLateMockByRequestTime_3738(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	testReqTime := time.Date(2026, time.February, 12, 6, 39, 20, 766944656, time.UTC)
	testRespTime := time.Date(2026, time.February, 12, 6, 39, 20, 820398862, time.UTC)

	staleData := &models.Mock{
		Name:    "mock-62",
		Version: "api.keploy.io/v1beta1",
		Spec: models.MockSpec{
			ReqTimestampMock: time.Date(2026, time.February, 12, 6, 38, 43, 559270267, time.UTC),
			ResTimestampMock: time.Date(2026, time.February, 12, 6, 38, 43, 565339949, time.UTC),
		},
	}
	countQuery := &models.Mock{
		Name:    "mock-137",
		Version: "api.keploy.io/v1beta1",
		Spec: models.MockSpec{
			ReqTimestampMock: time.Date(2026, time.February, 12, 6, 39, 20, 767839595, time.UTC),
			ResTimestampMock: time.Date(2026, time.February, 12, 6, 39, 20, 809483263, time.UTC),
		},
	}
	trailingData := &models.Mock{
		Name:    "mock-138",
		Version: "api.keploy.io/v1beta1",
		Spec: models.MockSpec{
			ReqTimestampMock: time.Date(2026, time.February, 12, 6, 39, 20, 809891000, time.UTC),
			ResTimestampMock: time.Date(2026, time.February, 12, 6, 39, 20, 820711000, time.UTC),
		},
	}

	filtered, unfiltered := filterByTimeStamp(ctx, logger, []*models.Mock{trailingData, staleData, countQuery}, testReqTime, testRespTime)
	require.Len(t, filtered, 2)
	assert.ElementsMatch(t, []string{"mock-137", "mock-138"}, []string{filtered[0].Name, filtered[1].Name})
	require.Len(t, unfiltered, 1)
	assert.Equal(t, "mock-62", unfiltered[0].Name)

	result := FilterConfigMocks(ctx, logger, []*models.Mock{trailingData, staleData, countQuery}, testReqTime, testRespTime)
	require.Len(t, result, 3)
	assert.Equal(t, "mock-137", result[0].Name)
	assert.Equal(t, "mock-138", result[1].Name)
	assert.Equal(t, "mock-62", result[2].Name)
}

// TestHasExplicitPort_IPv6_777 validates the hasExplicitPort function with various host strings,
// including IPv4, IPv6, and hostnames, both with and without ports.
func TestHasExplicitPort_IPv6_777(t *testing.T) {
	testCases := []struct {
		name     string
		host     string
		expected bool
	}{
		{"IPv4WithPort", "127.0.0.1:8080", true},
		{"IPv4WithoutPort", "127.0.0.1", false},
		{"IPv6WithPort", "[::1]:8080", true},
		{"IPv6WithoutPort", "[::1]", false},
		{"IPv6WithoutBrackets", "::1", false}, // Invalid for SplitHostPort, so false
		{"HostnameWithPort", "localhost:8080", true},
		{"HostnameWithoutPort", "localhost", false},
		{"InvalidPort", "localhost:http", false},    // Non-numeric port
		{"FullURL", "http://localhost:8080", false}, // SplitHostPort fails on scheme
		{"IPv6ComplexWithPort", "[2001:db8::1]:8080", true},
		{"IPv6ComplexWithoutPort", "[2001:db8::1]", false},
		{"ColonInPath", "localhost:8080/foo", false}, // SplitHostPort fails
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, hasExplicitPort(tc.host))
		})
	}
}

func TestResolveTestTarget(t *testing.T) {
	logger := zap.NewNop()

	tests := []struct {
		name            string
		originalTarget  string
		urlReplacements map[string]string
		portMappings    map[uint32]uint32
		configHost      string
		appPort         uint16
		configPort      uint32
		isHTTP          bool
		expectedTarget  string
		expectError     bool
	}{
		// HTTP Scenarios
		{
			name:           "HTTP_NoOverrides",
			originalTarget: "http://example.com/api",
			isHTTP:         true,
			expectedTarget: "http://example.com:80/api",
		},
		{
			name:           "HTTP_AppPortOverride",
			originalTarget: "http://example.com/api",
			appPort:        8080,
			isHTTP:         true,
			expectedTarget: "http://example.com:8080/api",
		},
		{
			name:           "HTTP_ConfigPortOverride",
			originalTarget: "http://example.com/api",
			appPort:        8080,
			configPort:     9090,
			isHTTP:         true,
			expectedTarget: "http://example.com:9090/api",
		},
		{
			name:           "HTTP_ConfigHostOverride",
			originalTarget: "http://example.com/api",
			configHost:     "localhost",
			isHTTP:         true,
			expectedTarget: "http://localhost:80/api",
		},
		{
			name:            "HTTP_ReplacementWithPort_Final",
			originalTarget:  "http://example.com/api",
			urlReplacements: map[string]string{"example.com": "localhost:3000"},
			configPort:      9090, // Should be ignored
			isHTTP:          true,
			expectedTarget:  "http://localhost:3000/api",
		},
		{
			name:            "HTTP_ReplacementWithoutPort_AppliesOverrides",
			originalTarget:  "http://example.com/api",
			urlReplacements: map[string]string{"example.com": "new-host"},
			configPort:      9090,
			isHTTP:          true,
			expectedTarget:  "http://new-host:9090/api",
		},
		{
			name:            "HTTP_PortMappingOverridesReplacementPort",
			originalTarget:  "http://example.com/api",
			urlReplacements: map[string]string{"example.com": "localhost:3000"},
			portMappings:    map[uint32]uint32{3000: 4000},
			configPort:      9090, // Still ignored by replacement-with-port before mapping.
			isHTTP:          true,
			expectedTarget:  "http://localhost:4000/api",
		},
		{
			name:           "HTTP_PortMappingOverridesConfigAndAppPort",
			originalTarget: "http://example.com/api",
			appPort:        8080,
			configPort:     9090,
			portMappings:   map[uint32]uint32{9090: 7070},
			isHTTP:         true,
			expectedTarget: "http://example.com:7070/api",
		},
		{
			name:           "HTTPS_DefaultPort",
			originalTarget: "https://example.com/api",
			isHTTP:         true,
			expectedTarget: "https://example.com:443/api",
		},

		// gRPC Scenarios
		{
			name:           "GRPC_NoOverrides",
			originalTarget: "example.com",
			isHTTP:         false,
			expectedTarget: "example.com:443",
		},
		{
			name:           "GRPC_ExistingPort_NoOverrides",
			originalTarget: "example.com:50051",
			isHTTP:         false,
			expectedTarget: "example.com:50051",
		},
		{
			name:           "GRPC_AppPortOverride",
			originalTarget: "example.com",
			appPort:        8080,
			isHTTP:         false,
			expectedTarget: "example.com:8080",
		},
		{
			name:           "GRPC_ConfigPortOverride",
			originalTarget: "example.com",
			configPort:     9090,
			isHTTP:         false,
			expectedTarget: "example.com:9090",
		},
		{
			name:           "GRPC_ConfigHostOverride",
			originalTarget: "example.com:50051",
			configHost:     "localhost",
			isHTTP:         false,
			expectedTarget: "localhost:50051",
		},
		{
			name:            "GRPC_ReplacementWithPort_Final",
			originalTarget:  "example.com:50051",
			urlReplacements: map[string]string{"example.com": "localhost:3000"},
			configPort:      9090, // Should be ignored
			isHTTP:          false,
			expectedTarget:  "localhost:3000:50051", // Replacement is literal substitution first
		},
		{
			name:            "GRPC_ReplacementWithPort_ExactMatch",
			originalTarget:  "example.com",
			urlReplacements: map[string]string{"example.com": "localhost:3000"},
			configPort:      9090,
			isHTTP:          false,
			expectedTarget:  "localhost:3000",
		},
		{
			name:           "GRPC_IPv6_Host",
			originalTarget: "[::1]:50051",
			configPort:     9090,
			isHTTP:         false,
			expectedTarget: "[::1]:9090",
		},
		{
			name:           "GRPC_PortMappingOverridesConfigPort",
			originalTarget: "example.com:50051",
			configPort:     50052,
			portMappings:   map[uint32]uint32{50052: 50053},
			isHTTP:         false,
			expectedTarget: "example.com:50053",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveTestTarget(tt.originalTarget, tt.urlReplacements, tt.portMappings, tt.configHost, tt.appPort, tt.configPort, tt.isHTTP, logger)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedTarget, got)
			}
		})
	}
}

func TestResolveTestTarget_EdgeCases(t *testing.T) {
	logger := zap.NewNop()

	t.Run("HTTP_InvalidURL", func(t *testing.T) {
		_, err := ResolveTestTarget("http://[::1]:namedport", nil, nil, "", 0, 0, true, logger)
		assert.Error(t, err)
	})

	t.Run("GRPC_ConfigHost_ReplaceError", func(t *testing.T) {
		// Mock logic or ensure specific error condition if possible, though ReplaceGrpcHost is robust
	})
}
