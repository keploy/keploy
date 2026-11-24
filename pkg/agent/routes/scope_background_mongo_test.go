package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

type bgMongoSvc struct {
	*poolSvc
	rules []models.BackgroundMongoRule
	calls int
}

func (s *bgMongoSvc) SetScopeBackgroundMongo(_ context.Context, rules []models.BackgroundMongoRule) error {
	s.calls++
	s.rules = rules
	return nil
}

func TestSetScopeTableForwardsBackgroundMongoRules(t *testing.T) {
	want := []models.BackgroundMongoRule{
		{
			ID:          "ui-join-backfill-lease",
			DB:          "api-server",
			Command:     "insert",
			Collections: []string{"janitor_leases"},
			Where:       []models.BackgroundMongoWhere{{Field: "body.docs.msgs.0._id", Prefix: "ui-join-backfill:"}},
		},
		{
			ID:          "onboarding-idle-sessions",
			DB:          "api-server",
			Command:     "find",
			Collections: []string{"onboardingSessions"},
			Where: []models.BackgroundMongoWhere{
				{Field: "body.cmd.filter.status", Equals: "active"},
				{Field: `body.cmd.filter.$or.1["intent.category"].$in`, Equals: []any{nil, ""}},
				{Field: "body.cmd.limit", Equals: float64(200)},
			},
		},
	}

	raw, err := json.Marshal(models.ScopeTableReq{
		Mappings:        map[string][]string{"TestA": {"mock-1"}},
		BackgroundMongo: want,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// With proven switch off, SetScopeBackgroundMongo is not called.
	t.Run("proven off", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		svc := &bgMongoSvc{poolSvc: &poolSvc{}}
		a := &Agent{logger: zap.NewNop(), svc: svc}
		rec := httptest.NewRecorder()
		a.HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", bytes.NewReader(raw)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s); want 200", rec.Code, rec.Body.String())
		}
		if svc.calls != 0 {
			t.Fatalf("proven off called SetScopeBackgroundMongo %d times; want 0", svc.calls)
		}
	})

	// Under proven switch, rules are forwarded and an empty table clears them.
	t.Run("proven on", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		svc := &bgMongoSvc{poolSvc: &poolSvc{}}
		a := &Agent{logger: zap.NewNop(), svc: svc}
		rec := httptest.NewRecorder()
		a.HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", bytes.NewReader(raw)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s); want 200", rec.Code, rec.Body.String())
		}
		if svc.calls != 1 || !reflect.DeepEqual(svc.rules, want) {
			t.Fatalf("calls=%d rules=%#v; want 1 and %#v", svc.calls, svc.rules, want)
		}

		raw2, _ := json.Marshal(models.ScopeTableReq{Mappings: map[string][]string{}})
		rec2 := httptest.NewRecorder()
		a.HandleScopeTable(rec2, httptest.NewRequest(http.MethodPost, "/agent/scope/table", bytes.NewReader(raw2)))
		if rec2.Code != http.StatusOK {
			t.Fatalf("clear status = %d; want 200", rec2.Code)
		}
		if svc.calls != 2 || len(svc.rules) != 0 {
			t.Fatalf("after clear: calls=%d rules=%#v; want 2 and empty", svc.calls, svc.rules)
		}
	})
}
