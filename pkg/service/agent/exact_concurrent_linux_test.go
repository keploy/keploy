//go:build linux

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
)

// ServeExactTestConcurrent implements coreAgent.ExactConcurrentServer on the
// test proxy: the real mock manager's shared serving.
func (p *servingProxy) ServeExactTestConcurrent(test string, owned, cands []*models.Mock, inputs, notLearnable []string) error {
	p.mm.ServeExactTestConcurrent(test, owned, proxyPkg.ExactConcurrent{Cands: cands, Inputs: inputs, NotLearnable: notLearnable})
	return nil
}

// OpenExactTestConcurrent implements coreAgent.ExactConcurrentServer on the
// test proxy: the real mock manager's shared serving, its variables begun in
// the same step.
func (p *servingProxy) OpenExactTestConcurrent(test string, owned, cands []*models.Mock, inputs, notLearnable []string, given []models.VariableBinding) error {
	p.mm.OpenExactTestConcurrent(test, owned, proxyPkg.ExactConcurrent{Cands: cands, Inputs: inputs, NotLearnable: notLearnable}, given)
	return nil
}

var _ coreAgent.ExactConcurrentServer = (*servingProxy)(nil)

// concurrentOpeningProxy is an openingProxy that counts the test cases opened
// with shared recordings, by each of the two steps.
type concurrentOpeningProxy struct {
	openingProxy
	servedShared, openedShared int
}

func (p *concurrentOpeningProxy) ServeExactTestConcurrent(test string, owned, cands []*models.Mock, inputs, notLearnable []string) error {
	p.servedShared++
	return p.servingProxy.ServeExactTestConcurrent(test, owned, cands, inputs, notLearnable)
}

func (p *concurrentOpeningProxy) OpenExactTestConcurrent(test string, owned, cands []*models.Mock, inputs, notLearnable []string, given []models.VariableBinding) error {
	p.openedShared++
	return p.servingProxy.OpenExactTestConcurrent(test, owned, cands, inputs, notLearnable, given)
}

// A test case of keploy record's full round that shares recordings with the
// tests that ran beside it opens with its variables in one step, as every
// test case under the switch does (OpenTestCase): the values its request gave
// are bound before any shared recording answers, and what its call binds is
// its own. Opened in two steps, a call a shared recording answered between
// them bound in the run's.
func TestKeployTestOpensATestCaseThatSharesRecordingsWithItsVariablesInOneStep(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &concurrentOpeningProxy{openingProxy: openingProxy{keepingProxy: keepingProxy{servingProxy: newServingProxy()}}}
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{keyMock("own-1", "/own-1", false, t0), keyMock("use-2", "/use-2", true, t0.Add(time.Second))}))
	p.mm.NewVariableSet()
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(),
		Exact: &models.ExactServing{Stage: true},
	}))
	given := []models.VariableBinding{{Name: "id", Value: "u1", Recorded: "u0", Mock: "test case test-1", Place: "request"}}
	// test-1 lists nothing of its own: the recording of its call is shared.
	require.NoError(t, a.OpenTestCase(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1", Concurrent: []string{"own-1"}}, Variables: given}))
	bound, kept := a.TestVariables()
	require.True(t, kept)
	require.Equal(t, "id=u1", keyNames(bound), "test-1 opens with the value its request gave")
	require.Equal(t, `200 {"echo":"x-1"}`, p.postTo(t, "/own-1", "x-1"))
	bound, _ = a.TestVariables()
	require.Equal(t, "id=u1,key=x-1", keyNames(bound), "test-1 holds what the shared recording it answered with bound")
	require.Equal(t, 1, p.openedShared, "a test case that shares recordings was opened apart from its variables")
	require.Zero(t, p.servedShared, "a test case that shares recordings was opened apart from its variables")
	require.Zero(t, p.opened+p.served, "a test case that shares recordings was opened without them")
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}))
	// Opened by UpdateMockParams (no variables to begin), it is served in
	// one step with no variables.
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-2", Concurrent: []string{"use-2"}}}))
	require.Equal(t, 1, p.servedShared)
	require.Equal(t, 1, p.openedShared)
}

// keploy record's full round: a test case opened with the recordings it
// shares with the tests that ran beside it (models.ExactServing.Concurrent)
// is answered, through the real HTTP matcher, by the one that is its call,
// whichever test lists it; a shared recording answers once in the staging,
// so the test that lists it, run after, does not get it again; and the agent
// says it serves them.
func TestKeployTestServesATestCaseTheRecordingsItShares(t *testing.T) {
	a, p := keployTestAgent(t,
		[]*models.Mock{
			pingMock("mock-a", "/ping?n=a", "for-a", t0),
			pingMock("mock-b", "/ping?n=b", "for-b", t0.Add(time.Second)),
		}, nil)
	require.True(t, a.ServesExactConcurrent())
	ctx := context.Background()
	open := func(name string, owned, shared []string) {
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: name, Owned: owned, Concurrent: shared}}))
	}
	end := func() {
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}))
	}
	shared := []string{"mock-a", "mock-b"}
	// test-b first: the recording of its call is listed by test-a.
	open("test-b", nil, shared)
	code, body := p.call(t, "/ping?n=b")
	require.Equal(t, 200, code)
	require.Equal(t, "for-b", strings.TrimSpace(body))
	consumed := p.mm.GetConsumedMocks()
	require.Len(t, consumed, 1)
	require.Equal(t, "mock-b", consumed[0].Name)
	require.True(t, consumed[0].Concurrent, "a shared recording's consume says so")
	end()
	open("test-a", nil, shared)
	code, _ = p.call(t, "/ping?n=b")
	require.Equal(t, 502, code, "a shared recording another test answered with answered again")
	code, body = p.call(t, "/ping?n=a")
	require.Equal(t, 200, code)
	require.Equal(t, "for-a", strings.TrimSpace(body))
	end()
}

// A test case sent shared recordings is refused by a proxy that cannot serve
// them: it is never served less than its plan says without a word.
func TestAProxyThatCannotShareRefusesSharedRecordings(t *testing.T) {
	a, p := keployTestAgent(t, []*models.Mock{pingMock("mock-a", "/ping", "a", t0)}, nil)
	err := a.serveTestConcurrently(noShareProxy{p}, &models.ExactServing{Test: "t", Concurrent: []string{"mock-a"}}, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot serve t the recordings it shares")
}

// noShareProxy is an exact server without shared serving: an older proxy.
type noShareProxy struct{ inner *servingProxy }

func (p noShareProxy) ServeExactTest(test string, owned []*models.Mock) error {
	return p.inner.ServeExactTest(test, owned)
}
func (p noShareProxy) ServeExactPool(key string, names []string) error {
	return p.inner.ServeExactPool(key, names)
}
func (p noShareProxy) ExactTestOutcome() (string, []string, []string, bool) {
	return p.inner.ExactTestOutcome()
}
