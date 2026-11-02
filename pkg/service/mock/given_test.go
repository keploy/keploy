package mock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Each test's recording keeps the values its latest window gave, in order: a
// test run twice keeps its last attempt's, none included, and a test that
// gave none gets no entry from them.
func TestStampGivenKeepsTheLatestWindowsGivens(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	byCase := map[string]models.MappedTestCase{"pkg::TestA": {Dir: "pkg"}, "pkg::TestC": {Dir: "pkg"}}
	stampGiven(byCase, []models.ScopeWindow{
		{Name: "pkg::TestA", Start: t0.Add(2 * time.Second), Given: []models.GivenValue{{Name: "given:app", Value: "app-2"}, {Name: "given:app#2", Value: "app-3", Source: "a_test.go:9"}}},
		{Name: "pkg::TestA", Start: t0, Given: []models.GivenValue{{Name: "given:app", Value: "app-1"}}},
		{Name: "pkg::TestB", Start: t0},
		{Name: "pkg::TestC", Start: t0, Given: []models.GivenValue{{Name: "given:n", Value: "1"}}},
		{Name: "pkg::TestC", Start: t0.Add(time.Second)},
	})
	require.Equal(t, models.MappedTestCase{Dir: "pkg", Given: []models.GivenValue{{Name: "given:app", Value: "app-2"}, {Name: "given:app#2", Value: "app-3", Source: "a_test.go:9"}}}, byCase["pkg::TestA"])
	require.Empty(t, byCase["pkg::TestC"].Given, "the last attempt gave none")
	_, ok := byCase["pkg::TestB"]
	require.False(t, ok, "a test that gave none gets no entry from it")
}

// The replay's table carries, by test, the values each test gave when it was
// recorded.
func TestRecordedGivensAreTheTestsThatGaveAny(t *testing.T) {
	require.Nil(t, recordedGivens(nil))
	require.Nil(t, recordedGivens(&models.Mapping{TestCases: []models.MappedTestCase{{ID: "pkg::TestB"}}}))
	g := []models.GivenValue{{Name: "given:app", Value: "app-1"}}
	require.Equal(t, map[string][]models.GivenValue{"pkg::TestA": g}, recordedGivens(&models.Mapping{TestCases: []models.MappedTestCase{
		{ID: "pkg::TestA", Given: g}, {ID: "pkg::TestB"},
	}}))
}

// A record writes into each test's mapping the values it gave while it was
// open (Record -> stampGiven), and a replay of the set hands the agent those
// values, by test, in the same push as its table (pushProvenTable ->
// recordedGivens): without either, every given binds with no recorded value
// and each call that carries one misses.
func TestARecordKeepsTheGivensAndItsReplayPushesThem(t *testing.T) {
	provenOn(t)
	g := []models.GivenValue{{Name: "given:nonce", Value: "1761609600123456789", Source: "a_test.go:20"}, {Name: "given:nonce#2", Value: "1761609600123456790"}}

	dir := t.TempDir()
	w := mark("orders::TestPlace", 0, 10*time.Millisecond)
	w.Outcome, w.Dir, w.Given = models.ScopeOutcomePassed, "orders", g
	other := mark("orders::TestNone", 20*time.Millisecond, 30*time.Millisecond)
	other.Outcome, other.Dir = models.ScopeOutcomePassed, "orders"
	instr := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped}),
		windows: []models.ScopeWindow{w, other}}
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path = dir
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	require.NoError(t, New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg).Record(context.Background()))
	set := (&mockService{config: cfg}).setName()
	mp, err := mapdb.New(zap.NewNop(), dir, "").GetMapping(context.Background(), set)
	require.NoError(t, err)
	require.NotNil(t, mp)
	byID := map[string]models.MappedTestCase{}
	for _, tc := range mp.TestCases {
		byID[tc.ID] = tc
	}
	require.Equal(t, g, byID["orders::TestPlace"].Given, "the mapping keeps the values the test gave, in order")
	require.Empty(t, byID["orders::TestNone"].Given)

	// The replay pushes them with the table.
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	mdb := mapdb.New(zap.NewNop(), keployDir, "")
	ctx := context.Background()
	require.NoError(t, mdb.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"orders::TestPlace": {{Name: "mock-0"}}, "orders::TestNone": nil}))
	require.NoError(t, mdb.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
		"orders::TestPlace": {At: runnerT0, Given: g}, "orders::TestNone": {At: runnerT0},
	}, nil, nil))
	rinstr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	rcfg := instrConfig(rinstr, utils.Native, "go test -p 1 -json ./...")
	rcfg.Path, rcfg.Mock.OnMiss = keployDir, "fail"
	require.NoError(t, New(zap.NewNop(), rinstr, named("mock-0"), mdb, nil, nil, rcfg).Replay(ctx))
	require.Equal(t, []string{"mock-0"}, rinstr.table["orders::TestPlace"])
	require.Equal(t, map[string][]models.GivenValue{"orders::TestPlace": g}, rinstr.given, "the replay pushes each test's recorded givens")
}
