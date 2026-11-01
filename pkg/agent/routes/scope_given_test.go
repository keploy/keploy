package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// givingSvc is a wire service that takes givens: it records each request and
// answers what the test sets.
type givingSvc struct {
	refusingSvc
	gave    []models.GivenReq
	giveErr *models.ScopeError
}

func (s *givingSvc) ProvenGive(_ context.Context, req models.GivenReq) (models.GivenResp, *models.ScopeError) {
	s.gave = append(s.gave, req)
	if s.giveErr != nil {
		return models.GivenResp{}, s.giveErr
	}
	return models.GivenResp{V: models.ScopeProtocolVersion, Status: "ok", Variable: models.GivenVariable(req.Name, len(s.gave))}, nil
}

// POST /agent/scope/given hands the service the value the test gave and
// answers with the variable keploy holds it as; off the switch the path is
// the plain 404 it was before it existed, for every method.
func TestAGivenReachesTheServiceUnderTheSwitchOnly(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &givingSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	rec := call(t, a.HandleScopeGiven, `{"name":"app","value":"app-1761609600123456789","source":"tests/e2e/apps_test.go:20"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp models.GivenResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, models.GivenResp{V: models.ScopeProtocolVersion, Status: "ok", Variable: "given:app"}, resp)
	require.Equal(t, []models.GivenReq{{Name: "app", Value: "app-1761609600123456789", Source: "tests/e2e/apps_test.go:20"}}, svc.gave)
	require.Empty(t, svc.refusals)

	// Only a POST gives.
	rec = httptest.NewRecorder()
	a.HandleScopeGiven(rec, httptest.NewRequest(http.MethodGet, "/agent/scope/given", nil))
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	require.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
	require.Len(t, svc.gave, 1)

	t.Setenv(provenswitch.Env, "")
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		rec := httptest.NewRecorder()
		a.HandleScopeGiven(rec, httptest.NewRequest(method, "/agent/scope/given", strings.NewReader(`{"name":"app","value":"v"}`)))
		require.Equal(t, http.StatusNotFound, rec.Code, method)
		require.Equal(t, "404 page not found\n", rec.Body.String(), method)
	}
	require.Len(t, svc.gave, 1, "off the switch nothing reaches the service")
}

// A given the service refuses, or whose body keploy cannot read, is a JSON
// wire error recorded for the run's verdict to name.
func TestARefusedGivenIsTheRunsRefusal(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &givingSvc{giveErr: &models.ScopeError{Status: http.StatusUnprocessableEntity, Code: models.ScopeErrNotOpen, Message: "no test was open", Remedy: "give it inside a test"}}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	rec := call(t, a.HandleScopeGiven, `{"name":"app","value":"v"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	var got map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, models.ScopeErrNotOpen, got["error"])
	require.Equal(t, "give it inside a test", got["remedy"])

	rec = call(t, a.HandleScopeGiven, `{"name":`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, models.ScopeErrBadRequest, got["error"])
	require.Len(t, svc.gave, 1, "an unreadable body never reaches the service")

	require.Len(t, svc.refusals, 2)
	require.Equal(t, models.ScopeErrNotOpen, svc.refusals[0].Code)
	require.Equal(t, models.ScopeErrBadRequest, svc.refusals[1].Code)
}

// An agent whose service cannot take a given says so on the wire: the test's
// value would otherwise check nothing.
func TestAnAgentThatCannotTakeAGivenSaysSo(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &refusingSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	rec := call(t, a.HandleScopeGiven, `{"name":"app","value":"v"}`)
	require.Equal(t, http.StatusNotImplemented, rec.Code)
	require.Contains(t, rec.Body.String(), "cannot take a value a test gave")
	require.Len(t, svc.refusals, 1)
}

// givenTableSvc takes the per-test table, the pool slices and the recorded
// givens.
type givenTableSvc struct {
	poolSvc
	given map[string][]models.GivenValue
	set   bool
}

func (s *givenTableSvc) SetScopeGiven(_ context.Context, g map[string][]models.GivenValue) error {
	s.given, s.set = g, true
	return nil
}

// Under the switch the scope table carries the values each test gave when it
// was recorded, and installs them on every push (an empty one clears the
// last); an agent that cannot take them says so when there are any, and
// with the switch off they are not read.
func TestScopeTableCarriesTheRecordedGivens(t *testing.T) {
	body := `{"mappings":{"o::TestA":["mock-3"]},"pool":{},"given":{"o::TestA":[{"name":"given:app","value":"app-1","source":"a_test.go:20"}]}}`
	post := func(a *Agent, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		return rec
	}
	t.Setenv(provenswitch.Env, "1")
	svc := &givenTableSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	rec := post(a, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, map[string][]models.GivenValue{"o::TestA": {{Name: "given:app", Value: "app-1", Source: "a_test.go:20"}}}, svc.given)
	rec = post(a, `{"mappings":{"o::TestA":["mock-3"]},"pool":{}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, svc.set)
	require.Empty(t, svc.given, "a push without givens clears the last push's")

	// An agent without givens refuses a table that has some, and takes one
	// that has none.
	require.Equal(t, http.StatusNotImplemented, post(&Agent{logger: zap.NewNop(), svc: &poolSvc{}}, body).Code)
	require.Equal(t, http.StatusOK, post(&Agent{logger: zap.NewNop(), svc: &poolSvc{}}, `{"mappings":{},"pool":{}}`).Code)

	t.Setenv(provenswitch.Env, "")
	svc = &givenTableSvc{}
	require.Equal(t, http.StatusOK, post(&Agent{logger: zap.NewNop(), svc: svc}, body).Code)
	require.False(t, svc.set, "off the switch the givens are not read")
}
