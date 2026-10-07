package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// timedScopeSvc takes the runner's time, as the OSS agent does.
type timedScopeSvc struct {
	agent.Service
	begun, ended string
	at           time.Time
}

func (s *timedScopeSvc) BeginScopeAt(_ context.Context, name string, _ int, at time.Time) error {
	s.begun, s.at = name, at
	return nil
}

func (s *timedScopeSvc) EndScopeAt(_ context.Context, name string, _ int, at time.Time) error {
	s.ended, s.at = name, at
	return nil
}

// plainScopeSvc predates the time: a wrapping build that only has the old methods.
type plainScopeSvc struct {
	agent.Service
	begun, ended string
}

func (s *plainScopeSvc) BeginScope(_ context.Context, name string, _ int) error {
	s.begun = name
	return nil
}

func (s *plainScopeSvc) EndScope(_ context.Context, name string, _ int) error {
	s.ended = name
	return nil
}

func post(t *testing.T, handle http.HandlerFunc, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	handle(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/x", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScopeHandlersPassTheRunnerTime(t *testing.T) {
	svc := &timedScopeSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	want := time.Date(2026, 9, 24, 10, 0, 0, 250000000, time.UTC)

	post(t, a.HandleScopeBegin, `{"name":"orders/e2e.TestX","at":"2026-09-24T10:00:00.25Z"}`)
	if svc.begun != "orders/e2e.TestX" || !svc.at.Equal(want) {
		t.Fatalf("begin got %q at %v", svc.begun, svc.at)
	}
	post(t, a.HandleScopeEnd, `{"name":"orders/e2e.TestX","at":"2026-09-24T10:00:00.75Z"}`)
	if svc.ended != "orders/e2e.TestX" || !svc.at.Equal(want.Add(500*time.Millisecond)) {
		t.Fatalf("end got %q at %v", svc.ended, svc.at)
	}
}

// A fixture that predates the field sends no "at"; the service sees a zero time and stamps its own.
func TestScopeHandlersWithoutATime(t *testing.T) {
	svc := &timedScopeSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	post(t, a.HandleScopeBegin, `{"name":"t","pid":42}`)
	if svc.begun != "t" || !svc.at.IsZero() {
		t.Fatalf("begin got %q at %v", svc.begun, svc.at)
	}
}

func TestScopeHandlersStillReachAServiceWithoutTheTime(t *testing.T) {
	svc := &plainScopeSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	post(t, a.HandleScopeBegin, `{"name":"t","at":"2026-09-24T10:00:00Z"}`)
	post(t, a.HandleScopeEnd, `{"name":"t"}`)
	if svc.begun != "t" || svc.ended != "t" {
		t.Fatalf("old service got begin %q end %q", svc.begun, svc.ended)
	}
}

// gatedScopeSvc gates tests and records the verdicts it is told.
type gatedScopeSvc struct {
	timedScopeSvc
	run       []string
	reason    string
	outcomes  map[string]string
	gated     []string
	ungatable []string
}

func (s *gatedScopeSvc) SetScopeGate(_ context.Context, run []string, reason string) error {
	s.run, s.reason = run, reason
	return nil
}

func (s *gatedScopeSvc) ScopeRun(name string) (bool, string) {
	if s.run == nil {
		return true, ""
	}
	for _, n := range s.run {
		if n == name {
			return true, ""
		}
	}
	return false, s.reason
}

func (s *gatedScopeSvc) NoteScopeGated(name string, _ int, _ time.Time) {
	s.gated = append(s.gated, name)
}

func (s *gatedScopeSvc) NoteUngatable(name string) {
	s.ungatable = append(s.ungatable, name)
}

func (s *gatedScopeSvc) NoteScopeOutcome(name string, _ int, outcome string) {
	if s.outcomes == nil {
		s.outcomes = map[string]string{}
	}
	s.outcomes[name] = outcome
}

// begin posts a scope begin and returns the answer, decoded as strings the way
// a harness that predates gating would: a gated answer must still decode.
func begin(t *testing.T, a *Agent, body string) map[string]string {
	t.Helper()
	rec := httptest.NewRecorder()
	a.HandleScopeBegin(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/begin", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("an answer a string-map harness cannot decode: %q: %v", rec.Body.String(), err)
	}
	return resp
}

// A test the replay gated out is told to skip, with the reason, BEFORE any of
// a begin's side effects: the service never hears of it beyond the record that
// it was gated. Every other answer is exactly {"status":"ok"}.
func TestScopeHandlersGateFirst(t *testing.T) {
	svc := &gatedScopeSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	post(t, a.HandleScopeGate, `{"run":["proven"],"reason":"not yet proven under replay"}`)

	if got := begin(t, a, `{"name":"proven"}`); len(got) != 1 || got["status"] != "ok" {
		t.Fatalf("a named test must get exactly {status: ok}: %v", got)
	}
	if svc.begun != "proven" {
		t.Fatalf("a named test must be begun, got %q", svc.begun)
	}
	svc.begun = ""
	got := begin(t, a, `{"name":"other","pid":42,"canSkip":true}`)
	if got["action"] != models.ScopeActionSkip || got["reason"] != "not yet proven under replay" {
		t.Fatalf("an unnamed test must be told to skip, with the reason: %v", got)
	}
	if svc.begun != "" {
		t.Fatalf("a gated test must not be begun (no window, no narrowed pool): begun %q", svc.begun)
	}
	if len(svc.gated) != 1 || svc.gated[0] != "other" {
		t.Fatalf("a gated test must be recorded as gated: %v", svc.gated)
	}
	if got := begin(t, a, `{"name":"suite","suite":true,"canSkip":true}`); got["action"] != "" {
		t.Fatalf("a suite scope is never gated: %v", got)
	}
}

// A harness that cannot skip (it does not say canSkip) would run a gated test
// anyway: the test is begun as usual — served its own mocks, its verdict
// counted — and the agent is told the gate could not apply.
func TestScopeHandlersBeginAGatedTestTheHarnessCannotSkip(t *testing.T) {
	svc := &gatedScopeSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	post(t, a.HandleScopeGate, `{"run":["proven"],"reason":"r"}`)
	got := begin(t, a, `{"name":"other"}`)
	if len(got) != 1 || got["status"] != "ok" {
		t.Fatalf("a harness that cannot skip must be answered {status: ok}: %v", got)
	}
	if svc.begun != "other" || len(svc.gated) != 0 || len(svc.ungatable) != 1 {
		t.Fatalf("begun %q, gated %v, ungatable %v: want the test begun as usual and the gate reported unappliable", svc.begun, svc.gated, svc.ungatable)
	}
}

// The verdict at the end of a scope reaches the service normalized; one the
// agent does not recognize still does, cut to size, so no policy reads it as a
// pass.
func TestScopeHandlersPassTheVerdict(t *testing.T) {
	svc := &gatedScopeSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	post(t, a.HandleScopeEnd, `{"name":"t1","outcome":"PASS"}`)
	post(t, a.HandleScopeEnd, `{"name":"t2","outcome":"pending"}`)
	post(t, a.HandleScopeEnd, `{"name":"t3"}`)
	if want := map[string]string{"t1": "passed", "t2": "skipped"}; len(svc.outcomes) != 2 || svc.outcomes["t1"] != want["t1"] || svc.outcomes["t2"] != want["t2"] {
		t.Fatalf("verdicts %v, want %v (and none for an end that reported none)", svc.outcomes, want)
	}
}

// A service that cannot gate refuses a gate rather than pretending to install it.
func TestScopeGateWithoutTheCapability(t *testing.T) {
	a := &Agent{logger: zap.NewNop(), svc: &plainScopeSvc{}}
	rec := httptest.NewRecorder()
	a.HandleScopeGate(rec, httptest.NewRequest(http.MethodPost, "/agent/replay/gate", strings.NewReader(`{"run":["x"]}`)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", rec.Code)
	}
	if got := begin(t, a, `{"name":"x"}`); got["action"] != "" {
		t.Fatalf("without a gate every test runs: %v", got)
	}
}
