package http_test

// GetMockStats must distinguish an agent that ANSWERS "nothing stored" from one
// that cannot answer the question at all.
//
// The replay guard that uses it (ensureAgentHoldsStoredMocks) treats a zero
// loaded count against a non-empty stored corpus as "this agent was replaced"
// and re-registers the session, failing the test set when it cannot confirm. So
// an agent older than /mock/stats (404), or one whose service has no stats
// reader (501), must not read as zero — otherwise ordinary version skew fails
// every docker-compose test set.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestGetMockStatsUnsupportedStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"agent older than the route", http.StatusNotFound, "404 page not found\n"},
		{"agent whose service has no stats reader", http.StatusNotImplemented, `{"error":"this agent cannot report mock stats"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := newClient(t, srv.URL+"/agent").GetMockStats(context.Background())
			if err == nil {
				t.Fatal("an agent that cannot report must not answer with a usable zero count")
			}
			if !errors.Is(err, models.ErrMockStatsUnsupported) {
				t.Fatalf("want ErrMockStatsUnsupported so the caller can skip its check; got %v", err)
			}
		})
	}
}

// A real answer must still come through, and a real failure must still be an
// ordinary error — otherwise the guard would skip itself into uselessness.
func TestGetMockStatsRealAnswersAndRealFailures(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"loaded":7,"consumed":2,"missed":1}`))
	}))
	defer ok.Close()

	stats, err := newClient(t, ok.URL+"/agent").GetMockStats(context.Background())
	if err != nil {
		t.Fatalf("a healthy agent must answer: %v", err)
	}
	if stats.Loaded != 7 {
		t.Fatalf("loaded = %d; want 7", stats.Loaded)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer broken.Close()

	_, err = newClient(t, broken.URL+"/agent").GetMockStats(context.Background())
	if err == nil {
		t.Fatal("a 500 must remain an error")
	}
	if errors.Is(err, models.ErrMockStatsUnsupported) {
		t.Fatal("a 500 is a real failure, not 'cannot report' — the guard must NOT skip itself on it")
	}
}
