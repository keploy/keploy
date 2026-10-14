//go:build linux

package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	nethttp "net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// These tests run the proven wire against the proxy's real mock manager and
// the real HTTP matcher: what a replayed test is answered with, end to end.

// realProxy is a proxy around a real mock manager. It keeps the misses the
// matcher reports, each filed as the proxy files it (models.Miss*).
type realProxy struct {
	coreAgent.Proxy
	// The calls each test makes are answered before its end (the tests'
	// calls are synchronous): every call is in hand when a test settles.
	settledProxy
	mm     *proxyPkg.MockManager
	mu     sync.Mutex
	misses []models.UnmatchedCall
}

func newRealProxy() *realProxy {
	return &realProxy{mm: proxyPkg.NewMockManager(nil, nil, zap.NewNop())}
}

func (p *realProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, s, e time.Time) error {
	p.mm.SetMocksWithWindow(f, u, s, e)
	return nil
}
func (p *realProxy) SetMocks(_ context.Context, f, u []*models.Mock) error {
	p.mm.SetFilteredMocks(f)
	p.mm.SetUnFilteredMocks(u)
	return nil
}
func (p *realProxy) ServeExactTest(test string, owned []*models.Mock) error {
	p.mm.ServeExactTest(test, owned)
	return nil
}
func (p *realProxy) ServeExactPool(key string, names []string) error {
	p.mm.ServeExactPool(key, names)
	return nil
}
func (p *realProxy) ExactTestOutcome() (string, []string, []string, bool) {
	return p.mm.ExactTestOutcome()
}
func (p *realProxy) ResetStatefulCursors()      { p.mm.ResetStatefulCursors() }
func (p *realProxy) ContinueTestErrorCapture()  {}
func (p *realProxy) SetMappedUniverse([]string) {}
func (p *realProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.misses
	p.misses = nil
	return out, nil
}

// noteMiss files a miss the matcher reported, as the proxy does.
func (p *realProxy) noteMiss(err error) {
	uc := models.UnmatchedCall{Protocol: "HTTP", Kind: models.MissOutsideTest}
	if test, _, _, _ := p.mm.ExactTestOutcome(); test != "" {
		uc.Kind = models.MissInTest
	}
	var r interface {
		MismatchReport() *models.MockMismatchReport
	}
	if errors.As(err, &r) && r.MismatchReport() != nil {
		rep := r.MismatchReport()
		uc.ActualSummary, uc.Destination = rep.ActualSummary, rep.Destination
		if uc.Kind == models.MissInTest && rep.MissKind == models.MissExtraCall {
			uc.Kind, uc.Recorded = models.MissExtraCall, rep.Recorded
		}
	}
	p.mu.Lock()
	p.misses = append(p.misses, uc)
	p.mu.Unlock()
}

// call sends GET path, through the real HTTP matcher, and returns the status
// and body the app got.
func (p *realProxy) call(t *testing.T, path string) (int, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	app, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer app.Close()
	prox := <-acc
	defer prox.Close()
	ctx := models.WithMockMismatchReporter(context.Background(), p.noteMiss)
	go func() {
		_ = httpparser.New(zap.NewNop()).MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Port: 18093}, p.mm, models.OutgoingOptions{})
	}()
	_, err = io.WriteString(app, "GET "+path+" HTTP/1.1\r\nHost: 127.0.0.1:18093\r\n\r\n")
	require.NoError(t, err)
	resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// pingMock is a recorded GET path answered body, made at at. It carries no
// recorder tag, so its kind makes it session-tier: reusable without the
// switch, a test mock with it.
func pingMock(name, path, body string, at time.Time) *models.Mock {
	return &models.Mock{
		Version: models.GetVersion(), Name: name, Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://127.0.0.1:18093" + path, Header: map[string]string{"Host": "127.0.0.1:18093"}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: body, Header: map[string]string{"Content-Length": fmt.Sprint(len(body))}},
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		},
	}
}

// exactAgent is a replaying agent on a real mock manager, holding mocks,
// with table as its per-test mappings and pool as its pool slices, claimed
// by this test process with key "o".
func exactAgent(t *testing.T, mocks []*models.Mock, table, pool map[string][]string) (*Agent, *realProxy) {
	t.Helper()
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newRealProxy()
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, mocks))
	require.NoError(t, a.SetScopeTable(ctx, table))
	require.NoError(t, a.SetScopePool(ctx, pool))
	claimSelf(t, a)
	return a, p
}

// runTest begins name, makes its calls, ends it as passed, and returns what
// each call got and the end's answer.
func runTest(t *testing.T, a *Agent, p *realProxy, name string, paths ...string) ([]string, models.ScopeEndResp) {
	t.Helper()
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin(name))
	require.Nil(t, serr)
	var got []string
	for _, path := range paths {
		code, body := p.call(t, path)
		got = append(got, fmt.Sprintf("%d %s", code, strings.TrimSpace(body)))
	}
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: name, CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	return got, e
}

// evidenceOf is the evidence of id's latest window, read without ending the
// run.
func evidenceOf(t *testing.T, a *Agent, id string) *models.ScopeEvidence {
	t.Helper()
	ws, err := a.GetScopeWindows(context.Background())
	require.NoError(t, err)
	for i := len(ws) - 1; i >= 0; i-- {
		if ws[i].Name == id {
			require.NotNil(t, ws[i].Evidence, "%s has no evidence", id)
			return ws[i].Evidence
		}
	}
	t.Fatalf("no window %q", id)
	return nil
}

var t0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

// Two tests that each made one identical call, replayed out of their
// recorded order, are each answered by their own recorded call, and pass
// (enterprise #2837's clock_runner case). Before exact serving, a test mock
// the kind made session-tier was served to every test: the first test to
// run got the other's recorded answer, and both failed for the other's mock.
func TestIdenticalCallsReplayedOutOfOrderAreEachAnsweredByTheirOwn(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/ping", "first", t0), pingMock("mock-2", "/ping", "second", t0.Add(3*time.Second))},
		map[string][]string{"o::test_first": {"mock-1"}, "o::test_second": {"mock-2"}}, map[string][]string{})
	for _, tc := range []struct{ name, want string }{{"test_second", "200 second"}, {"test_first", "200 first"}} {
		got, e := runTest(t, a, p, tc.name, "/ping")
		require.Equal(t, []string{tc.want}, got, tc.name)
		require.Empty(t, e.Action, "%s failed: %s", tc.name, e.Reason)
	}
}

// Each test mock answers at most one request, in recorded order; one more
// identical call is a miss of its own kind, and the test fails saying how
// often it called and recorded it.
func TestOneCallMoreThanRecordedIsAnExtraCall(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/n", "one", t0), pingMock("mock-2", "/n", "two", t0.Add(time.Second))},
		map[string][]string{"o::TestCounter": {"mock-2", "mock-1"}}, map[string][]string{})
	got, e := runTest(t, a, p, "TestCounter", "/n", "/n", "/n")
	require.Equal(t, "200 one", got[0], "recorded order, whatever the mapping's")
	require.Equal(t, "200 two", got[1])
	require.True(t, strings.HasPrefix(got[2], "502"), "the third call has no recorded answer: %s", got[2])
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "GET /n to 127.0.0.1:18093 was called 3 times, recorded 2")
	ev := windowOf(t, a, "o::TestCounter").Evidence
	require.NotNil(t, ev)
	require.Len(t, ev.Misses, 1)
	require.Equal(t, models.MissExtraCall, ev.Misses[0].Kind)
	require.Equal(t, 2, ev.Misses[0].Recorded)
	require.Empty(t, ev.Unconsumed)
}

// A retry begins afresh: its test mocks answer again, from the first.
func TestARetryIsServedFromItsFirstRecordedCall(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/n", "one", t0), pingMock("mock-2", "/n", "two", t0.Add(time.Second))},
		map[string][]string{"o::TestCounter": {"mock-1", "mock-2"}}, map[string][]string{})
	for attempt := 0; attempt < 2; attempt++ {
		got, e := runTest(t, a, p, "TestCounter", "/n", "/n")
		require.Equal(t, []string{"200 one", "200 two"}, got, "attempt %d", attempt)
		require.Empty(t, e.Action, e.Reason)
	}
}

// A test owns its own mocks only: a call only another test recorded is a
// miss in this test, and the other test's mock is still there for it.
func TestAnotherTestsMockNeverAnswers(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/a", "a", t0), pingMock("mock-2", "/b", "b", t0.Add(time.Second))},
		map[string][]string{"o::TestA": {"mock-1"}, "o::TestB": {"mock-2"}}, map[string][]string{})
	got, e := runTest(t, a, p, "TestA", "/a", "/b")
	require.Equal(t, "200 a", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), got[1])
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "GET /b to 127.0.0.1:18093 had no recorded answer")
	ev := evidenceOf(t, a, "o::TestA")
	require.Equal(t, models.MissInTest, ev.Misses[0].Kind)
	got, e = runTest(t, a, p, "TestB", "/b")
	require.Equal(t, []string{"200 b"}, got)
	require.Empty(t, e.Action, e.Reason)
}

// Between tests the key's pool slice answers, as often as asked; no test's
// mock answers there, and inside a test no data-plane pool mock answers.
func TestThePoolAnswersOnlyBetweenTests(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/health", "ok", t0), pingMock("mock-2", "/a", "a", t0.Add(time.Second))},
		map[string][]string{"o::TestA": {"mock-2"}}, map[string][]string{"o": {"mock-1"}})
	for i := 0; i < 2; i++ {
		code, body := p.call(t, "/health")
		require.Equal(t, 200, code)
		require.Equal(t, "ok", body)
	}
	code, _ := p.call(t, "/a")
	require.Equal(t, 502, code, "a test's mock answered between tests")
	got, e := runTest(t, a, p, "TestA", "/a", "/health")
	require.Equal(t, "200 a", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), "a data-plane pool mock answered inside a test: %s", got[1])
	require.Equal(t, models.ScopeActionFail, e.Action)
	ev := windowOf(t, a, "o::TestA").Evidence
	var kinds []string
	for _, m := range ev.Misses {
		kinds = append(kinds, m.Kind)
	}
	require.ElementsMatch(t, []string{models.MissOutsideTest, models.MissInTest}, kinds, "the miss between tests is charged to the next test")
}

// A protocol-phase mock an old mapping lists stays in the pool: every test
// is served it, as often as asked, and no test is failed for not making it.
func TestAMappedProtocolPhaseMockStaysInThePool(t *testing.T) {
	token := pingMock("mock-1", "/token", "tok", t0)
	token.Spec.Metadata = map[string]string{"type": "config"}
	a, p := exactAgent(t,
		[]*models.Mock{token, pingMock("mock-2", "/a", "a", t0.Add(time.Second))},
		map[string][]string{"o::TestA": {"mock-1", "mock-2"}, "o::TestB": {}}, map[string][]string{})
	got, e := runTest(t, a, p, "TestB", "/token", "/token")
	require.Equal(t, []string{"200 tok", "200 tok"}, got)
	require.Empty(t, e.Action, e.Reason)
	got, e = runTest(t, a, p, "TestA", "/a")
	require.Equal(t, []string{"200 a"}, got)
	require.Empty(t, e.Action, "a protocol-phase mock is no test mock: %s", e.Reason)
}

// A skipped test's recording never shifts to the next test (keploy #4745's
// F20 scenarios): the next test making the same call is answered by its own
// recording, and one call more than that is an extra call, never the
// skipped test's mock.
func TestASkippedTestsRecordingNeverShiftsToTheNextTest(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/n", "skipped", t0), pingMock("mock-2", "/n", "next", t0.Add(time.Second))},
		map[string][]string{"o::TestSkipped": {"mock-1"}, "o::TestNext": {"mock-2"}}, map[string][]string{})
	require.NoError(t, a.SetScopeSkips(context.Background(), map[string]string{"o::TestSkipped": "flaky: replay_only"}))
	b, serr := a.ProvenBegin(context.Background(), begin("TestSkipped"))
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionSkip, b.Action)
	got, e := runTest(t, a, p, "TestNext", "/n", "/n")
	require.Equal(t, "200 next", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), "the skipped test's recording answered the next test: %s", got[1])
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "was called 2 times, recorded 1")
}

// keployTestAgent is a keploy test replay's agent on a real mock manager,
// holding mocks, staged by the set's staging filter with pool as the set's
// pool (models.ExactServing).
func keployTestAgent(t *testing.T, mocks []*models.Mock, pool []string) (*Agent, *realProxy) {
	t.Helper()
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newRealProxy()
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, mocks))
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(),
		Exact: &models.ExactServing{Stage: true, Pool: pool},
	}))
	return a, p
}

// keployTestCase opens test case name, owning owned, makes its calls, serves
// the pool again, and returns what each call got and the misses it made.
func keployTestCase(t *testing.T, a *Agent, p *realProxy, name string, owned []string, paths ...string) ([]string, []models.UnmatchedCall) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: name, Owned: owned}}))
	var got []string
	for _, path := range paths {
		code, body := p.call(t, path)
		got = append(got, fmt.Sprintf("%d %s", code, strings.TrimSpace(body)))
	}
	misses, err := p.GetMockErrors(ctx)
	require.NoError(t, err)
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}))
	return got, misses
}

// keploy test under the switch: each test case is served exactly its own
// mocks, out of recorded order too; one call more is an extra call; a test
// case its mapping omits owns none; and between test cases the set's pool
// answers, never a test case's mock.
func TestKeployTestServesEachTestCaseExactlyItsOwnMocks(t *testing.T) {
	a, p := keployTestAgent(t,
		[]*models.Mock{
			pingMock("mock-1", "/ping", "first", t0),
			pingMock("mock-2", "/ping", "second", t0.Add(time.Second)),
			pingMock("mock-3", "/health", "ok", t0.Add(-time.Second)),
		}, []string{"mock-3"})

	got, misses := keployTestCase(t, a, p, "test-2", []string{"mock-2"}, "/ping", "/ping")
	require.Equal(t, "200 second", got[0], "a test case run out of recorded order is answered by its own mock")
	require.True(t, strings.HasPrefix(got[1], "502"), got[1])
	require.Len(t, misses, 1)
	require.Equal(t, models.MissExtraCall, misses[0].Kind)

	got, misses = keployTestCase(t, a, p, "test-1", []string{"mock-1"}, "/ping")
	require.Equal(t, []string{"200 first"}, got)
	require.Empty(t, misses)

	got, misses = keployTestCase(t, a, p, "test-3", nil, "/ping", "/health")
	require.True(t, strings.HasPrefix(got[0], "502") && strings.HasPrefix(got[1], "502"), "a test case that owns nothing was answered: %v", got)
	require.Len(t, misses, 2)

	for i := 0; i < 2; i++ {
		code, body := p.call(t, "/health")
		require.Equal(t, 200, code, "the pool answers between test cases")
		require.Equal(t, "ok", body)
	}
	code, _ := p.call(t, "/ping")
	require.Equal(t, 502, code, "a test case's mock answered between test cases")
}

// A test's end waits until its calls are settled: a call the application
// makes after the test's harness said it ended, while the proxy still holds
// the test's work, is the test's own, and the end judges it so, whatever the
// load. Without the wait the end would judge the mock unconsumed.
func TestAnEndJudgesTheCallsStillInFlight(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/audit", "ok", t0)},
		map[string][]string{"o::TestAsync": {"mock-1"}}, map[string][]string{})
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestAsync"))
	require.Nil(t, serr)
	p.set(0, 1) // the application's call is in the proxy's hands
	ended := make(chan models.ScopeEndResp, 1)
	go func() {
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestAsync", CanSkip: true, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
		ended <- e
	}()
	code, _ := p.call(t, "/audit")
	require.Equal(t, 200, code)
	select {
	case e := <-ended:
		t.Fatalf("the end judged while the test's call was in flight: %+v", e)
	default:
	}
	p.set(0, 0) // answered: nothing in the proxy's hands
	e := <-ended
	require.Empty(t, e.Action, e.Reason)
}

// A test whose calls keploy cannot count is not judged settled: its end
// fails it, and says why.
func TestAnEndThatCannotCountFailsTheTest(t *testing.T) {
	a, _ := exactAgent(t, nil, map[string][]string{"o::TestA": {}}, map[string][]string{})
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	a.Hooks.(*countHooks).err = models.ErrProxyConnsUnsupported
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.NotNil(t, serr)
	w := evidenceOf(t, a, "o::TestA")
	require.Contains(t, w.Reason, "could not tell that every call of o::TestA reached it")
}
