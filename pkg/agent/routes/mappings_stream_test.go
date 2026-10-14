package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// quietMappingsSvc serves a mapping stream that has nothing to send, as a
// recording without mappings (DaemonSet, proxyless) has.
type quietMappingsSvc struct {
	agent.Service // nil: any other call panics loudly
}

func (quietMappingsSvc) GetMapping(ctx context.Context) (<-chan models.TestMockMapping, error) {
	return make(chan models.TestMockMapping), nil
}

// The client's GetMappings returns once the stream's headers arrive. They
// must come at once, not with the first mapping: a recording that emits none
// left the client blocked in its request for the whole session, and its stop
// waited out the mapping drain's cap and warned that mappings.yaml may be
// missing tests it never had.
func TestHandleMappings_EstablishesTheStreamAtOnce(t *testing.T) {
	a := &Agent{logger: zap.NewNop(), svc: quietMappingsSvc{}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleMappings))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("mapping stream request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the mapping stream sent no headers before its first mapping: the client stays blocked in its request")
	}
}
