package routes

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
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

// OpenTestCase opens a test case in one step (testCaseOpener): its filter
// applies, and its variables begin as it does, logged as the two are.
func (s paramsSvc) OpenTestCase(ctx context.Context, p models.MockFilterParams) error {
	if err := s.UpdateMockParams(ctx, p); err != nil {
		return err
	}
	return s.StartTestVariables(p.Variables)
}

func (s paramsSvc) TestVariables() ([]models.VariableBinding, bool) {
	if s.unkept {
		return nil, false
	}
	return []models.VariableBinding{{Name: "orderId", Value: "o1", Recorded: "r1", Mock: "mock-1", Place: "body.id"}}, true
}

// Under the proven switch every mock filter a client sends starts a test: the
// variables bound so far are forgotten as the filter applies. With the
// switch off nothing is forgotten. The scope begins as the new pool goes in,
// in one step (testCaseOpener).
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

// An agent that cannot open a test case in one step (testCaseOpener) opens
// none whose variables it keeps, even one that can only forget them: begun
// apart from the test's mocks going live, a call they answer between the two
// would bind in no scope of the test's. One that keeps none opens a test case
// given nothing, and refuses the values a test case gave rather than drop
// them: every call that uses one would fail the app for keploy's fault. Each
// refusal comes before the filter applies.
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
		{"forgets, nothing given", func(e *[]string) agent.Service { return resetOnlySvc{plainSvc{events: e}} }, `{"filterParams":{}}`, http.StatusNotImplemented, ""},
		{"forgets, a value given", func(e *[]string) agent.Service { return resetOnlySvc{plainSvc{events: e}} }, given, http.StatusNotImplemented, ""},
		{"keeps none, nothing given", func(e *[]string) agent.Service { return plainSvc{events: e} }, `{"filterParams":{}}`, http.StatusOK, "filter"},
		{"keeps none, a value given", func(e *[]string) agent.Service { return plainSvc{events: e} }, given, http.StatusNotImplemented, ""},
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

// openerSvc is an agent that opens a test case in one step (testCaseOpener):
// it logs the open, and the values it begins the test's variables with.
type openerSvc struct{ paramsSvc }

func (s openerSvc) OpenTestCase(_ context.Context, p models.MockFilterParams) error {
	ev := "open"
	for _, b := range p.Variables {
		ev += " bind:" + b.Name + "=" + b.Value
	}
	*s.events = append(*s.events, ev)
	return nil
}

// Under the switch a test case's filter opens the test in one step, on an
// agent that can (Agent.OpenTestCase): with exact serving or without, the
// route begins no variables after the filter applied, which would leave a
// window between the test's mocks going live and its variables beginning.
// The staging and the pool call open no test, and apply as before; with the
// switch off nothing opens a test.
func TestATestCasesFilterOpensTheTestInOneStep(t *testing.T) {
	for _, tc := range []struct {
		name, proven, body, events string
	}{
		{"a test case's open, exact serving", "1", `{"filterParams":{"exact":{"test":"test-1","owned":["mock-1"]},"variables":[{"name":"id","value":"u1"}]}}`, "open bind:id=u1"},
		{"a test case's window", "1", `{"filterParams":{"afterTime":"2026-01-01T00:00:00Z","variables":[{"name":"id","value":"u1"}]}}`, "open bind:id=u1"},
		{"the staging", "1", `{"filterParams":{"afterTime":"2000-01-01T00:00:00Z","exact":{"stage":true}}}`, "filter"},
		{"the pool call", "1", `{"filterParams":{"exact":{}}}`, "filter"},
		{"a test case's window, switch off", "0", `{"filterParams":{"afterTime":"2026-01-01T00:00:00Z"}}`, "filter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			var events []string
			a := &Agent{logger: zap.NewNop(), svc: openerSvc{paramsSvc{events: &events}}}
			rec := httptest.NewRecorder()
			a.UpdateMockParams(rec, httptest.NewRequest(http.MethodPost, "/agent/updatemockparams", strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK || strings.Join(events, " ") != tc.events {
				t.Fatalf("status %d, events %v; want 200 and %q", rec.Code, events, tc.events)
			}
		})
	}
}

// windowedProxy is keploy's proxy as keploy test's windowed staging reaches
// it, over a real mock manager: it stages each test's window, and opens a
// test with its variables in one step (proxy.Proxy.OpenTestWithWindow).
// live, when set, is a call made as soon as the window from liveAt is live.
type windowedProxy struct {
	coreAgent.Proxy // nil: any other call panics loudly
	mm              *proxyPkg.MockManager
	liveAt          time.Time
	live            func()
}

func (p *windowedProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, start, end time.Time) error {
	p.mm.SetMocksWithWindow(f, u, start, end)
	p.goLive(start)
	return nil
}

func (p *windowedProxy) OpenTestWithWindow(_ context.Context, f, u []*models.Mock, start, end time.Time, given []models.VariableBinding) error {
	p.mm.OpenTestWithWindow(f, u, start, end, given)
	p.goLive(start)
	return nil
}

func (p *windowedProxy) StartVariables(given []models.VariableBinding) bool {
	p.mm.StartVariables(given)
	return true
}

func (p *windowedProxy) BoundVariables() ([]models.VariableBinding, bool) {
	return p.mm.BoundVariables(), true
}

func (p *windowedProxy) goLive(start time.Time) {
	if p.live != nil && start.Equal(p.liveAt) {
		live := p.live
		p.live = nil
		live()
	}
}

// keyMock is an HTTP recording of POST path with body {"key":"k-1"}, made at
// at: its variable key bound at body.key (used there, bound by an earlier
// call, when use), answered at body.echo.
func keyMock(name, path string, use bool, at time.Time) *models.Mock {
	v := models.MockVariable{Recorded: "k-1", Answer: []string{"body.echo"}}
	if use {
		v.Use = []string{"body.key"}
	} else {
		v.Bind = "body.key"
	}
	resp := `{"echo":"k-1"}`
	m := &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"type": models.HTTPClient},
		HTTPReq: &models.HTTPReq{Method: "POST", URL: "http://deps" + path,
			Header: map[string]string{"Host": "deps", "Content-Type": "application/json"}, Body: `{"key":"k-1"}`},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(resp))}, Body: resp},
		Variables:        models.MockVariables{"key": v},
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
	}}
	m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	return m
}

// postKey sends POST path with body {"key":key} through the real HTTP
// integration over mm, and returns the status and body answered.
func postKey(t *testing.T, mm *proxyPkg.MockManager, path, key string) string {
	t.Helper()
	app, proxyEnd := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = httpparser.New(zap.NewNop()).MockOutgoing(ctx, proxyEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		proxyEnd.Close()
	}()
	defer func() { cancel(); app.Close(); <-served }()
	body := `{"key":"` + key + `"}`
	if _, err := io.WriteString(app, "POST "+path+" HTTP/1.1\r\nHost: deps\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		return "no answer: " + err.Error()
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return strconv.Itoa(resp.StatusCode) + " " + string(got)
}

// keploy test without exact serving, under the switch, through this route and
// the agent: a set's first test case's window goes live, and a call its own
// mock answers binds key at once (#4827, round 4). The route opens the test
// case in one step, so the bind is the test's: the test reads it (GET
// /agent/variables), and the next test case, whose call only uses key,
// misses. Opened in two steps (the window, then the variables), the bind
// landed in the run's, which every later test of the set reads, and the next
// test case's call was answered with this one's value: a false pass.
func TestATestCasesOwnBindAsItsWindowGoesLiveIsTheTestsNeverTheRuns(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	p := &windowedProxy{mm: mm, liveAt: t0.Add(-time.Second)}
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	svc := agent.New(zap.NewNop(), nil, p, nil, nil, cfg)
	ctx := context.Background()
	if err := svc.StoreMocks(ctx, []*models.Mock{keyMock("own-1", "/own-1", false, t0), keyMock("use-2", "/use-2", true, t0.Add(5*time.Second))}, nil); err != nil {
		t.Fatal(err)
	}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	filter := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		a.UpdateMockParams(rec, httptest.NewRequest(http.MethodPost, "/agent/updatemockparams", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("filter %s: status %d %s", body, rec.Code, rec.Body.String())
		}
	}
	bound := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		a.HandleTestVariables(rec, httptest.NewRequest(http.MethodGet, "/agent/variables", nil))
		var got models.TestVariablesResp
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
			t.Fatalf("GET /agent/variables: status %d %s", rec.Code, rec.Body.String())
		}
		var names []string
		for _, b := range got.Variables {
			names = append(names, b.Name+"="+b.Value)
		}
		return strings.Join(names, ",")
	}
	window := func(from time.Duration) string {
		return `"afterTime":"` + t0.Add(from-time.Second).Format(time.RFC3339Nano) + `","beforeTime":"` + t0.Add(from+time.Second).Format(time.RFC3339Nano) + `"`
	}
	// The set begins (Proxy.Mock under the switch), and its staging goes in.
	mm.NewVariableSet()
	filter(`{"filterParams":{"afterTime":"2000-01-01T00:00:00Z","beforeTime":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}}`)
	answered := ""
	p.live = func() { answered = postKey(t, mm, "/own-1", "x-1") }
	filter(`{"filterParams":{` + window(0) + `,"variables":[{"name":"id","value":"u1","recorded":"u0","mock":"test case test-1","place":"request"}]}}`)
	if answered != `200 {"echo":"x-1"}` {
		t.Fatalf("test-1's call, made as its window went live, was answered %q", answered)
	}
	if got := bound(); got != "id=u1,key=x-1" {
		t.Fatalf("test-1 holds %q; want the id its request gave and the key its own call bound", got)
	}
	filter(`{"filterParams":{` + window(5*time.Second) + `}}`)
	if got := bound(); got != "" {
		t.Fatalf("test-2 begins with %q bound; test-1's own call bound key", got)
	}
	if got := postKey(t, mm, "/use-2", "x-1"); strings.HasPrefix(got, "200 ") {
		t.Fatalf("test-2's call, which only uses key, was answered %s with test-1's value", got)
	}
}

// readingSvc is a paramsSvc whose OpenTestCase hands the learn step the test
// case's request its filter carries, or does not (testRequestReader).
type readingSvc struct {
	paramsSvc
	reads bool
}

func (s readingSvc) ReadsTestRequest() bool { return s.reads }

// The filter that opens a test case is answered saying the learn step took
// its request (models.TestRequestReadHeader) only where the agent's
// OpenTestCase hands it over; an agent that does not, or a filter that opens
// no test case (a set's staging), says nothing, and the replay keeps none of
// what the agent learned as noise for the test.
func TestTheOpeningFilterSaysTheLearnStepReadItsRequest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	opening := `{"filterParams":{"afterTime":"2026-10-01T10:00:00Z","testRequest":{"test":"test-1","parts":[{"name":"url","recorded":"/a","sent":"/a"}]}}}`
	staging := `{"filterParams":{"exact":{"stage":true}}}`
	for _, tc := range []struct {
		name string
		svc  agent.Service
		body string
		want string
	}{
		{"an agent that reads it", readingSvc{reads: true}, opening, models.TestRequestRead},
		{"an agent whose proxy reads none", readingSvc{reads: false}, opening, ""},
		{"an agent that cannot tell", paramsSvc{}, opening, ""},
		{"a set's staging", readingSvc{reads: true}, staging, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			svc := tc.svc
			switch s := svc.(type) {
			case readingSvc:
				s.events = &events
				svc = s
			case paramsSvc:
				s.events = &events
				svc = s
			}
			a := &Agent{logger: zap.NewNop(), svc: svc}
			rec := httptest.NewRecorder()
			a.UpdateMockParams(rec, httptest.NewRequest(http.MethodPost, "/agent/updatemockparams", strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, events %v", rec.Code, events)
			}
			if got := rec.Header().Get(models.TestRequestReadHeader); got != tc.want {
				t.Fatalf("%s %q; want %q", models.TestRequestReadHeader, got, tc.want)
			}
		})
	}
}
