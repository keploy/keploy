package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestAgentClient_PushProvenTableWithBackgroundMongoAndReapply(t *testing.T) {
	var got []models.ScopeTableReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/scope/table" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req models.ScopeTableReq
		if err := json.Unmarshal(b, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got = append(got, req)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { sessions.Delete(srv.URL) })

	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	a.session().configure(true)

	rules := []models.BackgroundMongoRule{{
		ID:          "ui-join-backfill-lease",
		DB:          "api-server",
		Command:     "insert",
		Collections: []string{"janitor_leases"},
		Where: []models.BackgroundMongoWhere{
			{Field: "body.docs.msgs.0._id", Prefix: "ui-join-backfill:"},
			{Field: `body.cmd.filter.$or.1["intent.category"].$in`, Equals: []any{nil, ""}},
		},
		PeriodBy: "body.docs.msgs.0._id",
	}}
	httpRules := []models.BackgroundHTTPRule{{
		ID:     "openbao-liveness-check",
		Method: "GET",
		Host:   "localhost:8200",
		Path:   "/v1/sys/health",
	}}
	want := models.CloneBackgroundMongoRules(rules)
	wantHTTP := models.CloneBackgroundHTTPRules(httpRules)

	if err := a.PushProvenTableWithBackgroundRules(context.Background(), map[string][]string{"o::TestA": {"mock-3"}}, map[string][]string{"o": {"mock-1"}}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, true, rules, httpRules); err != nil {
		t.Fatalf("PushProvenTableWithBackgroundRules: %v", err)
	}
	// Mutating caller's slice/nested values must not alter the session reapply copy.
	rules[0].Collections[0] = "mutated"
	rules[0].Where[1].Equals.([]any)[1] = "mutated"
	httpRules[0].Path = "/mutated"

	if err := a.session().reapply(context.Background(), zap.NewNop()); err != nil {
		t.Fatalf("session reapply: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d scope table pushes; want 2 (initial + reapply)", len(got))
	}
	for i, req := range got {
		if !req.StartupRules {
			t.Fatalf("push %d: StartupRules=false; want true", i)
		}
		if !reflect.DeepEqual(req.BackgroundMongo, want) {
			t.Fatalf("push %d: BackgroundMongo=%#v; want %#v", i, req.BackgroundMongo, want)
		}
		if !reflect.DeepEqual(req.BackgroundHTTP, wantHTTP) {
			t.Fatalf("push %d: BackgroundHTTP=%#v; want %#v", i, req.BackgroundHTTP, wantHTTP)
		}
	}
}
