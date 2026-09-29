package mock

import (
	"context"
	"testing"
	"time"

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
	instr := newRunnerInstr(t, jsonSequential)
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
	instr := newRunnerInstr(t, jsonSequential)
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
	scope := feed(t, jsonEvent("run", "TestTestSuiteValidation", 0)+
		jsonEvent("run", "TestTestSuiteValidation/missing_name", 1)+jsonEvent("pass", "TestTestSuiteValidation/missing_name", 2)+
		jsonEvent("run", "TestTestSuiteValidation/missing_steps", 3)+jsonEvent("pass", "TestTestSuiteValidation/missing_steps", 4)+
		jsonEvent("pass", "TestTestSuiteValidation", 5))
	windows := mergeWindows(scope.windows(), nil)
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

func TestScopeTableGroupsEachFlowWithTheSharedMocksAndItsWindow(t *testing.T) {
	at := func(sec int) time.Time { return runnerT0.Add(time.Duration(sec) * time.Second) }
	mocks := []*models.Mock{mockAt("mock-0", at(0)), mockAt("mock-1", at(10)), mockAt("mock-2", at(12)), mockAt("mock-3", at(20)), mockAt("mock-4", at(30))}
	table := scopeTable(map[string][]models.MockEntry{
		"orders/e2e.TestA":        {{Name: "mock-1"}},
		"orders/e2e.TestA/create": {{Name: "mock-2"}},
		"orders/e2e.TestB":        {{Name: "mock-3"}},
		"orders/e2e.TestEmpty":    nil,
	}, []models.MockEntry{{Name: "mock-0"}, {Name: "mock-4"}}, mocks)

	require.Equal(t, []string{"mock-1", "mock-2", "mock-0", "mock-4"}, table.Mappings["orders/e2e.TestA"])
	require.Equal(t, table.Mappings["orders/e2e.TestA"], table.Mappings["orders/e2e.TestA/create"], "a subtest's scope serves its whole flow")
	require.Equal(t, []string{"mock-3", "mock-0", "mock-4"}, table.Mappings["orders/e2e.TestB"])
	require.Empty(t, table.Mappings["orders/e2e.TestEmpty"], "a flow with no mocks is not narrowed")
	require.Equal(t, models.ScopeWindow{Start: at(10), End: at(12)}, table.Windows["orders/e2e.TestA"])
	require.Equal(t, models.ScopeWindow{Start: at(20), End: at(20)}, table.Windows["orders/e2e.TestB"])
	require.NotContains(t, table.Windows, "orders/e2e.TestEmpty")
	require.True(t, table.FirstStart.Equal(at(10)))
}
