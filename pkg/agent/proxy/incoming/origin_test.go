package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Both HTTP loops stamp a request on the proxy's clock before they forward
// it, and hand its capture that clock: the window start a
// recorded test case's origin names.
func TestHandleHttp1Connection_TellsTheCaptureTheWindowsClock(t *testing.T) {
	for _, synchronous := range []bool{true, false} {
		t.Run(map[bool]string{true: "synchronous", false: "zero-copy"}[synchronous], func(t *testing.T) {
			stubIngressPaused(t, func() bool { return false })
			mgr := syncMock.New(zap.NewNop())
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("ok"))
			}))
			t.Cleanup(upstream.Close)
			clock := make(chan string, 1)
			stubCaptureHook(t, func(ctx context.Context, _ *zap.Logger, _ chan *models.TestCase,
				_ *http.Request, _ *http.Response, _, _ time.Time, _ models.IncomingOptions, _ bool, _ bool, _ uint16) {
				clock <- hooksUtils.WindowClockFromContext(ctx)
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
				if c, aerr := ln.Accept(); aerr == nil {
					accepted <- c
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
			if _, err := client.Write([]byte("GET /url HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			select {
			case got := <-clock:
				if want := fakeconn.ClockID(fakeconn.ClockProxy); got == "" || got != want || !strings.HasPrefix(got, "proxy:") {
					t.Fatalf("the capture was told the clock %q; want the proxy's, %q", got, want)
				}
			case <-ctx.Done():
				t.Fatal("the capture hook never ran")
			}
			_ = client.Close()
			<-handlerDone
		})
	}
}
