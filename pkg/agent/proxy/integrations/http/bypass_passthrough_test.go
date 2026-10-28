package http

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

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A call a bypassRule names reaches its real server in keploy test, and the
// server's answer reaches the client as the server gave it.
func TestPassOneCallThroughReachesTheServer(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Real", "yes")
		_, _ = io.WriteString(w, r.Method+" "+r.URL.Path+" "+string(body))
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	client, proxySide := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = proxySide.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	raw := []byte("POST /v1/n HTTP/1.1\r\nHost: " + addr + "\r\nContent-Length: 3\r\n\r\nabc")
	errc := make(chan error, 1)
	go func() {
		errc <- passOneCallThrough(ctx, zap.NewNop(), proxySide, &models.ConditionalDstCfg{Addr: addr}, raw, "POST")
	}()
	resp, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("the client got no answer: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if err := <-errc; err != nil {
		t.Fatalf("passOneCallThrough: %v", err)
	}
	if hits != 1 || resp.StatusCode != 200 || resp.Header.Get("X-Real") != "yes" || string(body) != "POST /v1/n abc" {
		t.Fatalf("hits %d, answer %d %q %q; want the real server's answer to the one call", hits, resp.StatusCode, resp.Header.Get("X-Real"), body)
	}

	if err := passOneCallThrough(ctx, zap.NewNop(), proxySide, &models.ConditionalDstCfg{}, raw, "POST"); err == nil {
		t.Fatal("a call with no destination address was passed through")
	}
}
