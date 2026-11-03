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

// The load row: a poller the app runs between tests, whose two calls record
// listed in worker:. On replay the app polls as often as it did at record
// (1x) and, as when replay is slower, twice as often (2x). Between tests the
// worker scope is served as the start-up pool is (reused, or saturated on
// its last answer), so every poll is answered, nothing is left unanswered
// outside every test, and each test's verdict at 2x is the one at 1x.
func TestKeployMockABetweenTestPollerAtTwiceItsCallsMovesNoVerdict(t *testing.T) {
	run := func(polls int) (verdicts, answers []string, misses []models.UnmatchedCall) {
		a, _ := wireAgent(t, models.MODE_TEST)
		p := newServingProxy()
		a.Proxy = p
		ctx := context.Background()
		require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{
			pingMock("mock-1", "/a", "a", t0),
			pingMock("mock-2", "/poll", "poll", t0.Add(time.Second)),
			pingMock("mock-3", "/poll", "poll", t0.Add(2*time.Second)),
			pingMock("mock-4", "/b", "b", t0.Add(3*time.Second)),
		}))
		require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::TestA": {"mock-1"}, "o::TestB": {"mock-4"}}))
		require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{"o": {"mock-2", "mock-3"}}))
		require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-2", "mock-3"}}))
		claimSelf(t, a)
		for i, name := range []string{"TestA", "TestB"} {
			got, e := runTest(t, a, p, name, []string{"/a", "/b"}[i])
			verdicts = append(verdicts, fmt.Sprintf("%s %v %s %q", name, got, e.Action, e.Reason))
			if i > 0 {
				break
			}
			for j := 0; j < polls; j++ {
				code, body := p.call(t, "/poll")
				answers = append(answers, fmt.Sprintf("%d %s", code, strings.TrimSpace(body)))
			}
		}
		return verdicts, answers, a.takeChargedMisses()
	}
	v1, a1, m1 := run(2)
	v2, a2, m2 := run(4)
	require.Equal(t, []string{"200 poll", "200 poll"}, a1, "1x: a poll between tests went unanswered")
	require.Equal(t, []string{"200 poll", "200 poll", "200 poll", "200 poll"}, a2, "2x: a poll between tests went unanswered")
	require.Empty(t, m1, "1x: a miss was charged")
	require.Empty(t, m2, "2x: a call between tests was left unanswered outside every test")
	require.Equal(t, v1, v2, "a test's verdict moved when the poller called twice as often")
	for _, v := range v1 {
		require.NotContains(t, v, " "+models.ScopeActionFail+" ", "a test that made its own calls failed")
	}
}

// A test window's answers never depend on how many calls landed between
// tests: a poller's three HTTP recordings are listed in worker:, TestB polls
// twice in its window, and only the number of polls between TestA and TestB
// changes (0, 1, 3 and 10, as a faster or slower replay makes it). The
// windows walk the worker scope on a cursor of their own, which no call
// between tests advances or saturates, so TestB is answered the first two
// recordings in placement order and passes every time. It holds for an
// untagged recording, which HTTP keeps as it answers, and for one stated
// per-test, as keploy mock record writes them.
func TestAWindowsVerdictDoesNotMoveWithThePollsBetweenTests(t *testing.T) {
	for _, shape := range []struct {
		name string
		meta map[string]string
	}{
		{"untagged", nil},
		{"stated per-test", map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			run := func(gap int) string {
				a, _ := wireAgent(t, models.MODE_TEST)
				p := newServingProxy()
				a.Proxy = p
				ctx := context.Background()
				mocks := []*models.Mock{pingMock("mock-a", "/a", "a", t0), pingMock("mock-b", "/b", "b", t0.Add(time.Hour))}
				var names []string
				for i := 0; i < 3; i++ {
					n := fmt.Sprintf("poll-%d", i)
					m := pingMock(n, "/poll", fmt.Sprintf("poll%d", i), t0.Add(time.Duration(i+1)*time.Second))
					m.Spec.Metadata = shape.meta
					mocks = append(mocks, m)
					names = append(names, n)
				}
				require.NoError(t, a.StoreMocks(ctx, nil, mocks))
				require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::TestA": {"mock-a"}, "o::TestB": {"mock-b"}}))
				require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{"o": names}))
				require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": names}))
				claimSelf(t, a)
				runTest(t, a, p, "TestA", "/a")
				for i := 0; i < gap; i++ {
					code, _ := p.call(t, "/poll")
					require.Equal(t, 200, code, "poll %d of %d between tests went unanswered", i+1, gap)
				}
				got, e := runTest(t, a, p, "TestB", "/b", "/poll", "/poll")
				return fmt.Sprintf("%s verdict=%q", strings.Join(got, ","), e.Action)
			}
			want := run(0)
			require.True(t, strings.HasPrefix(want, "200 b,200 poll0,200 poll1 "), "with no poll between tests TestB was answered %s; want b, then poll0 and poll1 in placement order", want)
			for _, gap := range []int{1, 3, 10} {
				require.Equal(t, want, run(gap), "TestB's answers and verdict moved with %d polls between tests", gap)
			}
		})
	}
}
