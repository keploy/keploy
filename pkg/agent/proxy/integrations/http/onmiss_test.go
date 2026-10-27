package http

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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

// containerAt names one address a container, as the agent's index of the
// local Docker daemon would.
type containerAt string

func (a containerAt) Lookup(dst netip.AddrPort) (containerkey.Container, bool, error) {
	return containerkey.Container{ID: "c1", Image: "localstack/localstack:3.8", Port: 4566, Fingerprint: "0123456789ab"}, dst.String() == string(a), nil
}

func (containerAt) Fingerprint(context.Context, string) (string, bool) { return "", false }
func (containerAt) Pending(netip.AddrPort) ([]string, bool)            { return nil, true }
func (containerAt) AwaitReadsOf(context.Context, []string)             {}

func (containerAt) Bind(context.Context)               {}
func (containerAt) Watch(context.Context, *zap.Logger) {}
func (containerAt) Endpoint() string                   { return "unix:///fake.sock" }

// A call served from the real dependency on a miss is captured with its
// connection's container key, as every recorded mock is: the capture never
// passes the sync manager that stamps the others.
func TestServeOnMissCapturesTheContainerKey(t *testing.T) {
	dep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(dep.Close)
	ResetCaptured()
	t.Cleanup(ResetCaptured)
	addr := strings.TrimPrefix(dep.URL, "http://")
	k := containerkey.NewKeyer(containerAt(addr))
	k.Name(zap.NewNop(), "c1", addr)

	for _, conn := range []struct {
		ctx  context.Context
		want string
	}{
		{containerkey.WithConn(context.Background(), k, "c1"), "container:localstack/localstack:4566@0123456789ab"},
		{context.Background(), ""}, // the switch off: no connection on the context
	} {
		reqBuf := []byte("GET /queue HTTP/1.1\r\nHost: dep.local\r\n\r\n")
		request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBuf)))
		if err != nil {
			t.Fatal(err)
		}
		keployEnd, appEnd := net.Pipe()
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		done := make(chan error, 1)
		go func() {
			_, err := h.serveOnMiss(conn.ctx, keployEnd, reqBuf, request, nil,
				&models.ConditionalDstCfg{Addr: addr}, models.OutgoingOptions{OnMiss: models.MissRecord})
			done <- err
		}()
		if _, err := http.ReadResponse(bufio.NewReader(appEnd), nil); err != nil {
			t.Fatalf("the app got no answer: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		_ = keployEnd.Close()
		_ = appEnd.Close()
		mocks := DrainCaptured()
		if len(mocks) != 1 || mocks[0].Spec.Metadata[containerkey.MetadataKey] != conn.want {
			t.Fatalf("captured %+v, want one mock keyed %q", mocks, conn.want)
		}
		// Stamped as a recorded mock is: with its container's instance and
		// scope, so a next run matches it by key.
		if inst := mocks[0].Spec.Metadata[containerkey.InstanceKey]; (conn.want != "") != (inst != "") {
			t.Fatalf("captured instance %q for key %q", inst, conn.want)
		}
	}
}

// A call served on a miss from the real dependency ends with the replay's
// context though the dependency never answers it. serveOnMiss reads the
// dependency's answer with no deadline, and decodeHTTP, which returns only
// once its goroutine has, waited as long: the context's end now ends that
// read as it ends the reads of the client. A call the end cut short is no
// miss: none is reported.
func TestAnOnMissCallTheDependencyNeverAnswersEndsWithTheContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	asked := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Read(make([]byte, 4096)) // the request, never answered
		asked <- c
	}()
	_, proxy := tcpPair(t)
	var reported atomic.Int32
	ctx, cancel := context.WithCancel(models.WithMockMismatchReporter(context.Background(), func(error) { reported.Add(1) }))
	defer cancel()
	h := &HTTP{Logger: zap.NewNop()}
	returned := make(chan error, 1)
	go func() {
		returned <- h.decodeHTTP(ctx, []byte("GET /u HTTP/1.1\r\nHost: dep.local\r\n\r\n"), proxy,
			&models.ConditionalDstCfg{Addr: ln.Addr().String()}, &mockMemDb{}, models.OutgoingOptions{OnMiss: models.MissPassthrough})
	}()
	// The bounds below only turn a hang into a failure.
	select {
	case c := <-asked:
		t.Cleanup(func() { _ = c.Close() })
	case <-time.After(time.Minute):
		t.Fatal("the miss was not passed to the dependency")
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("decodeHTTP = %v, want the context's error", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("decodeHTTP did not return once its context was cancelled: it waits for the dependency's answer")
	}
	if n := reported.Load(); n != 0 {
		t.Fatalf("the call the context's end cut short was reported as %d miss(es)", n)
	}
}
