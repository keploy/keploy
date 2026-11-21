package mock

import (
	"errors"
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
// it (commandClock), take d, and returns when it began.
func commandRuns(t *testing.T, d time.Duration) time.Time {
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
	return at
}

// servedAt is the recording name, consumed as the agent stamps it: when it
// first answered (models.MockState.Timestamp).
func servedAt(name string, at time.Time) models.MockState {
	return models.MockState{Name: name, Timestamp: at.UnixNano()}
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
// MADE. Its calls carry no time, so the app's life is read as the command's
// whole run, which no app it started outlived.
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
	require.Contains(t, lines[0], `rule={"due":"10s","event":"not-due","from":"command","id":"lease","lived":"8.2s","series":"lease/-/1"}`)
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
// that does not parse as a positive duration bounds nothing; a life keploy
// did not read (a compose run) marks no series early, so every series stays
// due as with no bound, and a run in which the app made no call marks every
// bounded series early.
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
		for _, life := range []appLife{{}, {timed: true, from: lifeNoCall}, {timed: true, lived: time.Second, from: lifeFirstCall}, {timed: true, lived: 11 * time.Second, from: lifeCommand}} {
			early := markEarly(req, life)["lease/-/1"].early
			require.Equal(t, life.timed && life.lived < row.want, early, "dues %q life %+v", row.dues, life)
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
	req := markEarly(requiredSeries(mp, map[string][]string{"o::TestA": nil, "o::TestB": nil}), appLife{timed: true, lived: 4 * time.Second, from: lifeFirstCall})
	ran := []TestReceipt{{Name: "o::TestA", Outcome: models.ScopeOutcomePassed}, {Name: "o::TestB", Outcome: models.ScopeOutcomeSkipped}}
	require.Equal(t, []RequiredNotMade{{Series: "lease/-/1", Rule: "lease@5e6f7a8b", Names: []string{"mock-8"}, Due: "10s"}}, notYetDue(req, ran, nil, true))
	require.Nil(t, notYetDue(req, ran, nil, false))
	notMade, unread, unreadSets := requiredNotMade(req, ran, nil, false)
	require.Nil(t, notMade)
	require.Nil(t, unread)
	require.Nil(t, unreadSets, "a series not due needs no read of whether it was made")
}

// The app's life is timed from its first call keploy saw, not from the test
// command's start: a command that ran 12 s (its build, then the app) whose
// app made its first call 3 s in lived 9 s, short of the lease's 10 s
// timer, so the lease it never made is not due, and its line says so.
func TestAnAppThatStartedLateIsTimedFromItsFirstCall(t *testing.T) {
	began := commandRuns(t, 12*time.Second)
	code, r, _, logs := seriesRunOf(t, dueMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed),
		[]models.MockState{servedAt("mock-1", began.Add(5*time.Second)), servedAt("mock-0", began.Add(3*time.Second))}, nil)
	require.Equal(t, 0, code)
	require.Nil(t, r.RequiredNotMade)
	require.Equal(t, []RequiredNotMade{{Series: "lease/-/1", Rule: "lease@5e6f7a8b", Names: []string{"mock-8"}, Due: "10s"}}, r.RequiredNotDue)
	require.Empty(t, ruleNotMadeLines(logs))
	lines := ruleNotDueLines(logs)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "RULE NOT DUE lease@5e6f7a8b: lease not due: the app exited 9s after its first call keploy saw in the run, before its 10s timer (due: 10s), and made no call of its background series 1 (mock-8)")
	require.Contains(t, lines[0], `rule={"due":"10s","event":"not-due","from":"first-call","id":"lease","lived":"9s","series":"lease/-/1"}`)
}

// An app that made its first call 1 s into a 12 s run lived 11 s, past the
// lease's 10 s timer: the lease it never made fails the set.
func TestAnAppThatRanPastItsBoundWithNoLeaseFails(t *testing.T) {
	began := commandRuns(t, 12*time.Second)
	code, r, _, logs := seriesRunOf(t, dueMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed),
		[]models.MockState{servedAt("mock-0", began.Add(time.Second)), servedAt("mock-1", began.Add(5*time.Second))}, nil)
	require.Equal(t, 1, code)
	require.Equal(t, FailedByRuleNotMade, r.FailedBy)
	require.Equal(t, []RequiredNotMade{{Series: "lease/-/1", Rule: "lease@5e6f7a8b", Names: []string{"mock-8"}}}, r.RequiredNotMade)
	require.Nil(t, r.RequiredNotDue)
	require.Empty(t, ruleNotDueLines(logs))
	require.Len(t, ruleNotMadeLines(logs), 1)
}

// A miss is a call the app made too: one 0.5 s into a 12 s run times the
// app from there (11.5 s), though its first served call came 3 s in, so the
// lease is due and not made. With the misses unread, when the app began is
// not known, and its life is the command's whole run: due as well.
func TestAMissTimesTheAppAndUnreadMissesTimeTheWholeRun(t *testing.T) {
	for _, row := range []struct {
		name   string
		misses func(began time.Time) []models.UnmatchedCall
		err    error
	}{
		{"an earlier miss", func(began time.Time) []models.UnmatchedCall {
			return []models.UnmatchedCall{{Protocol: "Mongo", ActualSummary: "find", At: began.Add(500 * time.Millisecond)}}
		}, nil},
		{"misses unread", func(time.Time) []models.UnmatchedCall { return nil }, errors.New("agent gone")},
	} {
		t.Run(row.name, func(t *testing.T) {
			began := commandRuns(t, 12*time.Second)
			_, r, _, logs := seriesRunWith(t, dueMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed),
				[]models.MockState{servedAt("mock-0", began.Add(3*time.Second)), servedAt("mock-1", began.Add(5*time.Second))}, nil,
				func(c *composeInstr) { c.mockErrors, c.mockErrorsErr = row.misses(began), row.err })
			require.Equal(t, []RequiredNotMade{{Series: "lease/-/1", Rule: "lease@5e6f7a8b", Names: []string{"mock-8"}}}, r.RequiredNotMade)
			require.Nil(t, r.RequiredNotDue)
			require.Empty(t, ruleNotDueLines(logs))
		})
	}
}

// A run in which the app made no call keploy saw never ran the app past
// its timer, however long the test command ran: the lease is not due, and
// the line says the app made no call.
func TestAnAppThatMadeNoCallIsNotDue(t *testing.T) {
	commandRuns(t, 30*time.Second)
	_, r, _, logs := seriesRunOf(t, dueMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed), []models.MockState{}, nil)
	require.Nil(t, r.RequiredNotMade)
	require.Equal(t, []RequiredNotMade{{Series: "lease/-/1", Rule: "lease@5e6f7a8b", Names: []string{"mock-8"}, Due: "10s"}}, r.RequiredNotDue)
	lines := ruleNotDueLines(logs)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "RULE NOT DUE lease@5e6f7a8b: lease not due: the app made no call keploy saw in the run, so it never ran to its 10s timer (due: 10s), and made no call of its background series 1 (mock-8)")
	require.Contains(t, lines[0], `"from":"no-call"`)
}

// With what the run served unread, whether the lease was made is not
// known: the series is not let off as not due (as no call would), and the
// run names it unread and fails.
func TestUnreadConsumesNeverLetALeaseOff(t *testing.T) {
	commandRuns(t, 30*time.Second)
	code, r, _, logs := seriesRunOf(t, dueMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed), nil, errors.New("agent gone"))
	require.Equal(t, 1, code)
	require.Nil(t, r.RequiredNotDue)
	require.Empty(t, ruleNotDueLines(logs))
	var named int
	for _, e := range logs.FilterMessageSnippet("cannot tell whether the run made the background series").All() {
		if f := e.ContextMap(); e.Level == zapcore.ErrorLevel && len(f["series"].([]interface{})) == 1 && f["series"].([]interface{})[0] == "lease/-/1" {
			named++
		}
	}
	require.Equal(t, 1, named, "the lease is named unread")
}

// lifeOf reads the app's life from the first call keploy served or missed
// in the run, to the exit of what the command started; from the command's
// start where that call's time is not known; as no call where it made none;
// and not at all where keploy did not time the run.
func TestLifeOfReadsTheAppFromItsFirstCall(t *testing.T) {
	began := time.Date(2026, 11, 12, 8, 44, 43, 0, time.UTC)
	run := commandRun{began: began, ended: began.Add(12 * time.Second)}
	at := func(d time.Duration) time.Time { return began.Add(d) }
	whole := appLife{timed: true, lived: 12 * time.Second, from: lifeCommand}
	for _, row := range []struct {
		name          string
		run           commandRun
		consumed      []models.MockState
		consumedKnown bool
		misses        []models.UnmatchedCall
		missesKnown   bool
		want          appLife
	}{
		{"not timed (compose)", commandRun{}, []models.MockState{servedAt("m", at(time.Second))}, true, nil, true, appLife{}},
		{"first served call", run, []models.MockState{servedAt("b", at(5*time.Second)), servedAt("a", at(3*time.Second))}, true, nil, true, appLife{timed: true, lived: 9 * time.Second, from: lifeFirstCall}},
		{"a miss before the first served call", run, []models.MockState{servedAt("a", at(3*time.Second))}, true, []models.UnmatchedCall{{At: at(2 * time.Second)}}, true, appLife{timed: true, lived: 10 * time.Second, from: lifeFirstCall}},
		{"a call at the end", run, []models.MockState{servedAt("a", at(12*time.Second))}, true, nil, true, appLife{timed: true, from: lifeFirstCall}},
		{"a call at the start", run, []models.MockState{servedAt("a", began)}, true, nil, true, appLife{timed: true, lived: 12 * time.Second, from: lifeFirstCall}},
		{"no call", run, nil, true, nil, true, appLife{timed: true, from: lifeNoCall}},
		{"consumes unread", run, nil, false, nil, true, whole},
		{"misses unread", run, []models.MockState{servedAt("a", at(3*time.Second))}, true, nil, false, whole},
		{"a consume with no time", run, []models.MockState{servedAt("a", at(3*time.Second)), {Name: "b"}}, true, nil, true, whole},
		{"a miss with no time", run, []models.MockState{servedAt("a", at(3*time.Second))}, true, []models.UnmatchedCall{{}}, true, whole},
		{"a call before the run", run, []models.MockState{servedAt("a", at(3*time.Second)), servedAt("b", at(-time.Second))}, true, nil, true, whole},
		{"a call after the run", run, []models.MockState{servedAt("a", at(3*time.Second))}, true, []models.UnmatchedCall{{At: at(13 * time.Second)}}, true, whole},
	} {
		require.Equal(t, row.want, lifeOf(row.run, row.consumed, row.consumedKnown, row.misses, row.missesKnown), row.name)
	}
}
