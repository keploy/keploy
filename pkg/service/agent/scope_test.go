package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// With config == nil, BeginScope/EndScope take the record branch (not MODE_TEST).
func newRecordAgent() *Agent { return &Agent{logger: zap.NewNop()} }

func windowNames(a *Agent) []string {
	ws, _ := a.GetScopeWindows(context.Background())
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Name)
	}
	return out
}

// Nested/overlapping scopes with NO reported PID (pid==0) must each record their
// own window — keying record-open scopes by PID alone would collapse them onto
// worker 0 and silently lose the outer one.
func TestRecordScopesNestedPidZero(t *testing.T) {
	a := newRecordAgent()
	ctx := context.Background()
	require.NoError(t, a.BeginScope(ctx, "suite", 0))
	require.NoError(t, a.BeginScope(ctx, "test1", 0))
	require.NoError(t, a.EndScope(ctx, "test1", 0))
	require.NoError(t, a.EndScope(ctx, "suite", 0))
	require.ElementsMatch(t, []string{"suite", "test1"}, windowNames(a),
		"both the nested and outer pid==0 scopes must be recorded")
}

// Parallel workers running the SAME test name must each get their own window,
// tagged with their PID, even with overlapping begin/end.
func TestRecordScopesParallelWorkers(t *testing.T) {
	a := newRecordAgent()
	ctx := context.Background()
	require.NoError(t, a.BeginScope(ctx, "t", 100))
	require.NoError(t, a.BeginScope(ctx, "t", 200)) // overlaps worker 100's scope
	require.NoError(t, a.EndScope(ctx, "t", 100))
	require.NoError(t, a.EndScope(ctx, "t", 200))

	ws, _ := a.GetScopeWindows(ctx)
	require.Len(t, ws, 2)
	pids := map[uint32]bool{}
	for _, w := range ws {
		require.Equal(t, "t", w.Name)
		pids[w.PID] = true
	}
	require.True(t, pids[100] && pids[200], "each worker's window carries its own PID")
}

// A runner that reports its own clock gets a window stamped with it, not with the agent's read time.
func TestRecordScopesUseTheRunnerTimeWhenGiven(t *testing.T) {
	a := newRecordAgent()
	ctx := context.Background()
	start := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	end := start.Add(7 * time.Millisecond)
	require.NoError(t, a.BeginScopeAt(ctx, "t", 0, start))
	require.NoError(t, a.EndScopeAt(ctx, "t", 0, end))

	ws, _ := a.GetScopeWindows(ctx)
	require.Len(t, ws, 1)
	require.True(t, ws[0].Start.Equal(start), "start %v", ws[0].Start)
	require.True(t, ws[0].End.Equal(end), "end %v", ws[0].End)
}

// A fixture that posts no time still gets a window, stamped when the agent saw each call.
func TestRecordScopesFallBackToTheAgentClock(t *testing.T) {
	a := newRecordAgent()
	ctx := context.Background()
	before := time.Now()
	require.NoError(t, a.BeginScope(ctx, "t", 0))
	require.NoError(t, a.EndScope(ctx, "t", 0))

	ws, _ := a.GetScopeWindows(ctx)
	require.Len(t, ws, 1)
	require.False(t, ws[0].Start.Before(before))
	require.False(t, ws[0].End.Before(ws[0].Start))
	require.True(t, ws[0].End.Before(time.Now().Add(time.Second)))
}

type windowCall struct {
	names      []string
	start, end time.Time
}

type flowProxy struct {
	coreAgent.Proxy
	calls   []windowCall
	cleared int
	scoped  map[uint32][]string
	cutoff  time.Time
}

func (f *flowProxy) SetMocksWithWindow(_ context.Context, filtered, _ []*models.Mock, start, end time.Time) error {
	c := windowCall{start: start, end: end}
	for _, m := range filtered {
		c.names = append(c.names, m.Name)
	}
	f.calls = append(f.calls, c)
	return nil
}
func (f *flowProxy) SetWorkerScope(pid uint32, names []string) {
	if f.scoped == nil {
		f.scoped = map[uint32][]string{}
	}
	f.scoped[pid] = names
}
func (f *flowProxy) ClearWorkerScope(pid uint32)       { delete(f.scoped, pid) }
func (f *flowProxy) SetMappedUniverse([]string)        {}
func (f *flowProxy) ClearTestWindow()                  { f.cleared++ }
func (f *flowProxy) SeedStartupCutoff(start time.Time) { f.cutoff = start }

func TestReplayScopeServesOneFlowInItsOwnWindow(t *testing.T) {
	t0 := time.Date(2026, 9, 29, 22, 35, 0, 0, time.UTC)
	mock := func(name string, sec int) *models.Mock {
		return &models.Mock{Name: name, Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: t0.Add(time.Duration(sec) * time.Second), ResTimestampMock: t0.Add(time.Duration(sec) * time.Second)}}
	}
	px := &flowProxy{}
	a := &Agent{logger: zap.NewNop(), Proxy: px, config: &config.Config{Agent: config.Agent{Mode: models.MODE_TEST}}}
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, []*models.Mock{mock("boot", 0), mock("a-1", 10), mock("a-2", 11), mock("b-1", 20)}, nil))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"TestA": {"a-1", "a-2", "boot"}, "TestB": {"b-1", "boot"}}))
	a.SetScopeWindows(ctx, map[string]models.ScopeWindow{
		"TestA": {Start: t0.Add(10 * time.Second), End: t0.Add(11 * time.Second)},
		"TestB": {Start: t0.Add(20 * time.Second), End: t0.Add(20 * time.Second)},
	}, t0.Add(10*time.Second))

	require.NoError(t, a.BeginScope(ctx, "TestB", 4242))
	last := px.calls[len(px.calls)-1]
	require.ElementsMatch(t, []string{"b-1", "boot"}, last.names, "the pool is the flow's mocks and the shared ones, never another flow's")
	require.True(t, last.start.Equal(t0.Add(20*time.Second)) && last.end.Equal(t0.Add(20*time.Second)), "the flow's own window is opened, as keploy test does per test")
	require.True(t, px.cutoff.Equal(t0.Add(10*time.Second)), "boot mocks are the ones before the first recorded flow")
	require.Equal(t, []string{"b-1", "boot"}, px.scoped[4242])

	require.NoError(t, a.EndScope(ctx, "TestB", 4242))
	last = px.calls[len(px.calls)-1]
	require.True(t, last.start.Equal(models.BaseTime), "between flows the whole pool is staged again")
	require.Equal(t, 1, px.cleared, "and the window is closed, so a call between flows is not counted against the last one")
	require.Empty(t, px.scoped)
}

func TestReplayScopeLeavesTheGlobalPoolAloneWhileWorkersOverlap(t *testing.T) {
	px := &flowProxy{}
	a := &Agent{logger: zap.NewNop(), Proxy: px, config: &config.Config{Agent: config.Agent{Mode: models.MODE_TEST}}}
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, []*models.Mock{{Name: "a-1"}, {Name: "b-1"}}, nil))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"TestA": {"a-1"}, "TestB": {"b-1"}}))
	require.NoError(t, a.BeginScope(ctx, "TestA", 1))
	calls := len(px.calls)
	require.NoError(t, a.BeginScope(ctx, "TestB", 2))
	require.Len(t, px.calls, calls, "a second worker running at the same time only narrows its own view")
	require.Equal(t, []string{"b-1"}, px.scoped[2])
}
