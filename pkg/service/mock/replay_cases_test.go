package mock

import (
	"context"
	"errors"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/ids"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
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
	compare := func(tc *models.TestCase, _ *models.HTTPReq, resp *models.HTTPResp) (bool, *models.Result) {
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
	compare := func(tc *models.TestCase, _ *models.HTTPReq, resp *models.HTTPResp) (bool, *models.Result) {
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
	out := pairCases(replayWindows, recorded, actual, func(*models.TestCase, *models.HTTPReq, *models.HTTPResp) (bool, *models.Result) {
		calls++
		return true, nil
	})
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
	out := pairCases(windows, recorded, actual, func(tc *models.TestCase, _ *models.HTTPReq, resp *models.HTTPResp) (bool, *models.Result) {
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
	compare := func(tc *models.TestCase, _ *models.HTTPReq, resp *models.HTTPResp) (bool, *models.Result) {
		return tc.HTTPResp.Body == resp.Body, nil
	}
	out := pairCases(replayWindows, recorded, actual, compare)
	require.Len(t, out, 2)
	require.Nil(t, out[0].Actual, "the call that was not made is reported as not made")
	require.True(t, out[1].Passed, "the call that was made is compared with its own recording")
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

// The case comparison follows the ids the AGENT bound, and no others.
//
// The app's answer is compared with the recording as it is first; only when
// that fails is it compared again with this run's ids mapped back to the
// recorded ones. Each row says what a healthy app or a regression gets, and
// what the same comparison gives with no pair at all (the recording as it is,
// which is the comparison without rebinding).
//
// This replaces two tests of a rule that is gone: the expected response used
// to be rewritten forward, with pairs read off the case's own request as well
// as the agent's. A test that merely sent another id than was recorded then
// had its expected response rewritten though nothing was followed, and a case
// that passes without rebinding failed (the first "as recorded" row below).
func TestCompareCaseFollowsTheIDsTheAgentBound(t *testing.T) {
	const x, y, z = "0f8fad5b-d9cb-469f-a165-70867728950e", "7c9e6679-7425-40de-944b-e07fc1f90ae7", "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
	const x2, y2 = "11111111-1111-4111-8111-111111111111", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	id := func(v string) string { return `{"id":"` + v + `","qty":1}` }
	get := func(v string) models.HTTPReq { return models.HTTPReq{Method: "GET", URL: "http://app/orders/" + v} }
	post := models.HTTPReq{Method: "POST", URL: "http://app/orders", Body: `{"name":"widget"}`}
	for _, c := range []struct {
		name     string
		recorded models.HTTPReq // the recorded case's request
		expected string         // and its response body
		sent     models.HTTPReq // what the test sent this run
		answered string         // and what the app answered
		pairs    map[string]string
		pass     bool
		main     bool // the verdict with no pair at all
	}{
		// A healthy app whose ids were followed.
		{"a create answered with the id made this run", post, id(x), post, id(y), map[string]string{x: y}, true, false},
		{"its read-back, asked for by that id", get(x), id(x), get(y), id(y), map[string]string{x: y}, true, false},
		{"a list that names two ids made this run", get(""), `[` + id(x) + `,` + id(x2) + `]`, get(""), `[` + id(y) + `,` + id(y2) + `]`, map[string]string{x: y, x2: y2}, true, false},

		// As recorded: what passes without rebinding passes with it,
		// whatever the agent bound.
		{"the test sent another id and the app answered as recorded", get(x), id(x), get(y), id(x), nil, true, true},
		{"the same, in a run that bound the id elsewhere", get(x), id(x), get(y), id(x), map[string]string{x: z}, true, true},
		{"an answer equal to the recording that names a live id", get(""), `{"id":"` + x + `","last":"` + y + `"}`, get(""), `{"id":"` + x + `","last":"` + y + `"}`, map[string]string{x: y}, true, true},

		// The case's own request has the say.
		{"the test sent Y where the recording has X, and the app made Z", get(x), id(x), get(y), id(z), map[string]string{x: z}, false, false},
		{"the test sent the recorded id itself, and the app made Z", get(x), id(x), get(x), id(z), map[string]string{x: z}, false, false},
		{"the id only in a header: the test sent Y, the app made Z",
			models.HTTPReq{Method: "POST", URL: "http://app/orders", Header: map[string]string{"Idempotency-Key": x}}, id(x),
			models.HTTPReq{Method: "POST", URL: "http://app/orders", Header: map[string]string{"Idempotency-Key": y}}, id(z), map[string]string{x: z}, false, false},
		{"the id only in a form value: the test sent Y, the app made Z",
			models.HTTPReq{Method: "POST", URL: "http://app/orders/lookup", Form: []models.FormData{{Key: "order", Values: []string{x}}}}, id(x),
			models.HTTPReq{Method: "POST", URL: "http://app/orders/lookup", Form: []models.FormData{{Key: "order", Values: []string{y}}}}, id(z), map[string]string{x: z}, false, false},
		{"the id only in a query parameter: the test sent Y, the app made Z",
			models.HTTPReq{Method: "GET", URL: "http://app/orders", URLParams: map[string]string{"order": x}}, id(x),
			models.HTTPReq{Method: "GET", URL: "http://app/orders", URLParams: map[string]string{"order": y}}, id(z), map[string]string{x: z}, false, false},
		{"the id only in the body: the test sent Y, the app made Z",
			models.HTTPReq{Method: "POST", URL: "http://app/orders/search", Body: `{"order":"` + x + `"}`}, id(x),
			models.HTTPReq{Method: "POST", URL: "http://app/orders/search", Body: `{"order":"` + y + `"}`}, id(z), map[string]string{x: z}, false, false},
		{"the id in a form value, sent as the app made it",
			models.HTTPReq{Method: "POST", URL: "http://app/orders/lookup", Form: []models.FormData{{Key: "order", Values: []string{x}}}}, id(x),
			models.HTTPReq{Method: "POST", URL: "http://app/orders/lookup", Form: []models.FormData{{Key: "order", Values: []string{y}}}}, id(y), map[string]string{x: y}, true, false},

		// Nothing is taken from the request alone.
		{"the test sent another id and the app echoed it, with no pair", get(x), id(x), get(y), id(y), nil, false, false},
		{"the same, in a run that bound another id", get(x), id(x), get(y), id(y), map[string]string{x2: y2}, false, false},

		// A followed id is still asserted.
		{"the app answered with another entity's id", get(""), id(x2), get(""), id(y), map[string]string{x: y, x2: y2}, false, false},
		{"something else differs too", post, id(x), post, `{"id":"` + y + `","qty":2}`, map[string]string{x: y}, false, false},
		{"an id inside a longer token is another word", post, `{"ref":"order-` + x + `"}`, post, `{"ref":"order-` + y + `"}`, map[string]string{x: y}, false, false},
		// One entity by two ids: the test asked for the id made this run, the
		// app asked its dependency for another, and the closest recording
		// answered as recorded. Mapped back, the two would read as one.
		{"an answer that names an entity by this run's id and by the recorded one", get(x), `{"asked":"` + x + `","item":"` + x + `"}`, get(y), `{"asked":"` + y + `","item":"` + x + `"}`, map[string]string{x: y}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "case", Kind: models.HTTP, HTTPReq: c.recorded, HTTPResp: models.HTTPResp{StatusCode: 200, Body: c.expected}}
			answer := func() *models.HTTPResp { return &models.HTTPResp{StatusCode: 200, Body: c.answered} }
			sent := c.sent

			main, mainResult := (&mockService{config: &config.Config{}, logger: zap.NewNop()}).compareCase(tc, &sent, answer())
			require.Equal(t, c.main, main, "what the comparison gives with no pair")

			got := answer()
			pass, result := (&mockService{config: &config.Config{}, logger: zap.NewNop(), ids: ids.New(c.pairs)}).compareCase(tc, &sent, got)
			require.Equal(t, c.pass, pass)
			if c.main {
				require.Equal(t, mainResult, result, "what passes as recorded is not compared again")
			}
			require.Equal(t, c.expected, tc.HTTPResp.Body, "the recorded case is never changed")
			require.Equal(t, c.answered, got.Body, "nor is the answer that is reported")
		})
	}
}

// An id of this run is mapped back as a whole word wherever the answer names
// it: in a header as in the body. And a comparison that still fails says what
// differs besides the ids.
func TestCompareCaseMapsIDsBackInHeadersAndSaysWhatElseDiffers(t *testing.T) {
	const x, y = "0f8fad5b-d9cb-469f-a165-70867728950e", "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	m := &mockService{config: &config.Config{}, logger: zap.NewNop(), ids: ids.New(map[string]string{x: y})}
	tc := httpCase("create", "POST", "http://app/orders", 201, `{"id":"`+x+`","qty":1}`, runnerT0)
	tc.HTTPResp.Header = map[string]string{"Location": "/orders/" + x}

	answer := &models.HTTPResp{StatusCode: 201, Header: map[string]string{"Location": "/orders/" + y}, Body: `{"id":"` + y + `","qty":1}`}
	pass, _ := m.compareCase(tc, &tc.HTTPReq, answer)
	require.True(t, pass)
	require.Equal(t, "/orders/"+y, answer.Header["Location"], "the answer that is reported keeps this run's id")

	answer = &models.HTTPResp{StatusCode: 201, Header: map[string]string{"Location": "/orders/" + y}, Body: `{"id":"` + y + `","qty":2}`}
	pass, result := m.compareCase(tc, &tc.HTTPReq, answer)
	require.False(t, pass)
	require.JSONEq(t, `{"id":"`+x+`","qty":2}`, result.BodyResult[0].Actual, "the ids are not what differs")
}

// askedInstr is an agent that keeps what a replay asked of it.
type askedInstr struct {
	*runnerInstr
	asked []models.OutgoingOptions
}

func (a *askedInstr) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	a.asked = append(a.asked, opts)
	return a.runnerInstr.MockOutgoing(ctx, opts)
}

// bindingInstr is one that can also be asked for the ids it bound: the pair
// it holds, when the replay asked it to follow ids.
type bindingInstr struct {
	*askedInstr
	pairs map[string]string
}

func (b *bindingInstr) GetIDPairs(context.Context) (map[string]string, error) {
	if len(b.asked) == 0 || !b.asked[len(b.asked)-1].RebindMinted {
		return nil, nil
	}
	return b.pairs, nil
}

// A replay asks the agent to follow the ids the app generates only where it
// can read back what was bound. The app's answers are compared with the
// recorded cases, and an answer that names an id of this run matches its case
// only with the pairs: under docker compose the agent is gone before they can
// be read, so a compose replay follows nothing and its mocks are served, and
// its cases compared, as they are without rebinding.
// test.disableMockRebinding turns it off everywhere.
func TestReplayFollowsIDsOnlyWhereItCanReadThemBack(t *testing.T) {
	const x, y = "0f8fad5b-d9cb-469f-a165-70867728950e", "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	for _, c := range []struct {
		name    string
		cmdType utils.CmdType
		disable bool
		reads   bool // the agent can be asked for the ids it bound
		want    bool // the agent is asked to follow ids, and the cases pass
	}{
		{"natively", utils.Native, false, true, true},
		{"natively, with rebinding off", utils.Native, true, true, false},
		{"natively, with an agent that cannot be asked for its pairs", utils.Native, false, false, false},
		{"under docker compose", utils.DockerCompose, false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := newRunnerInstr(t, sequentialMarks()...)
			if c.cmdType == utils.DockerCompose {
				// The agent is a service of the project: it comes up with it,
				// and is gone, leaving its account, when the runner exits.
				shortAgentBudget(t)
				base.lifetime, base.runUntilReady = agentUpAfterRun, true
				base.consumedErr, base.mockErrorsErr = errors.New("connection refused"), errors.New("connection refused")
				base.leftOutcome = models.MockOutcome{Windows: sequentialMarks()}
			}
			// The app answers the create with the id it made this run, and
			// the test reads the order back by that id.
			base.incoming = []*models.TestCase{
				httpCase("", "POST", "http://localhost:8080/orders", 201, `{"id":"`+y+`"}`, runnerT0.Add(5*time.Millisecond)),
				httpCase("", "GET", "http://localhost:8080/orders/"+y, 200, `{"id":"`+y+`","qty":1}`, runnerT0.Add(6*time.Millisecond)),
			}
			agent := &bindingInstr{askedInstr: &askedInstr{runnerInstr: base}, pairs: map[string]string{x: y}}
			var instr Instrumentation = agent
			if !c.reads {
				instr = agent.askedInstr
			}
			dir := t.TempDir()
			mapDB := mapdb.New(zap.NewNop(), dir, "")
			require.NoError(t, mapDB.UpsertCases(context.Background(), "set", map[string]models.MappedTestCase{"orders/e2e.TestA": {Cases: []string{"post-orders-1", "get-order-1"}}}, nil, nil))
			db := &memTestDB{existing: []*models.TestCase{
				httpCase("post-orders-1", "POST", "http://localhost:8080/orders", 201, `{"id":"`+x+`"}`, runnerT0),
				httpCase("get-order-1", "GET", "http://localhost:8080/orders/"+x, 200, `{"id":"`+x+`","qty":1}`, runnerT0),
			}}

			var got ReplayOutcome
			RegisterReplayOutcomeReporter(func(_ context.Context, o ReplayOutcome) { got = o })
			t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })
			cfg := instrConfig(base.composeInstr, c.cmdType, "./shop.test -test.v")
			cfg.Path = dir
			cfg.Test.DisableMockRebinding = c.disable
			withRequests(cfg)
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			svc := New(zap.NewNop(), instr, stubMockDB{}, mapDB, nil, nil, cfg)
			svc.(TestDBSetter).SetTestDB(db)
			require.NoError(t, svc.Replay(context.Background()))

			require.Len(t, agent.asked, 1)
			require.Equal(t, c.want, agent.asked[0].RebindMinted)
			require.Len(t, got.Cases, 2)
			for _, o := range got.Cases {
				require.NotNil(t, o.Actual, "precondition: %s was paired with what the test sent", o.Case.Name)
				require.Equal(t, c.want, o.Passed, o.Case.Name)
			}
		})
	}
}
