//go:build linux

package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// claimBrowser claims the run for this test process with key "o", as a
// harness that drives a browser page claims it.
func claimBrowser(t *testing.T, a *Agent) {
	t.Helper()
	_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: "o", Browser: true}, os.Getpid())
	require.Nil(t, serr)
}

// A browser page's poll that fires once more than recorded is refused as
// before (nothing is served twice), but its test, whose own checks passed,
// passes: the read is said on its window. A test process that drives no
// browser page is failed for it, as is a browser test that failed its checks.
func TestABrowserPagesExtraReadIsSaidNotCounted(t *testing.T) {
	mocks := []*models.Mock{pingMock("mock-1", "/n", "one", t0), pingMock("mock-2", "/n", "two", t0.Add(1))}
	table := map[string][]string{"o::TestPoll": {"mock-1", "mock-2"}}

	a, p := unclaimedExactAgent(t, mocks, table, map[string][]string{})
	claimBrowser(t, a)
	got, e := runTest(t, a, p, "TestPoll", "/n", "/n", "/n")
	require.True(t, strings.HasPrefix(got[2], "502"), "the extra read is refused: %v", got)
	require.Empty(t, e.Action, e.Reason)
	ev := evidenceOf(t, a, "o::TestPoll")
	require.Empty(t, ev.Misses)
	require.Equal(t, []string{"GET /n to 127.0.0.1:18093 was called 3 times, recorded 2: a read the page made more often, not counted"}, ev.CountFree)

	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestPoll"))
	require.Nil(t, serr)
	for range 3 {
		p.call(t, "/n")
	}
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestPoll", Outcome: models.ScopeOutcomeFailed})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "GET /n to 127.0.0.1:18093 was called 3 times, recorded 2", "a test that failed its own checks counts it")

	a, p = exactAgent(t, mocks, table, map[string][]string{})
	_, e = runTest(t, a, p, "TestPoll", "/n", "/n", "/n")
	require.Equal(t, models.ScopeActionFail, e.Action, "a test process that drives no browser page is judged as before")
	require.Contains(t, e.Reason, "was called 3 times, recorded 2")
}

// A browser page's poll that fires once fewer than recorded is not counted
// when the test made the same read and was answered the same; when the
// missing poll's answer was another (a job that finished), the test fails for
// it.
func TestABrowserPagesMissingReadIsNotCountedOnlyWhenAnsweredTheSame(t *testing.T) {
	for _, tc := range []struct {
		second string
		fails  string
	}{
		{second: "same"},
		{second: "done", fails: "recorded call GET 127.0.0.1:18093/n (mock-2) was never made"},
	} {
		mocks := []*models.Mock{pingMock("mock-1", "/n", "same", t0), pingMock("mock-2", "/n", tc.second, t0.Add(1))}
		a, p := unclaimedExactAgent(t, mocks, map[string][]string{"o::TestPoll": {"mock-1", "mock-2"}}, map[string][]string{})
		claimBrowser(t, a)
		_, e := runTest(t, a, p, "TestPoll", "/n")
		if tc.fails == "" {
			require.Empty(t, e.Action, e.Reason)
			require.Equal(t, []string{"recorded call GET 127.0.0.1:18093/n (mock-2) was never made, but the test made the same read and mock-1 answered it the same: not counted"},
				evidenceOf(t, a, "o::TestPoll").CountFree)
			continue
		}
		require.Equal(t, models.ScopeActionFail, e.Action, tc.second)
		require.Contains(t, e.Reason, tc.fails)
	}
}
