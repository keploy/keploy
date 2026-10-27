package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// realAgent is an agent answering the mock.real routes: each request's path
// and body are noted, and status (200 when 0) and answer are what it answers.
func realAgent(t *testing.T, status int, answer any) (*AgentClient, *[]string, *[]string) {
	t.Helper()
	var paths, bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		paths, bodies = append(paths, r.Method+" "+r.URL.Path), append(bodies, string(b))
		if status != 0 {
			http.Error(w, "refused", status)
			return
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(srv.Close)
	return &AgentClient{
		logger: zap.NewNop(),
		client: *srv.Client(),
		conf:   &config.Config{Agent: config.Agent{AgentURI: srv.URL}},
	}, &paths, &bodies
}

// The list goes to POST /real; an agent without the route is
// ErrNoRealRoute; another refusal is an error with the agent's text.
func TestPushRealDeps(t *testing.T) {
	a, paths, bodies := realAgent(t, 0, map[string]string{"status": "ok"})
	require.NoError(t, a.PushRealDeps(context.Background(), []string{"db:5432"}))
	require.Equal(t, []string{"POST /real"}, *paths)
	require.JSONEq(t, `{"real":["db:5432"]}`, (*bodies)[0])
	a, paths, bodies = realAgent(t, 0, map[string]string{"status": "ok"})
	require.NoError(t, a.KeepRealDeps(context.Background(), []string{"db:5432"}))
	require.Equal(t, []string{"POST /real"}, *paths)
	require.JSONEq(t, `{"real":["db:5432"],"keep":true}`, (*bodies)[0])
	a, _, _ = realAgent(t, http.StatusNotFound, nil)
	require.ErrorIs(t, a.PushRealDeps(context.Background(), nil), models.ErrNoRealRoute)
	a, _, _ = realAgent(t, http.StatusInternalServerError, nil)
	require.ErrorContains(t, a.PushRealDeps(context.Background(), []string{"db:5432"}), "refused")
}

// The run's and a test case's mock.real reads.
func TestRealRunAndRealTest(t *testing.T) {
	run := models.RealRun{Reached: []string{"db:5432"}}
	a, paths, _ := realAgent(t, 0, run)
	got, err := a.RealRun(context.Background())
	require.NoError(t, err)
	require.Equal(t, run, got)
	require.Equal(t, []string{"GET /real/run"}, *paths)

	test := models.RealTest{Real: []models.RealUse{{Dest: "db:5432", Conns: 1}}}
	a, paths, _ = realAgent(t, 0, test)
	gotTest, err := a.RealTest(context.Background())
	require.NoError(t, err)
	require.Equal(t, test, gotTest)
	require.Equal(t, []string{"GET /real/test"}, *paths)

	a, _, _ = realAgent(t, http.StatusNotFound, nil)
	_, err = a.RealTest(context.Background())
	require.Error(t, err)
	_, err = a.RealRun(context.Background())
	require.Error(t, err)
}

// A mock.real test case asks the agent for the real clock, with its list; a
// refusal or an agent that cannot be asked is an error, never a freeze.
func TestBeforeSimulateReal(t *testing.T) {
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	a, paths, bodies := realAgent(t, 0, nil)
	require.NoError(t, a.BeforeSimulateReal(context.Background(), &at, "set", "tc", []string{"db:5432"}))
	require.Equal(t, []string{"POST /hooks/before-simulate"}, *paths)
	var req models.BeforeSimulateRequest
	require.NoError(t, json.Unmarshal([]byte((*bodies)[0]), &req))
	require.Equal(t, models.BeforeSimulateRequest{TimeStamp: at, TestSetID: "set", TestCaseName: "tc", Real: []string{"db:5432"}}, req)

	a, _, _ = realAgent(t, http.StatusNotImplemented, nil)
	require.ErrorContains(t, a.BeforeSimulateReal(context.Background(), &at, "set", "tc", []string{"db:5432"}), "501")
	a.conf.Agent.AgentURI = "http://127.0.0.1:1"
	require.Error(t, a.BeforeSimulateReal(context.Background(), &at, "set", "tc", []string{"db:5432"}))
	a.conf.Agent.AgentURI = ""
	require.NoError(t, a.BeforeSimulateReal(context.Background(), &at, "set", "tc", []string{"db:5432"}), "no agent, no clock to move")
}
