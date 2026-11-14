package routes

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
	// The per-start table is the old wire's: the proven switch refuses it.
	t.Setenv(provenswitch.Env, "")
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

// windowsSvc answers each window read with the read it was.
type windowsSvc struct {
	agent.Service
	reads []string
}

func (s *windowsSvc) GetScopeWindows(context.Context) ([]models.ScopeWindow, error) {
	s.reads = append(s.reads, "plain")
	return []models.ScopeWindow{{Name: "plain"}}, nil
}

func (s *windowsSvc) FinalScopeWindows(context.Context) ([]models.ScopeWindow, error) {
	s.reads = append(s.reads, "final")
	return []models.ScopeWindow{{Name: "final"}}, nil
}

// The CLI's last read (?final=1) is the final one, which fails a test still
// open; a plain read is not; any other value is refused, since a client that
// meant the last read would otherwise get a plain one and leave an open test
// out of the verdict.
func TestScopeWindowsFinalReadIsTheRunsLast(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	svc := &windowsSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	for _, tc := range []struct {
		query, read string
		status      int
	}{
		{"", "plain", http.StatusOK},
		{"?final=1", "final", http.StatusOK},
		{"?final=true", "", http.StatusBadRequest},
		{"?final=0", "", http.StatusBadRequest},
	} {
		svc.reads = nil
		rec := httptest.NewRecorder()
		a.HandleScopeWindows(rec, httptest.NewRequest(http.MethodGet, "/agent/scope/windows"+tc.query, nil))
		if rec.Code != tc.status {
			t.Fatalf("%q: status %d, want %d (%s)", tc.query, rec.Code, tc.status, rec.Body.String())
		}
		if tc.read == "" {
			if len(svc.reads) != 0 {
				t.Fatalf("%q: read %v, want none", tc.query, svc.reads)
			}
			continue
		}
		var got []models.ScopeWindow
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%q: %v", tc.query, err)
		}
		if !slices.Equal(svc.reads, []string{tc.read}) || len(got) != 1 || got[0].Name != tc.read {
			t.Fatalf("%q: reads %v, answered %+v, want the %s read", tc.query, svc.reads, got, tc.read)
		}
	}
}

// With the switch off any final value other than 1 is the plain read, as
// before the final read existed.
func TestScopeWindowsWithTheSwitchOffReadAnyOtherFinalPlainly(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	svc := &windowsSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	rec := httptest.NewRecorder()
	a.HandleScopeWindows(rec, httptest.NewRequest(http.MethodGet, "/agent/scope/windows?final=true", nil))
	if rec.Code != http.StatusOK || !slices.Equal(svc.reads, []string{"plain"}) {
		t.Fatalf("status %d, reads %v, want one plain read", rec.Code, svc.reads)
	}
}

// poolSvc takes the per-test table and the pool slices.
type poolSvc struct {
	agent.Service
	table   map[string][]string
	pool    map[string][]string
	windows []models.TestWindow
	order   []string
	// worker is what workerSvc was handed.
	worker map[string][]string
	// notRequired is what notRequiredSvc was handed.
	notRequired map[string][]string
}

func (s *poolSvc) SetScopeTable(_ context.Context, t map[string][]string) error {
	s.table = t
	return nil
}

func (s *poolSvc) SetScopePool(_ context.Context, p map[string][]string) error {
	s.pool = p
	s.order = append(s.order, "pool")
	return nil
}

func (s *poolSvc) SetRecordedWindows(_ context.Context, ws []models.TestWindow) error {
	s.windows = ws
	s.order = append(s.order, "windows")
	return nil
}

// workerSvc is poolSvc that takes each key's worker scope too.
type workerSvc struct{ *poolSvc }

func (s workerSvc) SetScopeWorker(_ context.Context, w map[string][]string) error {
	s.worker = w
	s.order = append(s.order, "worker")
	return nil
}

// notRequiredSvc is poolSvc that takes the recordings keploy's judging marked served,
// not required, too.
type notRequiredSvc struct{ *poolSvc }

func (s notRequiredSvc) SetScopeNotRequired(_ context.Context, n map[string][]string) error {
	s.notRequired = n
	s.order = append(s.order, "notRequired")
	return nil
}

// tableOnlySvc takes the per-test table and no pool slices.
type tableOnlySvc struct{ agent.Service }

func (tableOnlySvc) SetScopeTable(context.Context, map[string][]string) error { return nil }

// Under the switch the scope table carries each key's pool slice; an agent
// that cannot serve them says so, since the CLI judges on them. With the
// switch off the slices are not read.
func TestScopeTableCarriesThePoolSlices(t *testing.T) {
	body := `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1"],"-":["mock-2"]}}`
	post := func(a *Agent) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		return rec
	}
	t.Setenv(provenswitch.Env, "1")
	svc := &poolSvc{}
	if rec := post(&Agent{logger: zap.NewNop(), svc: svc}); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !slices.Equal(svc.table["o::TestA"], []string{"mock-3"}) || !slices.Equal(svc.pool["o"], []string{"mock-1"}) || !slices.Equal(svc.pool["-"], []string{"mock-2"}) {
		t.Fatalf("table %v, pool %v", svc.table, svc.pool)
	}
	if rec := post(&Agent{logger: zap.NewNop(), svc: tableOnlySvc{}}); rec.Code != http.StatusNotImplemented {
		t.Fatalf("an agent without pool slices answered %d", rec.Code)
	}
	// A table without slices serves no pool mock between tests.
	svc = &poolSvc{}
	rec := httptest.NewRecorder()
	(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(`{"mappings":{"o::TestA":["mock-3"]}}`)))
	if rec.Code != http.StatusOK || svc.pool == nil || len(svc.pool) != 0 {
		t.Fatalf("a table without slices: status %d, pool %v, want an empty pool", rec.Code, svc.pool)
	}
	t.Setenv(provenswitch.Env, "")
	svc = &poolSvc{}
	if rec := post(&Agent{logger: zap.NewNop(), svc: svc}); rec.Code != http.StatusOK || svc.pool != nil {
		t.Fatalf("switch off: status %d, pool %v", rec.Code, svc.pool)
	}
}

// Under the switch the scope table carries each key's worker scope (keploy
// mock replay's worker: names), installed before the pool slices, which
// serve it; a table without one installs none, so a set never keeps the
// previous set's. An agent that cannot serve one refuses a table that has
// one, and takes a table without one. With the switch off it is not read.
func TestScopeTableCarriesTheWorkerScope(t *testing.T) {
	post := func(svc agent.Service, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		return rec
	}
	withWorker := `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1","mock-2"]},"worker":{"o":["mock-2"]}}`
	t.Setenv(provenswitch.Env, "1")
	ps := &poolSvc{}
	if rec := post(workerSvc{poolSvc: ps}, withWorker); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !slices.Equal(ps.worker["o"], []string{"mock-2"}) || !slices.Equal(ps.order, []string{"windows", "worker", "pool"}) {
		t.Fatalf("worker %v, installed %v; want o's worker scope installed before the pool", ps.worker, ps.order)
	}
	ps = &poolSvc{worker: map[string][]string{"o": {"stale"}}}
	if rec := post(workerSvc{poolSvc: ps}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || len(ps.worker) != 0 || !slices.Equal(ps.order, []string{"windows", "worker", "pool"}) {
		t.Fatalf("a table without one: status %d, worker %v, installed %v; want none installed before the pool", rec.Code, ps.worker, ps.order)
	}
	if rec := post(&poolSvc{}, withWorker); rec.Code != http.StatusNotImplemented {
		t.Fatalf("an agent without a worker scope answered %d to a table with one", rec.Code)
	}
	if rec := post(&poolSvc{}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("an agent without a worker scope answered %d to a table without one", rec.Code)
	}
	t.Setenv(provenswitch.Env, "")
	ps = &poolSvc{}
	if rec := post(workerSvc{poolSvc: ps}, withWorker); rec.Code != http.StatusOK || ps.worker != nil || len(ps.order) != 0 {
		t.Fatalf("switch off: status %d, worker %v, installed %v", rec.Code, ps.worker, ps.order)
	}
}

// trailsSvc is workerSvc that also takes the test each worker recording
// trails (scopeTrailsSetter).
type trailsSvc struct {
	workerSvc
	trails *map[string]string
	err    error
}

func (s trailsSvc) SetScopeTrails(_ context.Context, tr map[string]string) error {
	if s.err != nil {
		return s.err
	}
	*s.trails = tr
	s.order = append(s.order, "trails")
	return nil
}

// Under the switch the scope table carries the test each worker recording
// trails, installed with the worker scope, before the pool slices; a table
// without trails installs none, so a set never keeps the previous set's. An
// agent that cannot learn by them refuses a table that has some, and takes a
// table without. With the switch off they are not read.
func TestScopeTableCarriesTheTrails(t *testing.T) {
	post := func(svc agent.Service, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		return rec
	}
	withTrails := `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-2"]},"worker":{"o":["mock-2"]},"trails":{"mock-2":"o::TestA"}}`
	t.Setenv(provenswitch.Env, "1")
	ps, got := &poolSvc{}, map[string]string{}
	if rec := post(trailsSvc{workerSvc{ps}, &got, nil}, withTrails); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !maps.Equal(got, map[string]string{"mock-2": "o::TestA"}) || !slices.Equal(ps.order, []string{"windows", "worker", "trails", "pool"}) {
		t.Fatalf("trails %v, installed %v; want them installed after the worker scope, before the pool", got, ps.order)
	}
	ps, got = &poolSvc{}, map[string]string{"stale": "o::TestA"}
	if rec := post(trailsSvc{workerSvc{ps}, &got, nil}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || len(got) != 0 || !slices.Equal(ps.order, []string{"windows", "worker", "trails", "pool"}) {
		t.Fatalf("a table without trails: status %d, trails %v, installed %v; want none installed", rec.Code, got, ps.order)
	}
	ps = &poolSvc{}
	if rec := post(trailsSvc{workerSvc{ps}, &got, errors.New("refused")}, withTrails); rec.Code != http.StatusInternalServerError || len(ps.order) != 2 {
		t.Fatalf("an agent that could not install the trails answered %d and installed %v; want 500 before the pool", rec.Code, ps.order)
	}
	if rec := post(workerSvc{&poolSvc{}}, withTrails); rec.Code != http.StatusNotImplemented {
		t.Fatalf("an agent without trails answered %d to a table with some", rec.Code)
	}
	if rec := post(workerSvc{&poolSvc{}}, `{"mappings":{},"pool":{},"worker":{"o":["mock-2"]}}`); rec.Code != http.StatusOK {
		t.Fatalf("an agent without trails answered %d to a table without any", rec.Code)
	}
	t.Setenv(provenswitch.Env, "")
	ps, got = &poolSvc{}, nil
	if rec := post(trailsSvc{workerSvc{ps}, &got, nil}, withTrails); rec.Code != http.StatusOK || got != nil || len(ps.order) != 0 {
		t.Fatalf("switch off: status %d, trails %v, installed %v", rec.Code, got, ps.order)
	}
}

// Under the switch the scope table carries, by test, the recordings
// keploy's judging marked served, not required, installed before the pool slices; a
// table without them installs none, so a set never keeps the previous
// set's. An agent that cannot take them refuses a table that has some (it
// would charge each test for them), and takes a table without. With the
// switch off they are not read.
func TestScopeTableCarriesTheRecordingsMarkedNotRequired(t *testing.T) {
	post := func(svc agent.Service, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		return rec
	}
	marked := `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1"]},"notRequired":{"o::TestA":["mock-3"]}}`
	t.Setenv(provenswitch.Env, "1")
	ps := &poolSvc{}
	if rec := post(notRequiredSvc{poolSvc: ps}, marked); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !slices.Equal(ps.notRequired["o::TestA"], []string{"mock-3"}) || !slices.Equal(ps.order, []string{"windows", "notRequired", "pool"}) {
		t.Fatalf("marked %v, installed %v; want o::TestA's mark installed before the pool", ps.notRequired, ps.order)
	}
	ps = &poolSvc{notRequired: map[string][]string{"o::TestA": {"stale"}}}
	if rec := post(notRequiredSvc{poolSvc: ps}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || len(ps.notRequired) != 0 || !slices.Equal(ps.order, []string{"windows", "notRequired", "pool"}) {
		t.Fatalf("a table without marks: status %d, marked %v, installed %v; want none installed before the pool", rec.Code, ps.notRequired, ps.order)
	}
	if rec := post(&poolSvc{}, marked); rec.Code != http.StatusNotImplemented {
		t.Fatalf("an agent that cannot take marks answered %d to a table with one", rec.Code)
	}
	if rec := post(&poolSvc{}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("an agent that cannot take marks answered %d to a table without one", rec.Code)
	}
	t.Setenv(provenswitch.Env, "")
	ps = &poolSvc{}
	if rec := post(notRequiredSvc{poolSvc: ps}, marked); rec.Code != http.StatusOK || ps.notRequired != nil || len(ps.order) != 0 {
		t.Fatalf("switch off: status %d, marked %v, installed %v", rec.Code, ps.notRequired, ps.order)
	}
}

// skippedSvc is workerSvc that takes which of the worker scope are the reads
// of tests the replay's gate skips too.
type skippedSvc struct {
	workerSvc
	skipped *map[string]string
	err     error
}

func (s skippedSvc) SetScopeSkippedReads(_ context.Context, reads map[string]string) error {
	*s.skipped = reads
	s.order = append(s.order, "skipped")
	return s.err
}

// bothSvc is skippedSvc that takes the recordings keploy's judging marked
// served, not required, too, as the agent does.
type bothSvc struct{ skippedSvc }

func (s bothSvc) SetScopeNotRequired(_ context.Context, n map[string][]string) error {
	s.notRequired = n
	s.order = append(s.order, "notRequired")
	return nil
}

// Under the switch the scope table says which of the worker scope are the
// reads of tests keploy mock replay's gate skips, installed with the worker
// scope and before the pool slices, which serve it; a table without them
// installs none, so a set never keeps the previous set's.
func TestScopeTableCarriesTheSkippedReads(t *testing.T) {
	post := func(svc agent.Service, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		return rec
	}
	t.Setenv(provenswitch.Env, "1")
	ps, skipped := &poolSvc{}, map[string]string{"stale": "o::TestOld"}
	if rec := post(skippedSvc{workerSvc{ps}, &skipped, nil}, `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1","mock-2"]},"worker":{"o":["mock-2"]},"skippedReads":{"mock-2":"o::TestCRUD"}}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !maps.Equal(skipped, map[string]string{"mock-2": "o::TestCRUD"}) || !slices.Equal(ps.order, []string{"windows", "worker", "skipped", "pool"}) {
		t.Fatalf("skipped reads %v, installed %v; want mock-2 installed with the worker scope, before the pool", skipped, ps.order)
	}
	ps, skipped = &poolSvc{}, map[string]string{"stale": "o::TestOld"}
	if rec := post(skippedSvc{workerSvc{ps}, &skipped, nil}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || len(skipped) != 0 {
		t.Fatalf("a table without them: status %d, skipped reads %v; want none", rec.Code, skipped)
	}
	// An agent that cannot install them refuses the table, and serves no
	// pool from it.
	ps = &poolSvc{}
	if rec := post(skippedSvc{workerSvc{ps}, &skipped, errors.New("wire closed")}, `{"mappings":{},"pool":{},"skippedReads":{"mock-2":"o::TestCRUD"}}`); rec.Code != http.StatusInternalServerError || ps.pool != nil {
		t.Fatalf("a failed install: status %d, pool %v; want 500 and no pool", rec.Code, ps.pool)
	}
	// A table that carries both a skipped test's read and a test's mark
	// installs both, each before the pool: neither is dropped beside the
	// other.
	ps, skipped = &poolSvc{}, map[string]string{}
	if rec := post(bothSvc{skippedSvc{workerSvc{ps}, &skipped, nil}}, `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1","mock-2"]},"worker":{"o":["mock-2"]},"skippedReads":{"mock-2":"o::TestCRUD"},"notRequired":{"o::TestA":["mock-3"]}}`); rec.Code != http.StatusOK {
		t.Fatalf("both: status %d: %s", rec.Code, rec.Body.String())
	}
	if !maps.Equal(skipped, map[string]string{"mock-2": "o::TestCRUD"}) || !slices.Equal(ps.notRequired["o::TestA"], []string{"mock-3"}) || !slices.Equal(ps.order, []string{"windows", "worker", "skipped", "notRequired", "pool"}) {
		t.Fatalf("both: skipped reads %v, marked %v, installed %v; want both installed, before the pool", skipped, ps.notRequired, ps.order)
	}
}

// Under the switch the scope table's recorded test windows (keploy mock
// replay's, from the set's mappings) are installed before the pool slices,
// whose serving may stage the set; a table without them installs none, so a
// set never reads the previous one's. With the switch off they are not read.
func TestScopeTableInstallsTheRecordedWindowsBeforeThePool(t *testing.T) {
	post := func(svc *poolSvc, body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
	body := `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1"]},"windows":[{"testCase":"o::TestA","start":"2026-01-02T03:04:05Z","end":"2026-01-02T03:04:06Z"}]}`
	t.Setenv(provenswitch.Env, "1")
	svc := &poolSvc{}
	post(svc, body)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if want := []models.TestWindow{{TestCase: "o::TestA", Start: t0, End: t0.Add(time.Second)}}; !reflect.DeepEqual(svc.windows, want) {
		t.Fatalf("windows %v, want %v", svc.windows, want)
	}
	if !slices.Equal(svc.order, []string{"windows", "pool"}) {
		t.Fatalf("installed %v, want the windows before the pool", svc.order)
	}
	svc = &poolSvc{}
	post(svc, `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1"]}}`)
	if !slices.Equal(svc.order, []string{"windows", "pool"}) || len(svc.windows) != 0 {
		t.Fatalf("a table without windows: installed %v, windows %v; want none installed before the pool", svc.order, svc.windows)
	}
	t.Setenv(provenswitch.Env, "")
	svc = &poolSvc{}
	post(svc, body)
	if len(svc.order) != 0 {
		t.Fatalf("switch off: installed %v", svc.order)
	}
}

// placeSvc is poolSvc that takes the run place's windows too.
type placeSvc struct {
	*poolSvc
	placed      []models.TestWindow
	placedCalls int
}

func (s *placeSvc) SetRunPlaceWindows(_ context.Context, ws []models.TestWindow) error {
	s.placed = ws
	s.placedCalls++
	return nil
}

// keploy mock replay's scope table carries every test's recorded window for
// the run's place, under either switch: the agent installs them there (a
// merged window's tests included), with the switch off never as the set's
// recorded windows. A table without them installs none, so a set never reads
// the previous one's. A per-start table's are not the place's.
func TestScopeTableGivesTheRunPlaceItsWindows(t *testing.T) {
	post := func(svc *placeSvc, body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
	body := `{"mappings":{"o::TestA":["mock-3"]},"windows":[{"start":"2026-01-02T03:04:05Z","end":"2026-01-02T03:04:06Z","tests":["o::TestA","o::TestB"]}]}`
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t.Setenv(provenswitch.Env, "")
	svc := &placeSvc{poolSvc: &poolSvc{}}
	post(svc, body)
	if want := []models.TestWindow{{Start: t0, End: t0.Add(time.Second), Tests: []string{"o::TestA", "o::TestB"}}}; svc.placedCalls != 1 || !reflect.DeepEqual(svc.placed, want) {
		t.Fatalf("switch off: the place got %v (%d calls), want %v", svc.placed, svc.placedCalls, want)
	}
	if len(svc.order) != 0 {
		t.Fatalf("switch off: installed %v as the set's", svc.order)
	}
	svc = &placeSvc{poolSvc: &poolSvc{}, placed: []models.TestWindow{{TestCase: "stale"}}}
	post(svc, `{"mappings":{"o::TestA":["mock-3"]}}`)
	if svc.placedCalls != 1 || svc.placed != nil {
		t.Fatalf("switch off, a table without windows: the place got %v (%d calls), want none", svc.placed, svc.placedCalls)
	}
	t.Cleanup(starts.Default.Reset)
	svc = &placeSvc{poolSvc: &poolSvc{}}
	post(svc, `{"mappings":{},"sets":{"s":{}},"windows":[{"start":"2026-01-02T03:04:05Z","end":"2026-01-02T03:04:06Z"}]}`)
	if svc.placedCalls != 0 {
		t.Fatalf("a per-start table gave the place %v", svc.placed)
	}
	t.Setenv(provenswitch.Env, "1")
	svc = &placeSvc{poolSvc: &poolSvc{}}
	post(svc, `{"mappings":{"o::TestA":["mock-3"]},"pool":{"o":["mock-1"]},"windows":[{"testCase":"o::TestA","start":"2026-01-02T03:04:05Z","end":"2026-01-02T03:04:06Z"}]}`)
	if svc.placedCalls != 1 || len(svc.placed) != 1 || len(svc.windows) != 1 {
		t.Fatalf("switch on: the place got %v (%d calls), the set %v; want both", svc.placed, svc.placedCalls, svc.windows)
	}
}

// seriesSvc is workerSvc that also serves the series of the recordings a
// declared rule placed (scopeSeriesSetter).
type seriesSvc struct {
	workerSvc
	series *map[string][]string
	err    error
}

func (s seriesSvc) SetScopeSeries(_ context.Context, se map[string][]string) error {
	if s.err != nil {
		return s.err
	}
	*s.series = se
	s.order = append(s.order, "series")
	return nil
}

// Under the switch the scope table carries the series of the recordings a
// declared rule placed, installed with the worker scope, before the pool
// slices, and the answer names them served (models.ScopeTableResp), an
// empty table's too; a table without series installs none. An agent that
// cannot serve them refuses a table that has some, and takes one without,
// naming none served.
func TestScopeTableCarriesTheSeriesAndAcksThem(t *testing.T) {
	post := func(svc agent.Service, body string) (*httptest.ResponseRecorder, models.ScopeTableResp) {
		t.Helper()
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		var ack models.ScopeTableResp
		_ = json.Unmarshal(rec.Body.Bytes(), &ack)
		return rec, ack
	}
	withSeries := `{"mappings":{"o::TestA":[]},"pool":{"o":["mock-1","mock-2"]},"worker":{"o":["mock-1","mock-2"]},"series":{"sweep/o::TestA/1":["mock-1","mock-2"]}}`
	t.Setenv(provenswitch.Env, "1")
	ps, got := &poolSvc{}, map[string][]string{}
	rec, ack := post(seriesSvc{workerSvc{ps}, &got, nil}, withSeries)
	if rec.Code != http.StatusOK || !slices.Equal(ack.Served, []string{"series"}) || ack.Status != "ok" {
		t.Fatalf("status %d, answer %s; want 200 naming series served", rec.Code, rec.Body.String())
	}
	if !slices.Equal(got["sweep/o::TestA/1"], []string{"mock-1", "mock-2"}) || !slices.Equal(ps.order, []string{"windows", "worker", "series", "pool"}) {
		t.Fatalf("series %v, installed %v; want them installed after the worker scope, before the pool", got, ps.order)
	}
	ps, got = &poolSvc{}, map[string][]string{"stale": {"x"}}
	if rec, ack := post(seriesSvc{workerSvc{ps}, &got, nil}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || len(got) != 0 || !slices.Equal(ack.Served, []string{"series"}) {
		t.Fatalf("a table without series: status %d, series %v, answer %s; want none installed, series served", rec.Code, got, rec.Body.String())
	}
	ps = &poolSvc{}
	if rec, _ := post(seriesSvc{workerSvc{ps}, &got, errors.New("refused")}, withSeries); rec.Code != http.StatusInternalServerError || slices.Contains(ps.order, "pool") {
		t.Fatalf("an agent that could not install the series answered %d and installed %v; want 500 before the pool", rec.Code, ps.order)
	}
	if rec, _ := post(workerSvc{&poolSvc{}}, withSeries); rec.Code != http.StatusNotImplemented {
		t.Fatalf("an agent without series answered %d to a table with some", rec.Code)
	}
	if rec, ack := post(workerSvc{&poolSvc{}}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || len(ack.Served) != 0 {
		t.Fatalf("an agent without series answered %d, %s to a table without any; want 200 naming none", rec.Code, rec.Body.String())
	}
}

// onceSvc is seriesSvc that also serves the series that answer once in a
// run (scopeOnceSetter).
type onceSvc struct {
	seriesSvc
	once *[]string
	err  error
}

func (s onceSvc) SetScopeOnce(_ context.Context, once []string) error {
	if s.err != nil {
		return s.err
	}
	*s.once = once
	s.order = append(s.order, "once")
	return nil
}

// Under the switch the scope table carries the series that answer once in
// a run, installed after the series, before the pool slices, and the answer
// names them served, an empty table's too. An agent that cannot serve them
// refuses a table that has some (it would answer the request again), and
// takes one without, naming them not served; one that fails to install
// them fails before the pool.
func TestScopeTableCarriesTheOnceSeriesAndAcksThem(t *testing.T) {
	post := func(svc agent.Service, body string) (*httptest.ResponseRecorder, models.ScopeTableResp) {
		t.Helper()
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		var ack models.ScopeTableResp
		_ = json.Unmarshal(rec.Body.Bytes(), &ack)
		return rec, ack
	}
	withOnce := `{"mappings":{"o::TestA":[]},"pool":{"o":["mock-1"]},"worker":{"o":["mock-1"]},"series":{"pat/-/1":["mock-1"]},"once":["pat/-/1"]}`
	t.Setenv(provenswitch.Env, "1")
	ps, series, once := &poolSvc{}, map[string][]string{}, []string(nil)
	rec, ack := post(onceSvc{seriesSvc{workerSvc{ps}, &series, nil}, &once, nil}, withOnce)
	if rec.Code != http.StatusOK || !slices.Equal(ack.Served, []string{"series", "once"}) {
		t.Fatalf("status %d, answer %s; want 200 naming series and once served", rec.Code, rec.Body.String())
	}
	if !slices.Equal(once, []string{"pat/-/1"}) || !slices.Equal(ps.order, []string{"windows", "worker", "series", "once", "pool"}) {
		t.Fatalf("once %v, installed %v; want it installed after the series, before the pool", once, ps.order)
	}
	ps, once = &poolSvc{}, []string{"stale"}
	if rec, ack := post(onceSvc{seriesSvc{workerSvc{ps}, &series, nil}, &once, nil}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || len(once) != 0 || !slices.Contains(ack.Served, "once") {
		t.Fatalf("a table without once series: status %d, once %v, answer %s; want none installed, once served", rec.Code, once, rec.Body.String())
	}
	ps = &poolSvc{}
	if rec, _ := post(onceSvc{seriesSvc{workerSvc{ps}, &series, nil}, &once, errors.New("refused")}, withOnce); rec.Code != http.StatusInternalServerError || slices.Contains(ps.order, "pool") {
		t.Fatalf("an agent that could not install the once series answered %d and installed %v; want 500 before the pool", rec.Code, ps.order)
	}
	ps = &poolSvc{}
	if rec, _ := post(seriesSvc{workerSvc{ps}, &series, nil}, withOnce); rec.Code != http.StatusNotImplemented || slices.Contains(ps.order, "pool") {
		t.Fatalf("an agent without once series answered %d to a table with some (installed %v); want 501 before the pool", rec.Code, ps.order)
	}
	if rec, ack := post(seriesSvc{workerSvc{&poolSvc{}}, &series, nil}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || slices.Contains(ack.Served, "once") {
		t.Fatalf("an agent without once series answered %d, %s to a table without any; want 200 not naming once", rec.Code, rec.Body.String())
	}
}

// everySvc is onceSvc that also serves a once series again after an
// interval (scopeEverySetter).
type everySvc struct {
	onceSvc
	every *map[string]string
	err   error
}

func (s everySvc) SetScopeEvery(_ context.Context, every map[string]string) error {
	if s.err != nil {
		return s.err
	}
	*s.every = every
	s.order = append(s.order, "every")
	return nil
}

// Under the switch the scope table carries, by once series, the interval
// after which it answers again, installed after the once series, before the
// pool slices, and the answer names it served, an empty table's too. An
// agent that refuses the intervals (one is not a positive duration) answers
// 400 before the pool. An agent that cannot serve them refuses a table that
// has some (it would answer such a series once in the run), and takes one
// without, naming them not served.
func TestScopeTableCarriesTheIntervalsAndAcksThem(t *testing.T) {
	post := func(svc agent.Service, body string) (*httptest.ResponseRecorder, models.ScopeTableResp) {
		t.Helper()
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop(), svc: svc}).HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		var ack models.ScopeTableResp
		_ = json.Unmarshal(rec.Body.Bytes(), &ack)
		return rec, ack
	}
	withEvery := `{"mappings":{"o::TestA":[]},"pool":{"o":["mock-1"]},"worker":{"o":["mock-1"]},"series":{"pat/-/1":["mock-1"]},"once":["pat/-/1"],"every":{"pat/-/1":"5m"}}`
	t.Setenv(provenswitch.Env, "1")
	series, once := map[string][]string{}, []string(nil)
	svc := func(ps *poolSvc, every *map[string]string, err error) everySvc {
		return everySvc{onceSvc{seriesSvc{workerSvc{ps}, &series, nil}, &once, nil}, every, err}
	}
	ps, every := &poolSvc{}, map[string]string(nil)
	rec, ack := post(svc(ps, &every, nil), withEvery)
	if rec.Code != http.StatusOK || !slices.Equal(ack.Served, []string{"series", "once", "every"}) {
		t.Fatalf("status %d, answer %s; want 200 naming series, once and every served", rec.Code, rec.Body.String())
	}
	if every["pat/-/1"] != "5m" || len(every) != 1 || !slices.Equal(ps.order, []string{"windows", "worker", "series", "once", "every", "pool"}) {
		t.Fatalf("every %v, installed %v; want it installed after the once series, before the pool", every, ps.order)
	}
	ps, every = &poolSvc{}, map[string]string{"stale": "1m"}
	if rec, ack := post(svc(ps, &every, nil), `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || len(every) != 0 || !slices.Contains(ack.Served, "every") {
		t.Fatalf("a table without intervals: status %d, every %v, answer %s; want none installed, every served", rec.Code, every, rec.Body.String())
	}
	ps = &poolSvc{}
	if rec, _ := post(svc(ps, &every, errors.New("not a positive Go duration")), withEvery); rec.Code != http.StatusBadRequest || slices.Contains(ps.order, "pool") {
		t.Fatalf("an agent that refused the intervals answered %d and installed %v; want 400 before the pool", rec.Code, ps.order)
	}
	ps = &poolSvc{}
	if rec, _ := post(onceSvc{seriesSvc{workerSvc{ps}, &series, nil}, &once, nil}, withEvery); rec.Code != http.StatusNotImplemented || slices.Contains(ps.order, "pool") {
		t.Fatalf("an agent without intervals answered %d to a table with some (installed %v); want 501 before the pool", rec.Code, ps.order)
	}
	if rec, ack := post(onceSvc{seriesSvc{workerSvc{&poolSvc{}}, &series, nil}, &once, nil}, `{"mappings":{},"pool":{}}`); rec.Code != http.StatusOK || slices.Contains(ack.Served, "every") || !slices.Contains(ack.Served, "once") {
		t.Fatalf("an agent without intervals answered %d, %s to a table without any; want 200 naming once, not every", rec.Code, rec.Body.String())
	}
}
