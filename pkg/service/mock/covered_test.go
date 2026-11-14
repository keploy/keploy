package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// coverableSeries is a worker: entry of the every series "touch/-/0", round
// one's recording of a call made at most once per 5 minutes.
func coverableSeries(name string) models.MockEntry {
	return models.MockEntry{Name: name, Rule: "touch@0a1b2c3d", Series: "touch/-/0", Once: true, Every: "5m"}
}

// An entry covered by another entry of its series, under its key, that
// answers once in a round and again after a positive interval and is not
// covered itself is covered. Any other cover is refused, naming the entry,
// its cover and why: leaving the entry out would lose calls its cover
// cannot answer.
func TestCoveredRecordings(t *testing.T) {
	covered := func(name, by string) models.MockEntry {
		e := coverableSeries(name)
		e.CoveredBy = by
		return e
	}
	with := func(e models.MockEntry, edit func(*models.MockEntry)) models.MockEntry {
		edit(&e)
		return e
	}
	got, err := coveredRecordings(&models.Mapping{Worker: []models.MockEntry{coverableSeries("mock-1"), covered("mock-2", "mock-1"), {Name: "mock-3"}, covered("mock-4", "mock-1")}})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"mock-2": true, "mock-4": true}, got, "each later round's recording is covered by round one's")

	got, err = coveredRecordings(&models.Mapping{Worker: []models.MockEntry{coverableSeries("mock-1"), {Name: "mock-1"}, covered("mock-2", "mock-1")}})
	require.NoError(t, err, "a name worker: lists twice is its first entry's, as the pool slices read it")
	require.Equal(t, map[string]bool{"mock-2": true}, got)

	got, err = coveredRecordings(&models.Mapping{Worker: []models.MockEntry{coverableSeries("mock-1"), {Name: "mock-3"}}})
	require.NoError(t, err)
	require.Nil(t, got, "no entry is covered")
	got, err = coveredRecordings(nil)
	require.NoError(t, err)
	require.Nil(t, got)

	for _, c := range []struct {
		name   string
		worker []models.MockEntry
		says   string
	}{
		{"absent", []models.MockEntry{coverableSeries("mock-1"), covered("mock-2", "mock-9")}, "worker: entry mock-2 is covered by mock-9, which is no other entry of worker:"},
		{"itself", []models.MockEntry{covered("mock-2", "mock-2")}, "worker: entry mock-2 is covered by mock-2, which is no other entry of worker:"},
		{"each other", []models.MockEntry{covered("mock-1", "mock-2"), covered("mock-2", "mock-1")}, "worker: entry mock-1 is covered by mock-2, which is covered itself, by mock-1"},
		{"chain", []models.MockEntry{coverableSeries("mock-3"), covered("mock-2", "mock-1"), covered("mock-1", "mock-3")}, "worker: entry mock-2 is covered by mock-1, which is covered itself, by mock-3"},
		{"other series", []models.MockEntry{with(coverableSeries("mock-1"), func(e *models.MockEntry) { e.Series = "touch/-/1" }), covered("mock-2", "mock-1")}, `which is of series "touch/-/1", not of the entry's series "touch/-/0"`},
		{"no series", []models.MockEntry{with(coverableSeries("mock-1"), func(e *models.MockEntry) { e.Series = "" }), with(covered("mock-2", "mock-1"), func(e *models.MockEntry) { e.Series = "" })}, `which is of series "", not of the entry's series ""`},
		{"other key", []models.MockEntry{with(coverableSeries("mock-1"), func(e *models.MockEntry) { e.Key = "users" }), covered("mock-2", "mock-1")}, `which is recorded under key "users", not under the entry's key ""`},
		{"not once", []models.MockEntry{with(coverableSeries("mock-1"), func(e *models.MockEntry) { e.Once = false }), covered("mock-2", "mock-1")}, "which does not answer once in a round and again after a positive interval (once and every)"},
		{"no every", []models.MockEntry{with(coverableSeries("mock-1"), func(e *models.MockEntry) { e.Every = "" }), covered("mock-2", "mock-1")}, "which does not answer once in a round and again after a positive interval"},
		{"zero every", []models.MockEntry{with(coverableSeries("mock-1"), func(e *models.MockEntry) { e.Every = "0s" }), covered("mock-2", "mock-1")}, "which does not answer once in a round and again after a positive interval"},
		{"negative every", []models.MockEntry{with(coverableSeries("mock-1"), func(e *models.MockEntry) { e.Every = "-5m" }), covered("mock-2", "mock-1")}, "which does not answer once in a round and again after a positive interval"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := coveredRecordings(&models.Mapping{Worker: c.worker})
			require.Nil(t, got)
			require.ErrorContains(t, err, c.says)
		})
	}
}

// readFailsDB is a set's mappings whose read fails, with what the read
// returned beside the error.
type readFailsDB struct {
	MappingDB
	mapping *models.Mapping
}

func (d readFailsDB) GetMapping(context.Context, string) (*models.Mapping, error) {
	return d.mapping, errors.New("the read failed")
}

// leaveCoveredOut keeps what the agent is handed as it is when the set
// covers nothing, says nothing then, and keeps a nil entry; a mappings read
// that fails is not read here (the table push refuses it), whatever the
// read returned beside its error; a store that reads no mapping covers
// nothing.
func TestLeaveCoveredOut(t *testing.T) {
	mocks := func(names ...string) []*models.Mock {
		var out []*models.Mock
		for _, n := range names {
			out = append(out, &models.Mock{Name: n})
		}
		return out
	}
	namesOf := func(ms []*models.Mock) []string {
		var out []string
		for _, mk := range ms {
			if mk == nil {
				out = append(out, "<nil>")
				continue
			}
			out = append(out, mk.Name)
		}
		return out
	}
	cover, later := coverableSeries("mock-1"), coverableSeries("mock-2")
	later.CoveredBy = "mock-1"
	bad := later
	bad.CoveredBy = "mock-9"
	for _, c := range []struct {
		name       string
		db         MappingDB
		filtered   []string
		unfiltered []string
		said       int
	}{
		{"covers nothing", scopeTableDB{mapping: &models.Mapping{Worker: []models.MockEntry{cover, {Name: "mock-2"}}}}, []string{"mock-1", "<nil>"}, []string{"mock-2"}, 0},
		{"read fails", readFailsDB{mapping: &models.Mapping{Worker: []models.MockEntry{cover, bad}}}, []string{"mock-1", "<nil>"}, []string{"mock-2"}, 0},
		{"no mapping reader", nil, []string{"mock-1", "<nil>"}, []string{"mock-2"}, 0},
		{"covers mock-2", scopeTableDB{mapping: &models.Mapping{Worker: []models.MockEntry{cover, later}}}, []string{"mock-1", "<nil>"}, []string{}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			m := &mockService{logger: zap.New(core), mappingDB: c.db}
			var f, u []*models.Mock
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("leaveCoveredOut panicked on a nil mock: %v", r)
					}
				}()
				f, u, err = m.leaveCoveredOut(context.Background(), "set", append(mocks("mock-1"), nil), mocks("mock-2"))
			}()
			require.NoError(t, err)
			require.Equal(t, c.filtered, namesOf(f))
			require.Equal(t, c.unfiltered, append([]string{}, namesOf(u)...))
			require.Equal(t, c.said, logs.FilterMessageSnippet("each covered recording's calls").Len())
		})
	}
}

// A series' covered entry is in none of the series the run requires or
// counts: the agent is never handed it, and the entry it names stands for
// it. A series whose only write is covered is served, not required.
func TestPlacedSeriesLeavesACoveredEntryOut(t *testing.T) {
	cover, later := coverableSeries("mock-1"), coverableSeries("mock-2")
	later.CoveredBy = "mock-1"
	mp := &models.Mapping{Worker: []models.MockEntry{cover, later}}
	placed := placedSeries(mp, nil)
	require.Equal(t, []string{"mock-1"}, placed["touch/-/0"].Names)
	require.True(t, placed["touch/-/0"].write)
	require.Equal(t, []string{"mock-1"}, requiredSeries(mp, nil)["touch/-/0"].Names)

	cover.Read = true
	mp = &models.Mapping{Worker: []models.MockEntry{cover, later}}
	require.Equal(t, []string{"mock-1"}, placedSeries(mp, nil)["touch/-/0"].Names)
	require.Nil(t, requiredSeries(mp, nil), "the series' write is a covered entry's, which is never served: its read cover makes it served, not required")
}

// coveredSet writes a set whose worker: holds round one's recording of an
// every series (mock-1), a later round's that it covers (mock-2) and a call
// in no series (mock-3), with coveredBy as mock-2's cover, and returns its
// keploy folder.
func coveredSet(t *testing.T, coveredBy string) string {
	t.Helper()
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	series := "      rule: touch@0a1b2c3d\n      series: touch/-/0\n      once: true\n      every: 5m\n"
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\nworker:\n    - name: mock-1\n"+series+"    - name: mock-2\n"+series+"      coveredBy: "+coveredBy+"\n    - name: mock-3\n"), 0o644))
	return keployDir
}

// replayCovered replays coveredSet's set on instr, its agent having served
// mock-0 and mock-1, and returns what it logged at Info and above and
// Replay's error.
func replayCovered(t *testing.T, instr *composeInstr, keployDir string) (*observer.ObservedLogs, error) {
	t.Helper()
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	core, logs := observer.New(zap.InfoLevel)
	return logs, New(zap.New(core), instr, named("mock-0", "mock-1", "mock-2", "mock-3"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
}

// storedNames is the names of the mocks the replay handed instr's agent.
func storedNames(instr *composeInstr) []string {
	instr.mu.Lock()
	defer instr.mu.Unlock()
	var out []string
	for _, mk := range instr.stored {
		out = append(out, mk.Name)
	}
	slices.Sort(out)
	return out
}

// Under the switch the replay never hands the agent a recording the set's
// mappings mark covered: it is not stored on the agent, nor in any pool
// slice, worker scope or series, so the agent cannot serve it, and the
// recording it names answers its calls in each round of the series. With
// the switch off the mark is not read: the agent is handed the recording
// as before. The replay names the recordings it leaves out.
func TestAReplayNeverHandsTheAgentACoveredRecording(t *testing.T) {
	const said = "each covered recording's calls are answered by the recording of its series that covers it"
	t.Run("switch on", func(t *testing.T) {
		provenOn(t)
		instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
		logs, err := replayCovered(t, instr, coveredSet(t, "mock-1"))
		require.NoError(t, err)
		lines := logs.FilterMessageSnippet(said).All()
		require.Len(t, lines, 1)
		require.Equal(t, []any{"mock-2"}, lines[0].ContextMap()["covered"])
		require.Equal(t, []string{"mock-0", "mock-1", "mock-3"}, storedNames(instr), "mock-2 is never stored on the agent")
		require.Equal(t, map[string][]string{models.PoolKeyNone: {"mock-1", "mock-3"}}, instr.pool)
		require.Equal(t, map[string][]string{models.PoolKeyNone: {"mock-1", "mock-3"}}, instr.worker)
		require.Equal(t, map[string][]string{"touch/-/0": {"mock-1"}}, instr.series)
		require.Equal(t, []string{"touch/-/0"}, instr.once)
		require.Equal(t, map[string]string{"touch/-/0": "5m0s"}, instr.every, "round one's recording answers each round")
	})
	t.Run("switch off", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
		logs, err := replayCovered(t, instr, coveredSet(t, "mock-1"))
		require.NoError(t, err)
		require.Zero(t, logs.FilterMessageSnippet(said).Len())
		require.Equal(t, []string{"mock-0", "mock-1", "mock-2", "mock-3"}, storedNames(instr), "without the switch's rounds the later round's recording is served as before")
	})
}

// Under the switch a set whose mappings cover a recording by one that
// cannot answer its calls is refused before the agent is handed anything,
// naming the entry, its cover and the fix: leaving the recording out would
// lose its calls.
func TestAReplayRefusesACoverThatCannotAnswer(t *testing.T) {
	provenOn(t)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	_, err := replayCovered(t, instr, coveredSet(t, "mock-3"))
	require.ErrorContains(t, err, `keploy cannot judge each test of mock set "set": its mappings cannot be read: worker: entry mock-2 is covered by mock-3, which is of series "", not of the entry's series "touch/-/0"`)
	require.ErrorContains(t, err, "correct that edit")
	require.NotContains(t, instr.seq(), "store", "the agent is handed nothing")
}
