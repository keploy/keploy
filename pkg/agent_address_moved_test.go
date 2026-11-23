package pkg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.uber.org/zap"
)

// A compose start moves the agent off a port the engine refused to publish
// while keploy's health check waits for it (record, replay, mock and the
// runner start that check beside compose's up). The check reads the address
// at each check, as config.(*Config).AgentURI holds it, and finds the agent
// where it moved; a check that kept the address it began with waits on a
// port the agent never listened on. Run with -race: the move is made while
// the check reads.
func TestTheAgentHealthCheckFollowsAMovedAgent(t *testing.T) {
	resetProbeOnce(t)

	// Each check that reaches the first address is counted. Once the agent
	// has moved, the one under way may still land there, and no other: a
	// third is a check still made at the address the wait began with, and
	// fails the row then, not at go test's -timeout.
	firstAsked := make(chan struct{}, 1)
	var movedAt atomic.Int64
	var asked atomic.Int64
	stale := make(chan struct{})
	var staleOnce sync.Once
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := asked.Add(1)
		if m := movedAt.Load(); m > 0 && n-m >= 3 {
			staleOnce.Do(func() { close(stale) })
		}
		select {
		case firstAsked <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(first.Close)
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`"OK"`))
		case "/agent" + token.ProbePath:
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(moved.Close)

	conf := &config.Config{}
	conf.SetAgentURI(first.URL + "/agent")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan bool, 1)
	go AgentHealthTicker(ctx, zap.NewNop(), conf.AgentURI, ready)

	// No clock bounds the wait: the move comes once the check has reached the
	// first address, and the checks counted there after it end a wrong one.
	<-firstAsked
	movedAt.Store(asked.Load())
	conf.SetAgentURI(moved.URL + "/agent")
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("the health check ended without finding the agent at the address it moved to")
		}
	case <-stale:
		t.Fatal("the health check kept checking the address the agent moved off")
	}
}
