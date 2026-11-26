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

type setEndReadInstr struct {
	*runnerInstr
	outOpts    models.OutgoingOptions
	setEndRead bool
}

func (s *setEndReadInstr) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	s.outOpts = opts
	return s.runnerInstr.MockOutgoing(ctx, opts)
}

func (s *setEndReadInstr) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	s.setEndRead = models.SetEndRead(ctx)
	return s.runnerInstr.GetMockErrors(ctx)
}

// Under the proven switch keploy mock replay tells the agent it reads the
// set's end (ReadsSetEnds) and marks its final GetMockErrors read with
// models.WithSetEndRead so parsers that hold a call or defer a required
// recording's check until the set's end file it there. With the switch off it
// sets neither.
func TestMockReplayReadsSetEndOnlyUnderTheSwitch(t *testing.T) {
	prev := replaysJudged.Load()
	replaysJudged.Store(true)
	t.Cleanup(func() { replaysJudged.Store(prev) })

	for _, tc := range []struct {
		name string
		env  string
		want bool
	}{
		{"switch off", "", false},
		{"switch on", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.env)
			instr := &setEndReadInstr{runnerInstr: newRunnerInstr(t)}
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
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			require.NoError(t, New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
			require.Equal(t, tc.want, instr.outOpts.ReadsSetEnds)
			require.Equal(t, tc.want, instr.setEndRead)
		})
	}
}
