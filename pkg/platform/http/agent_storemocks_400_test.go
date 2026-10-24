package http_test

// StoreMocks sends a test set's mocks in one stream, and in no other framing:
// an agent that answers 400 fails the call, which is not retried. The retry it
// had, in the pre-streaming single-shot framing (keploy <= v3.5.84's agents),
// buffered the whole corpus in memory, which a replay no longer holds: each
// mock's payload is read back from its mock file only as it is streamed.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// bigCorpus builds n minimal mocks — enough of them to push the gob stream past
// the socket send buffer, so the client's encoder goroutine is still mid-write
// (blocked on pw.Write) when the agent replies 400.
func bigCorpus(n int) []*models.Mock {
	out := make([]*models.Mock, n)
	for i := range out {
		out[i] = &models.Mock{Name: "m", Kind: models.HTTP}
	}
	return out
}

// An agent that rejects the stream part-way, without draining it, fails the
// call, after that one request: nothing is sent again.
func TestStoreMocks_A400FailsWithoutARetry(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/storemocks", func(w http.ResponseWriter, r *http.Request) {
		var prefix [16]byte
		_, _ = io.ReadFull(r.Body, prefix[:])
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newClient(t, srv.URL+"/agent")
	err := client.StoreMocks(context.Background(), bigCorpus(20000), bigCorpus(5000))
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("StoreMocks against an agent that answers 400 returned %v; want the 400", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("the agent was sent %d requests; want the stream alone", calls)
	}
}

// TestStoreMocks_GenuineBadRequestStillFails proves a 400 after the whole
// stream is read fails the call too, rather than passing silently.
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
		t.Fatal("StoreMocks must fail when the agent returns 400, not silently pass")
	}
}
