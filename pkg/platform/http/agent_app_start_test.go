package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The notice that the application starts anew goes to its hook with the set
// and its earliest recorded case, and is not given again to a restarted
// agent: the application that agent serves is the one that started.
func TestAppStartsAnew_SendsTheSetAndIsNotGivenToARestartedAgent(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var got []models.AppStartsAnewReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req models.AppStartsAnewReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		paths, got = append(paths, r.URL.Path), append(got, req)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { sessions.Delete(srv.URL) })
	a := &AgentClient{logger: zap.NewNop(), client: agentHTTPClient(""), conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	a.session().configure(true)

	first := time.Date(2026, 10, 6, 10, 0, 1, 250000000, time.UTC)
	require.NoError(t, a.AppStartsAnew(context.Background(), "test-set-1", first))
	mu.Lock()
	require.Equal(t, []string{"/hooks/app-starts-anew"}, paths)
	require.Equal(t, "test-set-1", got[0].TestSetID)
	require.True(t, got[0].FirstRecorded.Equal(first), "first recorded %v, want %v", got[0].FirstRecorded, first)
	mu.Unlock()

	s := a.session()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Empty(t, s.steps, "the notice was kept for a restarted agent")
}
