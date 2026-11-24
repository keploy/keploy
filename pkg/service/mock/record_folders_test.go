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
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A record names each test's folder in mappings.yaml as the claim it ran
// under gave it on its window (the project's folder joined with the claim's
// key, absolute), relative to the repository's top, and keeps the claim's
// key beside it, which enterprise's refusal of a test with no folder reads.
// Under the proven switch, for two test processes of the project, one claimed
// "tests" whose test made a call, one claimed "." whose test made none:
// "tests" and "." when the project is the repository's top, "svc/tests" and
// "svc" when it is the repository's svc. The test that made no call is named
// in its folder too: keploy mock record's commit refuses a test that names
// none. Mutation: drop the folder's copy into the test's entry, or name it as
// the window gives it (absolute), and every row fails.
func TestARecordNamesEachClaimedTestsFolderFromTheRepositorysTop(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	judgedBuild(t)
	at := func(ms int) time.Time { return runnerT0.Add(time.Duration(ms) * time.Millisecond) }
	for _, nested := range []bool{false, true} {
		repo := t.TempDir()
		gitRepo(t, repo)
		repo, err := filepath.EvalSymlinks(repo)
		require.NoError(t, err)
		project, want := repo, map[string]string{"tests::TestCalls": "tests", ".::TestNoCall": "."}
		if nested {
			project = filepath.Join(repo, "svc")
			want = map[string]string{"tests::TestCalls": "svc/tests", ".::TestNoCall": "svc"}
		}
		require.NoError(t, os.MkdirAll(filepath.Join(project, "tests"), 0o755))
		t.Chdir(project)
		calls, none := mark("tests::TestCalls", 0, 10*time.Millisecond), mark(".::TestNoCall", 20*time.Millisecond, 30*time.Millisecond)
		calls.Outcome, none.Outcome = models.ScopeOutcomePassed, models.ScopeOutcomePassed
		calls.Dir, none.Dir = filepath.Join(project, "tests"), project
		claims := []models.ScopeWindow{{Claim: "tests", Start: at(-1)}, {Claim: ".", Start: at(18)}}
		instr := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{}), windows: []models.ScopeWindow{claims[0], calls, claims[1], none}}
		instr.mocks = []*models.Mock{mockAt("mock-0", at(5))}
		dir := t.TempDir()
		cfg := instrConfig(instr.composeInstr, utils.Native, "python -m pytest")
		cfg.Path, cfg.ConfigPath = dir, project
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = 0 })
		svc := New(zap.NewNop(), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
		require.NoError(t, svc.Record(context.Background()))
		mapping, err := mapdb.New(zap.NewNop(), dir, "").GetMapping(context.Background(), (&mockService{config: cfg}).setName())
		require.NoError(t, err)
		require.NotNil(t, mapping)
		got, keys, mocks := map[string]string{}, map[string]string{}, map[string][]string{}
		for _, tc := range mapping.TestCases {
			got[tc.ID], keys[tc.ID], mocks[tc.ID] = tc.Dir, tc.Key, tc.MockNames()
		}
		require.Equal(t, want, got, "nested=%v", nested)
		require.Equal(t, map[string]string{"tests::TestCalls": "tests", ".::TestNoCall": "."}, keys, "nested=%v: each test keeps its claim's key", nested)
		require.Equal(t, []string{"mock-0"}, mocks["tests::TestCalls"])
		require.Empty(t, mocks[".::TestNoCall"], "the test that made no call has no mocks, and its folder")
	}
}
