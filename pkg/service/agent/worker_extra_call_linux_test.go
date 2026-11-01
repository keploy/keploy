//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// On the keploy mock path (each test judged by its own calls), a test's
// own /x recorded once, and an identical /x in the set's worker: list (a call
// a reader moved there, or one record listed between tests). The test's
// second /x must miss as extra_call, and the worker recording stay unspent.
func TestKeployMockWorkerScopeNeverAnswersATestsExtraCall(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{
		pingMock("mock-1", "/x", "own", t0),
		pingMock("mock-2", "/x", "worker", t0.Add(time.Second)),
		pingMock("mock-3", "/y", "worker-y", t0.Add(2*time.Second)),
	}))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::TestA": {"mock-1"}, "o::TestB": {}}))
	require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{"o": {"mock-2", "mock-3"}}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-2", "mock-3"}}))
	claimSelf(t, a)
	got, e := runTest(t, a, p, "TestA", "/x", "/x", "/y")
	t.Logf("TestA got %v action=%q reason=%q", got, e.Action, e.Reason)
	require.Equal(t, "200 own", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), "the worker scope answered the test's extra /x: %s", got[1])
	require.Equal(t, "200 worker-y", got[2], "a call the test never recorded is still the worker scope's")
	require.Equal(t, models.ScopeActionFail, e.Action, "the extra call must fail the test")
	require.Contains(t, e.Reason, "was called 2 times, recorded 1")
	code, body := p.call(t, "/x")
	require.Equal(t, "200 worker", fmt.Sprintf("%d %s", code, body), "the worker recording stays unspent and answers between tests")
}

// A worker: list names only what its own key's pool slice holds: a name the
// table lists under key o that only key p's slice holds (another test
// process's recording, moved into o's worker: by an edit) answers none of
// o's calls, in a test's window or between tests. Every recording is staged
// once for the whole set, so without the slice check the proxy would show it
// as o's worker scope and serve o another process's recording.
func TestAWorkerNameOutsideItsKeysSliceNeverAnswers(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{
		pingMock("mock-1", "/token", "o's first use", t0),
		pingMock("mock-2", "/other", "p's", t0.Add(time.Second)),
	}))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::TestA": {}, "p::TestB": {}}))
	require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{"o": {"mock-1", "mock-2"}}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-1"}, "p": {"mock-2"}}))
	claimSelf(t, a)
	got, e := runTest(t, a, p, "TestA", "/token", "/other")
	require.Equal(t, "200 o's first use", got[0], "o's worker: recording answers in o's test")
	require.True(t, strings.HasPrefix(got[1], "502"), "key p's recording answered key o's test: %s", got[1])
	require.Equal(t, models.ScopeActionFail, e.Action, "the unanswered call is the test's miss")
	code, body := p.call(t, "/other")
	require.Equal(t, 502, code, "key p's recording answered key o between tests: %s", body)
}

// The worker scope a test's window shows is the key the test runs under, also
// when the key changed while a test was open: a test process that died in
// its test, and the next process, claiming another key, begins its first
// test. The previous key's worker scope never answers the new key's test.
func TestATestAfterAKeyChangeIsShownItsOwnKeysWorkerScope(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{
		pingMock("mock-1", "/a", "a", t0),
		pingMock("mock-2", "/token", "o's first use", t0.Add(time.Second)),
		pingMock("mock-3", "/other", "p's first use", t0.Add(2*time.Second)),
	}))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::TestA": {"mock-1"}, "p::TestB": {}}))
	require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{"o": {"mock-2"}, "p": {"mock-3"}}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-2"}, "p": {"mock-3"}}))
	first := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "o"}, first.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	code, body := p.call(t, "/a")
	require.Equal(t, "200 a", fmt.Sprintf("%d %s", code, body))
	kill(t, first) // the process dies with TestA open
	_, serr = a.ClaimInstance(ctx, models.InstanceReq{Key: "p"}, os.Getpid())
	require.Nil(t, serr)
	got, e := runTest(t, a, p, "TestB", "/token", "/other")
	require.True(t, strings.HasPrefix(got[0], "502"), "key o's worker scope answered a test of key p: %s", got[0])
	require.Equal(t, "200 p's first use", got[1], "key p's worker scope must answer its test")
	require.Equal(t, models.ScopeActionFail, e.Action, "o's /token is the p test's miss")
}
