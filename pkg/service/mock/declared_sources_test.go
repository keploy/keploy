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
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Under the switch the replay hands the agent, with the table, the sources
// declared rules give recordings' fields (mappings' sources:), off an entry
// of any list: a test's and worker: alike. An entry with none has none.
func TestTheReplayPushesTheDeclaredSourcesOfTheRecordings(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n          sources:\n            - rule: at@0a1b2c3d\n              kind: clock\n              field: body.at\n              clock: {min: -1s, max: 0s, res: 1s}\n        - name: mock-1\nworker:\n    - name: mock-3\n      sources:\n        - rule: id@44aa55bb\n          kind: minted\n          field: body._id\n          minted: {min_len: 36, max_len: 36}\n    - name: mock-4\n"), 0o644))
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	require.NoError(t, New(zap.NewNop(), instr, named("mock-0", "mock-1", "mock-3", "mock-4"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
	require.Equal(t, map[string][]models.DeclaredSource{
		"mock-0": {{Rule: "at@0a1b2c3d", Kind: models.DeclaredSourceClock, Field: "body.at", Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}}},
		"mock-3": {{Rule: "id@44aa55bb", Kind: models.DeclaredSourceMinted, Field: "body._id", Minted: &models.MintedForm{MinLen: 36, MaxLen: 36}}},
	}, instr.sources)
}

// Under the switch the replay hands the agent, with the table, whether the
// learn step checks a changed time by its form alone: on only where the
// command that runs the replay turned it on (config.MockCmd.TimeForm,
// keploy's T2 commands), off for every other caller.
func TestTheReplayPushesTheTimeDefaultItsCommandSet(t *testing.T) {
	provenOn(t)
	for _, on := range []bool{true, false} {
		repo := t.TempDir()
		t.Chdir(repo)
		keployDir := filepath.Join(repo, "keploy")
		require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n"), 0o644))
		instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
		instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
		instr.timeForm = !on // the push must set it, whichever it was
		cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
		cfg.Path, cfg.Mock.OnMiss, cfg.Mock.TimeForm = keployDir, "fail", on
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = 0 })
		require.NoError(t, New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
		require.NotNil(t, instr.table, "the replay pushed its table (time default %v)", on)
		require.Equal(t, on, instr.timeForm, "the time default the command set")
	}
}

// declaredSources reads every list of the mapping, and only a name the
// agent holds: a recording it never holds answers no call to learn. A
// mapping with no source, and none, gives none.
func TestDeclaredSourcesReadsEveryListAndOnlyHeldNames(t *testing.T) {
	minted := []models.DeclaredSource{{Rule: "id@44aa55bb", Kind: models.DeclaredSourceMinted, Field: "body._id", Minted: &models.MintedForm{MinLen: 36, MaxLen: 36}}}
	mp := &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "o::TestA", Mocks: []models.MockEntry{{Name: "mock-1", Sources: minted}, {Name: "mock-2"}}}},
		Startup:   []models.MockEntry{{Name: "mock-3", Sources: minted}},
		Runner:    []models.MockEntry{{Name: "mock-4", Sources: minted}},
		Worker:    []models.MockEntry{{Name: "mock-5", Sources: minted}, {Name: "mock-9", Sources: minted}},
		Shared:    []models.MockEntry{{Name: "mock-6", Sources: minted}},
	}
	var held []*models.Mock
	for _, n := range []string{"mock-1", "mock-2", "mock-3", "mock-4", "mock-5", "mock-6"} {
		held = append(held, &models.Mock{Name: n})
	}
	held = append(held, nil, &models.Mock{})
	require.Equal(t, map[string][]models.DeclaredSource{"mock-1": minted, "mock-3": minted, "mock-4": minted, "mock-5": minted, "mock-6": minted}, declaredSources(mp, held))
	require.Nil(t, declaredSources(&models.Mapping{Worker: []models.MockEntry{{Name: "mock-5"}}}, held))
	require.Nil(t, declaredSources(nil, held))
}
