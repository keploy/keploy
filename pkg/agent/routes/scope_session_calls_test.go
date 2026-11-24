package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// keploy mock replay's scope table hands the agent, under the proven switch,
// the session calls the client declares, before the set is staged: the agent
// installs them and says so (models.ScopeTableServedSessionCalls). A
// declaration over 128 KiB arrives whole; the next set's replaces it, none
// included; one the agent cannot read refuses the table; with the switch off
// it is not read.
// Mutation: ignore the declaration, leave the earlier one in force, or take
// an unreadable one.
func TestTheScopeTableHandsTheAgentTheDeclaredSessionCalls(t *testing.T) {
	t.Cleanup(func() { models.SetSessionCalls(nil) })
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: &poolSvc{}}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		return rec
	}
	table := func(rules []models.SessionCallRule) string {
		b, err := json.Marshal(models.ScopeTableReq{Mappings: map[string][]string{}, Pool: map[string][]string{}, SessionCalls: &models.DeclaredSessionCalls{Rules: rules}})
		require.NoError(t, err)
		return string(b)
	}
	declared := func(host, path string) bool {
		covered, named := models.SessionCallDeclared("GET", host, path)
		return covered && named
	}
	t.Setenv(provenswitch.Env, "1")
	big := make([]models.SessionCallRule, 2500)
	for i := range big {
		big[i] = models.SessionCallRule{Host: fmt.Sprintf("svc-%05d.internal.example.com", i), Method: "GET", PathPrefix: fmt.Sprintf("/v1/config/%05d/settings", i)}
	}
	body := table(big)
	require.Greater(t, len(body), 128<<10)
	rec := post(body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var ack models.ScopeTableResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ack))
	require.Contains(t, ack.Served, models.ScopeTableServedSessionCalls)
	require.True(t, declared(big[len(big)-1].Host, big[len(big)-1].PathPrefix), "the agent does not declare the last of the session calls the table handed it")

	// The next set's declaration replaces it, and one of none clears it.
	require.Equal(t, http.StatusOK, post(table([]models.SessionCallRule{{Host: "rates.example.com", Method: "GET", PathPrefix: "/rates"}})).Code)
	require.True(t, declared("rates.example.com", "/rates"))
	require.False(t, declared(big[0].Host, big[0].PathPrefix), "the earlier set's declaration outlived it")
	require.Equal(t, http.StatusOK, post(table(nil)).Code)
	require.False(t, models.SessionCallsDeclared(), "a declaration of none left the earlier one in force")

	// One the agent cannot read refuses the table.
	models.SetSessionCalls([]models.SessionCallRule{{Host: "kept.example.com"}})
	rec = post(`{"mappings":{},"pool":{},"sessionCalls":{"rules":"not a list"}}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.True(t, models.SessionCallsDeclared(), "an unreadable declaration changed the agent's")

	// With the switch off it is not read.
	t.Setenv(provenswitch.Env, "")
	require.Equal(t, http.StatusOK, post(table(nil)).Code)
	require.True(t, models.SessionCallsDeclared(), "off the switch the table's declaration was installed")
}

// orderSvc is poolSvc that notes, as each setter of the table is called,
// whether the declared session call rates.example.com GET /rates is
// installed then: each of them may stage the set (SetScopePool serves the
// pool, which stages the set once the run is claimed: servePoolExactly), and
// a staging marks the set's plain startup: recordings by the declaration in
// force (agent.backgroundRecordingsOf).
type orderSvc struct {
	*poolSvc
	declaredAt map[string]bool
}

func declaresRates() bool {
	covered, named := models.SessionCallDeclared("GET", "rates.example.com", "/rates")
	return covered && named
}

func (s *orderSvc) SetScopeTable(ctx context.Context, t map[string][]string) error {
	s.declaredAt["table"] = declaresRates()
	return s.poolSvc.SetScopeTable(ctx, t)
}

func (s *orderSvc) SetScopeWorker(_ context.Context, w map[string][]string) error {
	s.declaredAt["worker"] = declaresRates()
	s.worker = w
	return nil
}

func (s *orderSvc) SetScopeBackground(_ context.Context, _ []string) error {
	s.declaredAt["background"] = declaresRates()
	return nil
}

func (s *orderSvc) SetScopePool(ctx context.Context, p map[string][]string) error {
	s.declaredAt["pool"] = declaresRates()
	return s.poolSvc.SetScopePool(ctx, p)
}

// keploy mock replay's scope table installs the session calls the client
// declares before it hands the agent anything that stages the set (its
// table, worker scope, background names and pool, whose serving stages it),
// so the staging marks the set's plain startup: recordings by that
// declaration, not the previous one: a declared call's recording is never
// marked, and an undeclared one always is.
// Mutation: install the declaration after the pool, or after any setter
// that stages the set.
func TestTheScopeTableInstallsTheDeclaredSessionCallsBeforeItStagesTheSet(t *testing.T) {
	t.Cleanup(func() { models.SetSessionCalls(nil) })
	t.Setenv(provenswitch.Env, "1")
	models.SetSessionCalls(nil)
	svc := &orderSvc{poolSvc: &poolSvc{}, declaredAt: map[string]bool{}}
	b, err := json.Marshal(models.ScopeTableReq{
		Mappings:     map[string][]string{"o::TestA": {"mock-1"}},
		Pool:         map[string][]string{"o": {"mock-0"}},
		Worker:       map[string][]string{"o": {}},
		Background:   []string{"mock-0"},
		SessionCalls: &models.DeclaredSessionCalls{Rules: []models.SessionCallRule{{Host: "rates.example.com", Method: "GET", PathPrefix: "/rates"}}},
	})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(string(b))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, step := range []string{"table", "worker", "background", "pool"} {
		declared, called := svc.declaredAt[step]
		require.True(t, called, "the table never handed the agent its %s", step)
		require.True(t, declared, "the table handed the agent its %s before it installed the declared session calls: a staging then marks by the previous declaration", step)
	}
}
