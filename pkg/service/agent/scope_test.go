package agent

import (
	"context"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
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

// The verdict a harness reports as it ends a scope lands on that scope's
// window, for the run that ended it only.
func TestScopeOutcomeLandsOnTheWindowItEnds(t *testing.T) {
	a := newRecordAgent()
	ctx := context.Background()
	require.NoError(t, a.BeginScope(ctx, "t", 7))
	a.NoteScopeOutcome("t", 7, "failed")
	require.NoError(t, a.EndScope(ctx, "t", 7))
	require.NoError(t, a.BeginScope(ctx, "t", 7))
	require.NoError(t, a.EndScope(ctx, "t", 7)) // a re-run that reported nothing

	ws, _ := a.GetScopeWindows(ctx)
	require.Len(t, ws, 2)
	require.Equal(t, "failed", ws[0].Outcome)
	require.Equal(t, "", ws[1].Outcome, "a verdict must not carry over to the next run of the test")
}

// An end with no open scope closes nothing; its verdict must not wait to stamp
// the next run of that test.
func TestScopeOutcomeWithoutAnOpenScopeIsDropped(t *testing.T) {
	a := newRecordAgent()
	ctx := context.Background()
	a.NoteScopeOutcome("t", 7, "passed")
	require.NoError(t, a.EndScope(ctx, "t", 7))
	require.NoError(t, a.BeginScope(ctx, "t", 7))
	require.NoError(t, a.EndScope(ctx, "t", 7))
	ws, _ := a.GetScopeWindows(ctx)
	require.Len(t, ws, 1)
	require.Equal(t, "", ws[0].Outcome)
}

func newReplayAgent() *Agent {
	cfg := &config.Config{}
	cfg.Agent.Mode = models.MODE_TEST
	return &Agent{logger: zap.NewNop(), config: cfg}
}

// The gate lets every test run until a replay names the only ones that should,
// answers the rest with its reason, runs a subtest with its named parent, and
// lifts when the replay clears it or a new session begins.
func TestScopeGate(t *testing.T) {
	a := newReplayAgent()
	ctx := context.Background()
	run, _ := a.ScopeRun("anything")
	require.True(t, run, "no gate: every test runs")

	require.NoError(t, a.SetScopeGate(ctx, []string{"TestProven"}, "not yet proven"))
	for _, name := range []string{"TestProven", "TestProven/case_1", "TestProven/case_1/deep"} {
		run, why := a.ScopeRun(name)
		require.True(t, run, name)
		require.Empty(t, why)
	}
	for _, name := range []string{"TestOther", "TestProvenX", "", "TestOther/TestProven"} {
		run, why := a.ScopeRun(name)
		require.False(t, run, name)
		require.Equal(t, "not yet proven", why)
	}

	require.NoError(t, a.SetScopeGate(ctx, []string{}, "nothing proven"))
	run, _ = a.ScopeRun("TestProven")
	require.False(t, run, "an empty, non-nil list runs nothing")

	require.NoError(t, a.SetScopeGate(ctx, nil, ""))
	run, _ = a.ScopeRun("TestOther")
	require.True(t, run, "a nil list lifts the gate")

	require.NoError(t, a.SetScopeGate(ctx, []string{"TestProven"}, "x"))
	a.resetScopeState()
	run, _ = a.ScopeRun("TestOther")
	require.True(t, run, "a new session must not inherit the previous replay's gate")
}

// Only a replay is gated: a record session on an agent that still holds a
// gate records every test.
func TestScopeGateDoesNotApplyToRecord(t *testing.T) {
	a := newRecordAgent()
	require.NoError(t, a.SetScopeGate(context.Background(), []string{"TestProven"}, "x"))
	run, _ := a.ScopeRun("TestOther")
	require.True(t, run)
}

// A gated test leaves a record of having been gated, and nothing else.
func TestNoteScopeGatedRecordsAWindow(t *testing.T) {
	a := newReplayAgent()
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	a.NoteScopeGated("TestOther", 9, at)
	ws, _ := a.GetScopeWindows(context.Background())
	require.Len(t, ws, 1)
	require.Equal(t, models.ScopeWindow{Name: "TestOther", Start: at, End: at, PID: 9, Outcome: models.ScopeOutcomeGated}, ws[0])
}

// The agent says once per session that a gate could not be applied because the
// harness cannot skip, not once per test.
func TestNoteUngatableWarnsOncePerSession(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	a := newReplayAgent()
	a.logger = zap.New(core)
	a.NoteUngatable("t1")
	a.NoteUngatable("t2")
	require.Equal(t, 1, logs.Len())
	a.resetScopeState()
	a.NoteUngatable("t3")
	require.Equal(t, 2, logs.Len(), "a new session warns again")
}

// A harness that said it can skip, was told to, and ran the test anyway is
// called out once per session.
func TestEndOfAGatedTestWarnsOnce(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	a := newReplayAgent()
	a.logger = zap.New(core)
	ctx := context.Background()
	a.NoteScopeGated("t1", 3, time.Time{})
	a.NoteScopeGated("t2", 3, time.Time{})
	require.NoError(t, a.EndScope(ctx, "t1", 3))
	require.NoError(t, a.EndScope(ctx, "t2", 3))
	require.Equal(t, 1, logs.FilterMessageSnippet("told to skip").Len())

	require.NoError(t, a.BeginScope(ctx, "t3", 3))
	require.NoError(t, a.EndScope(ctx, "t3", 3))
	require.Equal(t, 1, logs.FilterMessageSnippet("told to skip").Len(), "an ordinary test's end is not a gated one")
}
