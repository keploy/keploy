package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TakeOwnedCaptureLosses POSTs /capturelosses/owned and gives back what a
// cut-short end owned, by test. An agent older than the route (404, 405) or
// one that has none to tell apart (501) is ErrCaptureLossesUnsupported: its
// counts hold every loss. Any other failure is an error keploy test cannot
// read past, never "none".
func TestAgentClient_TakesTheLossesACutShortEndOwned(t *testing.T) {
	owned := []models.OwnedCaptureLoss{{Test: "between test cases", Loss: models.CaptureLoss{Kind: models.CaptureLossAppBytesLost, Count: 1, What: "from 127.0.0.1:50912"}}}
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/capturelosses/owned" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(owned)
			return
		}
		// A body that decodes: only the status says the take failed.
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}

	got, err := a.TakeOwnedCaptureLosses(context.Background())
	if err != nil || len(got) != 1 || got[0] != owned[0] {
		t.Fatalf("took %+v, %v; want %+v", got, err, owned)
	}
	for _, code := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		status = code
		if _, err := a.TakeOwnedCaptureLosses(context.Background()); !errors.Is(err, models.ErrCaptureLossesUnsupported) {
			t.Fatalf("%d: %v, want ErrCaptureLossesUnsupported", code, err)
		}
	}
	status = http.StatusInternalServerError
	if _, err := a.TakeOwnedCaptureLosses(context.Background()); err == nil || errors.Is(err, models.ErrCaptureLossesUnsupported) {
		t.Fatalf("500: %v, want an error that is not ErrCaptureLossesUnsupported", err)
	}
}
