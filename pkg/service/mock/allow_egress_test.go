package mock

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// keploy mock replay hands test.allowEgress to the agent it starts, as keploy
// test does, so `--allow-egress dns` opens the same egress on both replays and
// a replay without it keeps a DNS miss inside its own network.
func TestMockReplayHandsAllowEgressToTheAgent(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, allow := range [][]string{nil, {models.AllowEgressDNS}} {
		instr := newRunnerInstr(t)
		repo := t.TempDir()
		t.Chdir(repo)
		keployDir := filepath.Join(repo, "keploy")
		require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
		writeMapping(t, keployDir)
		instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
		cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
		cfg.Path = keployDir
		cfg.Mock.OnMiss = "fail"
		cfg.Test.AllowEgress = allow
		utils.ErrCode = 0
		t.Cleanup(func() { utils.ErrCode = 0 })
		require.NoError(t, New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))

		opts, _ := instr.setup()
		require.Equal(t, models.MODE_TEST, opts.Mode, "the agent was not set up")
		require.Equal(t, allow, opts.AllowEgress)
	}
}
