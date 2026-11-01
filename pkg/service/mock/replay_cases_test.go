package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
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

// heldIncoming is a replay whose agent still holds requests from before the
// test command exited, and says so, while the stop's signal comes as the
// command ends: the drain waits on the agent and offers to stop now.
type heldIncoming struct{ *runnerInstr }

func (h heldIncoming) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	res := h.runnerInstr.Run(ctx, opts)
	utils.MarkInterrupted() // the user's Ctrl+C, as the command ends
	return res
}

func (heldIncoming) GetIncoming(ctx context.Context, _ models.IncomingOptions) (<-chan *models.TestCase, error) {
	out := make(chan *models.TestCase)
	go func() { <-ctx.Done(); close(out) }()
	return out, nil
}

func (heldIncoming) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return models.AgentBacklog{Verdict: models.BacklogPending}, nil
}

// A replay whose drain the user ends -- a press after it offered to stop now
// -- is judged on part of the run if it is judged at all, and a verdict on
// part of a run can pass a suite that did not. It ends as a replay the user
// stopped before it finished does: 128+N, no verdict, the last receipt left
// standing.
func TestReplayTheUserStoppedWhileItCollectedRequestsIsNotJudged(t *testing.T) {
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 20 * time.Millisecond
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	defer utils.SetInterruptSignalForTest(2)() // SIGINT
	instr := heldIncoming{newRunnerInstr(t, sequentialMarks()...)}
	dir := t.TempDir()
	mapDB := mapdb.New(zap.NewNop(), dir, "")
	require.NoError(t, mapDB.UpsertCases(context.Background(), "set", map[string]models.MappedTestCase{"orders/e2e.TestA": {Cases: []string{"post-orders-1"}}}, nil, nil))
	db := &memTestDB{existing: []*models.TestCase{httpCase("post-orders-1", "POST", "http://localhost:8080/orders", 201, `{}`, runnerT0)}}
	judged := false
	RegisterReplayOutcomeReporter(func(context.Context, ReplayOutcome) { judged = true })
	t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })
	cfg := instrConfig(instr.composeInstr, utils.Native, "./shop.test -test.v")
	cfg.Path = dir
	withRequests(cfg)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	core, logs := observer.New(zapcore.InfoLevel)
	svc := New(zap.New(core), instr, stubMockDB{}, mapDB, nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(db)
	returned := make(chan error, 1)
	go func() { returned <- svc.Replay(context.Background()) }()
	offered := replayDrainWords.saving + "; " + replayDrainWords.stopNow
	within := time.After(time.Minute) // only turns a hang into a failure
	for logs.FilterMessage(offered).Len() == 0 {
		select {
		case err := <-returned:
			t.Fatalf("the replay returned (%v) before its drain offered to stop now: %v", err, logs.All())
		case <-within:
			t.Fatalf("the drain never offered to stop now: %v", logs.All())
		case <-time.After(time.Millisecond):
		}
	}
	if !utils.MarkInterruptedAgain() { // the user, as told
		t.Fatal("setup: no offer standing")
	}
	select {
	case err := <-returned:
		require.NoError(t, err, "a replay the user stopped is not keploy's failure")
	case <-within:
		t.Fatal("the user's press did not end the replay")
	}
	if judged {
		t.Fatal("a replay the user stopped while it collected the requests was judged on part of them")
	}
	if utils.ErrCode != 130 {
		t.Fatalf("a replay the user stopped exits %d, want 130", utils.ErrCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "set", ReceiptFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a replay the user stopped wrote a receipt (%v)", err)
	}
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

// Each test carries the verdict of its latest run — "" when that run reported
// none, never an earlier run's — and a test that recorded no mocks, or that
// the replay gated out, still gets its line.
func TestAttributeMocksCarriesEachTestsLatestVerdict(t *testing.T) {
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Millisecond) }
	windows := []models.ScopeWindow{
		{Name: "pkg.TestA", Start: at(0), End: at(100), Outcome: models.ScopeOutcomeFailed},
		{Name: "pkg.TestA", Start: at(400), End: at(500), Outcome: models.ScopeOutcomePassed},
		{Name: "pkg.TestB", Start: at(200), End: at(300), Outcome: models.ScopeOutcomePassed},
		{Name: "pkg.TestOwn", Start: at(520), End: at(560), Outcome: models.ScopeOutcomeFailed, Reported: models.ScopeOutcomeFailed},
		{Name: "pkg.TestOwn", Start: at(570), End: at(590), Outcome: models.ScopeOutcomeFailed, Reported: models.ScopeOutcomePassed}, // keploy failed this run alone
		{Name: "pkg.TestB", Start: at(600), End: at(650)},                                                                            // a re-run that reported nothing
		{Name: "pkg.TestPure", Start: at(700), End: at(800), Outcome: models.ScopeOutcomePassed},
		{Name: "pkg.TestGated", Start: at(900), End: at(900), Outcome: models.ScopeOutcomeGated},
	}
	expected := map[string][]models.MockEntry{"pkg.TestA": {{Name: "mock-0"}}, "pkg.TestB": {{Name: "mock-1"}}, "pkg.TestGated": {{Name: "mock-2"}}}
	got := map[string]string{}
	reported := map[string]string{}
	for _, f := range attributeMocks(windows, expected, nil, nil) {
		got[f.Flow] = f.Outcome
		reported[f.Flow] = f.Reported
	}
	require.Equal(t, models.ScopeOutcomePassed, reported["pkg.TestOwn"], "the latest run's harness verdict, beside its keploy verdict")
	delete(got, "pkg.TestOwn")
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
	// The verdict the harness itself reported rides beside keploy's.
	require.Equal(t, []TestReceipt{{Name: "pkg.TestB", Outcome: models.ScopeOutcomeFailed, Reported: models.ScopeOutcomePassed}},
		testReceipts([]FlowMocks{{Flow: "pkg.TestB", Outcome: models.ScopeOutcomeFailed, Reported: models.ScopeOutcomePassed}}, true, true))
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

// An --on-miss record run appends mocks to the set, naming each past the
// highest "mock-N" in the file. A document this keploy's decoders skipped (a
// kind it cannot read, a connection failure it cannot replay) is still in the
// file under its name, so an appended mock must not take that name either.
func TestAnAppendedMockNeverTakesASkippedDocumentsName(t *testing.T) {
	dir := t.TempDir()
	set := filepath.Join(dir, "set-0")
	require.NoError(t, os.MkdirAll(set, 0o755))
	const doc = "version: api.keploy.io/v1beta1\nkind: %s\nname: %s\nspec:\n    metadata:\n        type: mocks\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n    resTimestampMock: 2026-10-08T11:12:00.041Z\n"
	file := fmt.Sprintf(doc, "Generic", "mock-1") +
		"---\n" + fmt.Sprintf(doc, "ConnectionFailure", "mock-7") +
		"---\n" + fmt.Sprintf(doc, "FutureKind", "mock-9")
	require.NoError(t, os.WriteFile(filepath.Join(set, "mocks.yaml"), []byte(file), 0o644))

	m := &mockService{logger: zap.NewNop(), mockDB: mockdb.New(zap.NewNop(), dir, "mocks")}
	highest, err := m.highestMockIndex(context.Background(), "set-0")
	require.NoError(t, err)
	require.Equal(t, int64(9), highest,
		"mock-9 (a kind this keploy cannot read) and mock-7 (a connection failure it cannot replay) are in the file")
}

// --on-miss record over a set on disk keeps the set's recorded mocks and adds
// the captured ones, and they are in the set when it says they were appended:
// for a set recorded in either format, under either configured format. Over a
// gob set it used to leave an empty mocks.gob: the writer truncated the set's
// file, and keploy exited without closing it, the captured mock still in its
// queue. Across formats the captured mock went to a file of the configured
// format that the readers did not read (a mocks.yaml beside a mocks.gob) or
// that hid the recorded one (a mocks.gob beside a mocks.yaml): it is written
// to the file the set has, in its format.
func TestOnMissRecordKeepsTheRecordedSetAndAddsTheCapturedCalls(t *testing.T) {
	ext := map[string]string{"yaml": "mocks.yaml", "gob": "mocks.gob"}
	for _, recordedIn := range []string{"yaml", "gob"} {
		for _, onMissIn := range []string{"yaml", "gob"} {
			t.Run(recordedIn+"/"+onMissIn, func(t *testing.T) {
				t.Setenv("KEPLOY_MOCK_FORMAT", "")
				dir := t.TempDir()
				recorded := mockdb.New(zap.NewNop(), dir, "mocks")
				recorded.MockFormat = recordedIn
				for _, u := range []string{"http://dep/a", "http://dep/b", "http://dep/c"} {
					require.NoError(t, recorded.InsertMock(context.Background(), httpTo(u), "set-0"))
				}
				require.NoError(t, recorded.Close())

				core, logs := observer.New(zapcore.InfoLevel)
				onMiss := mockdb.New(zap.NewNop(), dir, "mocks")
				onMiss.MockFormat = onMissIn
				m := &mockService{logger: zap.New(core), instrumentation: capturingInstr{captured: []*models.Mock{httpTo("http://dep/new")}},
					mockDB: onMiss, store: FileStore{}}
				require.NoError(t, m.persistCaptured(context.Background(), "set-0"))
				require.Empty(t, logs.FilterLevelExact(zapcore.ErrorLevel).All())
				require.Equal(t, 1, logs.FilterMessageSnippet("appended new dependency calls").Len())

				require.Equal(t, []string{"mock-0 http://dep/a", "mock-1 http://dep/b", "mock-2 http://dep/c", "mock-3 http://dep/new"}, setMocks(t, dir))
				files, err := os.ReadDir(filepath.Join(dir, "set-0"))
				require.NoError(t, err)
				var names []string
				for _, f := range files {
					names = append(names, f.Name())
				}
				require.Equal(t, []string{ext[recordedIn]}, names, "the set's one mock file, in the format it was recorded in")
			})
		}
	}
}

// A set that holds a mocks.gob and a mocks.yaml, as an older keploy's
// --on-miss record left one, is not added to: the captured calls are said, at
// ERROR, not to be saved, with both files named, neither file changes, and
// the error that fails the run says so.
func TestOnMissRecordRefusesASetWithTwoMockFiles(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	dir := t.TempDir()
	for _, format := range []string{"gob", "yaml"} {
		db := mockdb.New(zap.NewNop(), filepath.Join(dir, format), "mocks")
		db.MockFormat = format
		require.NoError(t, db.InsertMock(context.Background(), httpTo("http://dep/"+format), "set-0"))
		require.NoError(t, db.Close())
	}
	set := filepath.Join(dir, "set-0")
	require.NoError(t, os.MkdirAll(set, 0o755))
	before := map[string][]byte{}
	for _, f := range []string{"mocks.gob", "mocks.yaml"} {
		require.NoError(t, os.Rename(filepath.Join(dir, strings.TrimPrefix(filepath.Ext(f), "."), "set-0", f), filepath.Join(set, f)))
		b, err := os.ReadFile(filepath.Join(set, f))
		require.NoError(t, err)
		before[f] = b
	}

	for _, onMissIn := range []string{"yaml", "gob"} {
		t.Run(onMissIn, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			onMiss := mockdb.New(zap.NewNop(), dir, "mocks")
			onMiss.MockFormat = onMissIn
			m := &mockService{logger: zap.New(core), instrumentation: capturingInstr{captured: []*models.Mock{httpTo("http://dep/new")}},
				mockDB: onMiss, store: FileStore{}}
			perr := m.persistCaptured(context.Background(), "set-0")
			require.ErrorIs(t, perr, mockdb.ErrMixedMockFiles)
			require.ErrorIs(t, perr, models.ErrMocksNotWritten)
			require.Contains(t, perr.Error(), "1 of the 1 call(s) captured on miss were not added to the mock set set-0")
			require.Zero(t, logs.FilterMessageSnippet("appended new dependency calls").Len())
			errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
			require.Len(t, errs, 1)
			require.Contains(t, errs[0].Message, "it is not saved")
			msg := fmt.Sprint(errs[0].ContextMap()["error"])
			require.Contains(t, msg, filepath.Join(set, "mocks.gob"))
			require.Contains(t, msg, filepath.Join(set, "mocks.yaml"))
			for f, b := range before {
				got, err := os.ReadFile(filepath.Join(set, f))
				require.NoError(t, err)
				require.Equal(t, b, got, "%s changed", f)
			}
		})
	}
}

// httpTo is a recorded HTTP call to url.
func httpTo(url string) *models.Mock {
	return &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{Method: "GET", URL: url},
		HTTPResp: &models.HTTPResp{StatusCode: 200},
	}}
}

// setMocks is set-0 under dir as replay reads it: each mock's name and URL.
func setMocks(t *testing.T, dir string) []string {
	t.Helper()
	var got []string
	require.NoError(t, mockdb.New(zap.NewNop(), dir, "mocks").WalkTestSetMocks(context.Background(), "set-0", func(mk *models.Mock, _, _ bool) {
		got = append(got, mk.Name+" "+mk.Spec.HTTPReq.URL)
	}))
	return got
}

// closeFailStore is a mock set written in the background whose writer could
// not write what it was handed: closing it says so.
type closeFailStore struct {
	appendStore
	pushes int
}

func (s *closeFailStore) CloseUnless(<-chan struct{}) error {
	return fmt.Errorf("%w: 1 of 1", models.ErrMocksNotWritten)
}

func (s *closeFailStore) MocksLeft() uint64 { return 0 }

// countingPush is a Store that counts its pushes.
type countingPush struct {
	FileStore
	pushes *int
}

func (p countingPush) Push(context.Context, string) error { *p.pushes++; return nil }

// Captured calls the store could not write are not said to be appended, nor
// the set published: the run says, at ERROR, that they were not all added.
func TestOnMissRecordSaysWhenTheCapturedCallsWereNotWritten(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	s := &closeFailStore{}
	m := &mockService{logger: zap.New(core), instrumentation: capturingInstr{captured: []*models.Mock{{Kind: models.HTTP}}},
		mockDB: s, store: countingPush{pushes: &s.pushes}}
	err := m.persistCaptured(context.Background(), "set-0")
	require.ErrorIs(t, err, models.ErrMocksNotWritten)
	require.Contains(t, err.Error(), "were not all written to it")
	require.Equal(t, 0, logs.FilterMessageSnippet("appended new dependency calls").Len())
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	require.Len(t, errs, 1)
	require.Contains(t, errs[0].Message, "were not all added to the mock set")
	require.Zero(t, s.pushes, "a set whose captured calls were not written was published")

	// Some refused and the rest not written: the error counts both.
	core, logs = observer.New(zapcore.InfoLevel)
	s = &closeFailStore{appendStore: appendStore{insertErr: errors.New("disk full"), failInserts: 1}}
	m = &mockService{logger: zap.New(core), instrumentation: capturingInstr{captured: []*models.Mock{{Kind: models.HTTP}, {Kind: models.HTTP}}},
		mockDB: s, store: countingPush{pushes: &s.pushes}}
	err = m.persistCaptured(context.Background(), "set-0")
	require.ErrorIs(t, err, models.ErrMocksNotWritten)
	require.Contains(t, err.Error(), "1 of the 2 call(s) captured on miss were not added to the mock set set-0: disk full")
	require.Contains(t, err.Error(), "the 1 call(s) captured on miss that the mock set set-0 was handed were not all written to it")
	require.Equal(t, 0, logs.FilterMessageSnippet("appended new dependency calls").Len())
	errs = logs.FilterLevelExact(zapcore.ErrorLevel).All()
	require.Len(t, errs, 2)
	require.Contains(t, errs[0].Message, "failed to add a call captured on miss to the mock set")
	require.Contains(t, errs[1].Message, "were not all added to the mock set")
	require.Zero(t, s.pushes)
}

// A replay under --on-miss record whose captured calls the mock set refused
// (here a set with two mock files) fails as keploy's, whatever the test
// command did: exit 1, the cause in the error and the receipt. It used to
// exit 0, and a CI job that ran it to grow the set went green while the set
// did not grow.
func TestOnMissRecordThatAddsNothingFailsTheRun(t *testing.T) {
	refused := fmt.Errorf("%w: /keploy/set/mocks.gob and /keploy/set/mocks.yaml: %w", mockdb.ErrMixedMockFiles, models.ErrMocksNotWritten)
	keployDir, code, logs := replayIn(t, replayCase{onMiss: string(models.MissRecord), keployFails: true,
		captured: []*models.Mock{httpTo("http://dep/new")}, insertErr: refused})
	require.Equal(t, utils.ExitKeployError, code)
	require.Zero(t, logs.FilterMessageSnippet("appended new dependency calls").Len())
	r := mustReceipt(t, keployDir)
	require.Equal(t, FailedByKeploy, r.FailedBy)
	require.Equal(t, utils.ExitKeployError, r.ExitCode)
	require.Contains(t, r.Error, "mocks.gob and /keploy/set/mocks.yaml")
	require.False(t, r.Isolated)

	// The same run whose captured calls were added passes.
	_, code, _ = replayIn(t, replayCase{onMiss: string(models.MissRecord), captured: []*models.Mock{httpTo("http://dep/new")}})
	require.Zero(t, code)
}

// A test command that failed keeps its exit code when --on-miss record also
// did not add its captured calls, as keploy mirrors the runner's exit
// everywhere else: exit 3 stays 3, not 1. The run is still keploy's failure,
// named in the error and the receipt, and Replay returns it.
func TestOnMissRecordThatAddsNothingKeepsTheTestCommandsExit(t *testing.T) {
	refused := fmt.Errorf("%w: /keploy/set/mocks.gob and /keploy/set/mocks.yaml: %w", mockdb.ErrMixedMockFiles, models.ErrMocksNotWritten)
	keployDir, code, _ := replayIn(t, replayCase{onMiss: string(models.MissRecord), keployFails: true,
		runExit:  models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3},
		captured: []*models.Mock{httpTo("http://dep/new")}, insertErr: refused})
	require.Equal(t, 3, code)
	r := mustReceipt(t, keployDir)
	require.Equal(t, 3, r.ExitCode)
	require.Equal(t, 3, r.RunnerExitCode)
	require.Equal(t, FailedByKeploy, r.FailedBy)
	require.Contains(t, r.Error, "mocks.gob and /keploy/set/mocks.yaml")
}

// A keploy failure decided before --on-miss record's append failed keeps its
// name in the receipt: the root cause, not the append, is what triage reads.
// The append failure is still said, at ERROR.
func TestOnMissRecordThatAddsNothingKeepsAnEarlierKeployFailuresName(t *testing.T) {
	refused := fmt.Errorf("%w: /keploy/set/mocks.gob and /keploy/set/mocks.yaml: %w", mockdb.ErrMixedMockFiles, models.ErrMocksNotWritten)
	keployDir, code, logs := replayIn(t, replayCase{onMiss: string(models.MissRecord), keployFails: true,
		runExit:  models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("the app runner panicked")},
		captured: []*models.Mock{httpTo("http://dep/new")}, insertErr: refused})
	require.NotZero(t, code)
	r := mustReceipt(t, keployDir)
	require.Equal(t, FailedByKeploy, r.FailedBy)
	require.Equal(t, "the app runner panicked", r.Error)
	refusals := logs.FilterMessageSnippet("failed to add a call captured on miss").FilterLevelExact(zapcore.ErrorLevel)
	require.Equal(t, 1, refusals.Len(), "the append failure was not said at ERROR")
}

// Calls the agent captured on miss that it could not hand over are not in
// the set either: the run is told why, at ERROR (it used to be DEBUG alone),
// and nothing is appended or published.
func TestOnMissRecordSaysWhenTheAgentDidNotHandOverTheCapturedCalls(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	s := &closeFailStore{}
	m := &mockService{logger: zap.New(core), instrumentation: failingDrain{err: errors.New("captured mocks returned status 500")},
		mockDB: s, store: countingPush{pushes: &s.pushes}}
	err := m.persistCaptured(context.Background(), "set-0")
	require.EqualError(t, err, "the calls captured on miss could not be read from keploy's agent, so none was added to the mock set set-0: captured mocks returned status 500")
	require.Len(t, logs.FilterLevelExact(zapcore.ErrorLevel).All(), 1)
	require.Zero(t, s.pushes)
}

// failingDrain is an agent that cannot hand over what it captured on miss.
type failingDrain struct {
	Instrumentation
	err error
}

func (f failingDrain) DrainCapturedMocks(context.Context) ([]*models.Mock, error) { return nil, f.err }

// missCaptureInstr is a replay's agent that captured these calls on miss.
type missCaptureInstr struct {
	*runWrites
	captured []*models.Mock
}

func (c missCaptureInstr) DrainCapturedMocks(context.Context) ([]*models.Mock, error) {
	return c.captured, nil
}

// refusingMockDB is a mock store that refuses every insert with err.
type refusingMockDB struct {
	stubMockDB
	err error
}

func (r refusingMockDB) InsertMock(context.Context, *models.Mock, string) error { return r.err }

// capturingInstr is an agent connection that captured these calls on miss.
type capturingInstr struct {
	Instrumentation
	captured []*models.Mock
}

func (c capturingInstr) DrainCapturedMocks(context.Context) ([]*models.Mock, error) {
	return c.captured, nil
}

// appendStore is a mock set that is read (or fails to be, readErr) and
// appended to (or fails to be, insertErr), counting the appends.
type appendStore struct {
	stubMockDB
	readErr, insertErr error
	// failInserts is how many appends, from the first, fail with
	// insertErr (all of them when 0).
	failInserts int
	calls       int
	inserts     int
}

func (s *appendStore) GetTestSetMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) (models.TestSetMocks, error) {
	return models.TestSetMocks{}, s.readErr
}

func (s *appendStore) InsertMock(context.Context, *models.Mock, string) error {
	s.calls++
	if s.insertErr != nil && (s.failInserts == 0 || s.calls <= s.failInserts) {
		return s.insertErr
	}
	s.inserts++
	return nil
}

// listStore is a mock set without the one-pass read (pkg.TestSetMocksReader),
// read one pool at a time (or failing to be, readErr), counting the appends.
type listStore struct {
	stubMockDB
	readErr error
	inserts int
}

func (s *listStore) GetFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return []*models.Mock{{Name: "mock-4", Kind: models.HTTP}}, s.readErr
}

func (s *listStore) InsertMock(context.Context, *models.Mock, string) error {
	s.inserts++
	return nil
}

// --on-miss record appends what it captured to the set, numbered past the
// mocks already in it. A set it cannot read cannot be numbered: rather than
// number from mock-0 again, over names already in the file, it appends
// nothing, and says so at ERROR. An append that fails says so at ERROR too.
// Either way the error that fails the run says how many were not added.
func TestCapturedCallsAreAppendedOnlyWhenTheSetCanBeNumbered(t *testing.T) {
	captured := []*models.Mock{{Kind: models.HTTP}, {Kind: models.HTTP}}
	for _, tc := range []struct {
		name        string
		store       *appendStore
		wantInserts int
		wantError   string
		wantRunErr  string
	}{
		{"the set reads", &appendStore{}, 2, "", ""},
		{"the set does not read", &appendStore{readErr: errors.New("failed to decode the mocks")}, 0, "could not be read to number them",
			"the 2 call(s) captured on miss were not added to the mock set set-0: it could not be read to number them: failed to decode the mocks"},
		{"an append fails, the next is made", &appendStore{insertErr: errors.New("disk full"), failInserts: 1}, 1, "failed to add a call captured on miss",
			"1 of the 2 call(s) captured on miss were not added to the mock set set-0: disk full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			m := &mockService{logger: zap.New(core), instrumentation: capturingInstr{captured: captured}, mockDB: tc.store, store: FileStore{}}
			err := m.persistCaptured(context.Background(), "set-0")
			require.Equal(t, tc.wantInserts, tc.store.inserts)
			if tc.wantRunErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tc.wantRunErr)
			}
			errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
			if tc.wantError == "" {
				require.Empty(t, errs)
				return
			}
			require.NotEmpty(t, errs)
			require.Contains(t, errs[0].Message, tc.wantError)
		})
	}
	// A store without the one-pass read is numbered from its pools, and is
	// not appended to when they cannot be read.
	for _, tc := range []struct {
		name        string
		readErr     error
		wantInserts int
	}{
		{"read one pool at a time", nil, 2},
		{"a pool does not read", errors.New("failed to decode the mocks"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			store := &listStore{readErr: tc.readErr}
			m := &mockService{logger: zap.New(core), instrumentation: capturingInstr{captured: captured}, mockDB: store, store: FileStore{}}
			err := m.persistCaptured(context.Background(), "set-0")
			require.Equal(t, tc.wantInserts, store.inserts)
			require.Equal(t, tc.readErr != nil, err != nil, "persistCaptured = %v", err)
			errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
			if tc.readErr == nil {
				require.Empty(t, errs)
				return
			}
			require.Len(t, errs, 1)
			require.Contains(t, errs[0].Message, "could not be read to number them")
		})
	}
}
