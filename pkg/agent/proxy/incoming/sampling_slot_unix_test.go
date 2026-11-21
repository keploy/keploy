//go:build linux || darwin

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A sampled exchange takes a capture slot as its answer's end is found, to
// give its sampling slot back then. One that is not recorded after all (its
// client went before that end, and its app was told) gives the capture slot
// back as its handler returns: kept, every capture after it would wait for a
// slot that never comes back.
func TestASampledExchangeItsClientLeftGivesItsCaptureSlotBack(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	savedSem := captureHookSem
	t.Cleanup(func() { captureHookSem = savedSem })
	sem := make(chan struct{}, 1)
	captureHookSem = sem

	gone := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, `{"status":"RUNNING"}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(gone)
		_, _ = fmt.Fprintln(w, `{"status":"CLIENT GONE"}`)
	})}
	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(appLn) }()
	t.Cleanup(func() { _ = srv.Close() })

	captured := 0
	stubCaptureHook(t, func(context.Context, *zap.Logger, chan *models.TestCase,
		*http.Request, *http.Response, time.Time, time.Time,
		models.IncomingOptions, bool, bool, uint16) {
		captured++
	})

	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	cfg.Agent.EnableSampling = 1
	pm := New(zap.NewNop(), nil, cfg)

	client, proxied := tcpPairForWatch(t)
	served := make(chan struct{})
	t.Cleanup(func() {
		_ = client.Close()
		<-served
		pm.captures.Wait()
	})
	go func() {
		defer close(served)
		defer func() { _ = proxied.Close() }()
		pm.ServeLive(context.Background(), proxied, appLn.Addr().String(), 8083)
	}()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Write([]byte("GET /jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(client)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("the answer's first event: %v", err)
		}
		if strings.Contains(line, "RUNNING") {
			break
		}
	}
	// The client half-closes and reads the whole answer, which the app
	// ends once told: the answer's end is found, and the client went
	// before it.
	_ = client.(*net.TCPConn).CloseWrite()
	if rest, err := io.ReadAll(br); err != nil || !strings.Contains(string(rest), "CLIENT GONE") {
		t.Fatalf("the half-closed client got %q, %v", rest, err)
	}
	<-gone
	<-served
	pm.captures.Wait()

	if captured != 0 {
		t.Fatalf("the exchange its client left was captured %d time(s)", captured)
	}
	if n := len(sem); n != 0 {
		t.Fatalf("%d capture slot(s) still held once the exchange its client left was served: no capture after it could take one", n)
	}
}
