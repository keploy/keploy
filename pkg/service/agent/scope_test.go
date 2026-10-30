package agent

import (
	"context"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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

// Under the proven switch, a recorded test's window is a test span for the
// recorders while it is open and once it ended (syncMock.TestScope): keploy
// mock record's tests open no window of the proxy's, and without it the
// Generic recorder is told no test owns a call made inside one, so it states
// a connection made wholly inside one test its preamble, served to every test
// (a recorded call left unmade, or another test's request, passes). A window
// withdrawn at its begin, or left open by a session reset, is no test's; a
// replay, and the switch off, open none.
func TestARecordedTestsWindowIsATestSpanForTheRecorders(t *testing.T) {
	start := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	end := start.Add(3 * time.Second)
	mid := start.Add(time.Second)
	ctx := context.Background()
	newAgent := func(t *testing.T) *Agent {
		a := newRecordAgent()
		a.spanMgr = syncMock.New(zap.NewNop())
		return a
	}
	t.Run("ended", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		a := newAgent(t)
		require.NoError(t, a.BeginScopeAt(ctx, "t", 0, start))
		span, ok := a.spanMgr.TestSpanAt(mid)
		require.True(t, ok, "a call made inside an open test is told no test")
		require.False(t, span.Final())
		require.True(t, span.Start.Equal(start))
		require.NoError(t, a.EndScopeAt(ctx, "t", 0, end))
		select {
		case <-span.Changed():
		default:
			t.Fatal("the recorder waiting on the test was not told it ended")
		}
		span, ok = a.spanMgr.TestSpanAt(mid)
		require.True(t, ok)
		require.True(t, span.Final())
		require.True(t, span.Start.Equal(start) && span.End.Equal(end), "span %v..%v, want the window's", span.Start, span.End)
		_, ok = a.spanMgr.TestSpanAt(end.Add(time.Nanosecond))
		require.False(t, ok, "a call after the test's end is the test's")
		require.Empty(t, a.openScopes)
	})
	t.Run("the window's begin is its span's, to the nanosecond", func(t *testing.T) {
		// The Generic recorder's lifetimeUndecided mark names the test by
		// its span's start (TestSpanAt), and keploy mock record writes each
		// test's begin (its mapping's at) from its window's start
		// (stampBegins): enterprise places the mark on the test whose begin
		// is that instant, so the two must be one instant, read back as
		// written.
		t.Setenv(provenswitch.Env, "1")
		a := newAgent(t)
		at := start.Add(123456789 * time.Nanosecond)
		require.NoError(t, a.BeginScopeAt(ctx, "t", 0, at))
		require.NoError(t, a.EndScopeAt(ctx, "t", 0, end))
		span, ok := a.spanMgr.TestSpanAt(mid)
		require.True(t, ok && span.Final())
		ws, _ := a.GetScopeWindows(ctx)
		require.Len(t, ws, 1)
		require.True(t, ws[0].Start.Equal(span.Start) && ws[0].Start.Equal(at), "window %v, span %v", ws[0].Start, span.Start)
		m := models.Mock{Spec: models.MockSpec{Metadata: map[string]string{
			models.MetadataLifetimeUndecidedKey: models.MockLifetimeUndecided{Why: models.LifetimeUndecidedReadAfterAnswer, TestStart: span.Start}.String()}}}
		u, ok := m.LifetimeUndecided()
		require.True(t, ok)
		require.True(t, u.TestStart.Equal(ws[0].Start), "the mark reads back %v, the window began %v", u.TestStart, ws[0].Start)
	})
	t.Run("withdrawn", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		a := newAgent(t)
		require.NoError(t, a.BeginScopeAt(ctx, "t", 0, start))
		a.withdrawWindow("t", 0)
		_, ok := a.spanMgr.TestSpanAt(mid)
		require.False(t, ok, "a withdrawn test owns a call")
		require.NoError(t, a.BeginScopeAt(ctx, "u", 0, start))
		a.abandonWindow("u")
		_, ok = a.spanMgr.TestSpanAt(mid)
		require.False(t, ok, "an abandoned test owns a call")
	})
	t.Run("reset", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		a := newAgent(t)
		require.NoError(t, a.BeginScopeAt(ctx, "t", 0, start))
		a.resetScopeState()
		_, ok := a.spanMgr.TestSpanAt(mid)
		require.False(t, ok, "a test the previous session left open owns a call")
		require.NoError(t, a.EndScopeAt(ctx, "t", 0, end))
		_, ok = a.spanMgr.TestSpanAt(mid)
		require.False(t, ok)
	})
	t.Run("begun again", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		a := newAgent(t)
		require.NoError(t, a.BeginScopeAt(ctx, "t", 0, start))
		require.NoError(t, a.BeginScopeAt(ctx, "t", 0, mid))
		require.NoError(t, a.EndScopeAt(ctx, "t", 0, end))
		_, ok := a.spanMgr.TestSpanAt(start)
		require.False(t, ok, "the replaced begin's scope is still open, or became a test")
		span, ok := a.spanMgr.TestSpanAt(mid)
		require.True(t, ok && span.Final() && span.Start.Equal(mid) && span.End.Equal(end))
	})
	t.Run("replay", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		cfg := &config.Config{}
		cfg.Agent.Mode = models.MODE_TEST
		a := &Agent{logger: zap.NewNop(), config: cfg, spanMgr: syncMock.New(zap.NewNop())}
		a.openWindow("t", 0, start)
		_, ok := a.spanMgr.TestSpanAt(mid)
		require.False(t, ok, "a replayed test opened a recording's test scope")
	})
	t.Run("switch off", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		a := newAgent(t)
		require.NoError(t, a.BeginScopeAt(ctx, "t", 0, start))
		_, ok := a.spanMgr.TestSpanAt(mid)
		require.False(t, ok)
		require.NoError(t, a.EndScopeAt(ctx, "t", 0, end))
		_, ok = a.spanMgr.TestSpanAt(mid)
		require.False(t, ok, "a test span noted with the switch off")
		require.Nil(t, a.openScopes)
		require.Equal(t, []string{"t"}, windowNames(a), "the window itself is recorded as before")
	})
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
