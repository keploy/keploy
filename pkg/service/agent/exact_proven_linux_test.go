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

// servingProxy is a proxy around a real mock manager. It keeps the misses the
// matcher reports, each filed as the proxy files it (models.Miss*).
type servingProxy struct {
	coreAgent.Proxy
	mm     *proxyPkg.MockManager
	mu     sync.Mutex
	misses []models.UnmatchedCall
}

func newServingProxy() *servingProxy {
	return &servingProxy{mm: proxyPkg.NewMockManager(nil, nil, zap.NewNop())}
}

func (p *servingProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, s, e time.Time) error {
	p.mm.SetMocksWithWindow(f, u, s, e)
	return nil
}
func (p *servingProxy) SetMocks(_ context.Context, f, u []*models.Mock) error {
	p.mm.SetFilteredMocks(f)
	p.mm.SetUnFilteredMocks(u)
	return nil
}
func (p *servingProxy) ServeExactTest(test string, owned []*models.Mock) error {
	p.mm.ServeExactTest(test, owned)
	return nil
}
func (p *servingProxy) ServeExactPool(key string, names []string) error {
	p.mm.ServeExactPool(key, names)
	return nil
}
func (p *servingProxy) ExactTestOutcome() (string, []string, []string, bool) {
	return p.mm.ExactTestOutcome()
}
func (p *servingProxy) ResetStatefulCursors()      { p.mm.ResetStatefulCursors() }
func (p *servingProxy) ContinueTestErrorCapture()  {}
func (p *servingProxy) SetMappedUniverse([]string) {}
func (p *servingProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.misses
	p.misses = nil
	return out, nil
}

// noteMiss files a miss the matcher reported, as the proxy does.
func (p *servingProxy) noteMiss(err error) {
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
func (p *servingProxy) call(t *testing.T, path string) (int, string) {
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
func exactAgent(t *testing.T, mocks []*models.Mock, table, pool map[string][]string) (*Agent, *servingProxy) {
	t.Helper()
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
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
func runTest(t *testing.T, a *Agent, p *servingProxy, name string, paths ...string) ([]string, models.ScopeEndResp) {
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
