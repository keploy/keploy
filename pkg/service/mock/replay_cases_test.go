package mock

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestRequestKeyBlanksIdsInThePath(t *testing.T) {
	require.Equal(t, "GET /orders/{id}", requestKey("get", "http://localhost:8080/orders/6ab66046dd177a16b90732bd"))
	require.Equal(t, "POST /orders/{id}/cancel", requestKey("POST", "/orders/42/cancel?x=1"))
	require.Equal(t, "GET /users/{id}", requestKey("GET", "/users/3f2504e0-4f89-11d3-9a0c-0305e82c3301"))
	require.Equal(t, "GET /orders", requestKey("GET", "http://localhost:8080/orders"))
}

func httpCase(name, method, url string, status int, body string, at time.Time) *models.TestCase {
	return &models.TestCase{
		Name:     name,
		Kind:     models.HTTP,
		HTTPReq:  models.HTTPReq{Method: models.Method(method), URL: url, Timestamp: at},
		HTTPResp: models.HTTPResp{StatusCode: status, Body: body, Timestamp: at.Add(time.Millisecond)},
	}
}

var replayWindows = []models.ScopeWindow{
	{Name: "pkg.TestA", Start: runnerT0, End: runnerT0.Add(100 * time.Millisecond)},
	{Name: "pkg.TestB", Start: runnerT0.Add(200 * time.Millisecond), End: runnerT0.Add(300 * time.Millisecond)},
}

func TestPairCasesMatchesByCallWithinTheTestAndReportsTheUnseen(t *testing.T) {
	recorded := map[string][]*models.TestCase{
		"pkg.TestA": {httpCase("post-orders-1", "POST", "/orders", 201, `{"id":"a"}`, runnerT0), httpCase("get-orders-by-id-1", "GET", "/orders/aaaaaaaaaaaaaaaaaaaaaaaa", 200, `{"id":"a"}`, runnerT0)},
		"pkg.TestB": {httpCase("get-orders-1", "GET", "/orders", 200, `[]`, runnerT0)},
	}
	actual := []*models.TestCase{
		httpCase("", "GET", "/orders/bbbbbbbbbbbbbbbbbbbbbbbb", 200, `{"id":"b"}`, runnerT0.Add(20*time.Millisecond)),
		httpCase("", "POST", "/orders", 201, `{"id":"b"}`, runnerT0.Add(10*time.Millisecond)),
		httpCase("", "GET", "/orders", 500, `boom`, runnerT0.Add(250*time.Millisecond)),
	}
	compare := func(tc *models.TestCase, resp *models.HTTPResp) (bool, *models.Result) {
		return tc.HTTPResp.StatusCode == resp.StatusCode, &models.Result{StatusCode: models.IntResult{Expected: tc.HTTPResp.StatusCode, Actual: resp.StatusCode}}
	}
	out := pairCases(replayWindows, recorded, actual, compare)
	require.Len(t, out, 3)
	require.Equal(t, "post-orders-1", out[0].Case.Name)
	require.True(t, out[0].Passed)
	require.Equal(t, `{"id":"b"}`, out[0].Actual.Body, "the actual answer is kept next to the recorded one")
	require.Equal(t, "get-orders-by-id-1", out[1].Case.Name)
	require.True(t, out[1].Passed, "ids in the path do not stop a request from being recognised")
	require.Equal(t, "get-orders-1", out[2].Case.Name)
	require.False(t, out[2].Passed)
	require.Equal(t, 500, out[2].Result.StatusCode.Actual)

	unseen := pairCases(replayWindows, recorded, nil, compare)
	require.Len(t, unseen, 3)
	require.Nil(t, unseen[0].Actual)
	require.False(t, unseen[0].Passed)
}

func TestAttributeMocksUsesTheTestRunningAtTheTime(t *testing.T) {
	expected := map[string][]models.MockEntry{"pkg.TestA": {{Name: "mock-0"}}, "pkg.TestB": {{Name: "mock-1"}}}
	consumed := []models.MockState{
		{Name: "mock-0", Kind: models.HTTP, Timestamp: runnerT0.Add(10 * time.Millisecond).UnixNano()},
		{Name: "mock-1", Kind: models.Mongo, Timestamp: runnerT0.Add(210 * time.Millisecond).UnixNano()},
		{Name: "mock-9", Kind: models.HTTP}, // an older agent leaves the time out
	}
	misses := []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /price/SKU-9", At: runnerT0.Add(220 * time.Millisecond)}, {Protocol: "DNS"}}
	out := attributeMocks(replayWindows, expected, consumed, misses)
	require.Len(t, out, 2)
	require.Equal(t, "pkg.TestA", out[0].Flow)
	require.Equal(t, []models.MockEntry{{Name: "mock-0"}}, out[0].Expected)
	require.Equal(t, "mock-0", out[0].Consumed[0].Name)
	require.Empty(t, out[0].Missed)
	require.Equal(t, "pkg.TestB", out[1].Flow)
	require.Equal(t, "mock-1", out[1].Consumed[0].Name)
	require.Equal(t, "GET /price/SKU-9", out[1].Missed[0].ActualSummary)
	require.Nil(t, attributeMocks(replayWindows, nil, nil, nil))
}

// A replay with requests on compares what the app answered with the recorded cases and says which mocks each test used.
func TestReplayOutcomeCarriesTheCasesAndTheMocksPerTest(t *testing.T) {
	instr := newRunnerInstr(t, sequentialMarks()...)
	instr.incoming = []*models.TestCase{httpCase("", "POST", "http://localhost:8080/orders", 201, `{"id":"new"}`, runnerT0.Add(5*time.Millisecond))}
	instr.consumedMocks = []models.MockState{{Name: "mock-0", Kind: models.HTTP, Timestamp: runnerT0.Add(6 * time.Millisecond).UnixNano()}}
	instr.mockErrors = []models.UnmatchedCall{{Protocol: "Mongo", ActualSummary: "insert shop.orders", At: runnerT0.Add(7 * time.Millisecond)}}
	dir := t.TempDir()
	mapDB := mapdb.New(zap.NewNop(), dir, "")
	require.NoError(t, mapDB.UpsertBatch(context.Background(), "set", map[string][]models.MockEntry{"orders/e2e.TestA": {{Name: "mock-0"}}}))
	require.NoError(t, mapDB.UpsertCases(context.Background(), "set", map[string]models.MappedTestCase{"orders/e2e.TestA": {Cases: []string{"post-orders-1"}}}, nil, nil))
	recorded := httpCase("post-orders-1", "POST", "http://localhost:8080/orders", 201, `{"id":"old"}`, runnerT0)
	db := &memTestDB{existing: []*models.TestCase{recorded}}

	var got ReplayOutcome
	RegisterReplayOutcomeReporter(func(_ context.Context, o ReplayOutcome) { got = o })
	t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })
	cfg := instrConfig(instr.composeInstr, utils.Native, "./shop.test -test.v")
	cfg.Path = dir
	withRequests(cfg)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	svc := New(zap.NewNop(), instr, stubMockDB{}, mapDB, nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(db)
	require.NoError(t, svc.Replay(context.Background()))

	opts, ok := instr.setup()
	require.True(t, ok)
	require.True(t, opts.RecordRequests, "the agent keeps the ingress hooks on so the app's answers can be compared")
	require.Len(t, got.Cases, 1)
	c := got.Cases[0]
	require.Equal(t, "orders/e2e.TestA", c.Flow)
	require.Equal(t, "post-orders-1", c.Case.Name)
	require.NotNil(t, c.Actual)
	require.Equal(t, `{"id":"new"}`, c.Actual.Body)
	require.False(t, c.Passed, "the id changed, and nothing marks it as noise")
	require.NotNil(t, c.Result)
	require.Len(t, got.Mocks, 1)
	require.Equal(t, "orders/e2e.TestA", got.Mocks[0].Flow)
	require.Equal(t, "mock-0", got.Mocks[0].Consumed[0].Name)
	require.Equal(t, "insert shop.orders", got.Mocks[0].Missed[0].ActualSummary)
}

var _ = config.Config{}

func TestPairCasesAndAttributeMocksPickTheInnermostWindow(t *testing.T) {
	windows := []models.ScopeWindow{
		{Name: "pkg.TestA", Start: runnerT0, End: runnerT0.Add(100 * time.Millisecond)},
		{Name: "pkg.TestA/create", Start: runnerT0.Add(10 * time.Millisecond), End: runnerT0.Add(40 * time.Millisecond)},
		{Name: "pkg.TestA/create/nested", Start: runnerT0.Add(20 * time.Millisecond), End: runnerT0.Add(30 * time.Millisecond)},
	}
	recorded := map[string][]*models.TestCase{
		"pkg.TestA":               {httpCase("test-3", "GET", "/orders", 200, `[]`, runnerT0)},
		"pkg.TestA/create":        {httpCase("test-1", "POST", "/orders", 201, `{}`, runnerT0)},
		"pkg.TestA/create/nested": {httpCase("test-2", "GET", "/orders/1", 200, `{}`, runnerT0)},
	}
	actual := []*models.TestCase{
		httpCase("", "POST", "/orders", 201, `{}`, runnerT0.Add(15*time.Millisecond)),
		httpCase("", "GET", "/orders/1", 200, `{}`, runnerT0.Add(25*time.Millisecond)),
		httpCase("", "GET", "/orders", 200, `[]`, runnerT0.Add(60*time.Millisecond)),
	}
	compare := func(tc *models.TestCase, resp *models.HTTPResp) (bool, *models.Result) {
		return tc.HTTPResp.StatusCode == resp.StatusCode, &models.Result{}
	}
	for _, o := range pairCases(windows, recorded, actual, compare) {
		require.NotNil(t, o.Actual, "%s was not paired in its own window", o.Case.Name)
		require.True(t, o.Passed, o.Case.Name)
	}

	expected := map[string][]models.MockEntry{"pkg.TestA/create/nested": {{Name: "mock-0"}}}
	consumed := []models.MockState{{Name: "mock-0", Kind: models.HTTP, Timestamp: runnerT0.Add(25 * time.Millisecond).UnixNano()}}
	out := attributeMocks(windows, expected, consumed, nil)
	require.Len(t, out, 1)
	require.Equal(t, "pkg.TestA/create/nested", out[0].Flow)
	require.Equal(t, "mock-0", out[0].Consumed[0].Name)
}

type lateIncoming struct {
	*runnerInstr
	after time.Duration
	tc    *models.TestCase
}

func (l lateIncoming) GetIncoming(ctx context.Context, _ models.IncomingOptions) (<-chan *models.TestCase, error) {
	out := make(chan *models.TestCase)
	go func() {
		defer close(out)
		select {
		case <-time.After(l.after):
			out <- l.tc
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()
	return out, nil
}

func TestReplayWaitsForTheLastRequestBeforeComparing(t *testing.T) {
	instr := newRunnerInstr(t, sequentialMarks()...)
	late := lateIncoming{runnerInstr: instr, after: 200 * time.Millisecond, tc: httpCase("", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0.Add(5*time.Millisecond))}
	dir := t.TempDir()
	mapDB := mapdb.New(zap.NewNop(), dir, "")
	require.NoError(t, mapDB.UpsertCases(context.Background(), "set", map[string]models.MappedTestCase{"orders/e2e.TestA": {Cases: []string{"post-orders-1"}}}, nil, nil))
	db := &memTestDB{existing: []*models.TestCase{httpCase("post-orders-1", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0)}}
	var got ReplayOutcome
	RegisterReplayOutcomeReporter(func(_ context.Context, o ReplayOutcome) { got = o })
	t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })
	cfg := instrConfig(instr.composeInstr, utils.Native, "./shop.test -test.v")
	cfg.Path = dir
	withRequests(cfg)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	svc := New(zap.NewNop(), late, stubMockDB{}, mapDB, nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(db)
	require.NoError(t, svc.Replay(context.Background()))
	require.Len(t, got.Cases, 1)
	require.NotNil(t, got.Cases[0].Actual, "the request that arrived after the runner exited is still compared")
	require.True(t, got.Cases[0].Passed)
}

func TestPairCasesDoesNotCompareGRPCAsHTTP(t *testing.T) {
	grpc := &models.TestCase{Name: "grpc-1", Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Timestamp: runnerT0.Add(10 * time.Millisecond)}}
	recorded := map[string][]*models.TestCase{"pkg.TestA": {grpc, httpCase("get-1", "GET", "/orders", 200, `[]`, runnerT0)}}
	actual := []*models.TestCase{
		{Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Timestamp: runnerT0.Add(20 * time.Millisecond)}},
		httpCase("", "GET", "/orders", 200, `[]`, runnerT0.Add(30*time.Millisecond)),
	}
	calls := 0
	out := pairCases(replayWindows, recorded, actual, func(*models.TestCase, *models.HTTPResp) (bool, *models.Result) { calls++; return true, nil })
	require.Len(t, out, 2)
	require.True(t, out[0].Skipped)
	require.False(t, out[0].Passed)
	require.Nil(t, out[0].Actual)
	require.False(t, out[1].Skipped)
	require.True(t, out[1].Passed)
	require.Equal(t, 1, calls)
}

func TestPairCasesFindsSubtestRequestsUnderTheTopLevelFlow(t *testing.T) {
	windows := []models.ScopeWindow{
		mark("e2e/orders.TestTestSuiteValidation/missing_name", time.Second, 2*time.Second),
		mark("e2e/orders.TestTestSuiteValidation/missing_steps", 3*time.Second, 4*time.Second),
		mark("e2e/orders.TestTestSuiteValidation", 0, 5*time.Second),
	}
	at := func(sec float64) time.Time { return runnerT0.Add(time.Duration(sec * float64(time.Second))) }
	flow := "e2e/orders.TestTestSuiteValidation"
	recorded := map[string][]*models.TestCase{flow: {
		httpCase("missing_name-1", "POST", "/testsuite", 400, `{"error":"name"}`, at(1.5)),
		httpCase("missing_steps-1", "POST", "/testsuite", 400, `{"error":"steps"}`, at(3.5)),
	}}
	actual := []*models.TestCase{
		httpCase("", "POST", "/testsuite", 400, `{"error":"name"}`, at(1.5)),
		httpCase("", "POST", "/testsuite", 400, `{"error":"steps"}`, at(3.5)),
	}
	out := pairCases(windows, recorded, actual, func(tc *models.TestCase, resp *models.HTTPResp) (bool, *models.Result) {
		return tc.HTTPResp.Body == resp.Body, nil
	})
	require.Len(t, out, 2)
	for _, o := range out {
		require.NotNil(t, o.Actual, "%s was made in its subtest and must be paired", o.Case.Name)
		require.True(t, o.Passed, o.Case.Name)
	}

	expected := map[string][]models.MockEntry{flow: {{Name: "mock-0"}}}
	consumed := []models.MockState{{Name: "mock-0", Kind: models.Mongo, Timestamp: at(3.6).UnixNano()}}
	mocks := attributeMocks(windows, expected, consumed, []models.UnmatchedCall{{Protocol: "Mongo", At: at(1.6)}})
	require.Len(t, mocks, 1)
	require.Equal(t, flow, mocks[0].Flow)
	require.Len(t, mocks[0].Consumed, 1)
	require.Len(t, mocks[0].Missed, 1)
}

func TestPairCasesPrefersTheSameQueryWhenACallIsNotMade(t *testing.T) {
	recorded := map[string][]*models.TestCase{
		"pkg.TestA": {httpCase("open", "GET", "/items?status=open", 200, `open`, runnerT0), httpCase("closed", "GET", "/items?status=closed", 200, `closed`, runnerT0)},
	}
	actual := []*models.TestCase{httpCase("", "GET", "/items?status=closed", 200, `closed`, runnerT0.Add(10*time.Millisecond))}
	compare := func(tc *models.TestCase, resp *models.HTTPResp) (bool, *models.Result) {
		return tc.HTTPResp.Body == resp.Body, nil
	}
	out := pairCases(replayWindows, recorded, actual, compare)
	require.Len(t, out, 2)
	require.Nil(t, out[0].Actual, "the call that was not made is reported as not made")
	require.True(t, out[1].Passed, "the call that was made is compared with its own recording")
}

func TestFailScanKeepsTheTopLevelTestsTheRunnerFailed(t *testing.T) {
	f := &failScan{}
	for _, chunk := range []string{"=== RUN   TestA\n--- FAIL: Te", "stA (0.00s)\n    --- FAIL: TestA/sub (0.00s)\n--- PASS: TestB (0.00s)\n", "--- FAIL: TestC (1.20s)\n--- FAIL: TestA (0.00s)\nFAIL\n"} {
		_, _ = f.Write([]byte(chunk))
	}
	require.Equal(t, []string{"TestA", "TestC"}, f.list())
}

func TestReportMissesNamesTheTestOnce(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	m := &mockService{logger: zap.New(core)}
	at := runnerT0.Add(5 * time.Millisecond)
	miss := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /price/lamp", Destination: "127.0.0.1:9000", At: at}
	outside := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /tick", Destination: "127.0.0.1:9000", At: runnerT0.Add(time.Hour)}
	misses := []models.UnmatchedCall{miss, miss, outside}
	m.reportMisses(misses, attributeMocks(sequentialMarks(), nil, nil, misses))
	require.Equal(t, 2, logs.Len())
	first := logs.All()[0].ContextMap()
	require.Equal(t, "orders/e2e.TestA", first["test"])
	require.Equal(t, "GET /price/lamp", first["call"])
	require.EqualValues(t, 2, first["times"])
	require.Equal(t, "(outside any test)", logs.All()[1].ContextMap()["test"])
}

// Each test carries the verdict of its latest run — "" when that run reported
// none, never an earlier run's — and a test that recorded no mocks, or that
// the replay gated out, still gets its line.
func TestAttributeMocksCarriesEachTestsLatestVerdict(t *testing.T) {
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Millisecond) }
	windows := []models.ScopeWindow{
		{Name: "pkg.TestA", Start: at(0), End: at(100), Outcome: models.ScopeOutcomeFailed},
		{Name: "pkg.TestA", Start: at(400), End: at(500), Outcome: models.ScopeOutcomePassed},
		{Name: "pkg.TestB", Start: at(200), End: at(300), Outcome: models.ScopeOutcomePassed},
		{Name: "pkg.TestB", Start: at(600), End: at(650)}, // a re-run that reported nothing
		{Name: "pkg.TestPure", Start: at(700), End: at(800), Outcome: models.ScopeOutcomePassed},
		{Name: "pkg.TestGated", Start: at(900), End: at(900), Outcome: models.ScopeOutcomeGated},
	}
	expected := map[string][]models.MockEntry{"pkg.TestA": {{Name: "mock-0"}}, "pkg.TestB": {{Name: "mock-1"}}, "pkg.TestGated": {{Name: "mock-2"}}}
	got := map[string]string{}
	for _, f := range attributeMocks(windows, expected, nil, nil) {
		got[f.Flow] = f.Outcome
	}
	require.Equal(t, map[string]string{
		"pkg.TestA":     models.ScopeOutcomePassed,
		"pkg.TestB":     "",
		"pkg.TestPure":  models.ScopeOutcomePassed,
		"pkg.TestGated": models.ScopeOutcomeGated,
	}, got)
}

func TestTestReceiptsMarkAnUnreportedCount(t *testing.T) {
	flows := []FlowMocks{{
		Flow:     "pkg.TestA",
		Outcome:  models.ScopeOutcomePassed,
		Consumed: []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}},
		Missed:   []models.UnmatchedCall{{Protocol: "HTTP"}},
	}}
	require.Equal(t, []TestReceipt{{Name: "pkg.TestA", Outcome: models.ScopeOutcomePassed, Consumed: 2, Missed: 1}}, testReceipts(flows, true, true))
	require.Equal(t, []TestReceipt{{Name: "pkg.TestA", Outcome: models.ScopeOutcomePassed, Consumed: -1, Missed: 1}}, testReceipts(flows, false, true))
	require.Nil(t, testReceipts(nil, true, true))
}

// gatePusher records the gate a replay pushes.
type gatePusher struct {
	Instrumentation
	pushed bool
	run    []string
	reason string
	err    error
}

func (g *gatePusher) PushScopeGate(_ context.Context, run []string, reason string) error {
	g.pushed, g.run, g.reason = true, run, reason
	return g.err
}

// A replay always tells the agent which tests to run — with no gate source,
// every test (a nil list), so a long-lived agent never keeps an earlier gate.
// The source learns which recording the replay serves.
func TestPushScopeGate(t *testing.T) {
	g := &gatePusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: g, config: &config.Config{}}
	require.Equal(t, requestedGate{}, m.pushScopeGate(context.Background(), "set", "d1"))
	require.True(t, g.pushed)
	require.Nil(t, g.run)

	var asked ScopeGateRequest
	RegisterScopeGateSource(func(_ context.Context, req ScopeGateRequest) ([]string, string) {
		asked = req
		return []string{req.Set + "/proven"}, "not yet proven"
	})
	t.Cleanup(func() { RegisterScopeGateSource(nil) })
	require.Equal(t, requestedGate{run: []string{"set/proven"}, reason: "not yet proven"}, m.pushScopeGate(context.Background(), "set", "d1"))
	require.Equal(t, ScopeGateRequest{Set: "set", MocksDigest: "d1"}, asked)
	require.Equal(t, []string{"set/proven"}, g.run)

	// The user's --run-only wins over the installed policy.
	m.config = &config.Config{}
	m.config.Mock.RunOnly = []string{"TestMine"}
	require.Equal(t, requestedGate{run: []string{"TestMine"}, reason: "not in --run-only"}, m.pushScopeGate(context.Background(), "set", "d1"))
	require.Equal(t, []string{"TestMine"}, g.run)

	// A gate the agent could not install was not asked for.
	g.err = models.ErrScopeGateUnsupported
	require.Equal(t, requestedGate{}, m.pushScopeGate(context.Background(), "set", "d1"))
}

// A gate that held says nothing; one that no harness could honour — no scope
// reported, or a test outside the list ran, verdict or not — is called out, so
// a run of the full suite is never mistaken for the subset asked for.
func TestGateReport(t *testing.T) {
	g := requestedGate{run: []string{"TestA"}, reason: "r"}
	w := func(name, outcome string) models.ScopeWindow { return models.ScopeWindow{Name: name, Outcome: outcome} }
	note, ungated := gateReport(requestedGate{}, []models.ScopeWindow{w("TestB", models.ScopeOutcomePassed)})
	require.Empty(t, note, "no gate asked for")
	require.Zero(t, ungated)

	note, _ = gateReport(g, []models.ScopeWindow{
		w("TestA", models.ScopeOutcomePassed),
		w("TestA/case", ""),
		w("TestB", models.ScopeOutcomeGated),
	})
	require.Empty(t, note, "the gate held")

	note, ungated = gateReport(g, nil)
	require.Contains(t, note, "no test reported")
	require.Zero(t, ungated)

	// A harness that scopes but sends no verdicts, and cannot skip.
	note, ungated = gateReport(g, []models.ScopeWindow{w("TestA", ""), w("TestB", ""), w("TestC", ""), w("TestC", "")})
	require.Contains(t, note, "2 test(s) outside the run list ran")
	require.Equal(t, 2, ungated)
}

// A --run-only name lets a recorded test run when it is the test or a parent
// of its subtests; anything else matches nothing.
func TestGateNamesAny(t *testing.T) {
	recorded := map[string][]models.MockEntry{"TestA": nil, "TestB/case_1": nil}
	require.True(t, gateNamesAny("TestA", recorded))
	require.True(t, gateNamesAny("TestB", recorded))
	require.False(t, gateNamesAny("TestAX", recorded))
	require.False(t, gateNamesAny("TestB/case_2", recorded))
}

// An agent that cannot gate is fine when there is nothing to gate, and said
// out loud when there is: the run is not the one the gate asked for.
func TestPushScopeGateToAnAgentThatCannot(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	g := &gatePusher{err: models.ErrScopeGateUnsupported}
	m := &mockService{logger: zap.New(core), instrumentation: g, config: &config.Config{}}
	m.pushScopeGate(context.Background(), "set", "")
	require.Zero(t, logs.Len(), "no gate asked for: nothing to say")

	RegisterScopeGateSource(func(context.Context, ScopeGateRequest) ([]string, string) { return []string{"t"}, "r" })
	t.Cleanup(func() { RegisterScopeGateSource(nil) })
	m.pushScopeGate(context.Background(), "set", "")
	require.Equal(t, 1, logs.FilterMessageSnippet("every test runs this replay").Len())
}

// A replay lists each test once, with its latest run's verdict; a test the
// replay gated out is listed as gated, so a consumer can tell it did not run;
// same-named tests in two folders are two tests.
func TestRanTestsListsEachTestOnceWithItsVerdict(t *testing.T) {
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Millisecond) }
	windows := []models.ScopeWindow{
		{Name: "TestA", Dir: "e2e/orders", Start: at(0), End: at(100), Outcome: models.ScopeOutcomeFailed},
		{Name: "TestA/step", Dir: "e2e/orders", Start: at(10), End: at(90)},
		{Name: "TestB", Start: at(150), End: at(150), Outcome: models.ScopeOutcomeGated},
		{Name: "TestA", Dir: "e2e/orders", Start: at(200), End: at(300), Outcome: models.ScopeOutcomePassed},
		{Name: "TestA", Dir: "e2e/billing", Start: at(400), End: at(500), Outcome: models.ScopeOutcomeSkipped},
	}
	require.Equal(t, []RanTest{
		{Name: "TestA", Set: "e2e/orders", Status: models.ScopeOutcomePassed},
		{Name: "TestB", Set: "", Status: models.ScopeOutcomeGated},
		{Name: "TestA", Set: "e2e/billing", Status: models.ScopeOutcomeSkipped},
	}, ranTests(windows))
}

// A miss line carries what the matcher said about the call (how far matching
// got, the closest mock) and the protocol's own next step over the generic one.
func TestReportMissesCarriesTheMatchersDiagnosis(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	m := &mockService{logger: zap.New(core)}
	miss := models.UnmatchedCall{Protocol: "MongoDB", ActualSummary: "find on users", Destination: "127.0.0.1:27017", At: runnerT0.Add(time.Hour),
		MatchPhase: "exhausted", CandidateCount: 3, ClosestMock: "mock-7", NextSteps: "re-record with keploy mock record"}
	m.reportMisses([]models.UnmatchedCall{miss}, nil)
	require.Equal(t, 1, logs.Len())
	f := logs.All()[0].ContextMap()
	require.Equal(t, "exhausted", f["match_phase"])
	require.EqualValues(t, 3, f["candidates"])
	require.Equal(t, "mock-7", f["closest"])
	require.Equal(t, "re-record with keploy mock record", f["next_step"])
}

// missReader is the agent an interrupted replay asks for its misses.
type missReader struct {
	Instrumentation
	misses  []models.UnmatchedCall
	windows []models.ScopeWindow
	err     error
}

func (r *missReader) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.misses, r.err
}

func (r *missReader) GetScopeWindows(ctx context.Context) ([]models.ScopeWindow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.windows, nil
}

// A replay a signal stopped still names its misses, under their tests, even
// though its context is already cancelled; when they cannot be read, it says
// how to see them.
func TestAnInterruptedReplayNamesItsMisses(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	at := runnerT0.Add(5 * time.Millisecond)
	agent := &missReader{
		misses:  []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /price", Destination: "127.0.0.1:9000", At: at}},
		windows: []models.ScopeWindow{{Name: "TestA", Start: runnerT0, End: runnerT0.Add(time.Second)}},
	}
	m := &mockService{logger: zap.New(core), instrumentation: agent}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.reportInterruptedMisses(ctx, false)
	require.Equal(t, 1, logs.FilterMessage("no recorded mock matched a call").Len())
	require.Equal(t, "TestA", logs.FilterMessage("no recorded mock matched a call").All()[0].ContextMap()["test"])

	// The test the signal cut off never ended, so the agent has no window for
	// it: its miss is not said to be outside every test.
	core, logs = observer.New(zap.InfoLevel)
	agent.windows = nil
	m = &mockService{logger: zap.New(core), instrumentation: agent}
	m.reportInterruptedMisses(ctx, false)
	require.Equal(t, "(not in a finished test)", logs.FilterMessage("no recorded mock matched a call").All()[0].ContextMap()["test"])

	core, logs = observer.New(zap.InfoLevel)
	m = &mockService{logger: zap.New(core), instrumentation: &missReader{err: context.DeadlineExceeded}}
	m.reportInterruptedMisses(ctx, false)
	require.Equal(t, 1, logs.FilterMessageSnippet("--debug").Len())
}

// A line longer than the scanner keeps is dropped whole: text inside it that
// looks like a FAIL line is not one.
func TestFailScanDropsAnOverlongLine(t *testing.T) {
	f := &failScan{}
	// One write longer than the cap whose last maxFailScanLine bytes begin
	// with a FAIL-shaped text: keeping that tail would read it as a line.
	inside := "--- FAIL: TestInside (0.00s) "
	_, _ = f.Write([]byte(strings.Repeat("x", 10) + inside + strings.Repeat("y", maxFailScanLine-len(inside))))
	_, _ = f.Write([]byte(" still the same long line\n--- FAIL: TestReal (0.00s)\n"))
	require.Equal(t, []string{"TestReal"}, f.list())
}
