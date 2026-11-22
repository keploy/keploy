package http_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The read-back of a test's variables: the bindings an agent answers, and an
// error for an agent that cannot answer (an older agent, one that keeps no
// variables), never an empty list that would vouch for nothing.
func TestTestVariables(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/variables" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"variables":[{"name":"id","value":"u1","recorded":"u0","mock":"mock-1","place":"body.id"}]}`))
	}))
	defer ok.Close()
	got, err := newClient(t, ok.URL+"/agent").TestVariables(context.Background())
	if err != nil || len(got) != 1 || got[0].Name != "id" || got[0].Mock != "mock-1" {
		t.Fatalf("got %+v, %v", got, err)
	}
	// A body that reads as an empty list: only the status says the agent
	// bound nothing it can tell.
	for _, status := range []int{http.StatusNotFound, http.StatusNotImplemented} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"variables":[]}`))
		}))
		if _, err := newClient(t, srv.URL+"/agent").TestVariables(context.Background()); err == nil {
			t.Fatalf("an agent answering %d was read as having bound nothing", status)
		}
		srv.Close()
	}
}
