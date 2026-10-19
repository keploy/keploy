package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// These tests run `keploy mock replay` with the proven switch off as a test
// harness without a worker pid drives it (POST /agent/scope/begin|end
// {"name":...}): the agent's StoreMocks, SetScopeTable and set staging, then
// BeginScope/EndScope with pid 0, against a real MockManager, with the real
// HTTP parser answering the application's calls.

var stageT0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

// stageProxy is the part of the proxy the scope staging reaches, over a real
// MockManager, forwarding as proxy.Proxy does.
type stageProxy struct {
	coreAgent.Proxy
	mm *proxyPkg.MockManager
}

func (p *stageProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, start, end time.Time) error {
	p.mm.SetMocksWithWindow(f, u, start, end)
	return nil
}

func (p *stageProxy) SetScopeMocks(_ context.Context, f, u []*models.Mock, open bool) error {
	p.mm.SetScopeMocks(f, u, open)
	return nil
}

func (p *stageProxy) BeginTestSet(_ context.Context, first time.Time, ws []models.TestWindow, f, u []*models.Mock) error {
	p.mm.BeginTestSet(first, ws, f, u)
	return nil
}

func (p *stageProxy) StagePool(_ context.Context, f, u []*models.Mock) error {
	p.mm.StagePool(f, u)
	return nil
}

func (p *stageProxy) GetPersistentConsumed() map[string]models.MockState {
	return p.mm.GetPersistentConsumed()
}

func (p *stageProxy) FirstTestWindowStart() time.Time { return p.mm.FirstTestWindowStart() }
func (p *stageProxy) SetMappedUniverse([]string)      {}

// stageMock is a per-test HTTP mock: GET path, answered with its own name.
// It is classified per-test as a "mocks"-tagged mock is with
// KEPLOY_STRICT_MOCK_WINDOW set (with the default settings the lax rule files
// HTTP data mocks in the reusable tier, which a test's mapping does not
// narrow; models.Mock.DeriveLifetime). The HTTP parser stands in for any
// parser that reads the per-test and reusable tiers.
func stageMock(name, path string, at time.Time) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://api" + path, ProtoMajor: 1, ProtoMinor: 1},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: name, Header: map[string]string{"Content-Length": "0"}},
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
}

func stageSessionMock(name, path string) *models.Mock {
	m := stageMock(name, path, stageT0.Add(-time.Minute))
	m.Spec.Metadata = map[string]string{"type": "config"}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	return m
}

// newStageReplay is `keploy mock replay` up to the release of the app
// (pkg/service/mock/replay.go steps 4-6): the mocks stored, the per-test
// table, and the set staged.
func newStageReplay(t *testing.T, perTest, session []*models.Mock, table map[string][]string) (*Agent, *stageProxy) {
	t.Helper()
	t.Setenv("KEPLOY_PROVEN", "")
	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	p := &stageProxy{mm: mm}
	cfg := &config.Config{}
	cfg.Agent.Mode = models.MODE_TEST
	a := New(zap.NewNop(), nil, p, nil, nil, cfg)
	ctx := context.Background()
	if err := a.StoreMocks(ctx, perTest, session); err != nil {
		t.Fatal(err)
	}
	if err := a.SetScopeTable(ctx, table); err != nil {
		t.Fatal(err)
	}
	if err := a.BeginTestSet(ctx, models.TestSetBegin{}); err != nil {
		t.Fatal(err)
	}
	return a, p
}

func scopeBegin(t *testing.T, a *Agent, name string) {
	t.Helper()
	if err := a.BeginScope(context.Background(), name, 0); err != nil {
		t.Fatal(err)
	}
}

func scopeEnd(t *testing.T, a *Agent, name string) {
	t.Helper()
	if err := a.EndScope(context.Background(), name, 0); err != nil {
		t.Fatal(err)
	}
}

// callHTTP sends GET path through the real HTTP parser. It returns the
// recording that answered it, or "" when none did, and whether the parser
// handed the proxy a miss to report (with the switch off the proxy reports
// what MockOutgoing returns as models.ErrNoMockMatched).
func callHTTP(t *testing.T, mm *proxyPkg.MockManager, path string) (string, bool) {
	t.Helper()
	app, agentEnd := net.Pipe()
	// No deadline: a hang is the test binary's -timeout to report.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		returned <- httpparser.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		agentEnd.Close()
	}()
	if _, err := app.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	answer := ""
	if resp, err := http.ReadResponse(bufio.NewReader(app), nil); err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			answer = string(body)
		}
	}
	app.Close()
	return answer, errors.Is(<-returned, models.ErrNoMockMatched)
}

// answer is the recording that answered GET path, or "" when the call
// missed.
func answer(t *testing.T, mm *proxyPkg.MockManager, path string) string {
	t.Helper()
	got, missed := callHTTP(t, mm, path)
	if missed {
		return ""
	}
	return got
}

// expectMiss fails the test unless GET path misses and the miss is reported.
func expectMiss(t *testing.T, mm *proxyPkg.MockManager, path, what string) {
	t.Helper()
	got, missed := callHTTP(t, mm, path)
	if got != "" || !missed {
		t.Fatalf("%s: answered by %q, miss reported %v; want a reported miss", what, got, missed)
	}
}

// readable is every mock name a parser can read now, in any tier.
func readable(t *testing.T, mm *proxyPkg.MockManager) []string {
	t.Helper()
	seen := map[string]struct{}{}
	for _, read := range []func() ([]*models.Mock, error){mm.GetPerTestMocksInWindow, mm.GetSessionMocks, mm.GetUnFilteredMocks, mm.GetStartupMocks} {
		ms, err := read()
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			seen[m.Name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func hasName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// twoTests is a set where TestA and TestB each recorded GET /q, with
// different answers, a poller's GET /poll was recorded between them (in no
// test), and a config call is reusable.
func twoTests(t *testing.T) (*Agent, *stageProxy) {
	return newStageReplay(t,
		[]*models.Mock{
			stageMock("mock-a", "/q", stageT0),
			stageMock("mock-poll", "/poll", stageT0.Add(time.Second)),
			stageMock("mock-b", "/q", stageT0.Add(3*time.Second)),
		},
		[]*models.Mock{stageSessionMock("cfg", "/cfg")},
		map[string][]string{"TestA": {"mock-a"}, "TestB": {"mock-b"}})
}

// A test begun without a worker pid opens a test window over its own mocks:
// tier-aware parsers (Postgres v3, Mongo v2) route its calls to the test's own
// tier and report a miss as one inside a test. Before the fix the begin
// re-staged the pool, which opens no window, so they answered from
// the startup tier a connection had indexed earlier: another test's recording,
// with no miss.
func TestScopeBeginOpensTheTestsWindow(t *testing.T) {
	a, p := twoTests(t)
	scopeBegin(t, a, "TestB")
	if snap := p.mm.WindowSnapshot(); !snap.Active || !snap.FirstTestFired {
		t.Fatalf("TestB's begin left the window %+v; want a test window open, so its calls are routed to its own tier", snap)
	}
	per, _ := p.mm.GetPerTestMocksInWindow()
	if got := namesOf(per); len(got) != 1 || got[0] != "mock-b" {
		t.Fatalf("TestB's tier holds %v; want its own mock only, [mock-b]", got)
	}
	for _, other := range []string{"mock-a", "mock-poll"} {
		if hasName(readable(t, p.mm), other) {
			t.Fatalf("%s, not TestB's, is readable during TestB: %v", other, readable(t, p.mm))
		}
	}
	if got := answer(t, p.mm, "/q"); got != "mock-b" {
		t.Fatalf("TestB's GET /q was answered by %q, want its own recording mock-b", got)
	}
	scopeEnd(t, a, "TestB")
	if snap := p.mm.WindowSnapshot(); snap.Active || snap.FirstTestFired {
		t.Fatalf("TestB's end left the window %+v; want it closed, with calls between tests routed as before the first test", snap)
	}
}

// A scope's begin stages the test's own recording, which its mapping names
// (ExactMapping), with no window. A poll placed at its answer (models.PlaceOf) is staged for the test
// whose mapping names it alone: before, every one of the recording was
// staged as each test's own, the later tests' among them.
func TestScopeBeginStagesAPollPlacedAtItsAnswerOnlyForTheTestItsMappingNames(t *testing.T) {
	t.Cleanup(models.RegisterAnswerPlaced(models.HTTP, func(m *models.Mock) bool { return m.Spec.Metadata["poll"] == "yes" }))
	poll := func(name string, req, res time.Duration) *models.Mock {
		m := stageMock(name, "/poll", stageT0.Add(req))
		m.Spec.Metadata["poll"] = "yes"
		m.Spec.ResTimestampMock = stageT0.Add(res)
		return m
	}
	a, p := newStageReplay(t,
		[]*models.Mock{
			stageMock("mock-a", "/q", stageT0),
			poll("poll-a", 10*time.Millisecond, 2*time.Second),
			stageMock("mock-b", "/q", stageT0.Add(3*time.Second)),
			poll("poll-b", 3010*time.Millisecond, 4*time.Second),
		},
		[]*models.Mock{stageSessionMock("cfg", "/cfg")},
		map[string][]string{"TestA": {"mock-a", "poll-a"}, "TestB": {"mock-b", "poll-b"}})
	for _, tc := range []struct {
		test string
		want []string
	}{{"TestA", []string{"mock-a", "poll-a"}}, {"TestB", []string{"mock-b", "poll-b"}}} {
		scopeBegin(t, a, tc.test)
		per, err := p.mm.GetPerTestMocksInWindow()
		if err != nil {
			t.Fatal(err)
		}
		got := namesOf(per)
		sort.Strings(got)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s's tier holds %v; want its own recording, %v", tc.test, got, tc.want)
		}
		scopeEnd(t, a, tc.test)
	}
}

// A call in a test whose only recording was made outside every test (a
// background poller recorded between tests) misses in the test, and the miss
// is reported; between tests it is answered, as the whole pool is served
// there. No call in a test is answered with a recording the test did not
// make. This pins behaviour that already held for this HTTP parser, which
// reads its tiers live (it passes before the fix); the tier-aware parsers'
// regressions (Postgres v3, Mongo v2) are tested in keploy/integrations.
func TestScopeCallRecordedOutsideEveryTestMissesInATest(t *testing.T) {
	a, p := twoTests(t)
	scopeBegin(t, a, "TestA")
	expectMiss(t, p.mm, "/poll", "the poller's GET /poll during TestA")
	scopeEnd(t, a, "TestA")
	if got := answer(t, p.mm, "/poll"); got != "mock-poll" {
		t.Fatalf("the poller's GET /poll between tests was answered by %q, want mock-poll", got)
	}
}

// A test's extra call misses, reported in the test, instead of taking the
// next test's recording, and the next test is then served its own. Like the
// test above, this pins behaviour the HTTP parser already had; Postgres v3's
// regression (PG2) is tested in keploy/integrations.
func TestScopeExtraCallMissesInItsTest(t *testing.T) {
	a, p := twoTests(t)
	scopeBegin(t, a, "TestA")
	if got := answer(t, p.mm, "/q"); got != "mock-a" {
		t.Fatalf("TestA's GET /q was answered by %q, want mock-a", got)
	}
	expectMiss(t, p.mm, "/q", "TestA's extra GET /q")
	scopeEnd(t, a, "TestA")
	scopeBegin(t, a, "TestB")
	if got := answer(t, p.mm, "/q"); got != "mock-b" {
		t.Fatalf("TestB's GET /q was answered by %q, want mock-b", got)
	}
}

// A test's end does not re-arm what the test consumed. Before the fix the end
// re-staged the whole stored pool, so a call after the end (a background job,
// or the next test's setup before its begin) took the consumed recording
// again.
func TestScopeEndDoesNotReArmAConsumedMock(t *testing.T) {
	a, p := newStageReplay(t,
		[]*models.Mock{stageMock("mock-a", "/q", stageT0)},
		nil,
		map[string][]string{"TestA": {"mock-a"}})
	scopeBegin(t, a, "TestA")
	if got := answer(t, p.mm, "/q"); got != "mock-a" {
		t.Fatalf("TestA's GET /q was answered by %q, want mock-a", got)
	}
	scopeEnd(t, a, "TestA")
	expectMiss(t, p.mm, "/q", "a GET /q after TestA's end, whose only recording TestA consumed")
}

// A parser that records its consume by MarkMockAsUsed (Postgres v3 consumes in
// its own index) consumes a test's mapped mock as surely: it is not re-armed
// by the test's end either.
func TestScopeEndDoesNotReArmAMappedMockServedOnce(t *testing.T) {
	a, p := twoTests(t)
	scopeBegin(t, a, "TestA")
	per, _ := p.mm.GetPerTestMocksInWindow()
	for _, m := range per {
		if m.Name == "mock-a" {
			p.mm.MarkMockAsUsed(*m)
		}
	}
	scopeEnd(t, a, "TestA")
	if hasName(readable(t, p.mm), "mock-a") {
		t.Fatalf("TestA's served recording is back after its end: %v", readable(t, p.mm))
	}
	// What no test consumed stays, as between tests always served it.
	for _, want := range []string{"mock-b", "mock-poll", "cfg"} {
		if !hasName(readable(t, p.mm), want) {
			t.Fatalf("%s is gone after TestA's end: %v", want, readable(t, p.mm))
		}
	}
}

// Between tests the pool is served as it always was: a recording of no test
// consumed between tests is served again after the next test, as a pool
// staging re-arms its startup tier.
func TestScopeBetweenTestsServesTheMocksOfNoTestAsBefore(t *testing.T) {
	a, p := twoTests(t)
	for _, test := range []string{"TestA", "TestB"} {
		scopeBegin(t, a, test)
		scopeEnd(t, a, test)
		if got := answer(t, p.mm, "/poll"); got != "mock-poll" {
			t.Fatalf("GET /poll after %s was answered by %q, want mock-poll", test, got)
		}
	}
}

// A test's mapped mock served before its test ran (at startup, or between
// tests) is served between tests as it always was: only what a test served
// is left out of the pool after it. Its own test is staged it afresh.
func TestScopeBetweenTestsKeepsWhatWasServedOutsideATest(t *testing.T) {
	a, p := newStageReplay(t,
		[]*models.Mock{
			stageMock("mock-a", "/a", stageT0),
			stageMock("mock-b", "/b", stageT0.Add(3*time.Second)),
			stageMock("mock-b2", "/b", stageT0.Add(4*time.Second)),
		},
		nil,
		map[string][]string{"TestA": {"mock-a"}, "TestB": {"mock-b", "mock-b2"}})
	if got := answer(t, p.mm, "/b"); got != "mock-b" {
		t.Fatalf("GET /b at startup was answered by %q, want mock-b", got)
	}
	scopeBegin(t, a, "TestA")
	scopeEnd(t, a, "TestA")
	if got := answer(t, p.mm, "/b"); got != "mock-b" {
		t.Fatalf("GET /b between tests was answered by %q, want mock-b, served again after the staging as it always was", got)
	}
	scopeBegin(t, a, "TestB")
	if got := answer(t, p.mm, "/b"); got != "mock-b" {
		t.Fatalf("TestB's GET /b was answered by %q, want its first recording mock-b, staged afresh", got)
	}
}

// A test recorded with no call is served no other test's recording: it gets a
// test window over no per-test mock of its own, so its call misses, and the
// miss is reported. Before the fix its begin left the whole pool armed with no
// window, and another test's recording answered it.
func TestScopeTestRecordedWithNoCallIsServedNoOtherTestsRecording(t *testing.T) {
	a, p := newStageReplay(t,
		[]*models.Mock{stageMock("mock-a", "/q", stageT0)},
		[]*models.Mock{stageSessionMock("cfg", "/cfg")},
		map[string][]string{"TestA": {"mock-a"}, "TestC": {}})
	scopeBegin(t, a, "TestC")
	if snap := p.mm.WindowSnapshot(); !snap.Active || !snap.FirstTestFired {
		t.Fatalf("TestC's begin left the window %+v; want a test window open", snap)
	}
	expectMiss(t, p.mm, "/q", "TestC's GET /q, whose only recording is TestA's")
	if got := answer(t, p.mm, "/cfg"); got != "cfg" {
		t.Fatalf("TestC's GET /cfg was answered by %q, want the reusable cfg", got)
	}
	scopeEnd(t, a, "TestC")
	if got := answer(t, p.mm, "/q"); got != "mock-a" {
		t.Fatalf("GET /q between tests was answered by %q, want mock-a", got)
	}
}

// Each run of a test is served its whole recording: a test run again (a retry,
// go test -count=2) is staged its own mocks afresh.
func TestScopeRerunIsServedItsRecordingAgain(t *testing.T) {
	a, p := twoTests(t)
	for run := 1; run <= 2; run++ {
		scopeBegin(t, a, "TestA")
		if got := answer(t, p.mm, "/q"); got != "mock-a" {
			t.Fatalf("run %d of TestA: GET /q was answered by %q, want mock-a", run, got)
		}
		scopeEnd(t, a, "TestA")
	}
}

// recordingProxy records what each staging hands the proxy.
type recordingProxy struct {
	coreAgent.Proxy
	windowed [][2][]string
	scoped   [][2][]string
	begun    [][2][]string
	pools    [][2][]string
	firsts   []time.Time
	opens    []bool
	starts   []time.Time
}

func (p *recordingProxy) BeginTestSet(_ context.Context, first time.Time, _ []models.TestWindow, f, u []*models.Mock) error {
	p.begun = append(p.begun, [2][]string{namesOf(f), namesOf(u)})
	p.firsts = append(p.firsts, first)
	return nil
}

func (p *recordingProxy) StagePool(_ context.Context, f, u []*models.Mock) error {
	p.pools = append(p.pools, [2][]string{namesOf(f), namesOf(u)})
	return nil
}

func namesOf(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func (p *recordingProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, start, _ time.Time) error {
	p.windowed = append(p.windowed, [2][]string{namesOf(f), namesOf(u)})
	p.starts = append(p.starts, start)
	return nil
}

func (p *recordingProxy) SetScopeMocks(_ context.Context, f, u []*models.Mock, open bool) error {
	p.scoped = append(p.scoped, [2][]string{namesOf(f), namesOf(u)})
	p.opens = append(p.opens, open)
	return nil
}

func (p *recordingProxy) SetMappedUniverse([]string) {}

// A test's end hands the proxy exactly the pool the end staged before the
// fix, the set's whole pool in the same order: what the tests served is left
// out by the proxy (MockManager.SetScopeMocks), not by the agent.
func TestScopeEndStagesThePoolTheEndAlwaysStaged(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	p := &recordingProxy{}
	cfg := &config.Config{}
	cfg.Agent.Mode = models.MODE_TEST
	a := New(zap.NewNop(), nil, p, nil, nil, cfg)
	ctx := context.Background()
	perTest := []*models.Mock{
		stageMock("mock-b", "/q", stageT0.Add(3*time.Second)),
		stageMock("mock-a", "/q", stageT0),
		stageMock("mock-poll", "/poll", stageT0.Add(time.Second)),
	}
	if err := a.StoreMocks(ctx, perTest, []*models.Mock{stageSessionMock("cfg", "/cfg")}); err != nil {
		t.Fatal(err)
	}
	if err := a.SetScopeTable(ctx, map[string][]string{"TestA": {"mock-a"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.BeginTestSet(ctx, models.TestSetBegin{}); err != nil {
		t.Fatal(err)
	}
	scopeEnd(t, a, "TestA")
	if len(p.begun) != 1 || len(p.windowed) != 0 || len(p.scoped) != 1 || p.opens[0] {
		t.Fatalf("stagings: begun %v, windowed %v, scoped %v (open %v); want the set's begin, then one closing scope staging", p.begun, p.windowed, p.scoped, p.opens)
	}
	if got, want := p.scoped[0], p.begun[0]; len(got[0]) != len(want[0]) || len(got[1]) != len(want[1]) {
		t.Fatalf("the end staged %v, want what the set began with, %v", got, want)
	} else {
		for tier := range got {
			for i := range got[tier] {
				if got[tier][i] != want[tier][i] {
					t.Fatalf("the end staged %v, want what the set began with, %v", got, want)
				}
			}
		}
	}
}

// A proxy that cannot stage a scope is staged a pool between tests
// (TestSetStager.StagePool), which opens no window: the test's mapped mocks
// only, or none for a test recorded with no call.
func TestScopeWithoutAScopeStagerIsStagedAsBefore(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	rp := &legacyProxy{}
	cfg := &config.Config{}
	cfg.Agent.Mode = models.MODE_TEST
	a := New(zap.NewNop(), nil, rp, nil, nil, cfg)
	ctx := context.Background()
	if err := a.StoreMocks(ctx, []*models.Mock{stageMock("mock-a", "/q", stageT0), stageMock("mock-poll", "/poll", stageT0.Add(time.Second))}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SetScopeTable(ctx, map[string][]string{"TestA": {"mock-a"}, "TestC": {}}); err != nil {
		t.Fatal(err)
	}
	scopeBegin(t, a, "TestA")
	scopeEnd(t, a, "TestA")
	scopeBegin(t, a, "TestC")
	if len(rp.pools) != 3 || len(rp.windowed) != 0 {
		t.Fatalf("staged %d pools and %d windows; want three pools", len(rp.pools), len(rp.windowed))
	}
	if got := rp.pools[0][0]; len(got) != 1 || got[0] != "mock-a" {
		t.Fatalf("the begin staged %v; want the test's mapped mocks only, as before", got)
	}
	// A test recorded with no call is staged no per-test mock: before the fix
	// its begin left the whole pool armed.
	if got := rp.pools[2][0]; len(got) != 0 {
		t.Fatalf("TestC's begin staged %v; want no per-test mock", got)
	}
}

// workerProxy records what a worker-scoped (pid>0) begin and end ask of the
// proxy, besides the stagings.
type workerProxy struct {
	recordingProxy
	workerScopes map[uint32][]string
	cleared      []uint32
}

func (p *workerProxy) SetWorkerScope(pid uint32, names []string) {
	if p.workerScopes == nil {
		p.workerScopes = map[uint32][]string{}
	}
	p.workerScopes[pid] = names
}

func (p *workerProxy) ClearWorkerScope(pid uint32) { p.cleared = append(p.cleared, pid) }

// A worker that reports its pid is scoped on its own view, with the switch off
// as before: its begin and end stage nothing on the global pool, whether its
// test recorded calls or none.
func TestScopeWorkerScopeStagesNothingGlobal(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	p := &workerProxy{}
	cfg := &config.Config{}
	cfg.Agent.Mode = models.MODE_TEST
	a := New(zap.NewNop(), nil, p, nil, nil, cfg)
	ctx := context.Background()
	if err := a.StoreMocks(ctx, []*models.Mock{stageMock("mock-a", "/q", stageT0)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SetScopeTable(ctx, map[string][]string{"TestA": {"mock-a"}, "TestC": {}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []string{"TestA", "TestC"} {
		if err := a.BeginScope(ctx, test, 4242); err != nil {
			t.Fatal(err)
		}
		if err := a.EndScope(ctx, test, 4242); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.scoped) != 0 || len(p.windowed) != 0 {
		t.Fatalf("a worker's begins and ends staged scopes %v and windows %v on the global pool; want none", p.scoped, p.windowed)
	}
	if got := p.workerScopes[4242]; len(got) != 1 || got[0] != "mock-a" {
		t.Fatalf("the worker was scoped to %v, want TestA's [mock-a]", p.workerScopes)
	}
	if len(p.cleared) != 2 {
		t.Fatalf("the worker's view was cleared %d times, want once per end", len(p.cleared))
	}
}

// legacyProxy stages through WindowedProxy only.
type legacyProxy struct {
	coreAgent.Proxy
	windowed [][2][]string
	pools    [][2][]string
	starts   []time.Time
}

func (p *legacyProxy) BeginTestSet(context.Context, time.Time, []models.TestWindow, []*models.Mock, []*models.Mock) error {
	return nil
}

func (p *legacyProxy) StagePool(_ context.Context, f, u []*models.Mock) error {
	p.pools = append(p.pools, [2][]string{namesOf(f), namesOf(u)})
	return nil
}

func (p *legacyProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, start, _ time.Time) error {
	p.windowed = append(p.windowed, [2][]string{namesOf(f), namesOf(u)})
	p.starts = append(p.starts, start)
	return nil
}

func (p *legacyProxy) SetMappedUniverse([]string) {}

// exactProxy is a recordingProxy that also serves exactly, as the proven
// switch's begins and ends ask of the proxy.
type exactProxy struct {
	recordingProxy
	exactTests []string
	exactPools int
}

func (p *exactProxy) ServeExactTest(test string, _ []*models.Mock) error {
	p.exactTests = append(p.exactTests, test)
	return nil
}

func (p *exactProxy) ServeExactPool(string, []string) error {
	p.exactPools++
	return nil
}

func (p *exactProxy) ExactTestOutcome() (string, []string, []string, bool) {
	return "", nil, nil, false
}

// Under the proven switch a begin and an end are served exactly, as they are
// without this change: the scope staging is the switch-off path only.
func TestScopeStagingIsSwitchOffOnly(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p := &exactProxy{}
	cfg := &config.Config{}
	cfg.Agent.Mode = models.MODE_TEST
	a := New(zap.NewNop(), nil, p, nil, nil, cfg)
	ctx := context.Background()
	if err := a.StoreMocks(ctx, []*models.Mock{stageMock("mock-a", "/q", stageT0)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SetScopeTable(ctx, map[string][]string{"TestA": {"mock-a"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.BeginScope(ctx, "TestA", 0); err != nil {
		t.Fatal(err)
	}
	pools := p.exactPools
	if err := a.EndScope(ctx, "TestA", 0); err != nil {
		t.Fatal(err)
	}
	if len(p.scoped) != 0 {
		t.Fatalf("under the switch a begin or an end staged a scope: %v", p.scoped)
	}
	if len(p.exactTests) != 1 || p.exactTests[0] != "TestA" || p.exactPools != pools+1 {
		t.Fatalf("under the switch the begin and the end served exactly tests %v, and the end the pool %d times; want TestA, then its pool once", p.exactTests, p.exactPools-pools)
	}
	if len(p.begun) != 1 || len(p.windowed) != 0 {
		t.Fatalf("under the switch the recording was begun %d times and staged %d windows; want begun once, whole, for exact serving", len(p.begun), len(p.windowed))
	}
}

// With the per-test mocks in the agent's on-disk store (strict mock window), a
// begin reads only what it serves: the test's own mocks.
func TestScopeBeginReadsOnlyItsMocksFromDisk(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	p := &recordingProxy{}
	a := diskAgent(t, p,
		stageMock("mock-a", "/q", stageT0),
		stageMock("mock-poll", "/poll", stageT0.Add(time.Second)),
		stageMock("mock-b", "/q", stageT0.Add(3*time.Second)))
	a.config = &config.Config{}
	a.config.Agent.Mode = models.MODE_TEST
	core, logs := observer.New(zap.DebugLevel)
	a.logger = zap.New(core)
	if err := a.SetScopeTable(context.Background(), map[string][]string{"TestA": {"mock-a"}, "TestB": {"mock-b"}}); err != nil {
		t.Fatal(err)
	}
	scopeBegin(t, a, "TestA")
	if len(p.scoped) != 1 || !p.opens[0] {
		t.Fatalf("the begin staged %v (open %v); want one open scope staging", p.scoped, p.opens)
	}
	if got := p.scoped[0][0]; len(got) != 1 || got[0] != "mock-a" {
		t.Fatalf("the begin staged %v from disk; want its own mock, [mock-a]", got)
	}
	// What was read, not only what was staged: the mapping filter would cut a
	// whole-store read down to [mock-a] too.
	reads := logs.FilterMessage("agent mock residency: loaded per-test set from disk").All()
	if len(reads) != 1 {
		t.Fatalf("the begin read the on-disk store %d times, want once", len(reads))
	}
	if f := reads[0].ContextMap(); f["mode"] != "mapping" || f["loadedFromDisk"] != int64(1) {
		t.Fatalf("the begin read %v from disk; want its own mock only (mode mapping, 1 loaded)", f)
	}
}
