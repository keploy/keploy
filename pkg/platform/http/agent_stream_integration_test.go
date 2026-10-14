package http_test

// End-to-end streaming test: a real chi router + real agent route handlers
// (DefaultRoutes) wired to a stub Service, behind an httptest server, driven by
// the real AgentClient. Proves the client streams the corpus and the real
// /storemocks handler decodes the header and drives StoreMocksStream. Streaming
// is the default wire format; StoreMocks falls back to the legacy single-shot
// framing only when an older agent rejects the stream with 400 (covered in
// agent_fallback_test.go).

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// stubSvc implements just enough of agent.Service for the /storemocks paths.
// The embedded nil interface satisfies the type; unexercised methods would
// panic if hit (they aren't in this test).
type stubSvc struct {
	agentsvc.Service
	mu          sync.Mutex
	legacyCalls int
	streamCalls int
	filtered    int
	unfiltered  int
	names       []string
}

func (s *stubSvc) StoreMocks(_ context.Context, f, u []*models.Mock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.legacyCalls++
	s.filtered, s.unfiltered = len(f), len(u)
	return nil
}

// StoreMocksStream is the capability method the handler discovers via type
// assertion. It fully drains the stream (validating framing) before recording.
func (s *stubSvc) StoreMocksStream(_ context.Context, h models.MockStreamHeader, dec *gob.Decoder) error {
	total := h.FilteredCount + h.UnfilteredCount
	for i := 0; i < total; i++ {
		var m models.Mock
		if err := dec.Decode(&m); err != nil {
			return err
		}
		s.mu.Lock()
		s.names = append(s.names, m.Name)
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamCalls++
	s.filtered, s.unfiltered = h.FilteredCount, h.UnfilteredCount
	return nil
}

func newClient(t *testing.T, agentURI string) *httpclient.AgentClient {
	t.Helper()
	return httpclient.New(zap.NewNop(), nil, &config.Config{
		Agent: config.Agent{SetupOptions: models.SetupOptions{AgentURI: agentURI}},
	})
}

func fixtures() (filtered, unfiltered []*models.Mock) {
	return []*models.Mock{{Name: "f1", Kind: models.HTTP}, {Name: "f2", Kind: models.Mongo}},
		[]*models.Mock{{Name: "u1", Kind: models.DNS}}
}

// End-to-end: the client streams to the real agent routes, which decode the
// header and drive StoreMocksStream. (Streaming is the default path; the legacy
// single-shot fallback for older agents is covered in agent_fallback_test.go.)
func TestStoreMocks_StreamsToAgent(t *testing.T) {
	svc := &stubSvc{}
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	defer srv.Close()

	client := newClient(t, srv.URL+"/agent")
	f, u := fixtures()
	if err := client.StoreMocks(context.Background(), f, u); err != nil {
		t.Fatalf("StoreMocks: %v", err)
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.streamCalls != 1 || svc.legacyCalls != 0 {
		t.Fatalf("expected 1 stream call, 0 legacy; got stream=%d legacy=%d", svc.streamCalls, svc.legacyCalls)
	}
	if svc.filtered != len(f) || svc.unfiltered != len(u) {
		t.Fatalf("counts mismatch: got f=%d u=%d want f=%d u=%d", svc.filtered, svc.unfiltered, len(f), len(u))
	}
}

// This keploy does not replay connection failures, so it never sends one to
// the agent: an agent that predates the kind would read it as a mock with no
// metadata.destAddr, whose unknown port turns loopback refusal off for the
// whole test set. The caller's slices are left as they were.
func TestStoreMocks_NeverSendsAConnectionFailureToTheAgent(t *testing.T) {
	svc := &stubSvc{}
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	srv := httptest.NewServer(r)
	defer srv.Close()

	client := newClient(t, srv.URL+"/agent")
	f := []*models.Mock{{Name: "f1", Kind: models.HTTP}, {Name: "cf-1", Kind: models.ConnectionFailure}, {Name: "f2", Kind: models.Mongo}}
	u := []*models.Mock{{Name: "cf-2", Kind: models.ConnectionFailure}, {Name: "u1", Kind: models.DNS}}
	if err := client.StoreMocks(context.Background(), f, u); err != nil {
		t.Fatalf("StoreMocks: %v", err)
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()
	if got := strings.Join(svc.names, ","); got != "f1,f2,u1" || svc.filtered != 2 || svc.unfiltered != 1 {
		t.Fatalf("the agent got %q (filtered %d, unfiltered %d); want f1,f2,u1 (2, 1): no connection failure", got, svc.filtered, svc.unfiltered)
	}
	if len(f) != 3 || f[1].Name != "cf-1" || len(u) != 2 || u[0].Name != "cf-2" {
		t.Fatal("StoreMocks changed the caller's slices")
	}
}

// compressibleCorpus is n HTTP mocks whose bodies repeat the way recorded API
// responses do, so a compressed stream comes out much smaller than the raw one.
func compressibleCorpus(n int) []*models.Mock {
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		mocks[i] = &models.Mock{
			Name: fmt.Sprintf("mock-%d", i),
			Kind: models.HTTP,
			Spec: models.MockSpec{HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Body:       strings.Repeat(fmt.Sprintf(`{"id":%d,"name":"item","tags":["a","b"]}`, i), 32),
			}},
		}
	}
	return mocks
}

func mockNamesOf(mocks []*models.Mock) []string {
	names := make([]string, len(mocks))
	for i, m := range mocks {
		names[i] = m.Name
	}
	return names
}

// gobStreamSize is how many bytes the uncompressed stream of mocks takes.
func gobStreamSize(t *testing.T, mocks []*models.Mock) int {
	t.Helper()
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	require.NoError(t, enc.Encode(models.MockStreamHeader{FilteredCount: len(mocks)}))
	for _, m := range mocks {
		require.NoError(t, enc.Encode(m))
	}
	return buf.Len()
}

// storeMocksTap records the Content-Encoding and size of what reached
// /storemocks before passing it on.
type storeMocksTap struct {
	mu       sync.Mutex
	encoding string
	bytes    int
}

func (tap *storeMocksTap) read(r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/storemocks") {
		return
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	tap.mu.Lock()
	defer tap.mu.Unlock()
	tap.encoding, tap.bytes = r.Header.Get("Content-Encoding"), len(body)
}

// The agent's /health says it takes zstd, so the stream goes compressed, and
// the agent still decodes every mock in order.
func TestStoreMocks_CompressesForAnAgentThatAcceptsZstd(t *testing.T) {
	svc := &stubSvc{}
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	tap := &storeMocksTap{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tap.read(req)
		r.ServeHTTP(w, req)
	}))
	defer srv.Close()

	mocks := compressibleCorpus(500)
	require.NoError(t, newClient(t, srv.URL+"/agent").StoreMocks(context.Background(), mocks, nil))

	tap.mu.Lock()
	defer tap.mu.Unlock()
	require.Equal(t, models.MockStreamEncodingZstd, tap.encoding)
	raw := gobStreamSize(t, mocks)
	require.Less(t, tap.bytes*5, raw, "sent %d bytes for a %d-byte stream", tap.bytes, raw)

	svc.mu.Lock()
	defer svc.mu.Unlock()
	require.Equal(t, 1, svc.streamCalls)
	require.Equal(t, mockNamesOf(mocks), svc.names)
}

// An agent from before compression: its /health has no Accept-Encoding, and its
// /storemocks reads the body as gob whatever the headers say. The client must
// send it the stream uncompressed, or the agent fails on the zstd bytes.
func TestStoreMocks_SendsUncompressedToAnAgentThatDoesNotAcceptZstd(t *testing.T) {
	svc := &stubSvc{}
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
	tap := &storeMocksTap{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/agent/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		tap.read(req)
		req.Header.Del("Content-Encoding")
		r.ServeHTTP(w, req)
	}))
	defer srv.Close()

	mocks := compressibleCorpus(50)
	require.NoError(t, newClient(t, srv.URL+"/agent").StoreMocks(context.Background(), mocks, nil))

	tap.mu.Lock()
	defer tap.mu.Unlock()
	require.Empty(t, tap.encoding)
	require.Equal(t, gobStreamSize(t, mocks), tap.bytes)

	svc.mu.Lock()
	defer svc.mu.Unlock()
	require.Equal(t, mockNamesOf(mocks), svc.names)
}
