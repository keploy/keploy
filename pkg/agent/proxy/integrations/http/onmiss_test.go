package http

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// A call `keploy mock replay --on-miss` serves from the real dependency is
// answered with the dependency's final response, and that is the mock it
// records: interim responses (the 100 Continue to an upload, a 103 Early
// Hints) are not the answer. net/http's ReadResponse returns the first of them
// as if it were: the app was handed a 100 as its answer while the real one
// was dropped, and the mock recorded a 100 with no body.
func TestServeOnMissAnswersWithTheFinalResponse(t *testing.T) {
	dep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body) // net/http sends the 100 Continue here
		w.Header().Add("Link", "</s.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("got:" + string(b)))
	}))
	t.Cleanup(dep.Close)
	ResetCaptured()
	t.Cleanup(ResetCaptured)

	reqBuf := []byte("POST /upload HTTP/1.1\r\nHost: dep.local\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\nabc")
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBuf)))
	if err != nil {
		t.Fatal(err)
	}
	keployEnd, appEnd := net.Pipe()
	t.Cleanup(func() { _ = keployEnd.Close(); _ = appEnd.Close() })
	_ = appEnd.SetReadDeadline(time.Now().Add(5 * time.Second))

	h := &HTTP{Logger: zaptest.NewLogger(t)}
	type served struct {
		handled bool
		err     error
	}
	result := make(chan served, 1)
	go func() {
		handled, err := h.serveOnMiss(context.Background(), keployEnd, reqBuf, request, []byte("abc"),
			&models.ConditionalDstCfg{Addr: strings.TrimPrefix(dep.URL, "http://")},
			models.OutgoingOptions{OnMiss: models.MissRecord})
		result <- served{handled, err}
	}()

	resp, err := http.ReadResponse(bufio.NewReader(appEnd), nil)
	if err != nil {
		t.Fatalf("the app got no answer: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated || string(body) != "got:abc" {
		t.Fatalf("the app got %d %q, want the dependency's final 201 %q", resp.StatusCode, body, "got:abc")
	}
	if r := <-result; !r.handled || r.err != nil {
		t.Fatalf("serveOnMiss = (%v, %v), want the miss served", r.handled, r.err)
	}
	mocks := DrainCaptured()
	if len(mocks) != 1 {
		t.Fatalf("captured %d mocks, want 1", len(mocks))
	}
	if got := mocks[0].Spec.HTTPResp; got.StatusCode != http.StatusCreated || got.Body != "got:abc" {
		t.Fatalf("recorded mock answers %d %q, want the final 201 %q", got.StatusCode, got.Body, "got:abc")
	}
}
