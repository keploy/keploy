package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// keploy mock replay: a recording the test's mappings entry marks
// `repeat: any` (the scope table's repeat) answers the test's identical
// read once, twice or three times, and the test passes each time; read
// none, the test fails on it unconsumed. The same test unmarked fails its
// second read as one call more than recorded: the old exact rule.
func TestMockReplayServesAMarkedRecordingForEveryIdenticalReadOfItsTest(t *testing.T) {
	table := map[string][]string{"o::TestCluster": {"mock-1"}}
	for _, reads := range []int{1, 2, 3} {
		a, p := exactAgent(t, []*models.Mock{pingMock("mock-1", "/cluster", "c1", t0)}, table, map[string][]string{})
		require.NoError(t, a.SetScopeRepeat(context.Background(), map[string][]string{"o::TestCluster": {"mock-1"}}))
		paths := make([]string, reads)
		for i := range paths {
			paths[i] = "/cluster"
		}
		got, e := runTest(t, a, p, "TestCluster", paths...)
		for i, g := range got {
			require.Equal(t, "200 c1", g, "%d reads: read %d", reads, i+1)
		}
		require.Empty(t, e.Action, "%d reads: the test failed: %s", reads, e.Reason)
	}
	a, p := exactAgent(t, []*models.Mock{pingMock("mock-1", "/cluster", "c1", t0)}, table, map[string][]string{})
	require.NoError(t, a.SetScopeRepeat(context.Background(), map[string][]string{"o::TestCluster": {"mock-1"}}))
	_, e := runTest(t, a, p, "TestCluster")
	require.Equal(t, models.ScopeActionFail, e.Action, "a marked recording the test never read is still required of it")

	a, p = exactAgent(t, []*models.Mock{pingMock("mock-1", "/cluster", "c1", t0)}, table, map[string][]string{})
	got, e := runTest(t, a, p, "TestCluster", "/cluster", "/cluster")
	require.True(t, strings.HasPrefix(got[1], "502"), "unmarked, the second read was answered: %s", got[1])
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "was called 2 times, recorded 1")
}

// keploy test and record's full round: the call that opens a test case says
// its recordings marked `repeat: any` (ExactServing.Repeat), and each answers
// its identical reads at any count; another test case's mark is not this
// one's.
func TestKeployTestServesAMarkedRecordingForEveryIdenticalReadOfItsTestCase(t *testing.T) {
	a, p := keployTestAgent(t, []*models.Mock{pingMock("mock-1", "/cluster", "c1", t0), pingMock("mock-2", "/cluster", "c1", t0.Add(1))}, nil)
	ctx := context.Background()
	open := func(name string, owned, repeat []string, paths ...string) ([]string, int) {
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: name, Owned: owned, Repeat: repeat}}))
		var got []string
		for _, path := range paths {
			code, body := p.call(t, path)
			got = append(got, strings.TrimSpace(body))
			if code != 200 {
				got[len(got)-1] = "miss"
			}
		}
		misses, err := p.GetMockErrors(ctx)
		require.NoError(t, err)
		require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}))
		return got, len(misses)
	}
	got, misses := open("test-1", []string{"mock-1"}, []string{"mock-1"}, "/cluster", "/cluster", "/cluster")
	require.Equal(t, []string{"c1", "c1", "c1"}, got)
	require.Zero(t, misses)
	got, misses = open("test-2", []string{"mock-2"}, nil, "/cluster", "/cluster")
	require.Equal(t, []string{"c1", "miss"}, got, "test-2 marks nothing: its recording answers once")
	require.Equal(t, 1, misses)
}
