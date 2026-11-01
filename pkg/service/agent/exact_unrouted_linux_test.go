//go:build linux

package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	nethttp "net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// routingProxy is a servingProxy that forwards the staging calls to its real
// mock manager as the agent's Proxy does (proxy.go StageExactBackground,
// ServeExactBackground, StageExactUnrouted, SetExactListing, and Mock's
// session reset, ResetForReplaySession): servingProxy forwards none of them.
type routingProxy struct{ *servingProxy }

func (p *routingProxy) StageExactUnrouted(why string) error {
	p.mm.StageExactUnrouted(why)
	return nil
}

func (p *routingProxy) SetExactListing(listed map[string][]string) error {
	p.mm.SetExactListing(listed)
	return nil
}

func (p *routingProxy) StageExactBackground(rules string) error {
	p.mm.StageExactBackground(rules)
	return nil
}

func (p *routingProxy) ServeExactBackground(rules string, pool, worker []string) error {
	p.mm.ServeExactBackground(rules, pool, worker)
	return nil
}

func (p *routingProxy) Mock(context.Context, models.OutgoingOptions) error {
	p.mm.ResetForReplaySession()
	return nil
}

// routedMiss sends GET path through the real HTTP parser against p's mock
// manager and returns the response code and the miss's report, if any.
func routedMiss(t *testing.T, p *servingProxy, path string) (int, *models.MockMismatchReport) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	app, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer func() { _ = app.Close() }()
	prox := <-acc
	defer func() { _ = prox.Close() }()
	var mu sync.Mutex
	var rep *models.MockMismatchReport
	repCh := make(chan struct{}, 1)
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) {
		var r interface {
			MismatchReport() *models.MockMismatchReport
		}
		if errors.As(err, &r) && r.MismatchReport() != nil {
			mu.Lock()
			rep = r.MismatchReport()
			mu.Unlock()
		}
		select {
		case repCh <- struct{}{}:
		default:
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = httpparser.New(zap.NewNop()).MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Port: 18093}, p.mm, models.OutgoingOptions{})
	}()
	t.Cleanup(func() { _ = prox.Close(); <-done })
	_, err = io.WriteString(app, "GET "+path+" HTTP/1.1\r\nHost: 127.0.0.1:18093\r\n\r\n")
	require.NoError(t, err)
	resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != nethttp.StatusOK {
		// The report follows the response: the parser reports the miss as
		// it answers it. A report is a step of the row, never waited for
		// by the clock: the row fails, with what it got, if none comes.
		<-repCh
	}
	mu.Lock()
	defer mu.Unlock()
	return resp.StatusCode, rep
}

// routedMockSession is one keploy mock replay session as its CLI drives the
// agent: the recordings, the table (o::TestA lists mock-1, o::TestB mock-2,
// mock-3 and mock-4) and its pool, then the run's claim.
func routedMockSession(t *testing.T, a *Agent, mocks []*models.Mock) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, mocks))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::TestA": {"mock-1"}, "o::TestB": {"mock-2", "mock-3", "mock-4"}}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {}}))
	claimSelf(t, a)
}

// requireMappingEdit is a refused call's miss that names name as a
// recording made elsewhere, made where made says, with the mapping edit out
// of o::TestB's list, never record's judging nor a record again.
func requireMappingEdit(t *testing.T, code int, rep *models.MockMismatchReport, made, name, at string) {
	t.Helper()
	require.NotEqual(t, nethttp.StatusOK, code, at)
	require.NotNil(t, rep, at)
	require.True(t, rep.NotOwned, "%s: not named not-owned: %+v", at, rep)
	require.Equal(t, name, rep.ClosestMock, at)
	require.True(t, strings.HasPrefix(rep.NextSteps, "This call, made "+made+", is "+name+", which mappings.yaml lists for o::TestB."), "%s: %q", at, rep.NextSteps)
	require.Contains(t, rep.NextSteps, "move "+name+" out of o::TestB's list to worker: in mappings.yaml", at)
	require.NotContains(t, rep.NextSteps, "record again", at)
	require.NotContains(t, rep.NextSteps, "Re-record", at)
	require.NotContains(t, rep.NextSteps, "judging", at)
}

// keploy mock replay end to end (the agent's wire, the real mock manager and
// the real HTTP parser): calls made outside the window of the test whose list
// holds their recording (o::TestB's). Each miss is the mapping edit, naming
// where the call was made and the test whose list holds its recording, never
// record's judging nor a record again: between tests before any test (mock-3),
// in o::TestA's window (mock-2), and between tests after o::TestA (mock-4).
// A second session (MockOutgoing resets the proxy's session) is told again:
// the agent says it once per session, never once per agent.
func TestKeployMockReplaysMissIsTheMappingEditInEverySession(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &routingProxy{newServingProxy()}
	a.Proxy = p
	mocks := []*models.Mock{
		pingMock("mock-1", "/a", "a", t0), pingMock("mock-2", "/b", "b", t0.Add(time.Second)),
		pingMock("mock-3", "/c", "c", t0.Add(2*time.Second)), pingMock("mock-4", "/d", "d", t0.Add(3*time.Second)),
	}
	ctx := context.Background()
	check := func(session string) {
		code, rep := routedMiss(t, p.servingProxy, "/c")
		requireMappingEdit(t, code, rep, "between tests", "mock-3", session+", before any test")
		_, serr := a.ProvenBegin(ctx, begin("TestA"))
		require.Nil(t, serr)
		code, rep = routedMiss(t, p.servingProxy, "/b")
		_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
		requireMappingEdit(t, code, rep, "in o::TestA's window", "mock-2", session+", in o::TestA's window")
		code, rep = routedMiss(t, p.servingProxy, "/d")
		requireMappingEdit(t, code, rep, "between tests", "mock-4", session+", after o::TestA")
	}
	routedMockSession(t, a, mocks)
	check("session 1")
	require.NoError(t, a.MockOutgoing(ctx, models.OutgoingOptions{}))
	routedMockSession(t, a, mocks)
	check("session 2")
}

// keploy mock replay end to end: the application makes TestB's call twice
// between tests, once before o::TestA and once after it. The first miss is
// the mapping edit for mock-2. The second finds no recording left to name (a
// recording is named for one miss at most, as it answers one call at most):
// it names mock-2 again, with worker:'s limit (moving mock-2 there answers
// one of the two calls, never both), never the "no recorded mocks ...
// Re-record" of a set that holds no recording of the call, nor record's
// judging. A third, in o::TestA's window, says the same of its window.
func TestKeployMockReplaysSecondCallBetweenTestsNamesWorkersLimit(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &routingProxy{newServingProxy()}
	a.Proxy = p
	ctx := context.Background()
	routedMockSession(t, a, []*models.Mock{pingMock("mock-1", "/a", "a", t0), pingMock("mock-2", "/b", "b", t0.Add(time.Second))})
	code, rep := routedMiss(t, p.servingProxy, "/b")
	requireMappingEdit(t, code, rep, "between tests", "mock-2", "the first call")
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	again := func(made, at string) {
		t.Helper()
		code, rep := routedMiss(t, p.servingProxy, "/b")
		require.NotEqual(t, nethttp.StatusOK, code, at)
		require.NotNil(t, rep, at)
		require.False(t, rep.NotOwned, "%s: a recording is named not-owned for one miss at most: %+v", at, rep)
		require.True(t, strings.HasPrefix(rep.NextSteps, "This call, made "+made+", is mock-2, which mappings.yaml lists for o::TestB, again: an earlier call this replay made outside mock-2's window was already named as it."), "%s: %q", at, rep.NextSteps)
		require.Contains(t, rep.NextSteps, "so moving mock-2 to worker: answers one of these calls, never both.", at)
		require.Contains(t, rep.NextSteps, "'keploy mock record'", at)
		for _, never := range []string{"No recorded mocks", "Re-record", "record again", "judging", "'keploy record'"} {
			require.NotContains(t, rep.NextSteps, never, at)
		}
	}
	again("between tests", "the second call, after o::TestA")
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	again("in o::TestA's window", "the third call, in o::TestA's window")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
}

// keploy test end to end: a set staged with each reason the replay can send
// (no mark, the mark not enabled, none said). A test case that makes another
// test case's call is told record's judging, never keploy mock's mapping
// edit.
func TestKeployTestsMissIsNeverTheMappingEdit(t *testing.T) {
	for _, why := range []string{models.UnroutedUnmarked, models.UnroutedNotEnabled, ""} {
		t.Run("unrouted="+why, func(t *testing.T) {
			a, _ := wireAgent(t, models.MODE_TEST)
			p := &routingProxy{newServingProxy()}
			a.Proxy = p
			ctx := context.Background()
			require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{pingMock("mock-1", "/a", "a", t0), pingMock("mock-2", "/b", "b", t0.Add(time.Second))}))
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
				AfterTime: models.BaseTime, BeforeTime: time.Now(),
				Exact: &models.ExactServing{Stage: true, Unrouted: why},
			}))
			require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1", Owned: []string{"mock-1"}}}))
			require.Equal(t, why, p.mm.MarkCallRound().Unrouted(), "precondition: the set's views read the reason it was staged with")
			code, rep := routedMiss(t, p.servingProxy, "/b")
			require.NotEqual(t, nethttp.StatusOK, code)
			require.NotNil(t, rep)
			require.True(t, rep.NotOwned, "%+v", rep)
			require.Contains(t, rep.NextSteps, "keploy record's judging")
			require.NotContains(t, rep.NextSteps, "keploy mock does not decide")
			// The same call again names no recording (record's judging
			// moves each recording a miss names), and keeps keploy test's
			// words: never keploy mock's worker: limit.
			code, rep = routedMiss(t, p.servingProxy, "/b")
			require.NotEqual(t, nethttp.StatusOK, code)
			require.NotNil(t, rep)
			require.False(t, rep.NotOwned, "%+v", rep)
			require.NotContains(t, rep.NextSteps, "again:")
			require.NotContains(t, rep.NextSteps, "worker:")
		})
	}
}
