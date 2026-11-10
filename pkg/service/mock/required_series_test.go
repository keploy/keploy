package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// seriesMappings is a set whose worker scope holds two series declared rules
// placed: cleanup/o::TestA/1 (mock-3, mock-5), the trailing writes of
// o::TestA, and emptied/-/1 (mock-6, mock-7), a background series placed
// after o::TestB began; and mock-4, an ordinary background recording.
const seriesMappings = "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n" +
	"    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n" +
	"    - id: o::TestB\n      dir: o\n      mocks:\n        - name: mock-1\n" +
	"worker:\n" +
	"    - name: mock-3\n      trails: o::TestA\n      rule: cleanup@3f2a91c0\n      series: cleanup/o::TestA/1\n" +
	"    - name: mock-4\n" +
	"    - name: mock-5\n      trails: o::TestA\n      rule: cleanup@3f2a91c0\n      series: cleanup/o::TestA/1\n" +
	"    - name: mock-6\n      place: o::TestB\n      rule: emptied@0b1c2d3e\n      series: emptied/-/1\n" +
	"    - name: mock-7\n      place: o::TestB\n      rule: emptied@0b1c2d3e\n      series: emptied/-/1\n"

// seriesRun replays, under the switch, the set of seriesMappings with the
// agent reporting windows and consumed (consumedErr: the read of what it
// consumed failed). It returns the exit code, the receipt, the tests' receipt
// and the logs.
func seriesRun(t *testing.T, windows []models.ScopeWindow, consumed []models.MockState, consumedErr error) (int, *Receipt, *TestsReceipt, *observer.ObservedLogs) {
	t.Helper()
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte(seriesMappings), 0o644))
	core, logs := observer.New(zapcore.DebugLevel)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks, instr.consumedErr, instr.windows = consumed, consumedErr, windows
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	_ = New(zap.New(core), instr, named("mock-0", "mock-1", "mock-3", "mock-4", "mock-5", "mock-6", "mock-7"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	tests, err := ReadTestsReceipt(keployDir, "set")
	require.NoError(t, err)
	require.NotNil(t, tests)
	return utils.ErrCode, mustReceipt(t, keployDir), tests, logs
}

// seriesWindows is a run of o::TestA and o::TestB, ending as a and b say.
func seriesWindows(a, b string) []models.ScopeWindow {
	at := time.Now()
	return []models.ScopeWindow{
		{Name: "o::TestA", Start: at, End: at.Add(time.Second), Outcome: a},
		{Name: "o::TestB", Start: at.Add(2 * time.Second), End: at.Add(3 * time.Second), Outcome: b},
	}
}

// ruleNotMadeLines are the ERROR lines that say RULE NOT MADE.
func ruleNotMadeLines(logs *observer.ObservedLogs) []string {
	var out []string
	for _, e := range logs.FilterMessageSnippet("RULE NOT MADE").All() {
		if e.Level == zapcore.ErrorLevel {
			out = append(out, e.Message)
		}
	}
	return out
}

// K-1: a trailing series whose test ran and of which the run answered no
// recording is not made: the receipt names it, its test fails naming every
// recording of the series, the run fails, and one RULE NOT MADE line names
// the rule, the series and the test. M1's per-recording ERROR stays silent:
// the series is one requirement.
func TestRequiredSeriesNotMadeFailsRun(t *testing.T) {
	code, r, tr, logs := seriesRun(t, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed),
		[]models.MockState{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-6"}}, nil)
	require.Equal(t, 1, code)
	require.Equal(t, FailedByRuleNotMade, r.FailedBy)
	require.Equal(t, []RequiredNotMade{{Series: "cleanup/o::TestA/1", Rule: "cleanup@3f2a91c0", Trails: "o::TestA", Names: []string{"mock-3", "mock-5"}}}, r.RequiredNotMade)
	a := testLine(t, tr, "o::TestA")
	require.Equal(t, models.ScopeOutcomeFailed, a.Outcome)
	require.NotNil(t, a.Evidence)
	require.Equal(t, []string{"mock-3", "mock-5"}, a.Evidence.Unconsumed)
	require.Equal(t, models.ScopeOutcomePassed, testLine(t, tr, "o::TestB").Outcome)
	lines := ruleNotMadeLines(logs)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "RULE NOT MADE cleanup@3f2a91c0: o::TestA ran and its trailing series 1 (mock-3, mock-5) answered no call by the run's end")
	require.Contains(t, lines[0], `rule={"event":"not-made","id":"cleanup","series":"cleanup/o::TestA/1","trails":"o::TestA"}`)
	require.Zero(t, logs.FilterMessageSnippet("never made a call keploy recorded as a trailing write").Len(), "a series member is not required on its own")
}

// Guard 1 per series: one answered recording of a series makes it, whichever
// it is, so a faster run that made the sweep's call fewer times than record
// did passes. K-6's service half: a name the consumed set holds, an answer
// the agent gave between tests included, makes its series.
func TestRequiredSeriesIsMadeByAnyOneOfItsRecordings(t *testing.T) {
	for _, made := range [][]string{{"mock-3"}, {"mock-5"}, {"mock-3", "mock-5"}} {
		consumed := []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-7"}}
		for _, n := range made {
			consumed = append(consumed, models.MockState{Name: n})
		}
		code, r, tr, logs := seriesRun(t, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed), consumed, nil)
		require.Equal(t, 0, code, "made by %v", made)
		require.Empty(t, r.FailedBy)
		require.Nil(t, r.RequiredNotMade)
		require.Equal(t, models.ScopeOutcomePassed, testLine(t, tr, "o::TestA").Outcome)
		require.Empty(t, ruleNotMadeLines(logs))
	}
}

// K-2: a series trailing a test the run did not run (the gate skipped it) is
// not due, so the run passes; a background series whose place the run never
// reached is not due either.
func TestRequiredSeriesNotDueWhenTrailedTestNotRun(t *testing.T) {
	code, r, _, logs := seriesRun(t, seriesWindows(models.ScopeOutcomeSkipped, models.ScopeOutcomeSkipped), []models.MockState{{Name: "mock-0"}}, nil)
	require.Equal(t, 0, code)
	require.Empty(t, r.FailedBy)
	require.Nil(t, r.RequiredNotMade)
	require.Empty(t, ruleNotMadeLines(logs))
}

// A background series is due once the run reached its place: TestB ran and
// the run answered neither of its recordings, so the set fails (no test is
// failed for it), naming the series.
func TestRequiredBackgroundSeriesNotMadeFailsTheSet(t *testing.T) {
	code, r, tr, logs := seriesRun(t, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomeFailed),
		[]models.MockState{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-5"}}, nil)
	require.Equal(t, 1, code)
	require.Equal(t, FailedByRuleNotMade, r.FailedBy)
	require.Equal(t, []RequiredNotMade{{Series: "emptied/-/1", Rule: "emptied@0b1c2d3e", Names: []string{"mock-6", "mock-7"}}}, r.RequiredNotMade)
	require.Equal(t, models.ScopeOutcomePassed, testLine(t, tr, "o::TestA").Outcome)
	if b := testLine(t, tr, "o::TestB"); b.Evidence != nil {
		require.Empty(t, b.Evidence.Unconsumed, "a background series fails no test")
	}
	lines := ruleNotMadeLines(logs)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "RULE NOT MADE emptied@0b1c2d3e: the run reached its background series 1 (mock-6, mock-7), which answered no call by the run's end")
	require.Contains(t, lines[0], "the set fails and CI would be red")
	require.Contains(t, lines[0], `rule={"event":"not-made","id":"emptied","series":"emptied/-/1"}`)
}

// What the run consumed cannot be read: a due trailing series leaves its test
// unread, a due background series fails the run, and neither is said not
// made.
func TestRequiredSeriesUnreadIsNotSaidNotMade(t *testing.T) {
	code, r, tr, logs := seriesRun(t, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed), nil, errors.New("connection reset by peer"))
	require.Equal(t, 1, code)
	require.Nil(t, r.RequiredNotMade)
	a := testLine(t, tr, "o::TestA")
	require.NotNil(t, a.Evidence)
	require.True(t, a.Evidence.Unread)
	require.Contains(t, a.Evidence.Reason, "cannot tell whether the run made this test's trailing writes")
	require.Empty(t, ruleNotMadeLines(logs))
	var named int
	for _, e := range logs.FilterMessageSnippet("cannot tell whether the run made the background series").All() {
		if f := e.ContextMap(); e.Level == zapcore.ErrorLevel && len(f["series"].([]interface{})) == 1 && f["series"].([]interface{})[0] == "emptied/-/1" {
			named++
		}
	}
	require.Equal(t, 1, named)
}

// requiredSeries groups a set's worker entries by series in worker: order,
// leaves out an entry with no series (M1's requiredTrails keeps a plain
// trail) and a series trailing a test the set does not have, and keeps a
// background series' first place; requiredTrails leaves series members out.
func TestRequiredSeriesGroupsTheWorkerScope(t *testing.T) {
	mp := &models.Mapping{Worker: []models.MockEntry{
		{Name: "m1", Trails: "o::TestA", Rule: "r@1", Series: "r/o::TestA/2"},
		{Name: "m2", Trails: "o::TestA"},
		{Name: "m3", Trails: "o::TestA", Rule: "r@1", Series: "r/o::TestA/2"},
		{Name: "m4", Trails: "o::TestGone", Rule: "r@1", Series: "r/o::TestGone/1"},
		{Name: "m5", Place: "o::TestA", Rule: "b@2", Series: "b/-/1"},
		{Name: "m6", Place: "o::TestB", Rule: "b@2", Series: "b/-/1"},
		{Name: "m7"},
	}}
	table := map[string][]string{"o::TestA": {}, "o::TestB": {}}
	require.Equal(t, map[string]seriesDue{
		"r/o::TestA/2": {Names: []string{"m1", "m3"}, Rule: "r@1", Trails: "o::TestA"},
		"b/-/1":        {Names: []string{"m5", "m6"}, Rule: "b@2", Place: "o::TestA"},
	}, requiredSeries(mp, table))
	require.Equal(t, map[string]string{"m2": "o::TestA"}, requiredTrails(mp, table))
	require.Nil(t, requiredSeries(nil, table))
	require.Nil(t, requiredSeries(&models.Mapping{Worker: []models.MockEntry{{Name: "m7"}}}, table))
}

// A background series placed at the set's start is due in every run, and
// one placed after a test is due only once that test ran.
func TestRequiredBackgroundSeriesDueByPlace(t *testing.T) {
	req := map[string]seriesDue{
		"s/-/1": {Names: []string{"a"}, Rule: "s@1"},
		"p/-/1": {Names: []string{"b"}, Rule: "p@1", Place: "o::TestB"},
	}
	notMade, unread, sets := requiredNotMade(req, []TestReceipt{{Name: "o::TestB", Outcome: models.ScopeOutcomeSkipped}}, nil, true)
	require.Equal(t, []RequiredNotMade{{Series: "s/-/1", Rule: "s@1", Names: []string{"a"}}}, notMade)
	require.Nil(t, unread)
	require.Nil(t, sets)
	notMade, _, _ = requiredNotMade(req, []TestReceipt{{Name: "o::TestB", Outcome: models.ScopeOutcomePassed}}, []models.MockState{{Name: "a"}}, true)
	require.Equal(t, []RequiredNotMade{{Series: "p/-/1", Rule: "p@1", Names: []string{"b"}}}, notMade)
	notMade, unread, sets = requiredNotMade(nil, nil, nil, false)
	require.Nil(t, notMade)
	require.Nil(t, unread)
	require.Nil(t, sets)
}
