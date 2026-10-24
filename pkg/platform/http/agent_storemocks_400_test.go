package http_test

// StoreMocks sends a test set's mocks in one stream, and in no other framing:
// an agent that answers 400 fails the call, which is not retried. The retry it
// had, in the pre-streaming single-shot framing (keploy <= v3.5.84's agents),
// buffered the whole corpus in memory, which a replay no longer holds: each
// mock's payload is read back from its mock file only as it is streamed. Such
// an agent is named in the error instead (models.ErrAgentPredatesMockStream).

import (
	"context"
	"encoding/gob"
	"errors"
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

// preStreamingAgentResp and preStreamingStoreMocksReq are a keploy v3.5.84
// agent's wire types (pkg/models/agent.go at that tag). gob matches a struct by
// its field names, so they stand in for that agent's own.
type preStreamingAgentResp struct {
	Error     error
	IsSuccess bool
}

type preStreamingStoreMocksReq struct {
	Filtered   []*models.Mock
	UnFiltered []*models.Mock
}

// preStreamingAgent serves /agent/storemocks as a keploy v3.5.84 agent does
// (its pkg/agent/routes/replay.go, verbatim but for the types above): it reads
// the body as one StoreMocksReq, which the stream's MockStreamHeader does not
// decode as, and answers 400 with an AgentResp whose error it cannot encode.
func preStreamingAgent(calls *int, mu *sync.Mutex) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/storemocks", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-gob")
		var req preStreamingStoreMocksReq
		if err := gob.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = gob.NewEncoder(w).Encode(preStreamingAgentResp{Error: err, IsSuccess: false})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = gob.NewEncoder(w).Encode(preStreamingAgentResp{IsSuccess: true})
	})
	return mux
}

// An agent from before the mocks were streamed can never take them, and its
// 400 carries no reason. The call says which agent that is and what to do about
// it, as ErrAgentPredatesMockStream, so a caller that launched the agent (k8s-
// proxy's replay pods) can name its image instead of a bare "http 400".
func TestStoreMocks_AnAgentFromBeforeTheStreamIsNamed(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(preStreamingAgent(&calls, &mu))
	defer srv.Close()

	client := newClient(t, srv.URL+"/agent")
	// More than the agent reads before it answers, as a real recording's
	// mocks are: the 400 arrives while the stream is still being written.
	err := client.StoreMocks(context.Background(), bigCorpus(20000), bigCorpus(5000))
	if !errors.Is(err, models.ErrAgentPredatesMockStream) {
		t.Fatalf("StoreMocks against a keploy v3.5.84 agent returned %v; want models.ErrAgentPredatesMockStream", err)
	}
	for _, want := range []string{"400", models.MockStreamMinAgentVersion, "upgrade the agent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not say %q", err, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("the agent was sent %d requests; want the stream alone", calls)
	}
}

// A 400 that is not that agent's is not blamed on the agent's version: an empty
// one (a proxy in front of the agent, say) and a current agent's, which says
// why, both keep their own reading.
func TestStoreMocks_Other400sAreNotCalledAnOldAgent(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"an empty 400": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusBadRequest)
		},
		"a current agent's 400": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusBadRequest)
			_ = gob.NewEncoder(w).Encode(models.AgentResp{ErrorMsg: "storemocks: decode stream header: bad"})
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/agent/storemocks", h)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			client := newClient(t, srv.URL+"/agent")
			f, u := fixtures()
			err := client.StoreMocks(context.Background(), f, u)
			if err == nil || !strings.Contains(err.Error(), "400") {
				t.Fatalf("StoreMocks returned %v; want the 400", err)
			}
			if errors.Is(err, models.ErrAgentPredatesMockStream) {
				t.Fatalf("StoreMocks called %s an agent from before the stream: %v", name, err)
			}
		})
	}
}
