package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// keploy.yml's test.globalNoise reaches the config of keploy mock replay, the
// command the judging rounds of keploy mock record run with --config-path, at
// the path a miss's hint names (test.globalNoise.global.<bucket>, and
// test-sets.<set>.<bucket> for one set), through the CLI's own
// PreProcessFlags and ValidateFlags. A bucket written straight under
// test.globalNoise is no noise keploy reads, so no hint may name that path.
// The keys are lower case with no dot, as viper reads every key in lower
// case, and a dotted key decodes only in a build with the viper_bind_struct
// tag, as keploy's release is and a plain go test is not.
func TestMockReplayReadsKeployYmlNoise(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	t.Cleanup(viper.Reset)
	for _, tc := range []struct {
		name, yml string
		global    config.GlobalNoise
		sets      config.TestsetNoise
	}{
		{
			name:   "global requestbody noise",
			yml:    "test:\n  globalNoise:\n    global:\n      requestbody:\n        qty: []\n",
			global: config.GlobalNoise{"requestbody": {"qty": {}}},
		},
		{
			name: "one set's requestbody noise",
			yml:  "test:\n  globalNoise:\n    test-sets:\n      orders:\n        requestbody:\n          ts: []\n",
			sets: config.TestsetNoise{"orders": {"requestbody": {"ts": {}}}},
		},
		{
			name: "a bucket straight under globalNoise",
			yml:  "test:\n  globalNoise:\n    requestbody:\n      qty: []\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			dir := t.TempDir()
			t.Chdir(dir)
			if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(tc.yml), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd, cfg, c := mockVerbCmd(t, dir, "replay")
			if err := cmd.ParseFlags([]string{"-c", "python app.py", "-p", dir, "--config-path", dir}); err != nil {
				t.Fatal(err)
			}
			if err := c.PreProcessFlags(cmd); err != nil {
				t.Fatalf("PreProcessFlags: %v", err)
			}
			if err := c.ValidateFlags(context.Background(), cmd); err != nil {
				t.Fatalf("ValidateFlags: %v", err)
			}
			if got := cfg.Test.GlobalNoise.Global; len(got) != len(tc.global) || (len(got) > 0 && !reflect.DeepEqual(got, tc.global)) {
				t.Fatalf("test.globalNoise.global = %#v, want %#v", got, tc.global)
			}
			if got := cfg.Test.GlobalNoise.Testsets; len(got) != len(tc.sets) || (len(got) > 0 && !reflect.DeepEqual(got, tc.sets)) {
				t.Fatalf("test.globalNoise.test-sets = %#v, want %#v", got, tc.sets)
			}
		})
	}
}
