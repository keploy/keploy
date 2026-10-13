package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
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
	table map[string][]string
	pool  map[string][]string
}

func (s *poolSvc) SetScopeTable(_ context.Context, t map[string][]string) error {
	s.table = t
	return nil
}

func (s *poolSvc) SetScopePool(_ context.Context, p map[string][]string) error {
	s.pool = p
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

// oneContainer resolves every destination to one postgres container per
// destination port.
type oneContainer struct{}

func (oneContainer) Lookup(dst netip.AddrPort) (containerkey.Container, bool, error) {
	return containerkey.Container{ID: fmt.Sprint(dst.Port()), Image: "postgres:16", Port: 5432}, true, nil
}

func (oneContainer) Bind(context.Context)               {}
func (oneContainer) Watch(context.Context, *zap.Logger) {}
func (oneContainer) Endpoint() string                   { return "unix:///fake.sock" }

func useKeyer(t *testing.T) *containerkey.Keyer {
	t.Helper()
	saved := containerkey.Default
	containerkey.Default = containerkey.NewKeyer(oneContainer{})
	t.Cleanup(func() { containerkey.Default = saved })
	return containerkey.Default
}

func keyOf(t *testing.T, k *containerkey.Keyer, dst string) string {
	t.Helper()
	_, key, _ := k.Name(zap.NewNop(), "", dst)
	return key
}

// Under the proven switch a test's begin opens its own numbering of
// container destinations, and its end goes back to the run's: the suite's
// container keeps #1 and each test's own container is #2.
func TestScopeHandlersOpenAndCloseTheContainerNumbering(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	k := useKeyer(t)
	a := &Agent{logger: zap.NewNop(), svc: &timedScopeSvc{}}

	if got := keyOf(t, k, "127.0.0.1:40001"); got != "container:postgres:16:5432#1" {
		t.Fatalf("suite container = %q", got)
	}
	for _, test := range []struct{ name, dst string }{{"TestA", "127.0.0.1:40002"}, {"TestB", "127.0.0.1:40003"}} {
		post(t, a.HandleScopeBegin, `{"name":"`+test.name+`"}`)
		if got := keyOf(t, k, test.dst); got != "container:postgres:16:5432#2" {
			t.Fatalf("%s's own container = %q", test.name, got)
		}
		post(t, a.HandleScopeEnd, `{"name":"`+test.name+`","outcome":"passed"}`)
	}
	// The run numbers on from its own: the tests' containers are not its.
	if got := keyOf(t, k, "127.0.0.1:40004"); got != "container:postgres:16:5432#2" {
		t.Fatalf("outside tests after them = %q, want the run's next number", got)
	}
}

// numberedInTheRun reports whether a container first connected between the
// begin and end given (a suite scope, or any scope without the switch) was
// numbered by the run. A test begun next starts from the run's numbers, so its
// own container is then #3; had the scope been a test's (left open or not),
// the run would not know that container, and it would be #2.
func numberedInTheRun(t *testing.T, begin, end string) bool {
	t.Helper()
	k := useKeyer(t)
	a := &Agent{logger: zap.NewNop(), svc: &timedScopeSvc{}}
	_ = keyOf(t, k, "127.0.0.1:40001")
	post(t, a.HandleScopeBegin, begin)
	_ = keyOf(t, k, "127.0.0.1:40002")
	post(t, a.HandleScopeEnd, end)
	post(t, a.HandleScopeBegin, `{"name":"TestNext"}`)
	got := keyOf(t, k, "127.0.0.1:40003")
	post(t, a.HandleScopeEnd, `{"name":"TestNext","outcome":"passed"}`)
	return got == "container:postgres:16:5432#3"
}

// An app start (a suite scope) is not a test.
func TestScopeHandlersOpenNoTestNumberingForASuite(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	if !numberedInTheRun(t, `{"name":"app","suite":true}`, `{"name":"app","suite":true}`) {
		t.Fatal("a suite begin opened a test's numbering")
	}
	// The same calls without "suite" do open one.
	if numberedInTheRun(t, `{"name":"TestA"}`, `{"name":"TestA","outcome":"passed"}`) {
		t.Fatal("a test begin did not open a test's numbering")
	}
}

func TestScopeHandlersLeaveContainerNumberingAloneWithoutTheSwitch(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	if !numberedInTheRun(t, `{"name":"TestA"}`, `{"name":"TestA","outcome":"passed"}`) {
		t.Fatal("with the switch off a test begin opened a test's numbering")
	}
}
