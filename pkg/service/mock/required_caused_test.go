package mock

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// causedMappings is a set whose worker scope holds cleanup/o::TestA/1
// (mock-3, mock-5), the trailing writes of o::TestA, each naming the
// recording whose answer carried the value it sends: mock-3's and mock-5's
// as a3 and a5 say ("" for none). mock-1, o::TestB's, is a sweep's list
// that named o::TestA's item; mock-9 is a recording the mappings list
// nowhere.
func causedMappings(a3, a5 string) string {
	cause := func(c string) string {
		if c == "" {
			return ""
		}
		return "      causedBy: " + c + "\n"
	}
	return "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n" +
		"    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n" +
		"    - id: o::TestB\n      dir: o\n      mocks:\n        - name: mock-1\n" +
		"worker:\n" +
		"    - name: mock-3\n      trails: o::TestA\n      rule: cleanup@3f2a91c0\n      series: cleanup/o::TestA/1\n" + cause(a3) +
		"    - name: mock-5\n      trails: o::TestA\n      rule: cleanup@3f2a91c0\n      series: cleanup/o::TestA/1\n" + cause(a5)
}

// A trailing series whose every recording names a cause the run did not
// serve is not due: o::TestB, whose window recorded the list that led the
// application to clean o::TestA's item up, failed before the list was
// served, so the clean-up never had its cause. The run fails on nothing,
// o::TestA passes, the receipt names the series under RequiredNotDue with
// the cause, its window and that window's outcome, and one RULE NOT DUE
// line says so.
func TestASeriesWhoseCauseWasNotServedIsNotDue(t *testing.T) {
	code, r, tr, logs := seriesRunOf(t, causedMappings("mock-1", "mock-1"), seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomeFailed),
		[]models.MockState{{Name: "mock-0"}}, nil)
	require.NotEqual(t, FailedByRuleNotMade, r.FailedBy)
	require.Nil(t, r.RequiredNotMade)
	require.Equal(t, []RequiredNotMade{{Series: "cleanup/o::TestA/1", Rule: "cleanup@3f2a91c0", Trails: "o::TestA", Names: []string{"mock-3", "mock-5"},
		Unserved: []UnservedCause{{Name: "mock-1", Window: "o::TestB", Outcome: models.ScopeOutcomeFailed}}}}, r.RequiredNotDue)
	require.Equal(t, models.ScopeOutcomePassed, testLine(t, tr, "o::TestA").Outcome)
	require.Empty(t, ruleNotMadeLines(logs))
	lines := ruleNotDueLines(logs)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "RULE NOT DUE cleanup@3f2a91c0: cleanup not due: the application made the call of o::TestA's trailing series 1 (mock-3, mock-5)")
	require.Contains(t, lines[0], "mock-1, recorded in o::TestB's window: o::TestB failed in this run")
	require.Contains(t, lines[0], `rule={"event":"not-due","id":"cleanup","series":"cleanup/o::TestA/1","trails":"o::TestA","unserved":["mock-1"]}`)
	_ = code
}

// Served, the cause makes the series due as one with none: the run
// answered the list and not the clean-up, so o::TestA fails and the run
// fails on the series not made, as K-1.
func TestASeriesWhoseCauseWasServedIsStillRequired(t *testing.T) {
	code, r, tr, logs := seriesRunOf(t, causedMappings("mock-1", "mock-1"), seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed),
		[]models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}, nil)
	require.Equal(t, 1, code)
	require.Equal(t, FailedByRuleNotMade, r.FailedBy)
	require.Equal(t, []RequiredNotMade{{Series: "cleanup/o::TestA/1", Rule: "cleanup@3f2a91c0", Trails: "o::TestA", Names: []string{"mock-3", "mock-5"}}}, r.RequiredNotMade)
	require.Nil(t, r.RequiredNotDue)
	require.Equal(t, models.ScopeOutcomeFailed, testLine(t, tr, "o::TestA").Outcome)
	require.Len(t, ruleNotMadeLines(logs), 1)
	require.Empty(t, ruleNotDueLines(logs))
}

// A series is let off only when every recording that makes it names a cause
// a replay can serve: one maker with none, a cause the mappings list
// nowhere, or a cause that names the recording itself keeps the series
// required, and the run fails on it not made.
func TestASeriesWithoutAServableCauseOnEveryMakerStaysRequired(t *testing.T) {
	for name, c := range map[string][2]string{
		"one maker names none":       {"mock-1", ""},
		"a cause listed nowhere":     {"mock-9", "mock-9"},
		"a cause that is the call":   {"mock-3", "mock-5"},
		"one cause listed, one not":  {"mock-1", "mock-9"},
		"one cause itself, one list": {"mock-3", "mock-1"},
	} {
		t.Run(name, func(t *testing.T) {
			code, r, _, logs := seriesRunOf(t, causedMappings(c[0], c[1]), seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomeFailed),
				[]models.MockState{{Name: "mock-0"}}, nil)
			require.Equal(t, 1, code)
			require.Equal(t, FailedByRuleNotMade, r.FailedBy)
			require.Len(t, r.RequiredNotMade, 1)
			require.Nil(t, r.RequiredNotDue)
			require.Empty(t, ruleNotDueLines(logs))
		})
	}
}

// What the run consumed unread, whether a cause was served is not known:
// the series is not let off as uncaused, and the run fails with its test
// unread, as a series with no cause does.
func TestAnUnreadRunLetsNoSeriesOffAsUncaused(t *testing.T) {
	code, r, _, logs := seriesRunOf(t, causedMappings("mock-1", "mock-1"), seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomeFailed),
		nil, errors.New("connection reset by peer"))
	require.Equal(t, 1, code)
	require.Nil(t, r.RequiredNotDue)
	require.Empty(t, ruleNotDueLines(logs))
	require.NotZero(t, logs.FilterMessageSnippet(trailsUnreadReason).Len(), "the series' test is left unread")
}

// requiredSeries keeps a cause only where the mappings list it where a
// replay can serve it, and reads its window: a test's list, start-up (no
// window), a worker: entry's place; a covered worker: entry (never served),
// one listed nowhere and the entry itself are dropped. owedSeries lets a
// series off only when every maker's kept cause went unserved, and only
// on a read of what the run used: one served cause, of either maker, owes
// it, and so does an unread run. The wait for the series the run owes
// reads it so too.
func TestCausesKeptOnlyWhereAReplayCanServeThem(t *testing.T) {
	mp := &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "o::TestB", Mocks: []models.MockEntry{{Name: "list-b"}}}},
		Startup:   []models.MockEntry{{Name: "list-boot"}},
		Worker: []models.MockEntry{
			{Name: "list-w", Place: "o::TestC"},
			{Name: "list-cov", Place: "o::TestC", CoveredBy: "list-w"},
			{Name: "u1", Trails: "o::TestA", Rule: "r@1", Series: "r/o::TestA/1", CausedBy: "list-b"},
			{Name: "u2", Trails: "o::TestA", Rule: "r@1", Series: "r/o::TestA/2", CausedBy: "list-boot"},
			{Name: "u3", Trails: "o::TestA", Rule: "r@1", Series: "r/o::TestA/3", CausedBy: "list-w"},
			{Name: "u4", Trails: "o::TestA", Rule: "r@1", Series: "r/o::TestA/4", CausedBy: "list-cov"},
			{Name: "u5", Trails: "o::TestA", Rule: "r@1", Series: "r/o::TestA/5", CausedBy: "gone"},
			{Name: "u6", Trails: "o::TestA", Rule: "r@1", Series: "r/o::TestA/6", CausedBy: "u6"},
			{Name: "u7", Trails: "o::TestA", Rule: "r@1", Series: "r/o::TestA/7", CausedBy: "list-b"},
			{Name: "u8", Trails: "o::TestA", Rule: "r@1", Series: "r/o::TestA/7", CausedBy: "list-w"},
		},
	}
	table := map[string][]string{"o::TestA": nil, "o::TestB": nil, "o::TestC": nil}
	req := requiredSeries(mp, table)
	require.Equal(t, map[string]string{"u1": "list-b"}, req["r/o::TestA/1"].causes)
	require.Equal(t, map[string]string{"list-b": "o::TestB"}, req["r/o::TestA/1"].causeIn)
	require.Equal(t, map[string]string{"list-boot": ""}, req["r/o::TestA/2"].causeIn)
	require.Equal(t, map[string]string{"list-w": "o::TestC"}, req["r/o::TestA/3"].causeIn)
	for _, id := range []string{"r/o::TestA/4", "r/o::TestA/5", "r/o::TestA/6"} {
		require.Empty(t, req[id].causes, id)
	}
	ran := map[string]bool{"o::TestA": true}
	none := map[string]bool{}
	require.Equal(t, []string{"r/o::TestA/4", "r/o::TestA/5", "r/o::TestA/6"}, owedSeries(req, ran, none, true))
	require.Len(t, owedSeries(req, ran, none, false), 7, "unread, no series is let off")
	require.Contains(t, owedSeries(req, ran, map[string]bool{"list-w": true}, true), "r/o::TestA/7", "a series of two makers is owed once either's cause was served")
	require.Contains(t, owedSeries(req, ran, map[string]bool{"list-b": true}, true), "r/o::TestA/7")
	require.NotContains(t, owedSeries(req, ran, map[string]bool{"list-x": true}, true), "r/o::TestA/7")
	got := req["r/o::TestA/7"].unserved(map[string]string{"o::TestB": models.ScopeOutcomeFailed})
	require.Equal(t, []UnservedCause{{Name: "list-b", Window: "o::TestB", Outcome: models.ScopeOutcomeFailed}, {Name: "list-w", Window: "o::TestC"}}, got)
}

// Each window's outcome says why its cause went unserved, and a cause no
// test's window recorded says so.
func TestUncausedWhySaysTheWindowsOutcome(t *testing.T) {
	for _, c := range []struct {
		u    UnservedCause
		want string
	}{
		{UnservedCause{Name: "m", Window: "T", Outcome: models.ScopeOutcomeFailed}, "m, recorded in T's window: T failed in this run, so it may have ended before the application asked for it"},
		{UnservedCause{Name: "m", Window: "T", Outcome: models.ScopeOutcomePassed}, "m, recorded in T's window: T passed in this run without the application asking for it"},
		{UnservedCause{Name: "m", Window: "T"}, "m, recorded in T's window: T did not run in this run"},
		{UnservedCause{Name: "m", Window: "T", Outcome: models.ScopeOutcomeSkipped}, "m, recorded in T's window: T was skipped in this run"},
		{UnservedCause{Name: "m"}, "m, recorded outside every test's window, which the run never asked for"},
	} {
		require.Equal(t, c.want, uncausedWhy(c.u))
	}
}
