package pkg

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestSimulateHTTP_SSEStopsAfterExpectedEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		flusher, ok := w.(http.Flusher)
		require.True(t, ok)

		_, _ = fmt.Fprint(w, "data: one\n\n")
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)

		_, _ = fmt.Fprint(w, "data: two\n\n")
		flusher.Flush()

		// Simulate an open SSE stream that keeps running.
		time.Sleep(3 * time.Second)
		_, _ = fmt.Fprint(w, "data: three\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	tc := &models.TestCase{
		Kind: models.HTTP,
		Name: "sse-stream-test",
		HTTPReq: models.HTTPReq{
			Method:     models.Method(http.MethodGet),
			ProtoMajor: 1,
			ProtoMinor: 1,
			URL:        server.URL,
			Header: map[string]string{
				"Accept":     "text/event-stream",
				"Connection": "keep-alive",
			},
		},
		HTTPResp: models.HTTPResp{
			StreamType: models.HTTPStreamTypeSSE,
			StreamEvents: []models.HTTPStreamEvent{
				{Sequence: 1, Data: "data: one"},
				{Sequence: 2, Data: "data: two"},
			},
		},
	}

	start := time.Now()
	resp, err := SimulateHTTP(context.Background(), tc, "test-set", zap.NewNop(), 5, 0, "")
	require.NoError(t, err)
	require.Less(t, time.Since(start), 2*time.Second)
	require.Equal(t, models.HTTPStreamTypeSSE, resp.StreamType)
	require.Len(t, resp.StreamEvents, 2)
	require.Equal(t, "data: one", resp.StreamEvents[0].Data)
	require.Equal(t, "data: two", resp.StreamEvents[1].Data)
}

func TestSimulateHTTP_HTTPStreamMatchesRecordedEventBoundaries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)

		_, _ = fmt.Fprint(w, "He")
		flusher.Flush()
		time.Sleep(40 * time.Millisecond)

		_, _ = fmt.Fprint(w, "llo")
		flusher.Flush()
		time.Sleep(40 * time.Millisecond)

		_, _ = fmt.Fprint(w, "World")
		flusher.Flush()

		time.Sleep(2 * time.Second)
		_, _ = fmt.Fprint(w, "!")
		flusher.Flush()
	}))
	defer server.Close()

	tc := &models.TestCase{
		Kind: models.HTTP,
		Name: "http-stream-test",
		HTTPReq: models.HTTPReq{
			Method:     models.Method(http.MethodGet),
			ProtoMajor: 1,
			ProtoMinor: 1,
			URL:        server.URL,
			Header: map[string]string{
				"Connection": "keep-alive",
			},
		},
		HTTPResp: models.HTTPResp{
			StreamType: models.HTTPStreamTypeHTTP,
			StreamEvents: []models.HTTPStreamEvent{
				{Sequence: 1, Data: "Hello"},
				{Sequence: 2, Data: "World"},
			},
		},
	}

	start := time.Now()
	resp, err := SimulateHTTP(context.Background(), tc, "test-set", zap.NewNop(), 5, 0, "")
	require.NoError(t, err)
	require.Less(t, time.Since(start), 2*time.Second)
	require.Equal(t, models.HTTPStreamTypeHTTP, resp.StreamType)
	require.Len(t, resp.StreamEvents, 2)
	require.Equal(t, "Hello", resp.StreamEvents[0].Data)
	require.Equal(t, "World", resp.StreamEvents[1].Data)
}
