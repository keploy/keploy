package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A streaming answer (SSE, NDJSON, multipart) is never a test case: keploy does
// not support one. The ingress knows it at the answer's headers, in both
// loops, so the exchange's window ends there, while the stream goes on: an open
// window holds every mock the app makes, for as long as the stream lasts (an
// MCP subscription is held open by its client for minutes). The capture hook
// never runs for it, and the client still gets the stream whole. An answer
// that is chunked but no stream by its Content-Type (text/plain) is captured,
// as before.
func TestHandleHttp1Connection_AStreamIsNotRecordedAndHoldsNoWindow(t *testing.T) {
	cases := []struct {
		contentType string
		stream      bool
	}{
		{"text/event-stream", true},
		{"text/event-stream; charset=utf-8", true},
		{"application/x-ndjson", true},
		{"multipart/x-mixed-replace; boundary=frame", true},
		{"text/plain", false},
	}
	for _, synchronous := range []bool{true, false} {
		for _, c := range cases {
			name := map[bool]string{true: "synchronous", false: "zero-copy"}[synchronous] + "/" + c.contentType
			t.Run(name, func(t *testing.T) {
				stubIngressPaused(t, func() bool { return false })
				mgr := syncMock.New(zap.NewNop())

				release := make(chan struct{})
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", c.contentType)
					_, _ = w.Write([]byte("data: first\n\n"))
					w.(http.Flusher).Flush() // chunked: the answer streams
					<-release
					_, _ = w.Write([]byte("data: last\n\n"))
				}))
				t.Cleanup(upstream.Close)
				var releaseOnce sync.Once
				releaseStream := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(releaseStream)

				var hookRuns atomic.Int32
				stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
					req *http.Request, resp *http.Response, reqTS, respTS time.Time,
					opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
					hookRuns.Add(1)
				})

				pm := &IngressProxyManager{
					logger:      zap.NewNop(),
					tcChan:      make(chan *models.TestCase, 4),
					synchronous: synchronous,
					samplingSem: make(chan struct{}, 1),
				}
				t.Cleanup(pm.captures.Wait)
				ln, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ln.Close() })
				accepted := make(chan net.Conn, 1)
				go func() {
					if conn, aerr := ln.Accept(); aerr == nil {
						accepted <- conn
					}
				}()
				client, err := net.Dial("tcp4", ln.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = client.Close() })
				server := <-accepted
				t.Cleanup(func() { _ = server.Close() })

				ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
				defer cancel()
				handlerDone := make(chan struct{})
				go func() {
					defer close(handlerDone)
					pm.handleHttp1Connection(ctx, server, strings.TrimPrefix(upstream.URL, "http://"), pm.logger, pm.tcChan, make(chan struct{}, 1), 8080)
				}()
				// Run before the hook is put back (cleanups run last-in
				// first-out): a failing assertion still ends this exchange,
				// and any capture it starts, while this test's hook is in.
				t.Cleanup(func() {
					releaseStream()
					_ = client.Close()
					<-handlerDone
					pm.captures.Wait()
				})

				if _, err := client.Write([]byte("GET /events HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n")); err != nil {
					t.Fatal(err)
				}
				resp, err := http.ReadResponse(bufio.NewReader(client), nil)
				if err != nil {
					t.Fatal(err)
				}

				// The headers are through and the body is still streaming.
				want := 1
				if c.stream {
					want = 0
				}
				deadline := time.Now().Add(3 * time.Second)
				for mgr.OpenWindows() != want {
					if time.Now().After(deadline) {
						t.Fatalf("%d windows open while the answer streams, want %d", mgr.OpenWindows(), want)
					}
					time.Sleep(time.Millisecond)
				}
				releaseStream()
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if got := string(body); got != "data: first\n\ndata: last\n\n" {
					t.Fatalf("the client got %q, want the app's answer whole", got)
				}
				_ = client.Close()
				<-handlerDone
				pm.captures.Wait()

				if got := hookRuns.Load(); c.stream && got != 0 {
					t.Fatalf("the capture hook ran %d times for a stream, want none", got)
				} else if !c.stream && got != 1 {
					t.Fatalf("the capture hook ran %d times for an answer that is no stream, want once", got)
				}
				if n := mgr.OpenWindows(); n != 0 {
					t.Fatalf("%d windows open after the exchange, want none", n)
				}
			})
		}
	}
}

// The decision at a stream's headers ends its window and tells it at Info,
// unless the recording's filters leave the request out anyway: an SSE
// endpoint a user excluded, to which its clients reconnect every few seconds,
// is no line in the log each time. An answer that is no stream, or one HTTP
// says has no body whatever its Content-Type (a 204), is left to the capture.
func TestNotRecordedStream_EndsTheWindowAndTellsOnlyWhatTheFiltersKeep(t *testing.T) {
	for _, c := range []struct {
		name        string
		status      int
		contentType string
		excluded    bool
		stream      bool
		told        int
	}{
		{"a stream", http.StatusOK, "text/event-stream", false, true, 1},
		{"a stream the filters exclude", http.StatusOK, "text/event-stream", true, true, 0},
		{"no stream", http.StatusOK, "application/json", false, false, 0},
		{"a stream's 204", http.StatusNoContent, "text/event-stream", false, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			pm := &IngressProxyManager{}
			if c.excluded {
				pm.incomingOpts.Store(&models.IncomingOptions{Filters: []models.Filter{{
					BypassRule:   models.BypassRule{Path: "^/events$"},
					FilterPolicy: models.Exclude,
				}}})
			}
			mgr := syncMock.New(zap.NewNop())
			win := mgr.OpenWindow(time.Now(), nil)
			req := httptest.NewRequest(http.MethodGet, "/events", nil)
			resp := &http.Response{StatusCode: c.status, Header: http.Header{"Content-Type": {c.contentType}}, Request: req}

			if got := pm.notRecordedStream(zap.New(core), req, resp, &win); got != c.stream {
				t.Fatalf("notRecordedStream = %v, want %v", got, c.stream)
			}
			open := 1
			if c.stream {
				open = 0
			}
			if n := mgr.OpenWindows(); n != open || (win == nil) != c.stream {
				t.Fatalf("%d windows open (window nil: %v) after the decision, want %d", n, win == nil, open)
			}
			if n := logs.FilterMessageSnippet("Not recording a streaming response").Len(); n != c.told {
				t.Fatalf("told %d times, want %d", n, c.told)
			}
			win.Close()
		})
	}
}
