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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// These tests run the proven wire against the proxy's real mock manager and
// the real HTTP matcher: what a replayed test is answered with, end to end.

// servingProxy is a proxy around a real mock manager. It keeps the misses the
// matcher reports, each filed as the proxy files it (models.Miss*).
type servingProxy struct {
	coreAgent.Proxy
	// The calls each test makes are answered before its end (the tests'
	// calls are synchronous): every call is in hand when a test settles.
	settledProxy
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
func (p *servingProxy) ServeExactPoolAhead(key string, names []string, ahead []*models.Mock) error {
	p.mm.ServeExactPool(key, names, ahead...)
	return nil
}
func (p *servingProxy) ServeExactWorker(names []string) error {
	p.mm.ServeExactWorker(names)
	return nil
}
func (p *servingProxy) RestartExactKey(key string, names []string) error {
	p.mm.RestartExactKey(key, names)
	return nil
}
func (p *servingProxy) ExactTestOutcome() (string, []string, []string, bool) {
	return p.mm.ExactTestOutcome()
}
func (p *servingProxy) SetUnrequired(marks models.BackgroundMarks) error {
	p.mm.SetUnrequired(marks)
	return nil
}
func (p *servingProxy) ExactTestUnusedBackground() (string, []string, bool) {
	return p.mm.ExactTestUnusedBackground()
}
func (p *servingProxy) ServeExactAside(from string, mocks []*models.Mock) error {
	p.mm.ServeExactAside(from, mocks)
	return nil
}
func (p *servingProxy) ServeExactAsideOnDisk(from string, names []string) error {
	p.mm.ServeExactAsideOnDisk(from, names, nil)
	return nil
}
func (p *servingProxy) SetRecordingSource(src func(kind models.Kind, keys []string, visit func(name string, load func() (*models.Mock, error)) bool) error) error {
	p.mm.SetRecordingSource(src)
	return nil
}
func (p *servingProxy) ExactTestServedAside() (string, []string, bool) {
	return p.mm.ExactTestServedAside()
}
func (p *servingProxy) ResetStatefulCursors()      { p.mm.ResetStatefulCursors() }
func (p *servingProxy) ContinueTestErrorCapture()  {}
func (p *servingProxy) WithdrawTestErrorCapture()  {}
func (p *servingProxy) SetMappedUniverse([]string) {}
func (p *servingProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.misses
	p.misses = nil
	return out, nil
}

// noteMiss files a miss the matcher reported, as the proxy does
// (sendMockNotFoundError, GetMockErrors): by the view served when it is
// filed, an extra call as its matcher found it, with its closest recording
// and the fields its report says differ, and stamped with when its report
// was built.
func (p *servingProxy) noteMiss(err error) {
	uc := models.UnmatchedCall{Protocol: "HTTP", Kind: models.MissOutsideTest, At: time.Now()}
	if p.mm.WindowSnapshot().Active {
		uc.Kind = models.MissInTest
	}
	var r interface {
		MismatchReport() *models.MockMismatchReport
	}
	if errors.As(err, &r) && r.MismatchReport() != nil {
		rep := r.MismatchReport()
		uc.ActualSummary, uc.Destination, uc.Differs = rep.ActualSummary, rep.Destination, rep.Differs
		uc.ClosestMock = rep.ClosestMock
		if !rep.At.IsZero() {
			uc.At = rep.At
		}
		if rep.MissKind == models.MissExtraCall {
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
	a, p := unclaimedExactAgent(t, mocks, table, pool)
	claimSelf(t, a)
	return a, p
}

// unclaimedExactAgent is exactAgent before any test process claimed it.
func unclaimedExactAgent(t *testing.T, mocks []*models.Mock, table, pool map[string][]string) (*Agent, *servingProxy) {
	t.Helper()
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, mocks))
	require.NoError(t, a.SetScopeTable(ctx, table))
	require.NoError(t, a.SetScopePool(ctx, pool))
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

// Two calls of one shape, each made more often than recorded, are each said
// with the recording it repeats and their own counts, never summed as one
// call: a timer's call and the test's own may share a route.
func TestCallsOfOneShapeMadeAgainAreEachSaidWithTheRecordingTheyRepeat(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/items/1001", "a", t0), pingMock("mock-2", "/items/1002", "b", t0.Add(time.Second))},
		map[string][]string{"o::TestItems": {"mock-1", "mock-2"}}, map[string][]string{})
	got, e := runTest(t, a, p, "TestItems", "/items/1001", "/items/1002", "/items/1001", "/items/1002", "/items/1002")
	require.Equal(t, []string{"200 a", "200 b"}, got[:2])
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "was called 2 times, recorded 1 (it repeats mock-1)")
	require.Contains(t, e.Reason, "was called 3 times, recorded 1 (it repeats mock-2)")
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
	require.Contains(t, e.Reason, "GET /n to 127.0.0.1:18093 was called 3 times, recorded 2 (it repeats mock-2)", "the miss names the copy recorded last")
	ev := windowOf(t, a, "o::TestCounter").Evidence
	require.NotNil(t, ev)
	require.Len(t, ev.Misses, 1)
	require.Equal(t, models.MissExtraCall, ev.Misses[0].Kind)
	require.Equal(t, "mock-2", ev.Misses[0].ClosestMock, "judging reads the recording an extra call repeats off its evidence")
	require.Equal(t, 2, ev.Misses[0].Recorded)
	require.Empty(t, ev.Unconsumed)
}

// A call a test makes after the recording closest to it answered is
// diagnosed against that recording all the same (keploy S1's "(path
// differs)"): answered, the recording is in no read, and the miss named no
// recording at all ("no_mocks"; enterprise lane 2's replay-miss on keploy
// v3.6.127). The answered recording is only compared: the call is still a
// miss in the test, not an extra call, and its recording was consumed once.
func TestAMissAfterItsRecordingAnsweredNamesWhatDiffers(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/stock/book", "3", t0)},
		map[string][]string{"o::TestMiss": {"mock-1"}}, map[string][]string{})
	got, e := runTest(t, a, p, "TestMiss", "/stock/book", "/reserve/book")
	require.Equal(t, "200 3", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), "the changed call has no recorded answer: %s", got[1])
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "GET /reserve/book to 127.0.0.1:18093 had no recorded answer (path differs)")
	ev := windowOf(t, a, "o::TestMiss").Evidence
	require.NotNil(t, ev)
	require.Len(t, ev.Misses, 1)
	require.Equal(t, models.MissInTest, ev.Misses[0].Kind)
	require.Equal(t, []string{"path"}, ev.Misses[0].Differs)
	require.Empty(t, ev.Unconsumed)
}

// A test ID the run has already seen (a retry, -count=2, a re-run) begins
// afresh (keploy #4761's ResetConsumedMocks, as exact serving does it): each
// begin serves the test its own mocks again, from its first recorded call,
// and ConsumeExact holds per attempt, so one call more than recorded is an
// extra call in any attempt. What an attempt left unconsumed does not carry
// into the next, and no attempt re-arms another test's consumed mock.
func TestARepeatedTestIDIsServedItsOwnMocksAfreshEachAttempt(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{
			pingMock("mock-1", "/n", "one", t0),
			pingMock("mock-2", "/n", "two", t0.Add(time.Second)),
			pingMock("mock-3", "/o", "other", t0.Add(2*time.Second)),
		},
		map[string][]string{"o::TestCounter": {"mock-1", "mock-2"}, "o::TestOther": {"mock-3"}}, map[string][]string{})
	got, e := runTest(t, a, p, "TestOther", "/o")
	require.Equal(t, []string{"200 other"}, got)
	require.Empty(t, e.Action, e.Reason)

	for _, step := range []struct {
		calls []string
		want  []string
		// the attempt's end: "" passes, else the text its reason holds
		fails string
		kind  string
	}{
		{calls: []string{"/n", "/n"}, want: []string{"200 one", "200 two"}},
		{calls: []string{"/n", "/n"}, want: []string{"200 one", "200 two"}},
		{calls: []string{"/n", "/n", "/n"}, want: []string{"200 one", "200 two", "502"}, fails: "GET /n to 127.0.0.1:18093 was called 3 times, recorded 2", kind: models.MissExtraCall},
		{calls: []string{"/n"}, want: []string{"200 one"}, fails: "recorded call GET 127.0.0.1:18093/n (mock-2) was never made"},
		{calls: []string{"/n", "/n", "/o"}, want: []string{"200 one", "200 two", "502"}, fails: "GET /o to 127.0.0.1:18093 had no recorded answer", kind: models.MissInTest},
		{calls: []string{"/n", "/n", "/n"}, want: []string{"200 one", "200 two", "502"}, fails: "was called 3 times, recorded 2", kind: models.MissExtraCall},
		{calls: []string{"/n", "/n"}, want: []string{"200 one", "200 two"}},
	} {
		got, e := runTest(t, a, p, "TestCounter", step.calls...)
		require.Len(t, got, len(step.want))
		for i, w := range step.want {
			require.True(t, strings.HasPrefix(got[i], w), "calls %v: call %d got %q, want %q", step.calls, i, got[i], w)
		}
		if step.fails == "" {
			require.Empty(t, e.Action, "calls %v: %s", step.calls, e.Reason)
			continue
		}
		require.Equal(t, models.ScopeActionFail, e.Action, "calls %v", step.calls)
		require.Contains(t, e.Reason, step.fails, "calls %v", step.calls)
		if step.kind != "" {
			ev := evidenceOf(t, a, "o::TestCounter")
			require.Len(t, ev.Misses, 1, "calls %v", step.calls)
			require.Equal(t, step.kind, ev.Misses[0].Kind, "calls %v", step.calls)
		}
	}

	got, e = runTest(t, a, p, "TestOther", "/o")
	require.Equal(t, []string{"200 other"}, got, "a test run again is served its own mock again")
	require.Empty(t, e.Action, e.Reason)
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
// mock answers there. Inside a test a data-plane pool recording answers only
// a call none of the test's own does, by exact match, unchecked, never
// required, named on the test's evidence, and once in the staging: a
// start-up job or a timer whose call lands in a test's window at replay
// fails no test (founder: serve, never require). The miss between tests is
// the application's background work: charged to no test, the next one
// included. Before the pool answered in a test, /health failed TestA.
func TestThePoolAnswersBetweenTestsAndOnceInATest(t *testing.T) {
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
	require.Equal(t, []string{"200 a", "200 ok"}, got, "the pool's recording of /health did not answer it inside TestA")
	require.Empty(t, e.Action, "a pool recording served in a test failed it: %s", e.Reason)
	// A recording the calls between tests reuse still answers there, and
	// answers no second test.
	code, body := p.call(t, "/health")
	require.Equal(t, 200, code, "a reused pool recording a test was served answers between tests no more")
	require.Equal(t, "ok", body)
	got, e = runTest(t, a, p, "TestB", "/health")
	require.True(t, strings.HasPrefix(got[0], "502"), "a pool recording answered a second test: %s", got[0])
	require.Equal(t, models.ScopeActionFail, e.Action)
	ev := windowOf(t, a, "o::TestA").Evidence
	require.Empty(t, ev.Misses, "a pool recording served in a test is a miss of it")
	require.Equal(t, []string{models.ServedAsideLine("mock-1", models.ServedFromBackground)}, ev.ServedAside,
		"the evidence names the pool recording the test was served")
	require.Contains(t, ev.ServedAside[0], "start-up and between-tests recordings (background)")
	bg := backgroundCharged(a)
	require.Len(t, bg, 1)
	require.Equal(t, models.MissOutsideTest, bg[0].Kind)
	require.Contains(t, bg[0].ActualSummary, "/a")
}

// A start-up job that outlives /healthz makes its calls inside the first
// kept test's window, after two quarantined tests (api-server's
// TestUnauthenticated, 1 replay in 8): /janitor, whose only recording is the
// set's start-up one (its pool), and /pat, whose recording is the
// quarantined TestQ1's. Each is served aside, unchecked, and the kept test
// passes, its evidence naming both. Before, /janitor got 502 and failed it
// with "re-record it", though its recording exists.
func TestAStartUpCallInsideTheFirstKeptTestsWindowIsServed(t *testing.T) {
	a, rp := exactAgent(t,
		[]*models.Mock{pingMock("mock-0", "/janitor", "j", t0.Add(-time.Second)), pingMock("mock-1", "/pat", "pat", t0), pingMock("mock-2", "/q", "q", t0.Add(time.Second)), pingMock("mock-3", "/k", "k", t0.Add(2*time.Second))},
		map[string][]string{"o::TestQ1": {"mock-1"}, "o::TestQ2": {"mock-2"}, "o::TestK": {"mock-3"}}, map[string][]string{"o": {"mock-0"}})
	ctx := context.Background()
	require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestQ1": "flaky/request_changed", "o::TestQ2": "keploy/unmocked_call"}, nil))
	for _, q := range []string{"TestQ1", "TestQ2"} {
		b, serr := a.ProvenBegin(ctx, begin(q))
		require.Nil(t, serr)
		require.Equal(t, models.ScopeActionSkip, b.Action)
	}
	got, e := runTest(t, a, rp, "TestK", "/k", "/janitor", "/pat")
	require.Equal(t, []string{"200 k", "200 j", "200 pat"}, got)
	require.Empty(t, e.Action, "TestK failed: %s", e.Reason)
	ev := windowOf(t, a, "o::TestK").Evidence
	require.Empty(t, ev.Misses)
	require.Equal(t, []string{
		models.ServedAsideLine("mock-0", models.ServedFromBackground),
		models.ServedAsideLine("mock-1", "o::TestQ1"),
	}, ev.ServedAside)
}

// What the pool does not serve in a test: a call that is not exactly a
// pool recording (the job's call changed) still misses and fails the test;
// a pool recording a call between tests consumed answers no test (one
// ledger); and one a test was served answers no later test.
func TestWhatThePoolDoesNotServeInATest(t *testing.T) {
	perTest := func(m *models.Mock) *models.Mock {
		m.Spec.Metadata = map[string]string{"type": "mocks"}
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		m.TestModeInfo.LifetimeDerived = true
		return m
	}
	t.Run("a call that is not exactly the recording misses", func(t *testing.T) {
		a, p := exactAgent(t,
			[]*models.Mock{pingMock("mock-0", "/janitor", "j", t0), pingMock("mock-1", "/a", "a", t0.Add(time.Second))},
			map[string][]string{"o::TestA": {"mock-1"}}, map[string][]string{"o": {"mock-0"}})
		code, _ := p.call(t, "/a")
		require.Equal(t, 502, code)
		got, e := runTest(t, a, p, "TestA", "/a", "/janitor/2")
		require.Equal(t, "200 a", got[0])
		require.True(t, strings.HasPrefix(got[1], "502"), got[1])
		require.Equal(t, models.ScopeActionFail, e.Action)
		require.Contains(t, e.Reason, "GET /janitor/{} to 127.0.0.1:18093 had no recorded answer (path differs)")
		require.Empty(t, windowOf(t, a, "o::TestA").Evidence.ServedAside)
	})
	t.Run("consumed between tests, it answers no test", func(t *testing.T) {
		a, p := exactAgent(t,
			[]*models.Mock{perTest(pingMock("mock-0", "/janitor", "j", t0)), pingMock("mock-1", "/a", "a", t0.Add(time.Second))},
			map[string][]string{"o::TestA": {"mock-1"}}, map[string][]string{"o": {"mock-0"}})
		code, body := p.call(t, "/janitor")
		require.Equal(t, 200, code)
		require.Equal(t, "j", body)
		code, _ = p.call(t, "/janitor")
		require.Equal(t, 502, code, "a consumed pool recording answered twice between tests")
		got, e := runTest(t, a, p, "TestA", "/a", "/janitor")
		require.Equal(t, "200 a", got[0])
		require.True(t, strings.HasPrefix(got[1], "502"), "a pool recording consumed between tests answered in a test: %s", got[1])
		require.Equal(t, models.ScopeActionFail, e.Action)
		require.Empty(t, windowOf(t, a, "o::TestA").Evidence.ServedAside)
	})
	t.Run("served in a test, it answers no later test", func(t *testing.T) {
		a, p := exactAgent(t,
			[]*models.Mock{perTest(pingMock("mock-0", "/janitor", "j", t0)), pingMock("mock-1", "/a", "a", t0.Add(time.Second)), pingMock("mock-2", "/b", "b", t0.Add(2*time.Second))},
			map[string][]string{"o::TestA": {"mock-1"}, "o::TestB": {"mock-2"}}, map[string][]string{"o": {"mock-0"}})
		got, e := runTest(t, a, p, "TestA", "/a", "/janitor", "/janitor")
		require.Equal(t, "200 a", got[0])
		require.Equal(t, "200 j", got[1])
		require.True(t, strings.HasPrefix(got[2], "502"), "a pool recording answered twice in one test: %s", got[2])
		require.Equal(t, models.ScopeActionFail, e.Action)
		got, e = runTest(t, a, p, "TestB", "/b", "/janitor")
		require.Equal(t, "200 b", got[0])
		require.True(t, strings.HasPrefix(got[1], "502"), "a pool recording a test was served answered a later test: %s", got[1])
		require.Equal(t, models.ScopeActionFail, e.Action)
		require.Empty(t, windowOf(t, a, "o::TestB").Evidence.ServedAside)
	})
}

// sqsEmpty is the answer enterprise SQS's parser writes for a ReceiveMessage
// no recording answers (generateEmptySQSMock): the queue has nothing.
const sqsEmpty = "{} (the parser's own empty answer)"

// poll answers path as a parser of enterprise SQS's shape does: it reads the
// session pool, takes the first recording of the call, and consumes it
// through a delete door (DeleteUnFilteredMock), reading again when the
// consume is refused; with no recording of the call it answers its own
// empty answer (sqsEmpty).
func (p *servingProxy) poll(t *testing.T, path string) string {
	t.Helper()
	for i := 0; i < 100; i++ {
		mocks, err := p.mm.GetSessionScopedMocks()
		require.NoError(t, err)
		var hit *models.Mock
		for _, mk := range mocks {
			if mk.Spec.HTTPReq != nil && strings.HasSuffix(mk.Spec.HTTPReq.URL, path) {
				hit = mk
				break
			}
		}
		if hit == nil {
			return sqsEmpty
		}
		if p.mm.DeleteUnFilteredMock(*hit) {
			return hit.Spec.HTTPResp.Body
		}
	}
	t.Fatalf("%s: every consume was refused", path)
	return ""
}

// A recorded answer to a call made outside every test (a consumer's startup
// poll) is consumed once per staging, in recorded order (decision 31), and
// once the call's recordings are spent the parser answers it as it knows to
// (SQS: the queue is empty). It came back at every begin and end: the
// consumer received the seeded messages after every test, and the writes
// they drive, recorded once, missed (enterprise E-B4's sqs-localstack:
// ReceiveMessage mock-4 returned its 2 messages 7 times).
func TestAnAnswerOutsideEveryTestIsConsumedOncePerSet(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{
			pingMock("mock-4", "/receive", "2 messages", t0),
			pingMock("mock-16", "/receive", "empty", t0.Add(time.Second)),
			pingMock("mock-17", "/receive", "empty", t0.Add(2*time.Second)),
		},
		map[string][]string{}, map[string][]string{"o": {"mock-4", "mock-16", "mock-17"}})
	got := []string{p.poll(t, "/receive"), p.poll(t, "/receive"), p.poll(t, "/receive"), p.poll(t, "/receive")}
	for _, name := range []string{"TestA", "TestB", "TestC"} {
		_, e := runTest(t, a, p, name)
		require.Empty(t, e.Action, "%s failed: %s", name, e.Reason)
		got = append(got, p.poll(t, "/receive"), p.poll(t, "/receive"))
	}
	require.Equal(t, []string{"2 messages", "empty", "empty", sqsEmpty, sqsEmpty, sqsEmpty, sqsEmpty, sqsEmpty, sqsEmpty, sqsEmpty}, got,
		"the startup poll's recordings, once each in recorded order, then the parser's empty answer, across three tests: the messages are never delivered again")
}

// The answers spent outside every test are the staging's: a new staging
// (the next set of keploy test, or the next session) answers them afresh.
func TestANewStagingAnswersOutsideTestCallsAfresh(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-4", "/receive", "2 messages", t0), pingMock("mock-16", "/receive", "empty", t0.Add(time.Second))},
		map[string][]string{}, map[string][]string{"o": {"mock-4", "mock-16"}})
	require.Equal(t, "2 messages", p.poll(t, "/receive"))
	require.Equal(t, "empty", p.poll(t, "/receive"))
	require.Equal(t, sqsEmpty, p.poll(t, "/receive"), "spent for the staging")
	require.NoError(t, a.UpdateMockParams(context.Background(), models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now()}))
	require.Equal(t, "2 messages", p.poll(t, "/receive"), "a new staging starts the sequence afresh")
}

// A test process that claims its key again in the session (a retried
// package, --rerun-fails) is a new scope: its calls outside every test start
// afresh, the recordings its earlier run consumed answer again, and its
// HTTP sequences start over (decision 31, as ruled for re-runs). It found
// its startup recordings spent, and its startup calls missed. A claim the
// same process makes again starts nothing afresh.
func TestAKeyClaimedAgainStartsItsOutsideCallsAfresh(t *testing.T) {
	a, p := unclaimedExactAgent(t,
		[]*models.Mock{
			pingMock("mock-4", "/receive", "2 messages", t0),
			pingMock("mock-16", "/receive", "empty", t0.Add(time.Second)),
			pingMock("mock-1", "/n", "one", t0.Add(2*time.Second)),
			pingMock("mock-2", "/n", "two", t0.Add(3*time.Second)),
		},
		map[string][]string{}, map[string][]string{"o": {"mock-4", "mock-16", "mock-1", "mock-2"}})
	ctx := context.Background()
	first := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "o"}, first.Process.Pid)
	require.Nil(t, serr)
	call := func(path string) string {
		code, body := p.call(t, path)
		return fmt.Sprintf("%d %s", code, strings.TrimSpace(body))
	}
	require.Equal(t, []string{"2 messages", "empty", sqsEmpty}, []string{p.poll(t, "/receive"), p.poll(t, "/receive"), p.poll(t, "/receive")}, "the first run")
	require.Equal(t, []string{"200 one", "200 two"}, []string{call("/n"), call("/n")}, "the first run's HTTP sequence")
	kill(t, first)

	claimSelf(t, a) // the package run again: the same key, a new process
	require.Equal(t, []string{"2 messages", "empty"}, []string{p.poll(t, "/receive"), p.poll(t, "/receive")}, "the run again answers its startup calls afresh")
	require.Equal(t, "200 one", call("/n"), "the run again starts its HTTP sequence over")

	claimSelf(t, a) // the same process claims again: not a new scope
	require.Equal(t, sqsEmpty, p.poll(t, "/receive"), "a claim the same process repeats starts nothing afresh")
}

// HTTP's own parser saturates a call outside every test itself: its stateful
// cursor (the strict pick under the switch) answers the call's recordings
// once each in recorded order, then its last again, per endpoint and per
// claim key, across begins and ends, and starts afresh at the next staging.
// The manager gives it no last answer: the parser knows the call.
func TestHTTPOutsideEveryTestAnswersOnceThenItsLast(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{
			pingMock("mock-1", "/n", "one", t0),
			pingMock("mock-2", "/n", "two", t0.Add(time.Second)),
			pingMock("mock-3", "/n", "three", t0.Add(2*time.Second)),
			pingMock("mock-4", "/m", "other", t0.Add(3*time.Second)),
		},
		map[string][]string{}, map[string][]string{"o": {"mock-1", "mock-2", "mock-3", "mock-4"}})
	var got []string
	call := func(path string) {
		code, body := p.call(t, path)
		got = append(got, fmt.Sprintf("%d %s", code, strings.TrimSpace(body)))
	}
	call("/n")
	call("/n")
	for _, name := range []string{"TestA", "TestB"} {
		_, e := runTest(t, a, p, name)
		require.Empty(t, e.Action, "%s failed: %s", name, e.Reason)
		call("/n")
		call("/m")
	}
	require.Equal(t, []string{"200 one", "200 two", "200 three", "200 other", "200 three", "200 other"}, got,
		"once each in recorded order, then the last again, across tests; another endpoint is its own")
}

// In a test, a protocol-phase mock a parser consumes through a delete door is
// gone for the rest of the test and back at the next one, as before: the
// once-per-set rule is for calls made outside every test.
func TestAProtocolPhaseMockConsumedInATestIsBackAtTheNext(t *testing.T) {
	token := pingMock("mock-1", "/token", "tok", t0)
	token.Spec.Metadata = map[string]string{"type": "config"}
	a, p := exactAgent(t, []*models.Mock{token}, map[string][]string{}, map[string][]string{})
	for _, name := range []string{"TestA", "TestB"} {
		_, serr := a.ProvenBegin(context.Background(), begin(name))
		require.Nil(t, serr)
		require.Equal(t, "tok", p.poll(t, "/token"), "%s: the handshake answers", name)
		require.Equal(t, sqsEmpty, p.poll(t, "/token"), "%s: consumed in the test, it is gone for the rest of it (no recording answers)", name)
		_, serr = a.ProvenEnd(context.Background(), models.ScopeReq{Name: name, CanSkip: true, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
	}
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

// A skipped test's recording answers the next test's call beyond its own
// count only when it is exactly that call and no call took it yet: the set
// recorded the call once more, so the calls of that shape across the set
// never exceed what it recorded. Past the copies left, the call is an extra
// call (never a recording served again), and a recording of another call answers none.
//
// The shape is api-server's per-process first-use refresh of a vault PAT,
// which a set-aside test recorded at record (read the key, read its id, read
// the key again, stamp it used): at replay it runs in the window of the next
// test that uses the PAT, which recorded one read of the key itself (acceptance
// run 6: "called 2 times, recorded 1" where the set-aside test held the copy).
func TestASetAsideCopyAnswersACallBeyondTheTestsOwnCount(t *testing.T) {
	refresh := func(key string) []*models.Mock {
		return []*models.Mock{
			pingMock("aside-key", key, "key", t0),
			pingMock("aside-id", "/pat/id", "id", t0.Add(time.Second)),
			pingMock("aside-key-again", key, "key-again", t0.Add(2*time.Second)),
			pingMock("aside-used", "/pat/used", "used", t0.Add(3*time.Second)),
		}
	}
	for _, tc := range []struct {
		name, asideKey string
		calls, want    []string
		served         []string
		miss           string
	}{
		{
			name: "the refresh is served beside the test's own read", asideKey: "/pat/key",
			calls:  []string{"/k", "/pat/key", "/pat/id", "/pat/key", "/pat/used"},
			want:   []string{"200 k", "200 own-key", "200 id", "200 key", "200 used"},
			served: []string{"aside-id served from o::TestQ (set aside)", "aside-key served from o::TestQ (set aside)", "aside-used served from o::TestQ (set aside)"},
		},
		{
			name: "past every copy left the call is an extra call", asideKey: "/pat/key",
			calls:  []string{"/k", "/pat/key", "/pat/key", "/pat/key", "/pat/key"},
			want:   []string{"200 k", "200 own-key", "200 key", "200 key-again", "502"},
			served: []string{"aside-key served from o::TestQ (set aside)", "aside-key-again served from o::TestQ (set aside)"},
			miss:   "was called 4 times, recorded 3",
		},
		{
			name: "a copy that is not exactly the call answers none", asideKey: "/pat/key?v=2",
			calls: []string{"/k", "/pat/key", "/pat/key"},
			want:  []string{"200 k", "200 own-key", "502"},
			miss:  "was called 2 times, recorded 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mocks := append(refresh(tc.asideKey), pingMock("own-key", "/pat/key", "own-key", t0.Add(5*time.Second)), pingMock("mock-k", "/k", "k", t0.Add(6*time.Second)))
			a, rp := exactAgent(t, mocks,
				map[string][]string{"o::TestQ": {"aside-key", "aside-id", "aside-key-again", "aside-used"}, "o::TestK": {"own-key", "mock-k"}},
				map[string][]string{})
			ctx := context.Background()
			require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestQ": "flaky/request_changed"}, nil))
			b, serr := a.ProvenBegin(ctx, begin("TestQ"))
			require.Nil(t, serr)
			require.Equal(t, models.ScopeActionSkip, b.Action)
			got, e := runTest(t, a, rp, "TestK", tc.calls...)
			for i := range got {
				if tc.want[i] == "502" {
					require.True(t, strings.HasPrefix(got[i], "502"), "call %d (%s) was answered %s", i, tc.calls[i], got[i])
					continue
				}
				require.Equal(t, tc.want[i], got[i], "call %d (%s)", i, tc.calls[i])
			}
			ev := evidenceOf(t, a, "o::TestK")
			require.Equal(t, tc.served, ev.ServedAside)
			require.Empty(t, ev.Unconsumed, "the test made its own calls; a set-aside recording is never its to make")
			if tc.miss == "" {
				require.Empty(t, e.Action, e.Reason)
				require.Empty(t, ev.Misses)
				return
			}
			require.Equal(t, models.ScopeActionFail, e.Action)
			require.Contains(t, e.Reason, tc.miss)
			require.Len(t, ev.Misses, 1)
			require.Equal(t, models.MissExtraCall, ev.Misses[0].Kind)
		})
	}
}

// keploy #4745's F20 scenarios under ConsumeExact. Tests whose recorded
// windows overlap (concurrent incoming requests) each made the same call,
// recorded at 6s and 8s, inside both windows. The mapping gives each
// recording to one test: each test is answered by its own, in either order,
// never by the one the other test was served, and a second call is an extra
// call, not the last recording served again.
func TestOverlappingRecordedWindowsNeverReServe(t *testing.T) {
	for _, order := range [][]string{{"T1", "T2"}, {"T2", "T1"}} {
		t.Run(strings.Join(order, "-then-"), func(t *testing.T) {
			a, p := exactAgent(t,
				[]*models.Mock{
					pingMock("head-6s", "/bucket/set-1/mocks.yaml", "6s", t0.Add(6*time.Second)),
					pingMock("head-8s", "/bucket/set-1/mocks.yaml", "8s", t0.Add(8*time.Second)),
				},
				map[string][]string{"o::T1": {"head-6s"}, "o::T2": {"head-8s"}}, map[string][]string{})
			want := map[string]string{"T1": "200 6s", "T2": "200 8s"}
			for _, name := range order {
				got, e := runTest(t, a, p, name, "/bucket/set-1/mocks.yaml", "/bucket/set-1/mocks.yaml")
				require.Equal(t, want[name], got[0], name)
				require.True(t, strings.HasPrefix(got[1], "502"), "%s's second call was served again: %s", name, got[1])
				require.Equal(t, models.ScopeActionFail, e.Action, name)
				ev := evidenceOf(t, a, "o::"+name)
				require.Len(t, ev.Misses, 1, name)
				require.Equal(t, models.MissExtraCall, ev.Misses[0].Kind, name)
			}
		})
	}
}

// Past its own recordings a test is never served another's or its last
// again (keploy #4745's F20 scenarios under ConsumeExact); the set's
// recording of the call made between tests answers test2's third identical
// call, once in the staging (the pool in a test); test3, which recorded no
// such call, misses it, since that copy answered already; and the pool
// answers it between tests, where it is reused.
func TestPastItsRecordingsATestIsServedNothingElse(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{
			pingMock("test1-404", "/obj", "test1", t0.Add(500*time.Millisecond)),
			pingMock("test2-a", "/obj", "test2-a", t0.Add(2200*time.Millisecond)),
			pingMock("test2-b", "/obj", "test2-b", t0.Add(2600*time.Millisecond)),
			pingMock("between-tests", "/obj", "between", t0.Add(4*time.Second)),
		},
		map[string][]string{"o::test1": {"test1-404"}, "o::test2": {"test2-a", "test2-b"}, "o::test3": {}},
		map[string][]string{"o": {"between-tests"}})
	got, e := runTest(t, a, p, "test2", "/obj", "/obj", "/obj")
	require.Equal(t, "200 test2-a", got[0], "test1 never ran; its recording is not test2's")
	require.Equal(t, "200 test2-b", got[1])
	// Past its own, the set's recording of the call made between tests
	// answers it once, unchecked and never required (the pool in a test).
	require.Equal(t, "200 between", got[2], "past its recordings test2 was not served the pool's copy of the call")
	require.Empty(t, e.Action, e.Reason)
	require.Equal(t, []string{models.ServedAsideLine("between-tests", models.ServedFromBackground)}, evidenceOf(t, a, "o::test2").ServedAside)

	got, e = runTest(t, a, p, "test3", "/obj")
	require.True(t, strings.HasPrefix(got[0], "502"), "test3 recorded no such call and was served %s", got[0])
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Equal(t, models.MissInTest, evidenceOf(t, a, "o::test3").Misses[0].Kind)

	code, body := p.call(t, "/obj")
	require.Equal(t, 200, code, "the pool answers between tests")
	require.Equal(t, "between", body)
}

// keploy #4761's isInitialStaging finding, under exact serving. On main a
// test's begin re-staged its mocks from BaseTime, and SetMocksWithWindow,
// keying initial staging on the time alone, filed them in the startup tier:
// the per-test tier was empty, no window was active and no test had fired,
// so a tier-routing parser (Postgres v3, Mongo v2) served the test from its
// startup engine. Under exact serving a begin re-stages nothing: the test's
// per-test mocks are the per-test tier, the window is active, and a staging
// that lands mid-set while the test is open changes none of it, re-arms
// nothing the test consumed, and shows no other test's mock.
func TestAMidSetStagingLeavesTheOpenTestItsPerTestMocks(t *testing.T) {
	perTest := func(m *models.Mock) *models.Mock {
		m.Spec.Metadata = map[string]string{"type": "mocks"}
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		m.TestModeInfo.LifetimeDerived = true
		return m
	}
	a, p := exactAgent(t,
		[]*models.Mock{perTest(pingMock("mock-a", "/a", "a", t0)), perTest(pingMock("mock-b", "/b", "b", t0.Add(3*time.Second)))},
		map[string][]string{"o::TestA": {"mock-a"}, "o::TestB": {"mock-b"}}, map[string][]string{})
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	tier := func(when string) {
		t.Helper()
		pt, err := p.mm.GetPerTestMocksInWindow()
		require.NoError(t, err)
		require.Equal(t, []string{"mock-a"}, servedNames(pt), "%s: the per-test tier", when)
		require.True(t, p.mm.IsTestWindowActive(), "%s: no window is active while TestA is open", when)
		require.True(t, p.mm.HasFirstTestFired(), "%s: no test has fired while TestA is open", when)
	}
	tier("after the begin")
	code, body := p.call(t, "/a")
	require.Equal(t, 200, code)
	require.Equal(t, "a", body)

	// The mid-set staging main's per-test begin made, from BaseTime.
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now()}))
	pt, err := p.mm.GetPerTestMocksInWindow()
	require.NoError(t, err)
	require.Empty(t, servedNames(pt), "after the staging, TestA's consumed mock is back in the per-test tier")
	require.True(t, p.mm.IsTestWindowActive(), "the staging closed TestA's window")
	code, _ = p.call(t, "/a")
	require.Equal(t, 502, code, "the staging re-armed TestA's consumed mock")
	code, _ = p.call(t, "/b")
	require.Equal(t, 502, code, "the staging showed TestB's mock inside TestA")
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	ev := evidenceOf(t, a, "o::TestA")
	var kinds []string
	for _, m := range ev.Misses {
		kinds = append(kinds, m.Kind)
	}
	require.ElementsMatch(t, []string{models.MissExtraCall, models.MissInTest}, kinds)

	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	pt, err = p.mm.GetPerTestMocksInWindow()
	require.NoError(t, err)
	require.Equal(t, []string{"mock-b"}, servedNames(pt), "TestB's per-test tier after a mid-set staging")
	code, body = p.call(t, "/b")
	require.Equal(t, 200, code)
	require.Equal(t, "b", body)
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Empty(t, e.Action, e.Reason)
}

// keployTestAgent is a keploy test replay's agent on a real mock manager,
// holding mocks, staged by the set's staging filter with pool as the set's
// pool (models.ExactServing).
func keployTestAgent(t *testing.T, mocks []*models.Mock, pool []string) (*Agent, *servingProxy) {
	t.Helper()
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
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
func keployTestCase(t *testing.T, a *Agent, p *servingProxy, name string, owned []string, paths ...string) ([]string, []models.UnmatchedCall) {
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

	// A test case that owns nothing is answered no other test case's mock;
	// the pool's recording of /health answers it once (the pool in a test).
	got, misses = keployTestCase(t, a, p, "test-3", nil, "/ping", "/health")
	require.True(t, strings.HasPrefix(got[0], "502"), "a test case that owns nothing was answered another's mock: %v", got)
	require.Equal(t, "200 ok", got[1], "the pool's recording did not answer a test case's call")
	require.Len(t, misses, 1)

	for i := 0; i < 2; i++ {
		code, body := p.call(t, "/health")
		require.Equal(t, 200, code, "the pool answers between test cases")
		require.Equal(t, "ok", body)
	}
	code, _ := p.call(t, "/ping")
	require.Equal(t, 502, code, "a test case's mock answered between test cases")
}

// keploy test under the switch: a call in flight when test-2 began (recorded
// in the gap before its window, answered once it had begun) is test-2's, and
// the replayed app may ask for it in the gap, as it did at record. The pool
// call before test-2 names it (models.ExactServing.Ahead): it answers there
// once, and test-2 counts it as answered when it opens. A gap call made again
// finds no mock, and so does one in test-2.
func TestKeployTestAnswersAGapCallInFlightAsTheNextTestCases(t *testing.T) {
	a, p := keployTestAgent(t,
		[]*models.Mock{
			pingMock("fetch", "/fetch", "o2", t0),
			pingMock("own-2", "/own", "two", t0.Add(time.Second)),
			pingMock("health", "/health", "ok", t0.Add(-time.Second)),
		}, []string{"health"})
	ctx := context.Background()

	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Ahead: []string{"fetch"}}}))
	code, body := p.call(t, "/fetch")
	require.Equal(t, 200, code, "the call in flight when test-2 began is answered in the gap before it")
	require.Equal(t, "o2", body)
	code, _ = p.call(t, "/fetch")
	require.Equal(t, 502, code, "the call in flight answered twice in the gap")
	code, _ = p.call(t, "/health")
	require.Equal(t, 200, code, "the pool still answers beside it")

	got, _ := keployTestCase(t, a, p, "test-2", []string{"fetch", "own-2"}, "/own", "/fetch")
	require.Equal(t, "200 two", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), "the call in flight answered again in test-2: %s", got[1])
	test, consumed, unconsumed, ok := p.mm.ExactTestOutcome()
	require.True(t, ok)
	require.Equal(t, "test-2", test)
	require.Equal(t, []string{"fetch", "own-2"}, consumed, "the gap's answer counts as test-2's, first")
	require.Empty(t, unconsumed)

	code, _ = p.call(t, "/fetch")
	require.Equal(t, 502, code, "a pool call that names nothing ahead answers no test case's mock")
}

// keploy test runs a test case again (a retry) afresh: each run is served
// its own mocks from the first, and one call more than recorded is an extra
// call in any run.
func TestKeployTestRunsATestCaseAgainAfresh(t *testing.T) {
	a, p := keployTestAgent(t,
		[]*models.Mock{pingMock("mock-1", "/n", "one", t0), pingMock("mock-2", "/n", "two", t0.Add(time.Second))}, nil)
	owned := []string{"mock-1", "mock-2"}
	for run, calls := range []int{2, 3, 2, 3} {
		paths := make([]string, calls)
		for i := range paths {
			paths[i] = "/n"
		}
		got, misses := keployTestCase(t, a, p, "test-1", owned, paths...)
		require.Equal(t, []string{"200 one", "200 two"}, got[:2], "run %d", run)
		if calls == 2 {
			require.Empty(t, misses, "run %d", run)
			continue
		}
		require.True(t, strings.HasPrefix(got[2], "502"), "run %d: %s", run, got[2])
		require.Len(t, misses, 1, "run %d", run)
		require.Equal(t, models.MissExtraCall, misses[0].Kind, "run %d", run)
	}
}

// servedNames is the names of mocks, in order; never nil.
func servedNames(mocks []*models.Mock) []string {
	out := []string{}
	for _, m := range mocks {
		out = append(out, m.Name)
	}
	return out
}

// A mock the agent's store refused (variables this keploy cannot follow,
// said as it was refused) answers nothing: a test whose mapping lists it
// begins, owns the rest, the call it recorded misses, and a test that no
// longer makes that call fails for it all the same.
func TestAMockTheStoreRefusedIsOwnedByNoTest(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/a", "a", t0), pingMock("mock-2", "/b", "b", t0.Add(time.Second))},
		map[string][]string{"o::TestA": {"mock-1", "mock-2"}}, map[string][]string{})
	v, _ := a.clientMocks.Load(uint64(0))
	s := v.(*ClientMockStorage)
	s.mu.Lock()
	s.unfiltered = s.unfiltered[:1]
	s.noteRefused(pingMock("mock-2", "/b", "b", t0.Add(time.Second)))
	s.mu.Unlock()
	got, e := runTest(t, a, p, "TestA", "/a", "/b")
	require.Equal(t, "200 a", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), got[1])
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "GET /b to 127.0.0.1:18093 had no recorded answer")
	got, e = runTest(t, a, p, "TestA", "/a")
	require.Equal(t, []string{"200 a"}, got)
	require.Equal(t, models.ScopeActionFail, e.Action, "a test that made no call to the refused mock passed")
	require.Equal(t, []string{"mock-2"}, evidenceOf(t, a, "o::TestA").Unconsumed)
}

// A test that recorded no call is served no other test's mock, whatever
// lifetime the mock's kind and tag derive (UM lanes, enterprise main at
// v3.6.123): keploy mock record tags its HTTP mocks HTTP_CLIENT, which the lax
// kind fallback makes session-lifetime with cursor saturation, and t2, which
// recorded nothing, was served t1's already-answered mock and passed. Under
// the switch t2's call misses and fails it, whether the mapping lists t2 with
// no mocks or not at all.
func TestATestThatRecordedNoCallIsServedNoOtherTestsMock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		table map[string][]string
	}{
		{"t2 mapped with no mocks", map[string][]string{"o::t1": {"mock-0"}, "o::t2": {}}},
		{"t2 not in the mapping", map[string][]string{"o::t1": {"mock-0"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := pingMock("mock-0", "/c", "c", t0)
			m.Spec.Metadata = map[string]string{"type": "HTTP_CLIENT"}
			a, p := exactAgent(t, []*models.Mock{m}, tc.table, map[string][]string{})
			got, e := runTest(t, a, p, "t1", "/c")
			require.Equal(t, []string{"200 c"}, got)
			require.Empty(t, e.Action, e.Reason)
			got, e = runTest(t, a, p, "t2", "/c")
			require.True(t, strings.HasPrefix(got[0], "502"), "t2 recorded no call and was served %s", got[0])
			require.Equal(t, models.ScopeActionFail, e.Action)
			require.Contains(t, e.Reason, "GET /c to 127.0.0.1:18093 had no recorded answer")
			require.Equal(t, models.MissInTest, evidenceOf(t, a, "o::t2").Misses[0].Kind)
		})
	}
}

// inFlightProxy makes one more call of the open test as the end stops
// serving it: the test's last call, still in flight.
type inFlightProxy struct {
	*servingProxy
	t    *testing.T
	path string
}

func (p *inFlightProxy) ServeExactPool(key string, names []string) error {
	if err := p.servingProxy.ServeExactPool(key, names); err != nil {
		return err
	}
	if path := p.path; path != "" {
		p.path = ""
		p.call(p.t, path)
	}
	return nil
}

// A test's call that lands as its end stops serving it misses, fails the
// test, and is said to be made as the test ended, not before it began; a
// call made between tests is the application's background work: it fails
// no test, the next one included, and the run's misses carry it as
// background.
func TestACallLandingAsTheTestEndsIsSaidToBeMadeThen(t *testing.T) {
	a, rp := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/a", "a", t0), pingMock("mock-2", "/b", "b", t0.Add(time.Second)), pingMock("mock-3", "/c", "c", t0.Add(2*time.Second))},
		map[string][]string{"o::TestA": {"mock-1", "mock-2"}, "o::TestB": {"mock-3"}}, map[string][]string{})
	p := &inFlightProxy{servingProxy: rp, t: t}
	a.Proxy = p
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	code, _ := p.call(t, "/a")
	require.Equal(t, 200, code)
	p.path = "/b"
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "GET /b to 127.0.0.1:18093, made as this test ended, had no recorded answer")
	require.NotContains(t, e.Reason, "before this one began")

	code, _ = p.call(t, "/zzz") // between tests
	require.Equal(t, 502, code)
	got, e := runTest(t, a, p.servingProxy, "TestB", "/c")
	require.Equal(t, []string{"200 c"}, got)
	require.Empty(t, e.Action, e.Reason)
	bg := backgroundCharged(a)
	require.Len(t, bg, 1)
	require.Contains(t, bg[0].ActualSummary, "/zzz")
	require.False(t, bg[0].CountsAgainstTest())
}

// backgroundCharged is the misses the tests' ends drained that were made
// outside every test's window (models.UnmatchedCall.Background).
func backgroundCharged(a *Agent) []models.UnmatchedCall {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	var out []models.UnmatchedCall
	for _, m := range a.proven.charged {
		if m.Background {
			out = append(out, m)
		}
	}
	return out
}

// TestNonExistentApp's shape in api-server's acceptance run 3: the tests
// before a kept test are quarantined, so they open no window, and the
// application's timers and the call a quarantined test recorded are made
// before the kept test begins. Nothing answers them (the quarantined test's
// recording is served to no one), and the kept test still passes: a call
// made outside every test's window is charged to no test. The
// run's misses carry both, as background, for the set's report; a call the
// kept test makes that nothing answers still fails it.
func TestCallsBeforeAKeptTestAfterAQuarantinedOneFailNoTest(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(map[bool]string{false: "kept test passes", true: "its own miss still fails it"}[own], func(t *testing.T) {
			a, rp := exactAgent(t,
				[]*models.Mock{pingMock("mock-1", "/pat", "pat", t0), pingMock("mock-2", "/k", "k", t0.Add(time.Second))},
				map[string][]string{"o::TestQ": {"mock-1"}, "o::TestK": {"mock-2"}}, map[string][]string{})
			ctx := context.Background()
			require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestQ": "flaky/request_changed"}, nil))
			b, serr := a.ProvenBegin(ctx, begin("TestQ"))
			require.Nil(t, serr)
			require.Equal(t, models.ScopeActionSkip, b.Action)
			for _, path := range []string{"/tick", "/pat"} { // a timer's call; the quarantined test's recorded call
				code, _ := rp.call(t, path)
				require.Equal(t, 502, code)
			}
			paths := []string{"/k"}
			if own {
				paths = append(paths, "/own")
			}
			got, e := runTest(t, a, rp, "TestK", paths...)
			require.Equal(t, "200 k", got[0])
			if own {
				require.Equal(t, models.ScopeActionFail, e.Action)
				require.Contains(t, e.Reason, "GET /own to 127.0.0.1:18093 had no recorded answer")
				require.NotContains(t, e.Reason, "/tick")
				require.NotContains(t, e.Reason, "/pat")
				require.NotContains(t, e.Reason, "before this one began")
			} else {
				require.Empty(t, e.Action, e.Reason)
			}
			bg := backgroundCharged(a)
			require.Len(t, bg, 2)
			for i, path := range []string{"/tick", "/pat"} {
				require.Contains(t, bg[i].ActualSummary, path)
				require.False(t, bg[i].At.IsZero(), "a background miss says when it was made")
			}
		})
	}
}

// api-server's pinned replay of a committed keploy.lock, TestUnauthenticated's
// shape (1 replay in 8 failed it on keploy v3.8.97): the application's PAT
// janitor makes its calls as the application starts, before any test began,
// and every test before the first kept one is quarantined, so none opens a
// window. The first kept test still passes: a call made before the set's first
// served test is the application's start-up, charged to no test, and the
// set's report says each once, with when it was made. A call the first kept
// test makes that nothing answers still fails it, naming neither.
func TestStartUpCallsBeforeTheFirstKeptTestFailNoTest(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(map[bool]string{false: "the first kept test passes", true: "its own miss still fails it"}[own], func(t *testing.T) {
			a, rp := exactAgent(t,
				[]*models.Mock{pingMock("mock-1", "/pat", "pat", t0), pingMock("mock-2", "/q", "q", t0.Add(time.Second)), pingMock("mock-3", "/k", "k", t0.Add(2*time.Second))},
				map[string][]string{"o::TestQ1": {"mock-1"}, "o::TestQ2": {"mock-2"}, "o::TestK": {"mock-3"}}, map[string][]string{})
			ctx := context.Background()
			require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestQ1": "flaky/request_changed", "o::TestQ2": "keploy/unmocked_call"}, nil))
			// As the application starts, before any test began: the janitor's
			// insert, which no recording holds, and its PAT read, whose one
			// recording is the quarantined TestQ1's.
			for _, path := range []string{"/janitor", "/pat"} {
				code, _ := rp.call(t, path)
				require.Equal(t, 502, code)
			}
			for _, q := range []string{"TestQ1", "TestQ2"} {
				b, serr := a.ProvenBegin(ctx, begin(q))
				require.Nil(t, serr)
				require.Equal(t, models.ScopeActionSkip, b.Action)
			}
			paths := []string{"/k"}
			if own {
				paths = append(paths, "/own")
			}
			got, e := runTest(t, a, rp, "TestK", paths...)
			require.Equal(t, "200 k", got[0])
			if own {
				require.Equal(t, models.ScopeActionFail, e.Action)
				require.Contains(t, e.Reason, "GET /own to 127.0.0.1:18093 had no recorded answer")
				require.NotContains(t, e.Reason, "/janitor")
				require.NotContains(t, e.Reason, "/pat")
				require.NotContains(t, e.Reason, "before this one began")
			} else {
				require.Empty(t, e.Action, e.Reason)
				ev := windowOf(t, a, "o::TestK").Evidence
				if ev != nil {
					require.Empty(t, ev.Misses, "a start-up call is no miss of the first kept test's")
				}
			}
			bg := backgroundCharged(a)
			require.Len(t, bg, 2)
			for i, path := range []string{"/janitor", "/pat"} {
				require.Contains(t, bg[i].ActualSummary, path)
				require.False(t, bg[i].At.IsZero(), "a background miss says when it was made")
				require.False(t, bg[i].CountsAgainstTest())
			}
		})
	}
}

// TestNonExistentApp's full shape in api-server's acceptance run 3: the
// quarantined test before the kept one filled a cache at record (vault's PAT
// read), so at replay, with it set aside, the kept test makes that call in
// its own window. The set-aside test's recording answers it, unchecked: the
// kept test passes, its evidence names the serve, and nothing requires the
// call of it (served, never required: a data-plane call is answered by the
// recording it is, wherever the replay's timing puts it). The recording
// answers once: a later test that makes the call again misses. A call that is
// not exactly the recording still misses, and fails the test it is made in.
func TestASetAsideTestsRecordingAnswersTheCallAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		served     bool
	}{
		{"the same call is served, unchecked", "/pat", true},
		{"a call that is not exactly it still misses", "/pat?fresh=1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, rp := exactAgent(t,
				[]*models.Mock{pingMock("mock-1", "/pat", "pat", t0), pingMock("mock-2", "/k", "k", t0.Add(time.Second)), pingMock("mock-3", "/l", "l", t0.Add(2*time.Second))},
				map[string][]string{"o::TestQ": {"mock-1"}, "o::TestK": {"mock-2"}, "o::TestL": {"mock-3"}}, map[string][]string{})
			ctx := context.Background()
			require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestQ": "flaky/request_changed"}, nil))
			b, serr := a.ProvenBegin(ctx, begin("TestQ"))
			require.Nil(t, serr)
			require.Equal(t, models.ScopeActionSkip, b.Action)
			got, e := runTest(t, a, rp, "TestK", "/k", tc.path)
			if !tc.served {
				require.Equal(t, []string{"200 k"}, got[:1])
				require.NotEqual(t, 200, mustCode(got[1]), "a call that is not exactly the set-aside recording is not answered by it")
				require.Equal(t, models.ScopeActionFail, e.Action)
				require.Contains(t, e.Reason, "GET /pat to 127.0.0.1:18093 had no recorded answer")
				require.Empty(t, evidenceOf(t, a, "o::TestK").ServedAside)
				return
			}
			require.Equal(t, []string{"200 k", "200 pat"}, got)
			require.Empty(t, e.Action, e.Reason)
			ev := evidenceOf(t, a, "o::TestK")
			require.NotNil(t, ev)
			require.Equal(t, []string{"mock-1 served from o::TestQ (set aside)"}, ev.ServedAside)
			require.Empty(t, ev.Unconsumed, "a set-aside recording is never the test's to make")
			// Answered once: the next test's same call is no longer it.
			got, e = runTest(t, a, rp, "TestL", "/l", "/pat")
			require.Equal(t, "200 l", got[0])
			require.NotEqual(t, 200, mustCode(got[1]), "a set-aside recording answers once")
			require.Equal(t, models.ScopeActionFail, e.Action)
		})
	}
}

// api-server's first-use PAT read under a run that does not select the
// skipped test that recorded it (keploy mock replay --lock -- -run with a
// skip line on TestAppCRUD): that test never begins, yet the test recorded
// after it makes its call in its own window. The skipped test's recording
// answers it, as at the skipped test's own begin: unchecked, never required,
// named on the evidence. A skipped test recorded after the open one serves
// it nothing (what it answers may be what that test caused): the call
// misses and fails the test.
func TestASkippedTestNoBeginServesTheTestsRecordedAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name, skipped string
		served        bool
	}{
		{"skipped and recorded before: served", "o::TestQ", true},
		{"skipped and recorded after: not served", "o::TestZ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, rp := exactAgent(t,
				[]*models.Mock{pingMock("mock-1", "/pat", "pat", t0), pingMock("mock-2", "/k", "k", t0.Add(time.Second)), pingMock("mock-3", "/pat", "pat", t0.Add(3*time.Second))},
				map[string][]string{"o::TestQ": {"mock-1"}, "o::TestK": {"mock-2"}, "o::TestZ": {"mock-3"}}, map[string][]string{})
			ctx := context.Background()
			require.NoError(t, a.SetRecordedWindows(ctx, []models.TestWindow{
				{TestCase: "o::TestQ", Start: t0, End: t0.Add(500 * time.Millisecond)},
				{TestCase: "o::TestK", Start: t0.Add(time.Second), End: t0.Add(1500 * time.Millisecond)},
				{TestCase: "o::TestZ", Start: t0.Add(3 * time.Second), End: t0.Add(3500 * time.Millisecond)},
			}))
			require.NoError(t, a.SetScopeSkips(ctx, map[string]string{tc.skipped: "manual/the first use's owner"}, nil))
			if !tc.served {
				// TestQ, kept, runs first and makes its own call: a test
				// the run began hands nothing to the tests after it.
				got, e := runTest(t, a, rp, "TestQ", "/pat")
				require.Equal(t, []string{"200 pat"}, got)
				require.Empty(t, e.Action, e.Reason)
			}
			// The skipped test is never begun: the run does not select it.
			got, e := runTest(t, a, rp, "TestK", "/k", "/pat")
			ev := evidenceOf(t, a, "o::TestK")
			require.NotNil(t, ev)
			if !tc.served {
				require.Equal(t, "200 k", got[0])
				require.NotEqual(t, 200, mustCode(got[1]), "a test recorded after the open one serves it nothing")
				require.Equal(t, models.ScopeActionFail, e.Action)
				require.Contains(t, e.Reason, "GET /pat to 127.0.0.1:18093 had no recorded answer")
				require.Empty(t, ev.ServedAside)
				return
			}
			require.Equal(t, []string{"200 k", "200 pat"}, got)
			require.Empty(t, e.Action, e.Reason)
			require.Equal(t, []string{"mock-1 served from o::TestQ (set aside)"}, ev.ServedAside)
			require.Empty(t, ev.Unconsumed, "a set-aside recording is never the test's to make")
			// Its begin arriving later hands nothing again: answered once.
			b, serr := a.ProvenBegin(ctx, begin("TestQ"))
			require.Nil(t, serr)
			require.Equal(t, models.ScopeActionSkip, b.Action)
			got, e = runTest(t, a, rp, "TestZ", "/pat", "/pat")
			require.Equal(t, "200 pat", got[0], "TestZ's own recording")
			require.NotEqual(t, 200, mustCode(got[1]), "the skipped test's recording answered once, in TestK")
			require.Equal(t, models.ScopeActionFail, e.Action)
		})
	}
}

// notBegunAgent is TestQ (/pat), TestK (/k, and its own /pat when own) and
// TestZ (/z), recorded in that order, none skipped: a kept test's recordings
// with the windows that order them.
func notBegunAgent(t *testing.T, own bool) (*Agent, *servingProxy) {
	t.Helper()
	mocks := []*models.Mock{pingMock("mock-1", "/pat", "pat", t0), pingMock("mock-2", "/k", "k", t0.Add(time.Second)), pingMock("mock-5", "/z", "z", t0.Add(3*time.Second))}
	table := map[string][]string{"o::TestQ": {"mock-1"}, "o::TestK": {"mock-2"}, "o::TestZ": {"mock-5"}}
	if own {
		mocks = append(mocks, pingMock("mock-3", "/pat", "own", t0.Add(1100*time.Millisecond)))
		table["o::TestK"] = []string{"mock-2", "mock-3"}
	}
	a, rp := exactAgent(t, mocks, table, map[string][]string{})
	require.NoError(t, a.SetRecordedWindows(context.Background(), []models.TestWindow{
		{TestCase: "o::TestQ", Start: t0, End: t0.Add(500 * time.Millisecond)},
		{TestCase: "o::TestK", Start: t0.Add(time.Second), End: t0.Add(1500 * time.Millisecond)},
		{TestCase: "o::TestZ", Start: t0.Add(3 * time.Second), End: t0.Add(3500 * time.Millisecond)},
	}))
	return a, rp
}

// api-server's first-use PAT read under a partial replay (keploy mock replay
// -- -run TestListApps): TestAppCRUD, kept and recorded before it, made the
// read at record and is not run, so TestListApps makes it in its own window.
// A test the replay has not begun hands its recordings aside at the begin of
// a test recorded after it, as a skipped one does: the call beyond the
// open test's own recordings is answered by that test's exact copy,
// unchecked and named; past every copy the call is an extra call; a call
// that is not exactly the copy misses; a test the run began hands nothing
// (its recordings are its own, required of it), so a full run in recorded
// order is served as before.
func TestAKeptTestTheRunDoesNotBeginServesTheTestsRecordedAfterIt(t *testing.T) {
	t.Run("partial replay: served", func(t *testing.T) {
		a, rp := notBegunAgent(t, false)
		got, e := runTest(t, a, rp, "TestK", "/k", "/pat")
		require.Equal(t, []string{"200 k", "200 pat"}, got)
		require.Empty(t, e.Action, e.Reason)
		ev := evidenceOf(t, a, "o::TestK")
		require.Equal(t, []string{"mock-1 served from o::TestQ (set aside)"}, ev.ServedAside)
		require.Empty(t, ev.Unconsumed, "another test's recording is never the open test's to make")
		// Answered once: the next test's same call misses.
		got, e = runTest(t, a, rp, "TestZ", "/z", "/pat")
		require.Equal(t, "200 z", got[0])
		require.NotEqual(t, 200, mustCode(got[1]), "a recording answers once in the set")
		require.Equal(t, models.ScopeActionFail, e.Action)
	})
	t.Run("past its own and every copy: an extra call", func(t *testing.T) {
		a, rp := notBegunAgent(t, true)
		got, e := runTest(t, a, rp, "TestK", "/k", "/pat", "/pat", "/pat")
		require.Equal(t, []string{"200 k", "200 own", "200 pat"}, got[:3], "its own first, then the copy")
		require.NotEqual(t, 200, mustCode(got[3]), "no copy left")
		require.Equal(t, models.ScopeActionFail, e.Action)
		require.Contains(t, e.Reason, "GET /pat to 127.0.0.1:18093 was called 3 times, recorded 2")
	})
	t.Run("not exactly the copy: refused", func(t *testing.T) {
		a, rp := notBegunAgent(t, false)
		got, e := runTest(t, a, rp, "TestK", "/k", "/pat?fresh=1")
		require.Equal(t, "200 k", got[0])
		require.NotEqual(t, 200, mustCode(got[1]))
		require.Equal(t, models.ScopeActionFail, e.Action)
		require.Contains(t, e.Reason, "GET /pat to 127.0.0.1:18093 had no recorded answer")
		require.Empty(t, evidenceOf(t, a, "o::TestK").ServedAside)
	})
	t.Run("begun: its recordings stay its own", func(t *testing.T) {
		a, rp := notBegunAgent(t, false)
		// TestQ ran first, in recorded order, and did not make its call.
		_, e := runTest(t, a, rp, "TestQ")
		require.Equal(t, models.ScopeActionFail, e.Action)
		require.Equal(t, []string{"mock-1"}, evidenceOf(t, a, "o::TestQ").Unconsumed)
		got, e := runTest(t, a, rp, "TestK", "/k", "/pat")
		require.Equal(t, "200 k", got[0])
		require.NotEqual(t, 200, mustCode(got[1]), "a test the run began is handed to no later test")
		require.Equal(t, models.ScopeActionFail, e.Action)
		require.Empty(t, evidenceOf(t, a, "o::TestK").ServedAside)
	})
	t.Run("begun later: its own copy is the one answer", func(t *testing.T) {
		// Served aside to TestK, which ran first: TestQ's own call then
		// finds no answer left.
		a, rp := notBegunAgent(t, false)
		got, e := runTest(t, a, rp, "TestK", "/k", "/pat")
		require.Equal(t, []string{"200 k", "200 pat"}, got)
		require.Empty(t, e.Action, e.Reason)
		got, e = runTest(t, a, rp, "TestQ", "/pat")
		require.NotEqual(t, 200, mustCode(got[0]), "the recording answered once, in TestK")
		require.Equal(t, models.ScopeActionFail, e.Action)
		// Answered TestQ itself: no later test is served it aside.
		a, rp = notBegunAgent(t, false)
		got, e = runTest(t, a, rp, "TestK", "/k")
		require.Equal(t, []string{"200 k"}, got)
		require.Empty(t, e.Action, e.Reason)
		got, e = runTest(t, a, rp, "TestQ", "/pat")
		require.Equal(t, []string{"200 pat"}, got)
		require.Empty(t, e.Action, e.Reason)
		got, e = runTest(t, a, rp, "TestZ", "/z", "/pat")
		require.Equal(t, "200 z", got[0])
		require.NotEqual(t, 200, mustCode(got[1]), "TestQ's own call took the one answer")
		require.Equal(t, models.ScopeActionFail, e.Action)
		require.Empty(t, evidenceOf(t, a, "o::TestZ").ServedAside)
	})
}

// mustCode is the status code of a runTest answer ("<code> <body>").
func mustCode(answer string) int {
	var code int
	_, _ = fmt.Sscanf(answer, "%d", &code)
	return code
}

// A test with a recording of its own that is exactly a set-aside test's call
// is answered by its own, which it is required to make: the set-aside
// recording is filed after it and is not used.
func TestATestsOwnRecordingAnswersBeforeASetAsideOne(t *testing.T) {
	a, rp := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/pat", "aside", t0), pingMock("mock-2", "/pat", "own", t0.Add(time.Second))},
		map[string][]string{"o::TestQ": {"mock-1"}, "o::TestK": {"mock-2"}}, map[string][]string{})
	ctx := context.Background()
	require.NoError(t, a.SetScopeSkips(ctx, map[string]string{"o::TestQ": "flaky/request_changed"}, nil))
	_, serr := a.ProvenBegin(ctx, begin("TestQ"))
	require.Nil(t, serr)
	got, e := runTest(t, a, rp, "TestK", "/pat")
	require.Equal(t, []string{"200 own"}, got)
	require.Empty(t, e.Action, e.Reason)
	ws, err := a.GetScopeWindows(ctx)
	require.NoError(t, err)
	for _, w := range ws {
		require.Nil(t, w.Evidence, "%s: a test served only its own has no evidence", w.Name)
	}
	// Not made, the test's own recording is still required of it.
	a2, rp2 := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/pat", "aside", t0), pingMock("mock-2", "/pat", "own", t0.Add(time.Second))},
		map[string][]string{"o::TestQ": {"mock-1"}, "o::TestK": {"mock-2"}}, map[string][]string{})
	require.NoError(t, a2.SetScopeSkips(ctx, map[string]string{"o::TestQ": "flaky/request_changed"}, nil))
	_, serr = a2.ProvenBegin(ctx, begin("TestQ"))
	require.Nil(t, serr)
	_, e = runTest(t, a2, rp2, "TestK")
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Equal(t, []string{"mock-2"}, evidenceOf(t, a2, "o::TestK").Unconsumed)
}

// lateProxy holds a miss between its report being built and its filing
// until the end has swapped to the pool: the test's own call, matched while
// the test was open, is still in flight as the end begins.
type lateProxy struct {
	*servingProxy
	built, swapped, filed chan struct{}
}

func (p *lateProxy) ServeExactPool(key string, names []string) error {
	if err := p.servingProxy.ServeExactPool(key, names); err != nil {
		return err
	}
	if p.swapped != nil {
		close(p.swapped)
		p.swapped = nil
		<-p.filed
	}
	return nil
}

func (p *lateProxy) file(err error) {
	swapped := p.swapped
	close(p.built)
	<-swapped
	p.noteMiss(err)
	close(p.filed)
}

// A test's extra call, matched while the test was open and filed only once
// its end stopped serving it, stays an extra call of that test: its reason
// says how often it was made and recorded, never that it came before the
// test began (round-4 review).
func TestAnExtraCallFiledAsTheTestEndsStaysAnExtraCall(t *testing.T) {
	a, rp := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/a", "a", t0)},
		map[string][]string{"o::TestA": {"mock-1"}}, map[string][]string{})
	p := &lateProxy{servingProxy: rp, built: make(chan struct{}), filed: make(chan struct{})}
	a.Proxy = p
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	code, _ := rp.call(t, "/a")
	require.Equal(t, 200, code)

	p.swapped = make(chan struct{})
	done := make(chan int, 1)
	go func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			done <- -1
			return
		}
		defer ln.Close()
		acc := make(chan net.Conn, 1)
		go func() { c, _ := ln.Accept(); acc <- c }()
		app, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			done <- -1
			return
		}
		defer app.Close()
		prox := <-acc
		defer prox.Close()
		mctx := models.WithMockMismatchReporter(context.Background(), p.file)
		go func() {
			_ = httpparser.New(zap.NewNop()).MockOutgoing(mctx, prox, &models.ConditionalDstCfg{Port: 18093}, p.mm, models.OutgoingOptions{})
		}()
		_, _ = io.WriteString(app, "GET /a HTTP/1.1\r\nHost: 127.0.0.1:18093\r\n\r\n")
		resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil)
		if err != nil {
			done <- -1
			return
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	<-p.built
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Equal(t, 502, <-done)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "GET /a to 127.0.0.1:18093 was called 2 times, recorded 1")
	require.NotContains(t, e.Reason, "before this one began")
}

// stageSpy notes what the session tier shows as the next set's staging
// lands.
type stageSpy struct {
	*servingProxy
	seen []string
}

func (p *stageSpy) SetMocksWithWindow(ctx context.Context, f, u []*models.Mock, s, e time.Time) error {
	if p.mm.ExactServing() {
		sess, _ := p.mm.GetSessionMocks()
		for _, m := range sess {
			p.seen = append(p.seen, m.Name+"="+m.Spec.HTTPResp.Body)
		}
	}
	return p.servingProxy.SetMocksWithWindow(ctx, f, u, s, e)
}

// keploy test's next set is staged before its pool is served: until it is,
// no mock still staged from the previous set answers under one of the new
// set's pool names (names are unique only within a set; round-4 review).
func TestTheNextSetsPoolIsServedOnlyOnceItIsStaged(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	rp := newServingProxy()
	spy := &stageSpy{servingProxy: rp}
	a.Proxy = spy
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{pingMock("mock-1", "/x", "set-1 test-1's", t0)}))
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now(), Exact: &models.ExactServing{Stage: true}}))
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1", Owned: []string{"mock-1"}}}))
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}))

	rp.mm.ResetForReplaySession()
	spy.seen = nil
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{pingMock("mock-1", "/x", "set-2 pool's", t0)}))
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now(), Exact: &models.ExactServing{Stage: true, Pool: []string{"mock-1"}}}))
	require.Empty(t, spy.seen, "served before set 2 was staged")
	code, body := rp.call(t, "/x")
	require.Equal(t, 200, code)
	require.Equal(t, "set-2 pool's", body)
}

// echoMock is a reusable (cursor-saturate) recording of POST /charge whose
// key the dependency echoes (#4809's served marks).
func echoMock(name, key string, at time.Time) *models.Mock {
	resp := `{"echo":"` + key + `","n":"` + name + `"}`
	m := &models.Mock{
		Version: models.GetVersion(), Name: name, Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{Method: "POST", URL: "http://127.0.0.1:18093/charge",
				Header: map[string]string{"Host": "127.0.0.1:18093", "Content-Type": "application/json"}, Body: `{"key":"` + key + `"}`},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json", "Content-Length": fmt.Sprint(len(resp))}, Body: resp},
			Variables:        models.MockVariables{"key": {Recorded: key, Bind: "body.key", Answer: []string{"body.echo"}, Echo: true}},
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		},
	}
	m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived, m.TestModeInfo.Consume = models.LifetimeSession, true, models.ConsumeCursorSaturate
	return m
}

// postCharge sends POST /charge with key through the real HTTP matcher.
func (p *servingProxy) postCharge(t *testing.T, key string) string {
	t.Helper()
	return p.postTo(t, "/charge", key)
}

// postTo sends POST path with body {"key":key} through the real HTTP matcher.
func (p *servingProxy) postTo(t *testing.T, path, key string) string {
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
	body := `{"key":"` + key + `"}`
	_, err = io.WriteString(app, "POST "+path+" HTTP/1.1\r\nHost: 127.0.0.1:18093\r\nContent-Type: application/json\r\nContent-Length: "+fmt.Sprint(len(body))+"\r\n\r\n"+body)
	require.NoError(t, err)
	resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, b)
}

// #4809's served marks under exact serving. A test that owns two echo
// recordings of one call takes them in record order, each once, and a third
// call is one more than recorded, reported as such (the dry match follows
// the variables on a copy of the test's bindings). Between tests the pool's
// echo recordings take turns and saturate, as #4809 serves them.
func TestServedMarksComposeWithExactServing(t *testing.T) {
	t.Run("owned by the test", func(t *testing.T) {
		a, p := exactAgent(t,
			[]*models.Mock{echoMock("mock-a", "k-1", t0), echoMock("mock-b", "k-2", t0.Add(time.Second))},
			map[string][]string{"o::T": {"mock-a", "mock-b"}}, map[string][]string{})
		ctx := context.Background()
		_, serr := a.ProvenBegin(ctx, begin("T"))
		require.Nil(t, serr)
		var got []string
		for _, k := range []string{"x-1", "x-2", "x-3"} {
			got = append(got, p.postCharge(t, k))
		}
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "T", CanSkip: true, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
		require.Equal(t, `200 {"echo":"x-1","n":"mock-a"}`, got[0])
		require.Equal(t, `200 {"echo":"x-2","n":"mock-b"}`, got[1])
		require.True(t, strings.HasPrefix(got[2], "502"), "a third charge was answered: %s", got[2])
		require.Equal(t, models.ScopeActionFail, e.Action)
		require.Contains(t, e.Reason, "called 3 times, recorded 2")
	})
	t.Run("the pool between tests", func(t *testing.T) {
		_, p := exactAgent(t,
			[]*models.Mock{echoMock("mock-a", "k-1", t0), echoMock("mock-b", "k-2", t0.Add(time.Second))},
			map[string][]string{}, map[string][]string{"o": {"mock-a", "mock-b"}})
		var got []string
		for _, k := range []string{"x-1", "x-2", "x-3"} {
			got = append(got, p.postCharge(t, k))
		}
		require.Equal(t, []string{`200 {"echo":"x-1","n":"mock-a"}`, `200 {"echo":"x-2","n":"mock-b"}`, `200 {"echo":"x-3","n":"mock-b"}`}, got)
	})
}

// lateCall makes GET path through the real HTTP matcher and holds its miss
// between the report's build and its filing: built closes once the report
// is built, the miss is filed once release closes, filed closes after.
func (p *servingProxy) lateCall(path string) (built, release, filed chan struct{}, done chan int) {
	built, release, filed = make(chan struct{}), make(chan struct{}), make(chan struct{})
	done = make(chan int, 1)
	file := func(err error) {
		close(built)
		<-release
		p.noteMiss(err)
		close(filed)
	}
	go func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			done <- -1
			return
		}
		defer ln.Close()
		acc := make(chan net.Conn, 1)
		go func() { c, _ := ln.Accept(); acc <- c }()
		app, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			done <- -1
			return
		}
		defer app.Close()
		prox := <-acc
		defer prox.Close()
		mctx := models.WithMockMismatchReporter(context.Background(), file)
		go func() {
			_ = httpparser.New(zap.NewNop()).MockOutgoing(mctx, prox, &models.ConditionalDstCfg{Port: 18093}, p.mm, models.OutgoingOptions{})
		}()
		_, _ = io.WriteString(app, "GET "+path+" HTTP/1.1\r\nHost: 127.0.0.1:18093\r\n\r\n")
		resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil)
		if err != nil {
			done <- -1
			return
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	return built, release, filed, done
}

// A call an earlier test made, still in flight as that test ended, is filed
// once a later test is open or between tests, and charged to the later one,
// which never made it: its reason says the call was made before it began,
// never that it called it (round-5 and round-6 reviews). Its own calls are
// judged as before.
func TestAnEarlierTestsLateCallIsNotTheNextTests(t *testing.T) {
	for _, tc := range []struct {
		name string
		// path is TestA's late call: /a is one call more than it recorded,
		// /zzz a call it recorded none of.
		path string
		// duringB: filed once TestB began; else between the tests.
		duringB bool
		want    string
	}{
		{"an extra call filed between tests", "/a", false, "GET /a to 127.0.0.1:18093, made before this one began, had no recorded answer"},
		{"an extra call filed during the next test", "/a", true, "GET /a to 127.0.0.1:18093, made before this one began, had no recorded answer"},
		{"a miss filed during the next test", "/zzz", true, "GET /zzz to 127.0.0.1:18093, made before this one began, had no recorded answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, rp := exactAgent(t,
				[]*models.Mock{pingMock("mock-1", "/a", "a", t0), pingMock("mock-2", "/b", "b", t0.Add(time.Second))},
				map[string][]string{"o::TestA": {"mock-1"}, "o::TestB": {"mock-2"}}, map[string][]string{})
			ctx := context.Background()
			_, serr := a.ProvenBegin(ctx, begin("TestA"))
			require.Nil(t, serr)
			code, _ := rp.call(t, "/a")
			require.Equal(t, 200, code)
			built, release, filed, done := rp.lateCall(tc.path)
			<-built
			_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
			require.Nil(t, serr)
			if !tc.duringB {
				close(release)
				<-filed
			}
			_, serr = a.ProvenBegin(ctx, begin("TestB"))
			require.Nil(t, serr)
			if tc.duringB {
				close(release)
				<-filed
			}
			require.Equal(t, 502, <-done)
			code, _ = rp.call(t, "/b")
			require.Equal(t, 200, code)
			e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", CanSkip: true, Outcome: models.ScopeOutcomePassed})
			require.Nil(t, serr)
			require.Equal(t, models.ScopeActionFail, e.Action)
			require.Contains(t, e.Reason, tc.want)
			require.NotContains(t, e.Reason, "was called")
			// TestB's own extra call is still counted.
			got, e := runTest(t, a, rp, "TestB", "/b", "/b")
			require.Equal(t, "200 b", got[0])
			require.Contains(t, e.Reason, "GET /b to 127.0.0.1:18093 was called 2 times, recorded 1")
		})
	}
}

// The agent serves the pool between test cases whatever becomes of the next
// test case's calls in flight: with no mocks loaded to read them from, the
// pool is still served, and the test case that ended is closed, not left open.
func TestThePoolIsServedWhenTheCallsInFlightCannotBeRead(t *testing.T) {
	p := newServingProxy()
	t.Cleanup(p.mm.Close)
	a := &Agent{logger: zap.NewNop()}
	p.mm.ServeExactTest("test-1", nil)
	if err := a.servePoolAhead(p, []string{"pool"}, []string{"fetch"}); err != nil {
		t.Fatalf("servePoolAhead = %v; want the pool served all the same", err)
	}
	if p.mm.IsTestWindowActive() {
		t.Fatal("test-1 is still open: the pool was not served")
	}
}

// keploy test with a worker scope: a worker mock answers from its place in
// the set's run on, in a test case or between them, once in the set; a test
// case begun reaches the worker mocks placed after it, and no earlier one.
// The worker mocks are staged with the pool, by name, and are not the
// pool's.
func TestKeployTestServesTheWorkerScopeFromItsPlaces(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
	a.Proxy = p
	ctx := context.Background()
	mocks := []*models.Mock{
		pingMock("mock-own", "/own", "own", t0),
		pingMock("mock-boot", "/boot", "boot", t0.Add(time.Second)),
		pingMock("mock-late", "/late", "late", t0.Add(2*time.Second)),
	}
	require.NoError(t, a.StoreMocks(ctx, nil, mocks))
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(),
		Exact: &models.ExactServing{Stage: true, Worker: []models.MockEntry{{Name: "mock-boot"}, {Name: "mock-late", Place: "test-1"}}},
	}))
	code, _ := p.call(t, "/late")
	require.Equal(t, 502, code, "a worker mock answered before its place")
	before, err := p.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.Equal(t, models.MissOutsideTest, before[0].Kind, "a call before a worker mock's place is a miss outside every test")

	got, misses := keployTestCase(t, a, p, "test-1", []string{"mock-own"}, "/own", "/boot", "/boot")
	require.Equal(t, "200 own", got[0])
	require.Equal(t, "200 boot", got[1], "a worker mock placed at the set's start answers in a test case")
	require.True(t, strings.HasPrefix(got[2], "502"), "a worker mock answered twice: %v", got)
	require.Len(t, misses, 1)
	require.Equal(t, models.MissInTest, misses[0].Kind, "the worker mock's second call is a miss in the test")

	// test-1 has begun: mock-late is reached, between test cases too.
	code, body := p.call(t, "/late")
	require.Equal(t, 200, code, "a worker mock placed after a begun test case does not answer between test cases")
	require.Equal(t, "late", body)
	code, _ = p.call(t, "/late")
	require.Equal(t, 502, code, "a worker mock answered twice in the set")
	code, _ = p.call(t, "/own")
	require.Equal(t, 502, code, "a test case's mock answered between test cases")
}

// keyMock is a test's recording of POST path with body {"key":"k-1"}: its
// variable key bound at body.key (used there, bound by an earlier call, when
// use), answered at body.echo.
func keyMock(name, path string, use bool, at time.Time) *models.Mock {
	v := models.MockVariable{Recorded: "k-1", Answer: []string{"body.echo"}}
	if use {
		v.Use = []string{"body.key"}
	} else {
		v.Bind = "body.key"
	}
	resp := `{"echo":"k-1"}`
	m := &models.Mock{
		Version: models.GetVersion(), Name: name, Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{Method: "POST", URL: "http://127.0.0.1:18093" + path,
				Header: map[string]string{"Host": "127.0.0.1:18093", "Content-Type": "application/json"}, Body: `{"key":"k-1"}`},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json", "Content-Length": fmt.Sprint(len(resp))}, Body: resp},
			Variables:        models.MockVariables{"key": v},
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		},
	}
	m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	return m
}

// keepingProxy is exact serving's proxy whose mock manager keeps the test
// variables a replay binds, begun apart from a test's opening. served counts
// the tests it opened without them (ServeExactTest).
type keepingProxy struct {
	*servingProxy
	served int
}

func (p *keepingProxy) ServeExactTest(test string, owned []*models.Mock) error {
	p.served++
	return p.servingProxy.ServeExactTest(test, owned)
}

func (p *keepingProxy) StartVariables(given []models.VariableBinding) bool {
	p.mm.StartVariables(given)
	return true
}

func (p *keepingProxy) BoundVariables() ([]models.VariableBinding, bool) {
	return p.mm.BoundVariables(), true
}

func (p *keepingProxy) ResetVariables() { p.mm.ResetVariables() }

// openingProxy is a keepingProxy that opens a test with its variables in one
// step, as keploy's proxy does (proxy.Proxy.OpenExactTest).
type openingProxy struct {
	keepingProxy
	opened int
}

func (p *openingProxy) OpenExactTest(test string, owned []*models.Mock, given []models.VariableBinding) error {
	p.opened++
	p.mm.OpenExactTest(test, owned, given)
	return nil
}

// keyNames is the names and values of bound, in its order.
func keyNames(bound []models.VariableBinding) string {
	out := make([]string, 0, len(bound))
	for _, b := range bound {
		out = append(out, b.Name+"="+b.Value)
	}
	return strings.Join(out, ",")
}

// keploy test under the switch: a test case's filter opens the test case
// with its variables in one step (OpenTestCase), the values its request gave
// bound. What its own call binds is the test case's: the next test case,
// whose call only uses that variable, misses. A test opened in two steps (its
// mocks live, then its variables) left a call of a set's first test that was
// answered between them bound in the run's, which every later test reads.
func TestKeployTestOpensATestCaseWithItsVariablesInOneStep(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &openingProxy{keepingProxy: keepingProxy{servingProxy: newServingProxy()}}
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{keyMock("own-1", "/own-1", false, t0), keyMock("use-2", "/use-2", true, t0.Add(time.Second))}))
	// The set begins (Proxy.Mock under the switch), and its staging goes in.
	p.mm.NewVariableSet()
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(),
		Exact: &models.ExactServing{Stage: true},
	}))
	given := []models.VariableBinding{{Name: "id", Value: "u1", Recorded: "u0", Mock: "test case test-1", Place: "request"}}
	require.NoError(t, a.OpenTestCase(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1", Owned: []string{"own-1"}}, Variables: given}))
	bound, kept := a.TestVariables()
	require.True(t, kept)
	require.Equal(t, "id=u1", keyNames(bound), "test-1 opens with the value its request gave")
	require.Equal(t, `200 {"echo":"x-1"}`, p.postTo(t, "/own-1", "x-1"))
	bound, _ = a.TestVariables()
	require.Equal(t, "id=u1,key=x-1", keyNames(bound), "test-1 holds what its own call bound")
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}))
	require.NoError(t, a.OpenTestCase(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-2", Owned: []string{"use-2"}}}))
	bound, _ = a.TestVariables()
	require.Empty(t, bound, "test-2 begins with test-1's bindings")
	got := p.postTo(t, "/use-2", "x-1")
	require.False(t, strings.HasPrefix(got, "200 "), "test-2's call, which only uses key, was answered %s with test-1's value", got)
	require.Equal(t, 2, p.opened, "a test case opened without its variables")
	require.Zero(t, p.served, "a test case opened without its variables")
	require.Error(t, a.OpenTestCase(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}), "the pool call opened a test case")
}

// A proxy that keeps a replay's variables but cannot open a test with them in
// one step does not open a keploy test case: begun apart, a call the test's
// own mock answers between the two would bind in no scope of the test's, and
// before a set's first test in the run's. It is refused before anything of
// it is settled, marked begun or served.
func TestKeployTestRefusesATestCaseItCannotOpenWithItsVariables(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &keepingProxy{servingProxy: newServingProxy()}
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{keyMock("own-1", "/own-1", false, t0)}))
	p.mm.NewVariableSet()
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(),
		Exact: &models.ExactServing{Stage: true},
	}))
	for _, given := range [][]models.VariableBinding{nil, {{Name: "id", Value: "u1", Recorded: "u0", Mock: "test case test-1", Place: "request"}}} {
		require.Error(t, a.OpenTestCase(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1", Owned: []string{"own-1"}}, Variables: given}))
	}
	require.Zero(t, p.served, "a test case was opened apart from its variables")
	a.scopeMu.Lock()
	begun := a.testBegun["test-1"]
	a.scopeMu.Unlock()
	require.False(t, begun, "the refused test case was marked begun: it was refused after it was settled and served the worker's scope")
	got := p.postTo(t, "/own-1", "x-1")
	require.False(t, strings.HasPrefix(got, "200 "), "the refused test case's mock answered %s", got)
}

// keploy mock replay under the switch: a test begins (ProvenBegin) with its
// variables, none bound, in the step that opens it, so what its own call
// binds is the test's, and the next test, whose call only uses that variable,
// misses.
func TestKeployMockReplayOpensATestWithItsVariablesInOneStep(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &openingProxy{keepingProxy: keepingProxy{servingProxy: newServingProxy()}}
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{keyMock("own-1", "/own-1", false, t0), keyMock("use-2", "/use-2", true, t0.Add(time.Second))}))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::T1": {"own-1"}, "o::T2": {"use-2"}}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{}))
	claimSelf(t, a)
	p.mm.NewVariableSet()
	_, serr := a.ProvenBegin(ctx, begin("T1"))
	require.Nil(t, serr)
	require.Equal(t, `200 {"echo":"x-1"}`, p.postTo(t, "/own-1", "x-1"))
	bound, _ := a.TestVariables()
	require.Equal(t, "key=x-1", keyNames(bound), "T1 holds what its own call bound")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "T1", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("T2"))
	require.Nil(t, serr)
	bound, _ = a.TestVariables()
	require.Empty(t, bound, "T2 begins with T1's bindings")
	got := p.postTo(t, "/use-2", "x-1")
	require.False(t, strings.HasPrefix(got, "200 "), "T2's call, which only uses key, was answered %s with T1's value", got)
	require.Equal(t, 2, p.opened, "a test opened without its variables")
	require.Zero(t, p.served, "a test opened without its variables")
}

// requestServingProxy is a servingProxy whose learn step reads a test case's
// own request (coreAgent.TestRequestTaker): it notes each request it is
// given, and whose view the store showed as it was.
type requestServingProxy struct {
	*servingProxy
	given []string
}

func (p *requestServingProxy) BeginTestCaseCapture()    {}
func (p *requestServingProxy) ContinueTestCaseCapture() {}

func (p *requestServingProxy) TakeTestRequest(req *models.TestRequest) {
	name := "none"
	if req != nil {
		name = req.Test
	}
	view := "the pool's"
	if snap, ok := p.mm.RoundSnapshot(integrations.RoundQuery{}); ok && snap.Test != "" {
		view = snap.Test + "'s"
	}
	p.given = append(p.given, "request "+name+" while the store showed "+view+" view")
}

var _ coreAgent.TestRequestTaker = (*requestServingProxy)(nil)

// Under exact serving a keploy test case's opening hands its request to the
// learn step before the store installs the test's view, whatever the
// opening's outcome: the calls that waited for it in the gap since its
// capture window opened (learn.Tracker.AwaitRequest) are released, and read
// as calls made after the request. One the replay sends without a request
// (a keploy older than its agent) is handed as none, which the learn step
// reads as a request it could not read. An opening that never handed it
// left every call in the gap waiting until the window closed, then refused.
// The test's end hands nothing.
func TestAnExactOpeningGivesTheLearnStepItsRequestBeforeTheTestsView(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *models.TestRequest
		want string
	}{
		{"with its request", &models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "url", Recorded: "/own-1", Sent: "/own-1"}}},
			"request test-1 while the store showed the pool's view"},
		{"from a keploy older than its agent", nil, "request none while the store showed the pool's view"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := wireAgent(t, models.MODE_TEST)
			p := &requestServingProxy{servingProxy: newServingProxy()}
			a.Proxy = p
			ctx := context.Background()
			require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{keyMock("own-1", "/own-1", false, t0)}))
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
				AfterTime: models.BaseTime, BeforeTime: time.Now(),
				Exact: &models.ExactServing{Stage: true},
			}))
			require.Empty(t, p.given, "the set's staging handed the learn step a request")
			require.NoError(t, a.OpenTestCase(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1", Owned: []string{"own-1"}}, TestRequest: tc.req}))
			require.Equal(t, []string{tc.want}, p.given, "the exact opening's request, handed to the learn step before the test's view")
			snap, ok := p.mm.RoundSnapshot(integrations.RoundQuery{})
			require.True(t, ok)
			require.Equal(t, "test-1", snap.Test, "the opening installed test-1's view")
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}))
			require.Equal(t, []string{tc.want}, p.given, "the test's end handed the learn step a request")
		})
	}
}

// servesLogProxy is servingProxy that logs, in order, each settle's first
// read (Accepted), each test case it serves and each pool.
type servesLogProxy struct {
	*servingProxy
	lmu    sync.Mutex
	events []string
}

func (p *servesLogProxy) log(e string) { p.lmu.Lock(); p.events = append(p.events, e); p.lmu.Unlock() }
func (p *servesLogProxy) logged() []string {
	p.lmu.Lock()
	defer p.lmu.Unlock()
	return append([]string(nil), p.events...)
}
func (p *servesLogProxy) Accepted() (uint64, error) {
	p.log("settle")
	return p.servingProxy.Accepted()
}
func (p *servesLogProxy) ServeExactTest(test string, owned []*models.Mock) error {
	p.log("serve " + test)
	return p.servingProxy.ServeExactTest(test, owned)
}
func (p *servesLogProxy) ServeExactPool(key string, names []string) error {
	p.log("pool")
	return p.servingProxy.ServeExactPool(key, names)
}

// learnEndProxy is servesLogProxy whose learn step a filter can end
// (coreAgent.LearnEndSetter), on its mock manager, logged.
type learnEndProxy struct{ servesLogProxy }

func (p *learnEndProxy) SetLearnEnded(ended bool) error {
	p.log(fmt.Sprintf("learn-ended=%v", ended))
	p.mm.SetLearnEnded(ended)
	return nil
}

// A keploy test filter says whether the set's learn step has ended (a retry
// cycle of record's full round, models.MockFilterParams.LearnEnded), and the
// agent sets it on its proxy first: before the filter settles anything or
// serves anything, so no call read after the filter is paired or learned. It
// stays ended for the set, whatever a later filter says: only the next set's
// staging takes it back up. A proxy that cannot end its learn step
// refuses a filter that asks it to, and serves nothing of it.
func TestAKeployTestFilterEndsTheLearnStepBeforeItSettlesOrServes(t *testing.T) {
	ctx := context.Background()
	mocks := []*models.Mock{pingMock("mock-1", "/ping", "ok", t0)}
	stage := models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now(), Exact: &models.ExactServing{Stage: true}}

	a, _ := wireAgent(t, models.MODE_TEST)
	p := &learnEndProxy{servesLogProxy{servingProxy: newServingProxy()}}
	a.Proxy = p
	require.NoError(t, a.StoreMocks(ctx, nil, mocks))
	require.NoError(t, a.UpdateMockParams(ctx, stage))
	require.False(t, p.mm.LearnEnded(), "the set's staging leaves the learn step on")
	for _, f := range []struct {
		params models.MockFilterParams
		served string
	}{
		{models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}, LearnEnded: true}, "serve test-1"},
		{models.MockFilterParams{Exact: &models.ExactServing{}, LearnEnded: true}, "pool"},
	} {
		from := len(p.logged())
		require.NoError(t, a.UpdateMockParams(ctx, f.params))
		got := p.logged()[from:]
		require.NotEmpty(t, got)
		require.Equal(t, "learn-ended=true", got[0], "the learn step ends before the filter settles or serves: %v", got)
		require.Contains(t, got, "settle", "the filter settles the calls made before it: %v", got)
		require.Contains(t, got, f.served, "the filter is served: %v", got)
		require.True(t, p.mm.LearnEnded(), "the learn step stays ended for the rest of the set")
	}
	// A later filter that does not say it (an older or another client)
	// leaves it ended: only a staging takes it back up.
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}))
	require.True(t, p.mm.LearnEnded(), "a filter that does not say the learn step ended takes it back up mid-set")
	require.NoError(t, a.UpdateMockParams(ctx, stage))
	require.False(t, p.mm.LearnEnded(), "the next set's staging takes the learn step back up")

	b, _ := wireAgent(t, models.MODE_TEST)
	q := &servesLogProxy{servingProxy: newServingProxy()}
	b.Proxy = q
	require.NoError(t, b.StoreMocks(ctx, nil, mocks))
	require.NoError(t, b.UpdateMockParams(ctx, stage))
	from := len(q.logged())
	err := b.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}, LearnEnded: true})
	require.ErrorContains(t, err, "cannot stop its learn step")
	require.Empty(t, q.logged()[from:], "a refused filter settles and serves nothing")
	require.NoError(t, b.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}),
		"a filter that does not ask to end it is served as before")
}

// keptOwnedProxy is learnEndProxy that keeps, by test, the own mocks each
// test it serves is served.
type keptOwnedProxy struct {
	learnEndProxy
	owned map[string][][]*models.Mock
}

func (p *keptOwnedProxy) ServeExactTest(test string, owned []*models.Mock) error {
	p.lmu.Lock()
	p.owned[test] = append(p.owned[test], owned)
	p.lmu.Unlock()
	return p.learnEndProxy.ServeExactTest(test, owned)
}

// SetKeptLearning implements coreAgent.KeptLearningSetter on the real mock
// manager, as the proxy does.
func (p *keptOwnedProxy) SetKeptLearning(l *learn.KeptLearning) error {
	p.log("kept-learning")
	p.mm.SetKeptLearning(l)
	return nil
}

// stagedNamed is the recording named name the manager's reads hand out, of
// every tier; nil when none shows it.
func stagedNamed(t *testing.T, mm *proxyPkg.MockManager, name string) *models.Mock {
	t.Helper()
	f, err := mm.GetFilteredMocks()
	require.NoError(t, err)
	u, err := mm.GetUnFilteredMocks()
	require.NoError(t, err)
	for _, mk := range append(f, u...) {
		if mk.Name == name {
			return mk
		}
	}
	return nil
}

// A test set's retry cycles are served its recordings with what its first
// cycle learned on them (models.MockFilterParams.Learned), as the next
// keploy test reads them once it is persisted: a test that passed in the
// first cycle only through learned drift is judged with it again, never
// failed (a false red) for learning the retry cycles do not take. That holds
// for a test's own recordings whether the agent holds them resident or on
// disk, and on a copy: the store's recording is not changed. It holds for
// the recordings the proxy staged at the set's start too, the pool's: the
// pool the retry cycles are served between tests carries it, through every
// later filter of the set, each of which says the learn step ended and
// carries none: the retry cycle's test cases, or the deferred streaming
// pass's, and the pool between them. The learning comes only with the learn
// step ended: on a filter that does not end it, it is refused, and nothing
// is served. A proxy that cannot put it on its staged recordings is refused
// before anything is loaded.
func TestARetryCycleIsServedTheRecordingsWithWhatTheFirstCycleLearned(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	ctx := context.Background()
	learned := []models.MockState{
		{Name: "mock-1", ReqBodyNoise: map[string][]string{"body.at": {}}},
		{Name: "mock-pool", ReqBodyNoise: map[string][]string{"body.beat": {}}},
	}
	for _, onDisk := range []bool{false, true} {
		t.Run(fmt.Sprintf("onDisk=%v", onDisk), func(t *testing.T) {
			p := &keptOwnedProxy{learnEndProxy: learnEndProxy{servesLogProxy{servingProxy: newServingProxy()}}, owned: map[string][][]*models.Mock{}}
			mock := pingMock("mock-1", "/ping", "ok", t0)
			pool := pingMock("mock-pool", "/beat", "ok", t0.Add(-time.Minute))
			stage := models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: t0.Add(2 * time.Hour), FirstRecordedTestStart: t0, StrictMockWindow: true, Exact: &models.ExactServing{Stage: true, Pool: []string{"mock-pool"}}}
			a, _ := wireAgent(t, models.MODE_TEST)
			a.Proxy = p
			if onDisk {
				a.clientMocks.Store(uint64(0), diskAgent(t, p, mock, pool).storage())
			} else {
				require.NoError(t, a.StoreMocks(ctx, []*models.Mock{mock, pool}, nil))
			}
			require.NoError(t, a.UpdateMockParams(ctx, stage))
			first := stagedNamed(t, p.mm, "mock-pool")
			require.NotNil(t, first, "the set's pool is served between tests")
			require.NotContains(t, first.Spec.ReqBodyNoise, "body.beat", "the first cycle is served the pool's recording as recorded")
			open := models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1", Owned: []string{"mock-1"}}}
			require.NoError(t, a.UpdateMockParams(ctx, open))
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}))

			from := len(p.logged())
			err := a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}, Learned: learned})
			require.ErrorContains(t, err, "does not end its learn step")
			require.Empty(t, p.logged()[from:], "a refused filter settles and serves nothing")

			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}, LearnEnded: true, Learned: learned}))
			retry := stagedNamed(t, p.mm, "mock-pool")
			require.NotNil(t, retry, "the retry cycle is served the set's pool between tests")
			require.Contains(t, retry.Spec.ReqBodyNoise, "body.beat", "the retry cycle is served the pool's recording without what the first cycle learned on it")
			require.Same(t, retry, stagedNamed(t, p.mm, "mock-pool"), "a staged recording read again is not the same learned copy")
			require.NotContains(t, first.Spec.ReqBodyNoise, "body.beat", "the staged recording itself was changed")
			open.LearnEnded = true
			require.NoError(t, a.UpdateMockParams(ctx, open))
			// Every filter after says the learn step ended, and carries no
			// learning: the test case it opens (above) and the pool after it,
			// a retry cycle's or the deferred streaming pass's, are served
			// the kept learning all the same, until the next staging.
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}, LearnEnded: true}))
			after := stagedNamed(t, p.mm, "mock-pool")
			require.NotNil(t, after, "the pool is served between test cases")
			require.Contains(t, after.Spec.ReqBodyNoise, "body.beat", "the pool after a test case opened with the learn step ended is served its recording without what the first cycle learned on it")
			served := p.owned["test-1"]
			require.Len(t, served, 2, "test-1 opened twice")
			require.Len(t, served[0], 1)
			require.NotContains(t, served[0][0].Spec.ReqBodyNoise, "body.at", "the first cycle is served the recording as recorded")
			require.Len(t, served[1], 1)
			require.Contains(t, served[1][0].Spec.ReqBodyNoise, "body.at", "the retry cycle is served test-1's own recording without what the first cycle learned on it")
			require.Nil(t, mock.Spec.ReqBodyNoise, "the store's recording itself was changed")
		})
	}
	t.Run("a proxy that cannot carry it on its staged recordings", func(t *testing.T) {
		p := &learnEndProxy{servesLogProxy{servingProxy: newServingProxy()}}
		mock := pingMock("mock-1", "/ping", "ok", t0)
		a, _ := wireAgent(t, models.MODE_TEST)
		a.Proxy = p
		require.NoError(t, a.StoreMocks(ctx, []*models.Mock{mock}, nil))
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: t0.Add(2 * time.Hour), FirstRecordedTestStart: t0, StrictMockWindow: true, Exact: &models.ExactServing{Stage: true}}))
		err := a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}, LearnEnded: true, Learned: learned})
		require.ErrorContains(t, err, "cannot serve the staged recordings")
		s := a.storage()
		for _, tier := range [][]*models.Mock{s.filtered, s.unfiltered} {
			for _, mk := range tier {
				require.Same(t, mock, mk, "a refused filter loaded the learning on the agent's store")
			}
		}
	})
}

// storage is the agent's stored mocks.
func (a *Agent) storage() *ClientMockStorage {
	v, _ := a.clientMocks.Load(uint64(0))
	s, _ := v.(*ClientMockStorage)
	return s
}

// A partial replay of the last test of a large set (keploy mock replay --
// -run TestLast) hands, at its begin, every earlier test the run has not
// begun (founder rule no-oom-components: "a row pins peak heap"). With 1000
// such tests' 2 recordings each on the agent's disk store, each with a
// 64 KiB request (128 MiB decoded), the begin reads none of them: the heap
// held after it stays under 16 MiB. A call of the first test's recording is
// still served aside, read from disk as the call asks, by its keys (the
// big ones are of another shape and are never read), and after it the heap
// held stays under the bound. Mutant: the agent reads them at the begin.
func TestAPartialReplayOfALargeSetHandsAsideWithinABoundHeap(t *testing.T) {
	const tests, per, body, bound = 1000, 2, 64 << 10, 16 << 20
	ctx := context.Background()
	disk, err := proxyPkg.NewDiskMocks(zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = disk.Close() })
	pat := pingMock("pat", "/pat", "pat", t0)
	pat.TestModeInfo.Lifetime = models.LifetimePerTest
	require.NoError(t, disk.Add(pat))
	table := map[string][]string{"o::T0000": {"pat"}}
	windows := []models.TestWindow{{TestCase: "o::T0000", Start: t0, End: t0.Add(time.Second)}}
	pad := strings.Repeat("x", body)
	for i := 1; i <= tests; i++ {
		id := fmt.Sprintf("o::T%04d", i)
		at := t0.Add(time.Duration(i) * time.Second)
		for j := 0; j < per; j++ {
			mk := pingMock(fmt.Sprintf("big-%d-%d", i, j), fmt.Sprintf("/big/%d", i), "ok", at.Add(time.Duration(j)*time.Millisecond))
			mk.Spec.HTTPReq.Method = "POST"
			mk.Spec.HTTPReq.Body = pad
			mk.TestModeInfo.Lifetime = models.LifetimePerTest
			require.NoError(t, disk.Add(mk))
			table[id] = append(table[id], mk.Name)
		}
		windows = append(windows, models.TestWindow{TestCase: id, Start: at, End: at.Add(500 * time.Millisecond)})
	}
	disk.Finalize()
	last := t0.Add(time.Duration(tests+1) * time.Second)
	own := pingMock("own", "/own", "own", last)
	table["o::TestLast"] = []string{"own"}
	windows = append(windows, models.TestWindow{TestCase: "o::TestLast", Start: last, End: last.Add(time.Second)})

	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
	a.Proxy = p
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{own}))
	st := loadStorage(t, a)
	st.mu.Lock()
	st.diskMocks = disk
	st.mu.Unlock()
	p.mm.SetRecordingSource(disk.RangeKeys)
	require.NoError(t, a.SetScopeTable(ctx, table))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{}))
	require.NoError(t, a.SetRecordedWindows(ctx, windows))
	claimSelf(t, a)

	base := heapHeld()
	_, serr := a.ProvenBegin(ctx, begin("TestLast"))
	require.Nil(t, serr)
	held := int64(heapHeld()) - int64(base)
	require.LessOrEqual(t, held, int64(bound), "the begin hands %d tests aside: it must read none of their recordings", tests)
	var got []string
	for _, path := range []string{"/own", "/pat"} {
		code, b := p.call(t, path)
		got = append(got, fmt.Sprintf("%d %s", code, strings.TrimSpace(b)))
	}
	require.Equal(t, []string{"200 own", "200 pat"}, got, "the first test's recording is served aside, read from disk as the call asks")
	held = int64(heapHeld()) - int64(base)
	require.LessOrEqual(t, held, int64(bound), "after the call none of the handed recordings is kept")
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestLast", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Empty(t, e.Action, e.Reason)
	require.Equal(t, []string{"pat served from o::T0000 (set aside)"}, evidenceOf(t, a, "o::TestLast").ServedAside)
}

// heapHeld is the heap's live bytes after a collection.
func heapHeld() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}
