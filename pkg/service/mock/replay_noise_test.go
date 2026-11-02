package mock

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// keploy mock replay hands the agent the matching noise keploy.yml declares
// (test.globalNoise: its global noise, and the noise its test-sets entry
// gives the set the replay serves), as keploy test does, so a field declared
// noise there is not compared when a call is matched to its mock. The judging
// rounds of keploy mock record are keploy mock replay runs of the set the
// record made, with the switch on and learning: they take it the same way.
// Without it every matcher compared the field, and the noise a miss or a
// refusal told the user to add in keploy.yml changed nothing.
func TestMockReplayHandsTheAgentKeployYmlNoise(t *testing.T) {
	global := config.GlobalNoise{
		"header":      {"X-Request-Id": {}},
		"body":        {"ts": {}},
		"requestbody": {"cmd.filter._id": {}},
		// keploy test hands the agent the header, body and requestbody
		// buckets only; the rest is for its own assertions.
		"url": {"page": {}},
	}
	setNoise := config.TestsetNoise{
		"set":   {"requestbody": {"docs.0.createdAt": {}}},
		"other": {"requestbody": {"never": {}}},
	}
	for _, c := range []struct {
		name   string
		round  bool
		global config.GlobalNoise
		sets   config.TestsetNoise
		want   map[string]map[string][]string
	}{
		{"keploy mock replay, global noise", false, global, nil, map[string]map[string][]string{
			"header":      {"X-Request-Id": {}},
			"body":        {"ts": {}},
			"requestbody": {"cmd.filter._id": {}},
		}},
		{"keploy mock replay, global and the set's noise", false, global, setNoise, map[string]map[string][]string{
			"header":      {"X-Request-Id": {}},
			"body":        {"ts": {}},
			"requestbody": {"cmd.filter._id": {}, "docs.0.createdAt": {}},
		}},
		{"keploy mock replay, the set's noise alone", false, nil, setNoise, map[string]map[string][]string{
			"header":      {},
			"body":        {},
			"requestbody": {"docs.0.createdAt": {}},
		}},
		{"keploy mock replay, noise of another set only", false, nil, config.TestsetNoise{"other": {"requestbody": {"never": {}}}}, map[string]map[string][]string{}},
		{"keploy mock replay, no noise", false, nil, nil, map[string]map[string][]string{}},
		{"record's judging round, global and the set's noise", true, global, setNoise, map[string]map[string][]string{
			"header":      {"X-Request-Id": {}},
			"body":        {"ts": {}},
			"requestbody": {"cmd.filter._id": {}, "docs.0.createdAt": {}},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.round {
				provenOn(t)
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			instr := &outgoingInstr{runnerInstr: newRunnerInstr(t)}
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
			cfg.Test.GlobalNoise.Global = c.global
			cfg.Test.GlobalNoise.Testsets = c.sets
			if c.round {
				cfg.Test.Learn = true
				cfg.Test.LearnTests = map[string]bool{"o::TestA": true}
			}
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			require.NoError(t, New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))

			require.Len(t, instr.got, 1, "the replay did not put the agent in mock-serving mode once")
			got := instr.got[0].NoiseConfig
			if len(c.want) == 0 {
				require.Empty(t, got, "the agent was handed noise keploy.yml does not give this set")
				return
			}
			require.Equal(t, c.want, got)
			if len(got["requestbody"]) > 0 {
				got["requestbody"]["added"] = []string{}
				_, leaked := cfg.Test.GlobalNoise.Global["requestbody"]["added"]
				_, leakedSet := cfg.Test.GlobalNoise.Testsets["set"]["requestbody"]["added"]
				require.False(t, leaked || leakedSet, "the agent was handed the config's own noise map")
			}
		})
	}
}
