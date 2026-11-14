package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/logleak"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The ingress logs a call it does not record by facts about it (its method,
// host, path's shape and query's names), never its URL as sent: a streamed
// answer's URL often carries its token in the query (an EventSource can send
// no header), and a path a reset link's. A request it cannot parse is logged
// with the parser's error withheld: it quotes the request line or a header
// line. It logged the URL whole, at INFO, for every streamed answer.
func TestTheIngressLogsNoneOfTheCall(t *testing.T) {
	const s = logleak.Canary
	for _, tc := range []struct {
		name string
		send string
		// app answers the request the ingress hands on.
		app http.HandlerFunc
		// line is the message the case must drive (either loop's, "|"
		// between them).
		line string
	}{
		{
			name: "a streamed answer",
			line: "Not recording a streaming response as a test case",
			send: "GET /v1/events/" + s + "?access_token=" + s + " HTTP/1.1\r\nHost: app.local\r\nAuthorization: Bearer " + s + "\r\nConnection: close\r\n\r\n",
			app: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: " + s + "\n\n"))
			},
		},
		{
			name: "a malformed header line",
			line: "Failed to read client request|Failed to parse downstream HTTP request",
			send: "GET /v1/me HTTP/1.1\r\nHost: app.local\r\nAuthorization Bearer " + s + "\r\n\r\n",
			app:  func(w http.ResponseWriter, _ *http.Request) {},
		},
		{
			name: "a malformed request line",
			line: "Failed to read client request|Failed to parse downstream HTTP request",
			send: "GET /v1/me?access_token=" + s + "\r\nHost: app.local\r\n\r\n",
			app:  func(w http.ResponseWriter, _ *http.Request) {},
		},
	} {
		for _, synchronous := range []bool{true, false} {
			t.Run(tc.name+"/"+map[bool]string{true: "synchronous", false: "zero-copy"}[synchronous], func(t *testing.T) {
				stubIngressPaused(t, func() bool { return false })
				upstream := httptest.NewServer(tc.app)
				t.Cleanup(upstream.Close)
				logger, logs := logleak.Observe()
				pm := &IngressProxyManager{
					logger:      logger,
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

				ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())), 10*time.Second)
				defer cancel()
				handlerDone := make(chan struct{})
				go func() {
					defer close(handlerDone)
					pm.handleHttp1Connection(ctx, server, strings.TrimPrefix(upstream.URL, "http://"), logger, pm.tcChan, make(chan struct{}, 1), 8080)
				}()
				if _, err := client.Write([]byte(tc.send)); err != nil {
					t.Fatal(err)
				}
				// A request the ingress refuses may be answered with
				// nothing: the read is bounded.
				_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
				if resp, err := http.ReadResponse(bufio.NewReader(client), nil); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				_ = client.Close()
				<-handlerDone
				pm.captures.Wait()
				drove := false
				for _, line := range strings.Split(tc.line, "|") {
					drove = drove || logs.FilterMessageSnippet(line).Len() > 0
				}
				if !drove {
					t.Fatalf("precondition: the ingress did not log the case's line (%q); the test drove nothing", tc.line)
				}
				logleak.Check(t, logs, s, "the ingress")
			})
		}
	}
}

// The lines that say why a call is not recorded name it by its shape.
func TestTheNotRecordedLinesNameTheCallByItsShape(t *testing.T) {
	const s = logleak.Canary
	req := httptest.NewRequest(http.MethodPost, "/v1/reset/"+s+"?token="+s, nil)
	logger, logs := logleak.Observe()
	tellClientLeft(logger, req)
	warnNotRecorded(logger, req, &http.Response{StatusCode: http.StatusOK}, errors.New("the app answered early"))
	if len(logs.All()) != 2 {
		t.Fatalf("logged %d lines, want 2", len(logs.All()))
	}
	logleak.Check(t, logs, s, "the not-recorded lines")
}
