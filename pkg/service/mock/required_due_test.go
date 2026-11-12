package mock

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// dueMappings is a set of o::TestA and o::TestB whose worker scope holds one
// once background series at the set's start, lease/-/1 (mock-8, a write),
// whose rule says the application's timer makes it at most 10 s after it
// starts (due: 10s).
const dueMappings = "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n" +
	"    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n" +
	"    - id: o::TestB\n      dir: o\n      mocks:\n        - name: mock-1\n" +
	"worker:\n" +
	"    - name: mock-8\n      rule: lease@5e6f7a8b\n      series: lease/-/1\n      once: true\n      due: 10s\n"

// commandRuns makes the test command's run, as keploy mock replay measures
// it (commandClock), take d.
func commandRuns(t *testing.T, d time.Duration) {
	t.Helper()
	at := time.Date(2026, 11, 12, 8, 44, 43, 0, time.UTC)
	reads := 0
	prev := commandClock
	commandClock = func() time.Time {
		reads++
		if reads == 1 {
			return at
		}
		return at.Add(d)
	}
	t.Cleanup(func() { commandClock = prev })
}

// ruleNotDueLines are the INFO lines that say RULE NOT DUE.
func ruleNotDueLines(logs *observer.ObservedLogs) []string {
	var out []string
	for _, e := range logs.FilterMessageSnippet("RULE NOT DUE").All() {
		if e.Level == zapcore.InfoLevel {
			out = append(out, e.Message)
		}
	}
	return out
}

// A replay whose test command ran 8 s (a run of a few tests) ends before
// the 10 s bound of the lease's timer: the lease's series, which no call
// made, is not due. The run passes, its receipt lists the series as not due
// with its bound, one RULE NOT DUE line says why, and none says RULE NOT
// MADE.
func TestAOnceSeriesWithADueBoundIsNotDueInAShorterRun(t *testing.T) {
	commandRuns(t, 8200*time.Millisecond)
	code, r, tr, logs := seriesRunOf(t, dueMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed),
		[]models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}, nil)
	require.Equal(t, 0, code)
	require.Empty(t, r.FailedBy)
	require.Nil(t, r.RequiredNotMade)
	require.Equal(t, []RequiredNotMade{{Series: "lease/-/1", Rule: "lease@5e6f7a8b", Names: []string{"mock-8"}, Due: "10s"}}, r.RequiredNotDue)
	require.Equal(t, models.ScopeOutcomePassed, testLine(t, tr, "o::TestA").Outcome)
	require.Empty(t, ruleNotMadeLines(logs))
	lines := ruleNotDueLines(logs)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "RULE NOT DUE lease@5e6f7a8b: lease not due: the app exited at most 8.2s after it started (the test command's whole run), before its 10s timer (due: 10s), and made no call of its background series 1 (mock-8)")
	require.Contains(t, lines[0], `rule={"due":"10s","event":"not-due","id":"lease","lived":"8.2s","series":"lease/-/1"}`)
	used := logs.FilterMessageSnippet("RULE USED lease@5e6f7a8b").All()
	require.Len(t, used, 1)
	require.Contains(t, used[0].Message, "required 1: made 0, not made 0, not due 0 (their test did not run), not yet due 1 (the app exited before their timer bound); served")
	require.Contains(t, used[0].Message, `"not_yet_due":1`)
}

// A replay whose test command ran 68 s (the whole suite) outlived the bound:
// the lease's series is due, and a run that never made it fails the set as
// any due background series does.
func TestAOnceSeriesWithADueBoundIsRequiredInARunPastIt(t *testing.T) {
	commandRuns(t, 68*time.Second)
	code, r, _, logs := seriesRunOf(t, dueMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed),
		[]models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}, nil)
	require.Equal(t, 1, code)
	require.Equal(t, FailedByRuleNotMade, r.FailedBy)
	require.Equal(t, []RequiredNotMade{{Series: "lease/-/1", Rule: "lease@5e6f7a8b", Names: []string{"mock-8"}}}, r.RequiredNotMade)
	require.Nil(t, r.RequiredNotDue)
	require.Empty(t, ruleNotDueLines(logs))
	lines := ruleNotMadeLines(logs)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "RULE NOT MADE lease@5e6f7a8b: the run reached its background series 1 (mock-8)")
}

// A run that ran exactly as long as the bound reached it: the series is due.
func TestAOnceSeriesIsDueInARunAsLongAsItsBound(t *testing.T) {
	commandRuns(t, 10*time.Second)
	code, r, _, _ := seriesRunOf(t, dueMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed),
		[]models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}, nil)
	require.Equal(t, 1, code)
	require.Len(t, r.RequiredNotMade, 1)
	require.Nil(t, r.RequiredNotDue)
}

// A shorter run that made the lease anyway made its series: nothing is
// said not due, and the RULE USED line counts it made.
func TestAOnceSeriesMadeInAShorterRunIsMade(t *testing.T) {
	commandRuns(t, 3*time.Second)
	code, r, _, logs := seriesRunOf(t, dueMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed),
		[]models.MockState{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-8"}}, nil)
	require.Equal(t, 0, code)
	require.Nil(t, r.RequiredNotMade)
	require.Nil(t, r.RequiredNotDue)
	require.Empty(t, ruleNotDueLines(logs))
	used := logs.FilterMessageSnippet("RULE USED lease@5e6f7a8b").All()
	require.Len(t, used, 1)
	require.Contains(t, used[0].Message, "required 1: made 1, not made 0, not due 0 (their test did not run); served")
}

// The bound is read from the entries (the longest of a series'), and one
// that does not parse as a positive duration bounds nothing; a run of
// unknown length (lived 0: a compose run) marks no series early, so every
// series stays due as with no bound.
func TestADueBoundIsReadFromItsEntries(t *testing.T) {
	for _, row := range []struct {
		dues []string
		want time.Duration
	}{
		{[]string{"10s"}, 10 * time.Second},
		{[]string{"10s", "12s"}, 12 * time.Second},
		{[]string{"12s", ""}, 12 * time.Second},
		{[]string{""}, 0},
		{[]string{"ten seconds"}, 0},
		{[]string{"-10s"}, 0},
		{[]string{"0s"}, 0},
	} {
		mp := &models.Mapping{}
		for _, d := range row.dues {
			mp.Worker = append(mp.Worker, models.MockEntry{Name: "mock-8", Rule: "lease@5e6f7a8b", Series: "lease/-/1", Once: true, Due: d})
		}
		req := requiredSeries(mp, nil)
		require.Equal(t, row.want, req["lease/-/1"].due, "dues %q", row.dues)
		for _, lived := range []time.Duration{0, -time.Second, time.Second, 11 * time.Second} {
			early := markEarly(req, lived)["lease/-/1"].early
			require.Equal(t, row.want > 0 && lived > 0 && lived < row.want, early, "dues %q lived %s", row.dues, lived)
		}
		require.False(t, req["lease/-/1"].early, "markEarly leaves the map it is given as it is")
	}
}

// A not-due series the run did not reach (a background series placed after
// a test that did not run), or whose making could not be read, is not said
// not due: the first is not due by its place, the second is the run's end's
// to name.
func TestNotYetDueNamesOnlyAReachedSeriesTheRunCouldRead(t *testing.T) {
	mp := &models.Mapping{Worker: []models.MockEntry{
		{Name: "mock-8", Rule: "lease@5e6f7a8b", Series: "lease/-/1", Once: true, Due: "10s"},
		{Name: "mock-9", Place: "o::TestB", Rule: "sweep@1a2b3c4d", Series: "sweep/-/1", Once: true, Due: "10s"},
	}}
	req := markEarly(requiredSeries(mp, map[string][]string{"o::TestA": nil, "o::TestB": nil}), 4*time.Second)
	ran := []TestReceipt{{Name: "o::TestA", Outcome: models.ScopeOutcomePassed}, {Name: "o::TestB", Outcome: models.ScopeOutcomeSkipped}}
	require.Equal(t, []RequiredNotMade{{Series: "lease/-/1", Rule: "lease@5e6f7a8b", Names: []string{"mock-8"}, Due: "10s"}}, notYetDue(req, ran, nil, true))
	require.Nil(t, notYetDue(req, ran, nil, false))
	notMade, unread, unreadSets := requiredNotMade(req, ran, nil, false)
	require.Nil(t, notMade)
	require.Nil(t, unread)
	require.Nil(t, unreadSets, "a series not due needs no read of whether it was made")
}
