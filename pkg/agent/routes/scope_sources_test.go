package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// sourcesSvc is workerSvc that also takes the sources declared rules give
// recordings' fields (scopeSourcesSetter).
type sourcesSvc struct {
	workerSvc
	sources *map[string][]models.DeclaredSource
	err     error
}

func (s sourcesSvc) SetScopeSources(_ context.Context, so map[string][]models.DeclaredSource) error {
	if s.err != nil {
		return s.err
	}
	*s.sources = so
	s.order = append(s.order, "sources")
	return nil
}

// Under the switch the scope table carries the sources declared rules give
// recordings' fields, installed before the pool slices, and the answer
// names them (models.ScopeTableServedSources), which the CLI refuses an
// agent without; a table without sources installs none, so a set never
// keeps the previous set's. An agent that cannot learn by them refuses a
// table that has some, and one that cannot install them answers 500 before
// the pool; either takes a table without. With the switch off they are not
// read.
func TestScopeTableCarriesTheDeclaredSources(t *testing.T) {
	post := func(svc agent.Service, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		return rec
	}
	served := func(rec *httptest.ResponseRecorder) []string {
		t.Helper()
		var ack models.ScopeTableResp
		if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
			t.Fatalf("the answer %q does not decode: %v", rec.Body.String(), err)
		}
		return ack.Served
	}
	withSources := `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-2"]},"sources":{"mock-3":[{"rule":"audit-id@0a1b2c3d","kind":"minted","field":"body.docs.msgs.0._id","minted":{"classes":["-","digit","lower"]}}]}}`
	want := map[string][]models.DeclaredSource{"mock-3": {{Rule: "audit-id@0a1b2c3d", Kind: models.DeclaredSourceMinted, Field: "body.docs.msgs.0._id", Minted: &models.MintedForm{Classes: []string{"-", "digit", "lower"}}}}}
	t.Setenv(provenswitch.Env, "1")
	ps, got := &poolSvc{}, map[string][]models.DeclaredSource{}
	rec := post(sourcesSvc{workerSvc{ps}, &got, nil}, withSources)
	if rec.Code != http.StatusOK || !reflect.DeepEqual(got, want) || !slices.Equal(ps.order, []string{"windows", "worker", "sources", "pool"}) {
		t.Fatalf("status %d, sources %v, installed %v; want them installed before the pool", rec.Code, got, ps.order)
	}
	if s := served(rec); !slices.Contains(s, models.ScopeTableServedSources) {
		t.Fatalf("the answer names %v, not the sources it installed", s)
	}
	ps, got = &poolSvc{}, want
	if rec := post(sourcesSvc{workerSvc{ps}, &got, nil}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || len(got) != 0 || !slices.Contains(ps.order, "sources") {
		t.Fatalf("a table without sources: status %d, sources %v, installed %v; want none installed", rec.Code, got, ps.order)
	}
	ps = &poolSvc{}
	if rec := post(sourcesSvc{workerSvc{ps}, &got, errors.New("refused")}, withSources); rec.Code != http.StatusInternalServerError || slices.Contains(ps.order, "pool") {
		t.Fatalf("an agent that could not install the sources answered %d and installed %v; want 500 before the pool", rec.Code, ps.order)
	}
	rec = post(workerSvc{&poolSvc{}}, withSources)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("an agent without sources answered %d to a table with some", rec.Code)
	}
	rec = post(workerSvc{&poolSvc{}}, `{"mappings":{},"pool":{}}`)
	if rec.Code != http.StatusOK || slices.Contains(served(rec), models.ScopeTableServedSources) {
		t.Fatalf("an agent without sources answered %d naming %q to a table without any", rec.Code, rec.Body.String())
	}
	t.Setenv(provenswitch.Env, "")
	ps, got = &poolSvc{}, nil
	if rec := post(sourcesSvc{workerSvc{ps}, &got, nil}, withSources); rec.Code != http.StatusOK || got != nil || len(ps.order) != 0 {
		t.Fatalf("switch off: status %d, sources %v, installed %v", rec.Code, got, ps.order)
	}
}
