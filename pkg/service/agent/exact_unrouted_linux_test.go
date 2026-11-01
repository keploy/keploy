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
// agent: the recordings, the table (o::TestA lists mock-1, o::TestB mock-2)
// and its pool, then the run's claim.
func routedMockSession(t *testing.T, a *Agent, mocks []*models.Mock) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, mocks))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::TestA": {"mock-1"}, "o::TestB": {"mock-2"}}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {}}))
	claimSelf(t, a)
}

// keploy mock replay end to end (the agent's wire, the real mock manager and
// the real HTTP parser): TestA makes TestB's call. Its miss is the mapping
// edit, naming both the window it was made in (o::TestA's) and the test whose
// list holds its recording (o::TestB), never record's judging nor a record
// again. A second session (MockOutgoing resets the proxy's session) is told
// again: the agent says it once per session, never once per agent. (Between
// tests the agent holds no test's recording staged, so none is named there;
// the HTTP parser's rows word that case.)
func TestKeployMockReplaysMissIsTheMappingEditInEverySession(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &routingProxy{newServingProxy()}
	a.Proxy = p
	mocks := []*models.Mock{pingMock("mock-1", "/a", "a", t0), pingMock("mock-2", "/b", "b", t0.Add(time.Second))}
	ctx := context.Background()
	check := func(session string) {
		_, serr := a.ProvenBegin(ctx, begin("TestA"))
		require.Nil(t, serr)
		code, rep := routedMiss(t, p.servingProxy, "/b")
		_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
		require.NotEqual(t, nethttp.StatusOK, code, session)
		require.NotNil(t, rep, session)
		require.True(t, rep.NotOwned, "%s: not named not-owned: %+v", session, rep)
		require.True(t, strings.HasPrefix(rep.NextSteps, "This call, made in o::TestA's window, is mock-2, which mappings.yaml lists for o::TestB."), "%s: %q", session, rep.NextSteps)
		require.Contains(t, rep.NextSteps, "move mock-2 out of o::TestB's list to worker: in mappings.yaml", session)
		require.NotContains(t, rep.NextSteps, "record again", session)
		require.NotContains(t, rep.NextSteps, "judging", session)
	}
	routedMockSession(t, a, mocks)
	check("session 1")
	require.NoError(t, a.MockOutgoing(ctx, models.OutgoingOptions{}))
	routedMockSession(t, a, mocks)
	check("session 2")
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
		})
	}
}
