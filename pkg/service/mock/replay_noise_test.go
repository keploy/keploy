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

// mockServingOptions runs keploy mock replay of a one-mock set named "set",
// with the proven switch on or off, on a config edit() shapes, and returns the
// options it put the agent in mock-serving mode with.
func mockServingOptions(t *testing.T, sw bool, edit func(*config.Config)) models.OutgoingOptions {
	t.Helper()
	if sw {
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
	edit(cfg)
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	require.NoError(t, New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
	require.Len(t, instr.got, 1, "the replay did not put the agent in mock-serving mode once")
	return instr.got[0]
}

var switchName = map[bool]string{false: "switch off/", true: "switch on/"}

// keploy mock replay, and so each judging round of keploy mock record, hands
// the agent keploy.yml's request noise (test.globalNoise) when it puts it in
// mock-serving mode, as keploy test's replay does
// (replay.PrepareMockNoiseConfig): a header the user declared noise (a
// browser's Referer, the page it was on) is then neither compared nor
// required to be present when a call is matched to its mock. The set's own
// test-sets entry joins the global one, another set's does not, and a
// keploy.yml that declares none hands none. With and without the proven
// switch: the hand-off does not depend on it. A bucket handed with no entries
// is the same to the matchers as one not handed, so the rows compare the
// buckets that have entries.
func TestMockReplayHandsTheAgentKeployYmlRequestNoise(t *testing.T) {
	for _, sw := range []bool{false, true} {
		for _, c := range []struct {
			name   string
			global map[string]map[string][]string
			sets   map[string]map[string]map[string][]string
			want   map[string]map[string][]string
		}{
			{
				name:   "a global header",
				global: map[string]map[string][]string{"header": {"Referer": {}}},
				want:   map[string]map[string][]string{"header": {"Referer": {}}},
			},
			{
				name:   "global header and request-body noise, and the response body's",
				global: map[string]map[string][]string{"header": {"Referer": {}}, "requestbody": {"nonce": {}}, "body": {"ts": {}}},
				want:   map[string]map[string][]string{"header": {"Referer": {}}, "requestbody": {"nonce": {}}, "body": {"ts": {}}},
			},
			{
				name:   "the set's own header beside the global request body, not another set's",
				global: map[string]map[string][]string{"requestbody": {"nonce": {}}},
				sets:   map[string]map[string]map[string][]string{"set": {"header": {"X-Trace": {}}}, "other": {"header": {"X-Other": {}}}},
				want:   map[string]map[string][]string{"header": {"X-Trace": {}}, "requestbody": {"nonce": {}}},
			},
			{
				name: "only another set's noise",
				sets: map[string]map[string]map[string][]string{"other": {"header": {"X-Other": {}}}},
				want: map[string]map[string][]string{},
			},
			{
				name: "no noise",
				want: map[string]map[string][]string{},
			},
		} {
			t.Run(switchName[sw]+c.name, func(t *testing.T) {
				got := mockServingOptions(t, sw, func(cfg *config.Config) {
					cfg.Test.GlobalNoise.Global = c.global
					cfg.Test.GlobalNoise.Testsets = c.sets
				}).NoiseConfig
				withEntries := map[string]map[string][]string{}
				for bucket, fields := range got {
					if len(fields) > 0 {
						withEntries[bucket] = fields
					}
				}
				require.Equal(t, c.want, withEntries, "noise handed to the agent: %v", got)
			})
		}
	}
}

// keploy mock replay hands the agent keploy.yml's other matching settings as
// keploy test's replay does: test.disableAutoHeaderNoise (the flaky-header
// list is then not excused), test.mockNoiseDetection and test.mockNoiseStrict
// (each under its pre-rename spelling too, schemaNoiseDetection and
// schemaNoiseStrict). A keploy.yml that sets none hands none.
func TestMockReplayHandsTheAgentKeployYmlMatchingSettings(t *testing.T) {
	type handed struct{ disableAutoHeaderNoise, detection, strict bool }
	for _, sw := range []bool{false, true} {
		for _, c := range []struct {
			name string
			edit func(*config.Test)
			want handed
		}{
			{"none set", func(*config.Test) {}, handed{}},
			{"disableAutoHeaderNoise", func(ct *config.Test) { ct.DisableAutoHeaderNoise = true }, handed{disableAutoHeaderNoise: true}},
			{"mockNoiseDetection", func(ct *config.Test) { ct.MockNoiseDetection = true }, handed{detection: true}},
			{"mockNoiseStrict", func(ct *config.Test) { ct.MockNoiseStrict = true }, handed{strict: true}},
			{"schemaNoiseDetection, the pre-rename spelling", func(ct *config.Test) { ct.SchemaNoiseDetection = true }, handed{detection: true}}, //nolint:staticcheck // SA1019: the pre-rename spelling is what this row sets.
			{"schemaNoiseStrict, the pre-rename spelling", func(ct *config.Test) { ct.SchemaNoiseStrict = true }, handed{strict: true}},          //nolint:staticcheck // SA1019: the pre-rename spelling is what this row sets.
		} {
			t.Run(switchName[sw]+c.name, func(t *testing.T) {
				got := mockServingOptions(t, sw, func(cfg *config.Config) { c.edit(&cfg.Test) })
				require.Equal(t, c.want, handed{got.DisableAutoHeaderNoise, got.NoiseDetection(), got.NoiseStrict()},
					"settings handed to the agent (disableAutoHeaderNoise, detection, strict)")
			})
		}
	}
}
