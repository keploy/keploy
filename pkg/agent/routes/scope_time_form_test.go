package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// timeFormSvc is workerSvc that also takes whether the learn step checks a
// changed time by its form alone (scopeTimeFormSetter).
type timeFormSvc struct {
	workerSvc
	on  *bool
	err error
}

func (s timeFormSvc) SetScopeTimeForm(_ context.Context, on bool) error {
	if s.err != nil {
		return s.err
	}
	*s.on = on
	s.order = append(s.order, "timeForm")
	return nil
}

// Under the switch the scope table says whether the learn step checks a
// changed time by its form alone, installed before the pool slices; a
// table that does not say it turns it off, so a set never keeps the
// previous set's. One that cannot install it answers 500 before the pool.
// An agent that cannot take it takes the table: it decides times by the
// clock checks alone, which refuses more, never passes more. With the
// switch off it is not read.
func TestScopeTableCarriesTheTimeDefault(t *testing.T) {
	post := func(svc agent.Service, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		return rec
	}
	withTimeForm := `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-2"]},"timeForm":true}`
	t.Setenv(provenswitch.Env, "1")
	ps, on := &poolSvc{}, false
	rec := post(timeFormSvc{workerSvc{ps}, &on, nil}, withTimeForm)
	if rec.Code != http.StatusOK || !on || !slices.Equal(ps.order, []string{"windows", "worker", "timeForm", "pool"}) {
		t.Fatalf("status %d, time default %v, installed %v; want it on, installed before the pool", rec.Code, on, ps.order)
	}
	ps = &poolSvc{}
	if rec := post(timeFormSvc{workerSvc{ps}, &on, nil}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || on || !slices.Contains(ps.order, "timeForm") {
		t.Fatalf("a table that does not say it: status %d, time default %v, installed %v; want it installed off", rec.Code, on, ps.order)
	}
	ps = &poolSvc{}
	if rec := post(timeFormSvc{workerSvc{ps}, &on, errors.New("refused")}, withTimeForm); rec.Code != http.StatusInternalServerError || slices.Contains(ps.order, "pool") {
		t.Fatalf("an agent that could not install the time default answered %d and installed %v; want 500 before the pool", rec.Code, ps.order)
	}
	ps = &poolSvc{}
	if rec := post(workerSvc{ps}, withTimeForm); rec.Code != http.StatusOK || !slices.Contains(ps.order, "pool") {
		t.Fatalf("an agent without the time default answered %d and installed %v; want the table taken", rec.Code, ps.order)
	}
	t.Setenv(provenswitch.Env, "")
	ps, on = &poolSvc{}, false
	if rec := post(timeFormSvc{workerSvc{ps}, &on, nil}, withTimeForm); rec.Code != http.StatusOK || on || len(ps.order) != 0 {
		t.Fatalf("switch off: status %d, time default %v, installed %v", rec.Code, on, ps.order)
	}
}
