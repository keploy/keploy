package mock

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// ruleLines are the INFO lines whose message starts with prefix ("RULE USED
// " or "RULES: "), in log order.
func ruleLines(logs *observer.ObservedLogs, prefix string) []string {
	var out []string
	for _, e := range logs.All() {
		if e.Level == zapcore.InfoLevel && len(e.Message) >= len(prefix) && e.Message[:len(prefix)] == prefix {
			out = append(out, e.Message)
		}
	}
	return out
}

// counted is an agent that counts series' repeats: by series.
func counted(bySeries map[string]int64) func(*composeInstr) {
	return func(f *composeInstr) {
		f.mockStats = models.MockStats{SeriesCounted: true, SeriesRepeats: bySeries}
	}
}

// K-RU1: a run that made both series prints one RULE USED line per rule, in
// rule order, and the RULES line for both: each series' recordings it used
// plus the repeats the agent counted are the calls it answered, and each
// required series is made.
func TestRuleUsedCountsWhatEachRuleAnswered(t *testing.T) {
	used := []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-3"}, {Name: "mock-6"}}
	code, _, _, logs := seriesRunWith(t, seriesMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed), used, nil,
		counted(map[string]int64{"cleanup/o::TestA/1": 2}))
	require.Equal(t, 0, code)
	require.Equal(t, []string{
		`RULE USED cleanup@3f2a91c0: 1 series; this run answered 3 calls (2 repeats in tests past the recorded count, by the last of their series); 0 misses carried the value of a test it trails; required 1: made 1, not made 0, not due 0 (their test did not run); served, not required 0 (reads, or writes that changed nothing when recorded). rule={"answered":3,"event":"used","id":"cleanup","made":1,"misses":0,"not_due":0,"not_made":0,"repeats":2,"required":1,"series":1,"served_only":0}`,
		`RULE USED emptied@0b1c2d3e: 1 series; this run answered 1 call (0 repeats in tests past the recorded count, by the last of their series); 0 misses carried the value of a test it trails; required 1: made 1, not made 0, not due 0 (their test did not run); served, not required 0 (reads, or writes that changed nothing when recorded). rule={"answered":1,"event":"used","id":"emptied","made":1,"misses":0,"not_due":0,"not_made":0,"repeats":0,"required":1,"series":1,"served_only":0}`,
	}, ruleLines(logs, "RULE USED "))
	require.Equal(t, []string{
		`RULES: 2 declared rules placed 2 series on this set; this run answered 4 calls under them (2 repeats past the recorded count); 0 misses carried the value of a test they trail; required series not made: 0. rule={"answered":4,"event":"rules","misses":0,"not_made":0,"repeats":2,"rules":2,"series":2}`,
	}, ruleLines(logs, "RULES: "))
}

// K-RU2: a series whose test ran and that the run never made counts as not
// made, and one whose test did not run (o::TestB skipped: the background
// series placed after its begin) as not due; the RULES line counts the one
// not made, as the run's RULE NOT MADE line names it.
func TestRuleUsedCountsNotMadeAndNotDue(t *testing.T) {
	none := []models.MockState{{Name: "mock-0"}}
	code, _, _, logs := seriesRunWith(t, seriesMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomeSkipped), none, nil, counted(nil))
	require.Equal(t, 1, code)
	lines := ruleLines(logs, "RULE USED ")
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], "required 1: made 0, not made 1, not due 0")
	require.Contains(t, lines[0], "this run answered 0 calls (0 repeats in tests past the recorded count")
	require.Contains(t, lines[1], "required 1: made 0, not made 0, not due 1 (their test did not run)")
	require.Len(t, ruleNotMadeLines(logs), 1)
	rules := ruleLines(logs, "RULES: ")
	require.Len(t, rules, 1)
	require.Contains(t, rules[0], "required series not made: 1.")
}

// K-RU3: an agent that does not count repeats, an agent client that cannot
// ask, and a read that fails each leave the repeats uncounted, never 0: the
// line says why, and its JSON says -1. What the run made is still read.
func TestRuleUsedSaysRepeatsNotCounted(t *testing.T) {
	used := []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-3"}, {Name: "mock-6"}}
	for _, c := range []struct {
		name, why string
		with      func(*composeInstr)
	}{
		{"agent does not count", "this keploy agent does not count them", nil},
		{"read fails", "the read failed: agent gone", func(f *composeInstr) { f.mockStatsErr = errors.New("agent gone") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, logs := seriesRunWith(t, seriesMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed), used, nil, c.with)
			lines := ruleLines(logs, "RULE USED ")
			require.Len(t, lines, 2)
			require.Contains(t, lines[0], "this run answered 1 call (repeats past the recorded count not counted: "+c.why+")")
			require.Contains(t, lines[0], `"repeats":-1`)
			require.Contains(t, lines[0], "required 1: made 1, not made 0")
			rules := ruleLines(logs, "RULES: ")
			require.Len(t, rules, 1)
			require.Contains(t, rules[0], "this run answered 2 calls under them (repeats past the recorded count not counted);")
			require.Contains(t, rules[0], `"repeats":-1`)
		})
	}
	require.Equal(t, seriesRepeats{unread: "this keploy's agent client cannot ask for them"}, readSeriesRepeats(context.Background(), nil))
}

// K-RU4: when what the run consumed could not be read, a RULE USED line says
// only how many series the rule placed, and that it says nothing of what the
// run answered or made: never "made 0".
func TestRuleUsedUnreadSaysNothingOfWhatTheRunMade(t *testing.T) {
	_, _, _, logs := seriesRunWith(t, seriesMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed), nil, errors.New("agent gone"), counted(nil))
	lines := ruleLines(logs, "RULE USED ")
	require.Len(t, lines, 2)
	for _, l := range lines {
		require.Contains(t, l, "1 series; what this run's agent served could not be read, so this line says nothing of what the run answered or made.")
		require.Contains(t, l, `"unread":true`)
		require.NotContains(t, l, "made 0")
	}
	rules := ruleLines(logs, "RULES: ")
	require.Len(t, rules, 1)
	require.Contains(t, rules[0], "what this run's agent served could not be read.")
}

// K-RU5: a series of reads is counted served, not required, never required;
// a miss that carried the value of a test a rule's series trails counts on
// that rule's line alone; misses not read are said so, never 0.
func TestRuleUsedCountsServedOnlyAndTrailingMisses(t *testing.T) {
	used := []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}
	_, _, _, logs := seriesRunWith(t, readSeriesMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed), used, nil, func(f *composeInstr) {
		counted(nil)(f)
		f.mockErrors = []models.UnmatchedCall{{TrailsOf: "o::TestA"}, {TrailsOf: "o::TestB"}, {}}
	})
	lines := ruleLines(logs, "RULE USED ")
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], "; 1 miss carried the value of a test it trails; required 0: made 0, not made 0, not due 0 (their test did not run); served, not required 1 ")
	require.Contains(t, lines[1], "; 0 misses carried the value of a test it trails; required 0: made 0, not made 0, not due 0 (their test did not run); served, not required 1 ")

	_, _, _, logs = seriesRunWith(t, seriesMappings, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed), used, nil, func(f *composeInstr) {
		counted(nil)(f)
		f.mockErrorsErr = errors.New("agent gone")
	})
	lines = ruleLines(logs, "RULE USED ")
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], "; misses not read; ")
	require.Contains(t, lines[0], `"misses":-1`)
	rules := ruleLines(logs, "RULES: ")
	require.Len(t, rules, 1)
	require.Contains(t, rules[0], "(0 repeats past the recorded count); misses not read; required series not made: 2.")
	require.Contains(t, rules[0], `"misses":-1`)
}

// K-RU6: the series keploy record moved on its own (no rule) get their own
// line, named as RULE NOT MADE names them, and the RULES line says they are
// among the series; a set with no series prints no RULE USED or RULES line.
func TestRuleUsedNamesRecordMovesAndIsSilentWithoutSeries(t *testing.T) {
	moves := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n" +
		"    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n" +
		"    - id: o::TestB\n      dir: o\n      mocks:\n        - name: mock-1\n" +
		"worker:\n" +
		"    - name: mock-3\n      trails: o::TestA\n      series: o::TestA/k1\n"
	used := []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-3"}}
	_, _, _, logs := seriesRunWith(t, moves, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed), used, nil, counted(nil))
	lines := ruleLines(logs, "RULE USED ")
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "RULE USED (keploy record's trail move): 1 series; this run answered 1 call")
	require.Contains(t, lines[0], `"by":"record"`)
	require.NotContains(t, lines[0], `"id"`)
	rules := ruleLines(logs, "RULES: ")
	require.Len(t, rules, 1)
	require.Contains(t, rules[0], "RULES: 0 declared rules placed 1 series on this set, keploy record's own trail moves among them;")

	plain := "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n" +
		"    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n" +
		"worker:\n    - name: mock-4\n"
	var agent *composeInstr
	_, _, _, logs = seriesRunWith(t, plain, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed), used, nil, func(f *composeInstr) {
		counted(nil)(f)
		agent = f
	})
	require.Empty(t, ruleLines(logs, "RULE USED "))
	require.Empty(t, ruleLines(logs, "RULES: "))
	agent.mu.Lock()
	reads := agent.mockStatsReads
	agent.mu.Unlock()
	require.Zero(t, reads, "a set with no series never asks the agent for their repeats")
}

// K-RU7: under docker compose the agent's account carries no repeat count:
// the lines say so (the compose read sets it before the outcome is read).
func TestRuleUsedUnderComposeSaysRepeatsNotCounted(t *testing.T) {
	r := ruleUses(map[string]seriesDue{"x/o::TestA/1": {Names: []string{"mock-3"}, Rule: "x@00000000", Trails: "o::TestA", write: true}},
		[]TestReceipt{{Name: "o::TestA", Outcome: models.ScopeOutcomePassed}}, []models.MockState{{Name: "mock-3"}}, true,
		seriesRepeats{unread: "under docker compose"}, nil, true)
	require.Equal(t, []ruleUse{{Rule: "x@00000000", Series: 1, Known: true, Answered: 1, Repeats: -1, RepeatsUnread: "under docker compose", Required: 1, Made: 1}}, r)

	// Unread consumption counts nothing but the series; no series is nil.
	r = ruleUses(map[string]seriesDue{"x/o::TestA/1": {Names: []string{"mock-3"}, Rule: "x@00000000", Trails: "o::TestA", write: true}},
		[]TestReceipt{{Name: "o::TestA", Outcome: models.ScopeOutcomePassed}}, nil, false, seriesRepeats{bySeries: map[string]int64{"x/o::TestA/1": 2}}, nil, true)
	require.Equal(t, []ruleUse{{Rule: "x@00000000", Series: 1}}, r)
	require.Nil(t, ruleUses(nil, nil, nil, true, seriesRepeats{}, nil, true))
}
