package http_test

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// tcpResetProxy forwards TCP connections to targetAddr and forcefully sends a
// TCP RST (SO_LINGER=0) once after dropAfterBytes have been forwarded,
// simulating a Kubernetes SPDY port-forward drop mid-transfer.
type tcpResetProxy struct {
	ln             net.Listener
	targetAddr     string
	dropAfterBytes int64
	dropped        atomic.Bool
	dropCount      atomic.Int64
	closed         atomic.Bool
}

func startTCPResetProxy(t *testing.T, targetAddr string, dropAfterBytes int64) *tcpResetProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp proxy: %v", err)
	}
	p := &tcpResetProxy{
		ln:             ln,
		targetAddr:     targetAddr,
		dropAfterBytes: dropAfterBytes,
	}
	go p.acceptLoop()
	t.Cleanup(func() {
		p.closed.Store(true)
		_ = ln.Close()
	})
	return p
}

func (p *tcpResetProxy) acceptLoop() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handleConn(c)
	}
}

func (p *tcpResetProxy) handleConn(clientConn net.Conn) {
	defer func() { _ = clientConn.Close() }()
	backendConn, err := net.DialTimeout("tcp", p.targetAddr, 5*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = backendConn.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		var forwarded int64
		for {
			n, rerr := clientConn.Read(buf)
			if n > 0 {
				if _, werr := backendConn.Write(buf[:n]); werr != nil {
					return
				}
				forwarded += int64(n)
				if !p.dropped.Load() && forwarded >= p.dropAfterBytes {
					if p.dropped.CompareAndSwap(false, true) {
						p.dropCount.Add(1)
						if tc, ok := clientConn.(*net.TCPConn); ok {
							_ = tc.SetLinger(0)
						}
						if bc, ok := backendConn.(*net.TCPConn); ok {
							_ = bc.SetLinger(0)
						}
						_ = clientConn.Close()
						_ = backendConn.Close()
						return
					}
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(clientConn, backendConn)
	}()
	<-done
}

func pseudoRandomBody(seed uint64, size int) string {
	b := make([]byte, size)
	x := seed + 0x9e3779b97f4a7c15
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = alphabet[x&63]
	}
	return string(b)
}

func buildLargeStoreMocksCorpus(filteredN, unfilteredN int) (filtered, unfiltered []*models.Mock) {
	base := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	filtered = make([]*models.Mock, filteredN)
	for i := range filtered {
		ts := base.Add(time.Duration(i) * time.Millisecond)
		filtered[i] = &models.Mock{
			Name: fmt.Sprintf("filtered-%04d", i),
			Kind: models.HTTP,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "mocks"},
				HTTPReq:          &models.HTTPReq{Method: "GET", URL: fmt.Sprintf("http://svc/f/%d", i), ProtoMajor: 1, ProtoMinor: 1},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: pseudoRandomBody(uint64(i+1), 2048)},
				ReqTimestampMock: ts,
				ResTimestampMock: ts.Add(time.Millisecond),
			},
		}
	}
	unfiltered = make([]*models.Mock, unfilteredN)
	for i := range unfiltered {
		ts := base.Add(time.Duration(filteredN+i) * time.Millisecond)
		unfiltered[i] = &models.Mock{
			Name: fmt.Sprintf("unfiltered-%04d", i),
			Kind: models.HTTP,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "config"},
				HTTPReq:          &models.HTTPReq{Method: "GET", URL: fmt.Sprintf("http://svc/u/%d", i), ProtoMajor: 1, ProtoMinor: 1},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: pseudoRandomBody(uint64(filteredN+i+1), 2048)},
				ReqTimestampMock: ts,
				ResTimestampMock: ts.Add(time.Millisecond),
			},
		}
	}
	return filtered, unfiltered
}

func readAgentLoadedMocks(t *testing.T, baseURL string) int {
	t.Helper()
	resp, err := http.Get(baseURL + "/agent/mock/stats")
	if err != nil {
		t.Fatalf("GET /agent/mock/stats: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var stats struct {
		Loaded int `json:"loaded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode mock stats: %v", err)
	}
	return stats.Loaded
}

// TestStoreMocks_ResumesAfterMidTransferConnectionReset proves that a TCP reset
// halfway through uploading 2,000 mocks is recovered by retrying only the
// affected batch (with idempotent seq), landing 100% of mocks with zero
// duplicates.
func TestStoreMocks_ResumesAfterMidTransferConnectionReset(t *testing.T) {
	cfg := config.New()
	cfg.Test.StrictMockWindow = true
	ag := agentsvc.New(zap.NewNop(), nil, nil, nil, nil, cfg)

	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, ag, zap.NewNop())
	srv := httptest.NewServer(r)
	defer srv.Close()

	// Drop once after ~1 MiB of upload bytes (squarely halfway through the ~3 MiB
	// 2,000-mock transfer).
	proxy := startTCPResetProxy(t, srv.Listener.Addr().String(), 1024*1024)

	client := newClient(t, fmt.Sprintf("http://%s/agent", proxy.ln.Addr().String()))
	filtered, unfiltered := buildLargeStoreMocksCorpus(1500, 500)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.StoreMocks(ctx, filtered, unfiltered); err != nil {
		t.Fatalf("StoreMocks failed across mid-transfer TCP reset (drops=%d): %v", proxy.dropCount.Load(), err)
	}
	if proxy.dropCount.Load() == 0 {
		t.Fatal("precondition failed: TCP reset was never injected during upload")
	}

	wantTotal := len(filtered) + len(unfiltered)
	if got := readAgentLoadedMocks(t, srv.URL); got != wantTotal {
		t.Fatalf("agent loaded %d mocks, want exactly %d (no missing, no duplicates)", got, wantTotal)
	}
}

// TestStoreMocks_IdempotentRetryAfterLostBatchAck simulates a TCP reset AFTER
// the agent has already committed batch seq=1 but BEFORE its HTTP 200 ACK
// reaches the client. The client retries seq=1, the agent treats it as an
// idempotent duplicate without duplicating mocks, and commit publishes the
// exact mock count.
func TestStoreMocks_IdempotentRetryAfterLostBatchAck(t *testing.T) {
	cfg := config.New()
	cfg.Test.StrictMockWindow = true
	ag := agentsvc.New(zap.NewNop(), nil, nil, nil, nil, cfg)

	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, ag, zap.NewNop())

	var seq1Hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/agent/storemocks/batch" && req.URL.Query().Get("seq") == "1" {
			if seq1Hits.Add(1) == 1 {
				// Let the real handler commit batch 1 into a recorder, then
				// hijack and reset the TCP connection so the ACK is lost.
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("precondition: first seq=1 returned %d, want 200", rec.Code)
				}
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						if tc, ok := conn.(*net.TCPConn); ok {
							_ = tc.SetLinger(0)
						}
						_ = conn.Close()
						return
					}
				}
			}
		}
		r.ServeHTTP(w, req)
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/agent")
	filtered, unfiltered := buildLargeStoreMocksCorpus(1200, 100)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.StoreMocks(ctx, filtered, unfiltered); err != nil {
		t.Fatalf("StoreMocks failed on lost batch ACK: %v", err)
	}
	if seq1Hits.Load() < 2 {
		t.Fatalf("seq=1 hits = %d, want >= 2 (initial commit + idempotent retry)", seq1Hits.Load())
	}
	wantTotal := len(filtered) + len(unfiltered)
	if got := readAgentLoadedMocks(t, srv.URL); got != wantTotal {
		t.Fatalf("agent loaded %d mocks after idempotent retry, want exactly %d", got, wantTotal)
	}
}

// TestStoreMocks_RestartsCleanlyOnAgentRestartMidUpload proves that if the
// agent restarts mid-upload (losing staging state -> 404 + new instance ID),
// the client cleanly restarts from begin and delivers 100% of mocks.
func TestStoreMocks_RestartsCleanlyOnAgentRestartMidUpload(t *testing.T) {
	newRouter := func() (*agentsvc.Agent, chi.Router) {
		cfg := config.New()
		cfg.Test.StrictMockWindow = true
		ag := agentsvc.New(zap.NewNop(), nil, nil, nil, nil, cfg)
		r := chi.NewRouter()
		routes.DefaultRoutes{}.New(r, ag, zap.NewNop())
		return ag, r
	}

	_, r1 := newRouter()
	_, r2 := newRouter()
	var restarted atomic.Bool
	var beginCalls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/agent/storemocks/begin" {
			beginCalls.Add(1)
		}
		if req.URL.Path == "/agent/storemocks/batch" && req.URL.Query().Get("seq") == "1" && !restarted.Load() {
			// Simulate agent pod restart between batch 0 and batch 1.
			restarted.Store(true)
		}
		target := r1
		inst := "agent-inst-1"
		if restarted.Load() {
			target = r2
			inst = "agent-inst-2"
		}
		// Override instance header so the client observes the restarted instance.
		rec := httptest.NewRecorder()
		target.ServeHTTP(rec, req)
		for k, vs := range rec.Header() {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if w.Header().Get(models.AgentInstanceHeader) != "" {
			w.Header().Set(models.AgentInstanceHeader, inst)
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/agent")
	filtered, unfiltered := buildLargeStoreMocksCorpus(1100, 100)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.StoreMocks(ctx, filtered, unfiltered); err != nil {
		t.Fatalf("StoreMocks failed across simulated agent restart: %v", err)
	}
	if beginCalls.Load() != 2 {
		t.Fatalf("begin calls = %d, want 2 (initial + clean restart)", beginCalls.Load())
	}
	wantTotal := len(filtered) + len(unfiltered)
	if got := readAgentLoadedMocks(t, srv.URL); got != wantTotal {
		t.Fatalf("restarted agent loaded %d mocks, want %d", got, wantTotal)
	}
}

// TestStoreMocks_SurfacesReceiverErrorOnMidStoreFailure proves that when the
// receiver fails mid-upload (e.g. ENOSPC on disk), the exact receiver error
// message surfaces cleanly to the caller instead of being hidden behind a TCP
// connection reset, and the client aborts the staging session.
func TestStoreMocks_SurfacesReceiverErrorOnMidStoreFailure(t *testing.T) {
	const wantErr = "disk full: ENOSPC while writing mock batch"

	cfg := config.New()
	ag := agentsvc.New(zap.NewNop(), nil, nil, nil, nil, cfg)
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, ag, zap.NewNop())

	var batchHits, abortHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/agent/storemocks/abort" {
			abortHits.Add(1)
		}
		if req.URL.Path == "/agent/storemocks/batch" && req.URL.Query().Get("seq") == "1" {
			batchHits.Add(1)
			_, _ = io.Copy(io.Discard, req.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(models.AgentResp{IsSuccess: false, ErrorMsg: wantErr})
			return
		}
		if req.URL.Path == "/agent/storemocks" {
			_, _ = io.CopyN(io.Discard, req.Body, 16*1024)
			w.Header().Set("Content-Type", "application/x-gob")
			w.WriteHeader(http.StatusInternalServerError)
			_ = gob.NewEncoder(w).Encode(models.AgentResp{IsSuccess: false, ErrorMsg: wantErr})
			return
		}
		r.ServeHTTP(w, req)
	}))
	defer srv.Close()

	client := newClient(t, srv.URL+"/agent")
	filtered, unfiltered := buildLargeStoreMocksCorpus(1200, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := client.StoreMocks(ctx, filtered, unfiltered)
	if err == nil {
		t.Fatal("StoreMocks succeeded despite receiver ENOSPC failure")
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("receiver error was masked!\n got: %v\nwant substring: %q", err, wantErr)
	}
	if batchHits.Load() == 0 {
		t.Fatalf("expected batched protocol to deliver batch seq=1 and surface error cleanly; fell back to legacy stream")
	}
	if abortHits.Load() == 0 {
		t.Fatal("expected client to call /agent/storemocks/abort after unrecoverable batch failure")
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("unexpected bare EOF instead of receiver error: %v", err)
	}
}

// TestStoreMocks_BatchedWithOnDiskPayloads verifies end-to-end batched upload
// from mockdb.IndexTestSetMocks (on-disk payloads) into a real agent, proving
// all 750 mocks arrive intact and batch payload mutation runs once per batch.
func TestStoreMocks_BatchedWithOnDiskPayloads(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	dir := t.TempDir()
	ys := mockdb.New(zap.NewNop(), dir, "mocks")

	base := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	const n = 750
	for i := 0; i < n; i++ {
		m := &models.Mock{
			Version: models.GetVersion(),
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "mocks"},
				HTTPReq:          &models.HTTPReq{Method: "GET", URL: fmt.Sprintf("http://svc/item/%d", i), ProtoMajor: 1, ProtoMinor: 1},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: fmt.Sprintf(`{"i":%d}`, i)},
				ReqTimestampMock: base.Add(time.Duration(i) * time.Millisecond),
				ResTimestampMock: base.Add(time.Duration(i)*time.Millisecond + time.Microsecond),
			},
		}
		if err := ys.InsertMock(ctx, m, "set-0"); err != nil {
			t.Fatalf("InsertMock %d: %v", i, err)
		}
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	idx, err := ys.IndexTestSetMocks(ctx, "set-0", time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		t.Fatalf("IndexTestSetMocks: %v", err)
	}
	var batchMutateCalls atomic.Int64
	if bmSrc, ok := idx.Payloads.(models.BatchPayloadMutatorSource); ok {
		bmSrc.SetBatchPayloadMutator(func(_ context.Context, ms []*models.Mock) error {
			batchMutateCalls.Add(1)
			return nil
		})
	} else {
		t.Fatalf("Payloads (%T) does not implement BatchPayloadMutatorSource", idx.Payloads)
	}

	cfg := config.New()
	cfg.Test.StrictMockWindow = true
	ag := agentsvc.New(zap.NewNop(), nil, nil, nil, nil, cfg)
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, ag, zap.NewNop())
	srv := httptest.NewServer(r)
	defer srv.Close()

	client := newClient(t, srv.URL+"/agent")
	if err := client.StoreMocks(ctx, idx.Filtered, idx.Unfiltered); err != nil {
		t.Fatalf("StoreMocks with on-disk payloads: %v", err)
	}
	if got := readAgentLoadedMocks(t, srv.URL); got != n {
		t.Fatalf("agent loaded %d mocks, want %d", got, n)
	}
	// 750 small mocks at 500/batch -> 2 batches -> 2 batchMutate calls (not 750).
	if got := batchMutateCalls.Load(); got != 2 {
		t.Fatalf("batch mutator calls = %d for %d mocks, want 2 (1 per 500-mock batch)", got, n)
	}
}

type stagedConcSvc struct {
	*agentsvc.Agent
	serves bool
}

func (s *stagedConcSvc) ServesExactConcurrent() bool { return s.serves }

// TestStoreMocks_BatchedExactConcurrentHeader verifies that /storemocks/commit
// propagates ExactConcurrentHeader under KEPLOY_PROVEN=1 and leaves it off
// without the switch.
func TestStoreMocks_BatchedExactConcurrentHeader(t *testing.T) {
	ctx := context.Background()
	f, u := fixtures()

	for _, tc := range []struct {
		name   string
		proven string
		serves bool
		want   bool
	}{
		{"proven on, serves", "1", true, true},
		{"proven on, does not serve", "1", false, false},
		{"proven off, serves", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			svc := &stagedConcSvc{
				Agent:  agentsvc.New(zap.NewNop(), nil, nil, nil, nil, config.New()),
				serves: tc.serves,
			}
			r := chi.NewRouter()
			routes.DefaultRoutes{}.New(r, svc, zap.NewNop())
			srv := httptest.NewServer(r)
			defer srv.Close()

			c := newClient(t, srv.URL+"/agent")
			if err := c.StoreMocks(ctx, f, u); err != nil {
				t.Fatalf("StoreMocks: %v", err)
			}
			if got := c.ServesConcurrent(); got != tc.want {
				t.Fatalf("ServesConcurrent() = %v, want %v", got, tc.want)
			}
		})
	}
}
