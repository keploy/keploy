package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.keploy.io/server/v3/pkg/models"
	"golang.org/x/net/http2"
)

// h2cThrough sends req from an HTTP/2 client with prior knowledge (h2c) to
// an HTTP/2 server serving handler, through the ingress g, and returns the
// answer's body.
func h2cThrough(t *testing.T, g *grpcIngress, handler http.HandlerFunc, req *http.Request) string {
	t.Helper()
	go (&http2.Server{}).ServeConn(g.appApp, &http2.ServeConnOpts{Handler: handler})
	tr := &http2.Transport{AllowHTTP: true, DialTLSContext: func(context.Context, string, string, *tls.Config) (net.Conn, error) {
		return g.cliApp, nil
	}}
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return string(body)
}

// recordedOne is the one test case g records within a few seconds.
func (g *grpcIngress) recordedOne(t *testing.T) *models.TestCase {
	t.Helper()
	select {
	case tc := <-g.tcCh:
		return tc
	case <-time.After(5 * time.Second):
		t.Fatal("no test case was recorded")
		return nil
	}
}

// An HTTP request sent over cleartext HTTP/2 (h2c: a mesh sidecar on an
// "http2" port, a client with prior knowledge) shares gRPC's preface, so the
// ingress hands its connection to the gRPC recorder. Its response has no
// trailers, and a stream was complete only once trailers came: the request
// was never recorded, and was held until its connection closed. It is
// recorded as the HTTP request it is, as http.ReadRequest / ReadResponse
// would give it on the HTTP/1.x path.
func TestRecordIncoming_AnHTTPRequestOverH2CIsRecordedAsHTTP(t *testing.T) {
	g := startGRPCIngress(t)
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Add("Link", "</a.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"got":` + string(body) + `}`))
	}
	req, err := http.NewRequest(http.MethodPost, "http://vault.local:8200/v1/auth/login?x=1", strings.NewReader(`{"role":"web"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	require.Equal(t, `{"got":{"role":"web"}}`, h2cThrough(t, g, handler, req))

	tc := g.recordedOne(t)
	require.Equal(t, models.HTTP, tc.Kind, "recorded as %s", tc.Kind)
	require.Equal(t, http.MethodPost, string(tc.HTTPReq.Method))
	require.Equal(t, "http://vault.local:8200/v1/auth/login?x=1", tc.HTTPReq.URL)
	require.Equal(t, `{"role":"web"}`, tc.HTTPReq.Body)
	require.Equal(t, http.StatusCreated, tc.HTTPResp.StatusCode, "the response's own status, not the 103 before it")
	require.Equal(t, `{"got":{"role":"web"}}`, tc.HTTPResp.Body)
	require.Contains(t, tc.HTTPResp.Header["Set-Cookie"], "a=1")
	require.Contains(t, tc.HTTPResp.Header["Set-Cookie"], "b=2")
	_, kept := tc.HTTPResp.Header["Link"]
	require.False(t, kept, "the 103's header was kept as the response's: %v", tc.HTTPResp.Header)
}

// A response's trailers are not its headers, and a GET's answer with no body
// is recorded too.
func TestRecordIncoming_AnH2CResponsesTrailersAreNotItsHeaders(t *testing.T) {
	g := startGRPCIngress(t)
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		w.Header().Set("X-Checksum", "abc")
	}
	req, err := http.NewRequest(http.MethodGet, "http://vault.local:8200/v1/sys/health", nil)
	require.NoError(t, err)
	require.Equal(t, "ok", h2cThrough(t, g, handler, req))
	tc := g.recordedOne(t)
	require.Equal(t, models.HTTP, tc.Kind)
	require.Equal(t, "ok", tc.HTTPResp.Body)
	_, inHeaders := tc.HTTPResp.Header["X-Checksum"]
	require.False(t, inHeaders, "the trailer was kept as a header: %v", tc.HTTPResp.Header)
}

// noneRecorded fails if g records a test case within a moment.
func (g *grpcIngress) noneRecorded(t *testing.T) {
	t.Helper()
	select {
	case tc := <-g.tcCh:
		t.Fatalf("recorded %s %s, want no test case", tc.Kind, tc.HTTPReq.URL)
	case <-time.After(time.Second):
	}
}

// A pause of the memory guard as an HTTP request over h2c was read costs it,
// as it costs an HTTP/1.x request read then: its mocks went with the pause,
// though the pause was over by its answer.
func TestRecordIncoming_AnH2CRequestReadInAPauseIsNotRecorded(t *testing.T) {
	g := startGRPCIngress(t)
	restore := memoryguard.SetRecordingPausedForTest(true)
	t.Cleanup(restore)
	handler := func(w http.ResponseWriter, _ *http.Request) {
		memoryguard.SetRecordingPausedForTest(false)
		_, _ = w.Write([]byte("ok"))
	}
	req, err := http.NewRequest(http.MethodGet, "http://vault.local:8200/v1/sys/health", nil)
	require.NoError(t, err)
	require.Equal(t, "ok", h2cThrough(t, g, handler, req))
	g.noneRecorded(t)
}

// A streamed answer (SSE) over h2c is no test case, as over HTTP/1.x: keploy
// records no streaming test case. It is decided at the answer's headers, so
// its window ends then, holding no mock while the stream goes on.
func TestRecordIncoming_AStreamedH2CAnswerIsNotRecordedAndHoldsNoWindow(t *testing.T) {
	g := startGRPCIngress(t)
	more := make(chan struct{})
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: hi\n\n"))
		w.(http.Flusher).Flush()
		<-more
		_, _ = w.Write([]byte("data: bye\n\n"))
	}
	go (&http2.Server{}).ServeConn(g.appApp, &http2.ServeConnOpts{Handler: http.HandlerFunc(handler)})
	tr := &http2.Transport{AllowHTTP: true, DialTLSContext: func(context.Context, string, string, *tls.Config) (net.Conn, error) {
		return g.cliApp, nil
	}}
	req, err := http.NewRequest(http.MethodGet, "http://vault.local:8200/v1/sys/events", nil)
	require.NoError(t, err)
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	first := make([]byte, len("data: hi\n\n"))
	_, err = io.ReadFull(resp.Body, first)
	require.NoError(t, err)
	deadline := time.Now().Add(5 * time.Second)
	for g.m.OpenWindows() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d windows open while the answer streams, want none", g.m.OpenWindows())
		}
		time.Sleep(time.Millisecond)
	}
	close(more)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	g.noneRecorded(t)
}

// An HTTP/2 client may send each cookie as a field of its own (Go's does):
// the request is recorded with one Cookie, joined with "; " (RFC 9113
// §8.2.3), as the app reads it. Joined with ",", it was replayed as one
// cookie whose value held the others.
func TestRecordIncoming_AnH2CRequestsCookiesAreOneCookieHeader(t *testing.T) {
	g := startGRPCIngress(t)
	var seen string
	handler := func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Cookie")
		_, _ = w.Write([]byte("ok"))
	}
	req, err := http.NewRequest(http.MethodGet, "http://vault.local:8200/v1/me", nil)
	require.NoError(t, err)
	req.Header.Set("Cookie", "a=1; b=2")
	require.Equal(t, "ok", h2cThrough(t, g, handler, req))
	require.Equal(t, "a=1; b=2", seen)
	tc := g.recordedOne(t)
	require.Equal(t, "a=1; b=2", tc.HTTPReq.Header["Cookie"])
}

// A request whose body ends in trailers is recorded with its body, and its
// trailers are not headers it was sent with.
func TestRecordIncoming_AnH2CRequestEndingInTrailersKeepsItsBody(t *testing.T) {
	g := startGRPCIngress(t)
	handler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("ok"))
	}
	req, err := http.NewRequest(http.MethodPost, "http://vault.local:8200/v1/upload", strings.NewReader(`{"chunk":1}`))
	require.NoError(t, err)
	req.Trailer = http.Header{"X-Checksum": []string{"abc"}}
	require.Equal(t, "ok", h2cThrough(t, g, handler, req))
	tc := g.recordedOne(t)
	require.Equal(t, models.HTTP, tc.Kind)
	require.Equal(t, `{"chunk":1}`, tc.HTTPReq.Body)
	_, inHeaders := tc.HTTPReq.Header["X-Checksum"]
	require.False(t, inHeaders, "the request's trailer was kept as a header: %v", tc.HTTPReq.Header)
}
