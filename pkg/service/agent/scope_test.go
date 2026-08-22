package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
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
