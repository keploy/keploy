package http_test

// Backward-compat fallback: when a deployed agent is older than the streaming
// controller (keploy <= v3.5.84), it gob-decodes the stream body as a single
// StoreMocksReq and returns HTTP 400. AgentClient.StoreMocks must transparently
// retry that 400 with the legacy single-shot framing so a rolling upgrade (new
// controller, old agent still running) doesn't break storemocks — the exact
// failure seen as `storemocks http 400` in k8s auto-replay.

import (
	"context"
	"encoding/gob"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// bigCorpus builds n minimal mocks — enough of them to push the gob stream past
// the socket send buffer, so the client's encoder goroutine is still mid-write
// (blocked on pw.Write) when the old agent replies 400 and the fallback fires.
func bigCorpus(n int) []*models.Mock {
	out := make([]*models.Mock, n)
	for i := range out {
		out[i] = &models.Mock{Name: "m", Kind: models.HTTP}
	}
	return out
}

// legacyAgent is an httptest handler that behaves like a pre-streaming agent:
// it rejects the streaming Content-Type with 400 and only accepts the legacy
// single-shot StoreMocksReq gob framing.
func legacyAgent(streamCalls, legacyCalls, filtered, unfiltered *int, mu *sync.Mutex) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/storemocks", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") == models.StoreMocksStreamContentType {
			// Faithful to a real pre-streaming agent: it gob-decodes a PREFIX of
			// the body and bails on the header mismatch WITHOUT draining the rest,
			// leaving the client's encoder goroutine blocked mid-write on pw.Write.
			// This asserts the fallback still completes cleanly through that
			// mid-write state. It does NOT prove the no-goroutine-leak invariant —
			// httptest tearing down the undrained connection can unblock the
			// encoder on its own, and -race does not detect leaks; the anti-leak
			// reasoning lives in the StoreMocks/storeMocksStream code comments.
			var prefix [16]byte
			_, _ = io.ReadFull(r.Body, prefix[:])
			mu.Lock()
			*streamCalls++
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req models.StoreMocksReq
		if err := gob.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		*legacyCalls++
		*filtered, *unfiltered = len(req.Filtered), len(req.UnFiltered)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = gob.NewEncoder(w).Encode(models.AgentResp{IsSuccess: true})
	})
	return mux
}

// TestStoreMocks_FallsBackToLegacyOn400 proves a fresh streaming controller
// still stores mocks against an old (pre-streaming) agent: the stream is
// rejected with 400 and the corpus is re-sent verbatim in the legacy framing.
func TestStoreMocks_FallsBackToLegacyOn400(t *testing.T) {
	var mu sync.Mutex
	var streamCalls, legacyCalls, filtered, unfiltered int

	srv := httptest.NewServer(legacyAgent(&streamCalls, &legacyCalls, &filtered, &unfiltered, &mu))
	defer srv.Close()

	client := newClient(t, srv.URL+"/agent")
	// A corpus large enough to overflow the socket send buffer, so the stream's
	// encoder goroutine is still blocked on pw.Write when the old agent's 400
	// arrives — the realistic mid-write state the fallback must complete through.
	f, u := bigCorpus(20000), bigCorpus(5000)
	if err := client.StoreMocks(context.Background(), f, u); err != nil {
		t.Fatalf("StoreMocks should succeed via legacy fallback against an old agent: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if streamCalls != 1 || legacyCalls != 1 {
		t.Fatalf("expected 1 stream attempt then 1 legacy retry; got stream=%d legacy=%d", streamCalls, legacyCalls)
	}
	if filtered != len(f) || unfiltered != len(u) {
		t.Fatalf("legacy retry corpus mismatch: got f=%d u=%d want f=%d u=%d", filtered, unfiltered, len(f), len(u))
	}
}

// TestStoreMocks_GenuineBadRequestStillFails proves the fallback does not MASK a
// real error: if both the stream and the legacy retry 400, StoreMocks surfaces
// the failure instead of silently passing.
func TestStoreMocks_GenuineBadRequestStillFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/storemocks", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newClient(t, srv.URL+"/agent")
	f, u := fixtures()
	if err := client.StoreMocks(context.Background(), f, u); err == nil {
		t.Fatal("StoreMocks must fail when both stream and legacy return 400 (genuine bad request), not silently pass")
	}
}
