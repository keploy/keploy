package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/starts"
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

func TestScopeHandlersAddTheOffsetBackWhileTheRunnersClockIsShifted(t *testing.T) {
	var pids []int
	OnMark = func(pid int, _ string, _ time.Time, _ []string) time.Duration {
		pids = append(pids, pid)
		return 48 * time.Hour
	}
	t.Cleanup(func() { OnMark = nil })
	at := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Millisecond)
	body := func(s string) string { return strings.Replace(s, "AT", at.Format(time.RFC3339Nano), 1) }
	svc := &timedScopeSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	post(t, a.HandleScopeBegin, body(`{"name":"t","pid":7,"at":"AT"}`))
	if svc.begun != "t" || !svc.at.Equal(at.Add(48*time.Hour)) {
		t.Fatalf("begin got %q at %v", svc.begun, svc.at)
	}
	post(t, a.HandleScopeEnd, body(`{"name":"t","pid":7,"at":"AT"}`))
	if svc.ended != "t" || !svc.at.Equal(at.Add(48*time.Hour)) {
		t.Fatalf("end got %q at %v", svc.ended, svc.at)
	}
	post(t, a.HandleAppStart, body(`{"pid":9,"at":"AT"}`))
	if len(pids) != 3 || pids[0] != 0 || pids[1] != 0 || pids[2] != 9 {
		t.Fatalf("OnMark saw %v", pids)
	}
}

func TestScopeHandlersKeepTheRunnerTimeWhenItsClockIsNotShifted(t *testing.T) {
	OnMark = func(int, string, time.Time, []string) time.Duration { return 48 * time.Hour }
	t.Cleanup(func() { OnMark = nil })
	at := time.Now().UTC().Truncate(time.Millisecond)
	svc := &timedScopeSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	post(t, a.HandleScopeBegin, `{"name":"t","at":"`+at.Format(time.RFC3339Nano)+`"}`)
	if !svc.at.Equal(at) {
		t.Fatalf("begin at %v", svc.at)
	}
	OnMark = func(int, string, time.Time, []string) time.Duration { return 0 }
	post(t, a.HandleScopeBegin, `{"name":"t","at":"2026-09-24T10:00:00Z"}`)
	if !svc.at.Equal(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("begin at %v", svc.at)
	}
}

func TestScopeHandlersTellOnMarkTheSetsRecordedStart(t *testing.T) {
	type call struct {
		set   string
		start time.Time
		live  []string
	}
	var got []call
	OnMark = func(_ int, set string, start time.Time, live []string) time.Duration {
		got = append(got, call{set, start, live})
		return 0
	}
	t.Cleanup(func() { OnMark = nil; starts.Default.Reset() })
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	starts.Default.SetTable("/r", map[string]models.SetTable{"a": {Start: start}, "b": {}})
	a := &Agent{logger: zap.NewNop(), svc: &timedScopeSvc{}}
	post(t, a.HandleScopeBegin, `{"name":"s","pid":7,"dir":"/r/a","suite":true}`)
	post(t, a.HandleScopeEnd, `{"name":"t","pid":7}`)
	post(t, a.HandleScopeBegin, `{"name":"u","pid":8,"dir":"/r/b"}`)
	if len(got) != 3 || got[0].set != "a" || !got[0].start.Equal(start) || len(got[0].live) != 0 {
		t.Fatalf("OnMark saw %v", got)
	}
	if got[1].set != "a" || !got[1].start.Equal(start) || !slices.Equal(got[1].live, []string{"a"}) {
		t.Fatalf("OnMark saw %v", got)
	}
	if got[2].set != "b" || !got[2].start.IsZero() || !slices.Equal(got[2].live, []string{"a"}) {
		t.Fatalf("OnMark saw %v", got)
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

type anchorHooks struct {
	agent.AgentHook
	got []time.Time
}

func (h *anchorHooks) SetFreezeAnchor(_ context.Context, at time.Time) error {
	h.got = append(h.got, at)
	return nil
}

func TestScopeTableAnchorsTheClockAtTheFirstSuiteStart(t *testing.T) {
	h := &anchorHooks{}
	prev := agent.ActiveHooks
	agent.ActiveHooks = h
	t.Cleanup(func() { agent.ActiveHooks = prev })
	a := &Agent{logger: zap.NewNop(), svc: &plainScopeSvc{}}
	post(t, a.HandleScopeTable, `{"sets":{"a":{"start":"2026-10-06T10:00:05Z"},"b":{"start":"2026-10-06T10:00:01Z"},"c":{}}}`)
	if len(h.got) != 1 || !h.got[0].Equal(time.Date(2026, 10, 6, 10, 0, 1, 0, time.UTC)) {
		t.Fatalf("anchored at %v", h.got)
	}
	post(t, a.HandleScopeTable, `{"sets":{"a":{}}}`)
	if len(h.got) != 1 {
		t.Fatalf("anchored without a suite start: %v", h.got)
	}
}
