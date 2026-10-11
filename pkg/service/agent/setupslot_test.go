package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// replayAgentWithMocks is replayAgent with full mocks (timestamps, bodies), which
// the spec pool needs to order tests and slot setup copies.
func replayAgentWithMocks(t *testing.T, mocks []*models.Mock, table map[string][]string) (*Agent, *fakeScopeProxy) {
	t.Helper()
	p := newFakeScopeProxy()
	a := &Agent{logger: zap.NewNop(), Proxy: p}
	a.config = &config.Config{}
	a.config.Agent.Mode = models.MODE_TEST
	a.clientMocks.Store(uint64(0), &ClientMockStorage{filtered: mocks})
	require.NoError(t, a.SetScopeTable(context.Background(), table, false))
	return a, p
}

func laterSet(p *fakeScopeProxy) map[string]bool {
	out := map[string]bool{}
	for _, m := range p.last {
		if m.TestModeInfo.FenceLater {
			out[m.Name] = true
		}
	}
	return out
}

func TestPlaceSetupSlotsCopiesByRecordedTestOrder(t *testing.T) {
	const q = `{"operationName":"Q","query":"query Q { q }"}`
	mocks := []*models.Mock{
		httpMock("s-before", "POST", "/query", q, 200, "a", 5),
		httpMock("t1-a", "POST", "/query", q, 200, "b", 10),
		httpMock("t1-b", "POST", "/query", q, 200, "c", 12),
		httpMock("s-gap1", "POST", "/query", q, 200, "d", 20),
		httpMock("t2-a", "POST", "/query", q, 200, "e", 30),
		httpMock("s-after2", "POST", "/query", q, 200, "f", 40),
	}
	owner := map[string]string{
		"s-before": "__suite__:x.spec.ts", "s-gap1": "__suite__:x.spec.ts", "s-after2": "__suite__:x.spec.ts",
		"t1-a": "x.spec.ts > d > t1", "t1-b": "x.spec.ts > d > t1", "t2-a": "x.spec.ts > d > t2",
	}
	sp := &specPool{}
	sp.placeSetup(owner, mocks)
	require.Equal(t, map[string]int{"x.spec.ts > d > t1": 1, "x.spec.ts > d > t2": 2}, sp.index)
	require.Equal(t, map[string]int{"s-before": 0, "s-gap1": 1, "s-after2": 2}, sp.setupSlot,
		"a setup copy's slot is how many tests had started when it was recorded")
}

// filtering.spec.ts: the suite list was recorded by three beforeAll hooks
// (before test 1, after test 2, after test 3). After test 1 ends, only the
// first hook's copy may be used up; the later hooks' copies are held until the
// replay reaches them.
func TestSetupPoolHoldsLaterHooksCopiesUntilTheirTurn(t *testing.T) {
	ctx := context.Background()
	const list = `{"operationName":"Suites","query":"query Suites { suites { id } }"}`
	const other = `{"operationName":"Other","query":"query Other { o }"}`
	mocks := []*models.Mock{
		httpMock("hook1-list", "POST", "/query", list, 200, "six suites", 5),
		httpMock("t1-own", "POST", "/query", other, 200, "t1", 10),
		httpMock("t2-own", "POST", "/query", other, 200, "t2", 20),
		httpMock("hook2-list", "POST", "/query", list, 200, "six suites, reordered", 25),
		httpMock("t3-own", "POST", "/query", other, 200, "t3", 30),
		httpMock("hook3-list", "POST", "/query", list, 200, "two suites", 35),
	}
	a, p := replayAgentWithMocks(t, mocks, map[string][]string{
		"__suite__:f.spec.ts": {"hook1-list", "hook2-list", "hook3-list"},
		"f.spec.ts > d1 > t1": {"t1-own"},
		"f.spec.ts > d1 > t2": {"t2-own"},
		"f.spec.ts > d2 > t3": {"t3-own"},
	})

	_, err := a.BeginScope(ctx, "__suite__:f.spec.ts", 0, 0)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"hook2-list": true, "hook3-list": true}, laterSet(p),
		"before any test, only the first beforeAll's copy is its turn")

	for _, tc := range []struct {
		test  string
		later map[string]bool
	}{
		{"f.spec.ts > d1 > t1", map[string]bool{"hook2-list": true, "hook3-list": true}},
		{"f.spec.ts > d1 > t2", map[string]bool{"hook3-list": true}},
		{"f.spec.ts > d2 > t3", map[string]bool{}},
	} {
		_, err := a.BeginScope(ctx, tc.test, 0, 0)
		require.NoError(t, err)
		require.NoError(t, a.EndScope(ctx, tc.test, 0))
		require.Equal(t, tc.later, laterSet(p), "after %s", tc.test)
		for _, m := range p.last {
			require.Equal(t, "setup:f.spec.ts#"+map[string]string{"f.spec.ts > d1 > t1": "1", "f.spec.ts > d1 > t2": "2", "f.spec.ts > d2 > t3": "3"}[tc.test],
				m.TestModeInfo.FenceScope, "every mock in a setup pool carries the setup period, so repeat-last works in hooks and gaps")
		}
	}
}

// A worker restart reopens the suite mid-spec; the replay position must
// survive it, or the re-run hook would be handed the first hook's turn again.
func TestSuiteReopenedMidSpecKeepsThePosition(t *testing.T) {
	ctx := context.Background()
	const list = `{"operationName":"Suites","query":"query Suites { suites { id } }"}`
	mocks := []*models.Mock{
		httpMock("hook1-list", "POST", "/query", list, 200, "a", 5),
		httpMock("t1-own", "GET", "/x", "", 200, "t1", 10),
		httpMock("hook2-list", "POST", "/query", list, 200, "b", 15),
		httpMock("t2-own", "GET", "/y", "", 200, "t2", 20),
		httpMock("hook3-list", "POST", "/query", list, 200, "c", 25),
	}
	a, p := replayAgentWithMocks(t, mocks, map[string][]string{
		"__suite__:f.spec.ts": {"hook1-list", "hook2-list", "hook3-list"},
		"f.spec.ts > d > t1":  {"t1-own"},
		"f.spec.ts > d > t2":  {"t2-own"},
	})
	_, err := a.BeginScope(ctx, "__suite__:f.spec.ts", 0, 0)
	require.NoError(t, err)
	_, err = a.BeginScope(ctx, "f.spec.ts > d > t1", 0, 0)
	require.NoError(t, err)
	require.NoError(t, a.EndScope(ctx, "f.spec.ts > d > t1", 0))
	_, err = a.BeginScope(ctx, "__suite__:f.spec.ts", 0, 0)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"hook3-list": true}, laterSet(p))
}

// A test with no recording of its own has no place in the order: ending it
// moves nothing.
func TestUnrecordedTestDoesNotMoveThePosition(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	a.specPools = map[string]*specPool{"f.spec.ts": {index: map[string]int{"f.spec.ts > d > t1": 1}}}
	a.noteTestEnded("f.spec.ts > d > t1")
	a.noteTestEnded("f.spec.ts > d > never recorded")
	require.Equal(t, 1, a.specPos["f.spec.ts"])
}
