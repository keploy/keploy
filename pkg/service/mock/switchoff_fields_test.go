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
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// mapdb reads a test entry's begin, mock.real destinations, given values,
// seed and claim key whatever the proven switch, so that a rewrite without
// the switch keeps them. A replay acts on them under the switch only:
// without it, a replay of a set keploy mock record wrote under the switch
// sends the agent no test's begin, destinations or seed, and no recorded
// givens or windows, as before mapdb kept them.
func TestWithTheSwitchOffAReplayActsOnNoTestsRecordedFields(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	t0 := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	t.Setenv(provenswitch.Env, "1")
	require.NoError(t, mapdb.New(zap.NewNop(), keployDir, "").Insert(context.Background(), &models.Mapping{TestSetID: "set", TestCases: []models.MappedTestCase{{
		ID: "o::TestA", Key: "o", At: t0, Real: []string{"db:5432"}, Given: []models.GivenValue{{Name: "given:a", Value: "v"}}, Seed: "0123456789abcdef",
		Mocks: []models.MockEntry{{Name: "mock-0", Kind: "Http"}},
	}}}))
	t.Setenv(provenswitch.Env, "")
	got, err := mapdb.New(zap.NewNop(), keployDir, "").GetMapping(context.Background(), "set")
	require.NoError(t, err)
	require.Len(t, got.TestCases, 1)
	require.True(t, got.TestCases[0].At.Equal(t0), "premise: the begin is read with the switch off: %+v", got.TestCases[0])

	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
	instr.windows = []models.ScopeWindow{{Name: "o::TestA", Start: t0, End: t0.Add(time.Second), Outcome: models.ScopeOutcomePassed}}
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path = keployDir
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	_ = New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	if runs, _ := instr.counts(); runs == 0 {
		t.Fatal("premise: the test command did not run")
	}
	require.Nil(t, instr.begins, "a replay without the switch sent the agent the tests' begins")
	require.Nil(t, instr.seeds, "a replay without the switch sent the agent the tests' seeds")
	require.Nil(t, instr.given, "a replay without the switch sent the agent the tests' recorded givens")
	require.Nil(t, instr.recordedWindows, "a replay without the switch sent the agent the tests' recorded windows")
}
