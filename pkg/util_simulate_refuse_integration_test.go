package pkg

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// startRefuseThenServe reserves a free port, releases it so connects are REFUSED
// for `refuse`, then re-binds it and serves GET /ping -> 200 {"msg":"pong"}.
// This reproduces an app container that briefly refuses connections while it is
// still coming up (the CI contention scenario the retry exists for).
func startRefuseThenServe(t *testing.T, refuse time.Duration) (baseURL string, stop func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close() // the port now actively refuses until we re-bind below

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"msg":"pong"}`))
	})
	srv := &http.Server{Handler: mux}
	go func() {
		time.Sleep(refuse) // connects to addr are refused during this window
		ln, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			return
		}
		_ = srv.Serve(ln)
	}()
	return "http://" + addr, func() { _ = srv.Close() }
}

func pingTestCase(baseURL string) *models.TestCase {
	return &models.TestCase{
		Name: "ping",
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    baseURL + "/ping",
			Header: map[string]string{},
		},
		HTTPResp: models.HTTPResp{StatusCode: http.StatusOK, Body: `{"msg":"pong"}`},
	}
}

// SimulateHTTP (the real replay send path) must RECOVER a request to a port that
// refuses briefly, via the bounded connection-refused retry — proving the retry
// is wired into the production code path, not just the helper in isolation. If
// the retry is ever removed, SimulateHTTP returns the refused error and this
// test fails (red), guarding the fix end-to-end without any CI credential.
func TestSimulateHTTP_RecoversBriefConnRefused_RefuseInteg(t *testing.T) {
	baseURL, stop := startRefuseThenServe(t, 300*time.Millisecond)
	defer stop()
	resp, err := SimulateHTTP(context.Background(), pingTestCase(baseURL), "test-set", zap.NewNop(), SimulationConfig{APITimeout: 10})
	if err != nil {
		t.Fatalf("expected recovery via the connection-refused retry, got error: %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusOK || !strings.Contains(resp.Body, "pong") {
		t.Fatalf("expected 200 pong, got %+v", resp)
	}
}

// A refuse longer than the ~900ms retry budget must FAIL — the retry is bounded
// and must never fabricate a pass for a genuinely-down app.
func TestSimulateHTTP_LongConnRefusedFailsBounded_RefuseInteg(t *testing.T) {
	baseURL, stop := startRefuseThenServe(t, 3*time.Second)
	defer stop()
	_, err := SimulateHTTP(context.Background(), pingTestCase(baseURL), "test-set", zap.NewNop(), SimulationConfig{APITimeout: 10})
	if err == nil {
		t.Fatal("expected a connection error after the retry budget exhausts (must not fabricate a pass)")
	}
}
