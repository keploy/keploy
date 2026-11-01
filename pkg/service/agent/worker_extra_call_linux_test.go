//go:build linux

package agent

import (
	"context"
	"fmt"
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
