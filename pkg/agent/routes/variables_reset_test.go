package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// paramsSvc takes mock filters and counts the times it forgot the bound
// variables, and when (before the filter, or after).
type paramsSvc struct {
	agent.Service // nil: any other call panics loudly
	events        *[]string
	failFilter    bool
	unkept        bool
}

func (s paramsSvc) UpdateMockParams(context.Context, models.MockFilterParams) error {
	*s.events = append(*s.events, "filter")
	if s.failFilter {
		return errors.New("no mocks stored")
	}
	return nil
}

func (s paramsSvc) StartTestVariables(bs []models.VariableBinding) error {
	ev := "reset"
	for _, b := range bs {
		ev += " bind:" + b.Name + "=" + b.Value
	}
	*s.events = append(*s.events, ev)
	if len(bs) > 0 && bs[0].Name == "unkept" {
		return errors.New("this keploy agent's proxy keeps no test variables")
	}
	return nil
}

func (s paramsSvc) TestVariables() ([]models.VariableBinding, bool) {
	if s.unkept {
		return nil, false
	}
	return []models.VariableBinding{{Name: "orderId", Value: "o1", Recorded: "r1", Mock: "mock-1", Place: "body.id"}}, true
}

// Under the proven switch every mock filter a client sends starts a test: the
// variables bound so far are forgotten as the filter applies. With the
// switch off nothing is forgotten. The scope begins once the new pool is in.
func TestAMockFilterStartsATestsVariables(t *testing.T) {
	for _, tc := range []struct {
		proven string
		want   string
	}{
		{"1", "filter reset bind:id=u1"},
		{"0", "filter"},
	} {
		t.Run("KEPLOY_PROVEN="+tc.proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			var events []string
			a := &Agent{logger: zap.NewNop(), svc: paramsSvc{events: &events}}
			rec := httptest.NewRecorder()
			a.UpdateMockParams(rec, httptest.NewRequest(http.MethodPost, "/agent/updatemockparams", strings.NewReader(`{"filterParams":{"variables":[{"name":"id","value":"u1","recorded":"u0","mock":"test case t","place":"request"}]}}`)))
			if rec.Code != http.StatusOK || strings.Join(events, " ") != tc.want {
				t.Fatalf("status %d, events %v; want 200 and %q", rec.Code, events, tc.want)
			}
		})
	}
}

// A filter that fails swapped no pool in, and begins no test: the variables
// stay with the pool they were bound against.
func TestAFailedMockFilterBeginsNoTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	var events []string
	a := &Agent{logger: zap.NewNop(), svc: paramsSvc{events: &events, failFilter: true}}
	rec := httptest.NewRecorder()
	a.UpdateMockParams(rec, httptest.NewRequest(http.MethodPost, "/agent/updatemockparams", strings.NewReader(`{"filterParams":{}}`)))
	if rec.Code != http.StatusInternalServerError || strings.Join(events, " ") != "filter" {
		t.Fatalf("status %d, events %v; want 500 and the filter alone", rec.Code, events)
	}
}

// An agent that cannot begin the test's variables fails the filter, after it
// applied, so keploy test does not run the test case as if they were bound.
func TestAMockFilterWhoseVariablesCannotBeginFails(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	var events []string
	a := &Agent{logger: zap.NewNop(), svc: paramsSvc{events: &events}}
	rec := httptest.NewRecorder()
	a.UpdateMockParams(rec, httptest.NewRequest(http.MethodPost, "/agent/updatemockparams", strings.NewReader(`{"filterParams":{"variables":[{"name":"unkept","value":"a"}]}}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, events %v; want 500", rec.Code, events)
	}
}

// GET /agent/variables answers what the test bound under the switch, and is
// not served without it.
func TestHandleTestVariables(t *testing.T) {
	var events []string
	a := &Agent{logger: zap.NewNop(), svc: paramsSvc{events: &events}}
	t.Setenv(provenswitch.Env, "0")
	rec := httptest.NewRecorder()
	a.HandleTestVariables(rec, httptest.NewRequest(http.MethodGet, "/agent/variables", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("switch off: status %d, want 404", rec.Code)
	}
	t.Setenv(provenswitch.Env, "1")
	rec = httptest.NewRecorder()
	a.HandleTestVariables(rec, httptest.NewRequest(http.MethodGet, "/agent/variables", nil))
	var got models.TestVariablesResp
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got.Variables) != 1 || got.Variables[0].Mock != "mock-1" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	// An agent that keeps no variables says so: a read that cannot vouch for
	// the test's bindings is not an empty list.
	a = &Agent{logger: zap.NewNop(), svc: paramsSvc{events: &events, unkept: true}}
	rec = httptest.NewRecorder()
	a.HandleTestVariables(rec, httptest.NewRequest(http.MethodGet, "/agent/variables", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("an agent that keeps no variables answered %d", rec.Code)
	}
}

// resetOnlySvc is an agent that can forget the test's variables but not bind
// a test case's values; plainSvc keeps no variables at all.
type resetOnlySvc struct{ plainSvc }

func (s resetOnlySvc) ResetVariables() { *s.events = append(*s.events, "reset") }

type plainSvc struct {
	agent.Service // nil: any other call panics loudly
	events        *[]string
}

func (s plainSvc) UpdateMockParams(context.Context, models.MockFilterParams) error {
	*s.events = append(*s.events, "filter")
	return nil
}

// An agent that can only forget still begins each test with nothing bound,
// and one that cannot bind refuses the values a test case gave rather than
// drop them: every call that uses one would fail the app for keploy's fault.
func TestAnAgentThatCannotBindRefusesGivenValues(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const given = `{"filterParams":{"variables":[{"name":"id","value":"u1","recorded":"u0","mock":"test case t","place":"request"}]}}`
	for _, tc := range []struct {
		name   string
		svc    func(*[]string) agent.Service
		body   string
		code   int
		events string
	}{
		{"forgets, nothing given", func(e *[]string) agent.Service { return resetOnlySvc{plainSvc{events: e}} }, `{"filterParams":{}}`, http.StatusOK, "filter reset"},
		{"forgets, a value given", func(e *[]string) agent.Service { return resetOnlySvc{plainSvc{events: e}} }, given, http.StatusNotImplemented, "filter reset"},
		{"keeps none, nothing given", func(e *[]string) agent.Service { return plainSvc{events: e} }, `{"filterParams":{}}`, http.StatusOK, "filter"},
		{"keeps none, a value given", func(e *[]string) agent.Service { return plainSvc{events: e} }, given, http.StatusNotImplemented, "filter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			a := &Agent{logger: zap.NewNop(), svc: tc.svc(&events)}
			rec := httptest.NewRecorder()
			a.UpdateMockParams(rec, httptest.NewRequest(http.MethodPost, "/agent/updatemockparams", strings.NewReader(tc.body)))
			if rec.Code != tc.code || strings.Join(events, " ") != tc.events {
				t.Fatalf("status %d, events %v; want %d and %q", rec.Code, events, tc.code, tc.events)
			}
		})
	}
}

// Under exact serving keploy test sends three filters: the set's staging, a
// test case's open, and the pool call between test cases. Only the open
// begins a test's variables. The staging begins none: the set's variables
// began at its Mock, and what its start-up binds after the staging is the
// run's, which the first test must not forget (MockManager.NewVariableSet).
// The pool call begins none, so what the test case's dependency calls bound
// stays readable until the next open, and keploy test reads it after the pool
// call. A staging or a pool call that gives values is refused before it
// applies: they would be bound nowhere. Without exact serving a set's staging
// (from BaseTime, no test's mapping) begins none either.
func TestExactServingsPoolCallKeepsTheTestCasesVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name   string
		body   string
		code   int
		events string
	}{
		{"staging", `{"filterParams":{"afterTime":"2000-01-01T00:00:00Z","exact":{"stage":true,"pool":["mock-9"]}}}`, http.StatusOK, "filter"},
		{"a staging that gives values", `{"filterParams":{"afterTime":"2000-01-01T00:00:00Z","exact":{"stage":true},"variables":[{"name":"id","value":"u1"}]}}`, http.StatusUnprocessableEntity, ""},
		{"a staging without exact serving", `{"filterParams":{"afterTime":"2000-01-01T00:00:00Z"}}`, http.StatusOK, "filter"},
		{"a test case's open", `{"filterParams":{"exact":{"test":"test-1","owned":["mock-1"]},"variables":[{"name":"id","value":"u1"}]}}`, http.StatusOK, "filter reset bind:id=u1"},
		{"the pool call", `{"filterParams":{"exact":{}}}`, http.StatusOK, "filter"},
		{"a pool call that gives values", `{"filterParams":{"exact":{},"variables":[{"name":"id","value":"u1"}]}}`, http.StatusUnprocessableEntity, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			a := &Agent{logger: zap.NewNop(), svc: paramsSvc{events: &events}}
			rec := httptest.NewRecorder()
			a.UpdateMockParams(rec, httptest.NewRequest(http.MethodPost, "/agent/updatemockparams", strings.NewReader(tc.body)))
			if rec.Code != tc.code || strings.Join(events, " ") != tc.events {
				t.Fatalf("status %d, events %v; want %d and %q", rec.Code, events, tc.code, tc.events)
			}
		})
	}
}
