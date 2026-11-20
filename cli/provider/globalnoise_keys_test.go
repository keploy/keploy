package provider

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/config"
)

// keploy's own read of keploy.yml, PreProcessFlags, keeps test.globalNoise's
// keys as written, with the override merged over it: a url regex keeps its
// case and its escaped dot, a requestbody field path its dots and brackets,
// and a test set its name. Before, keploy refused this keploy.yml ("failed
// to unmarshal the config") for its first dotted key, and lowercased the
// rest, so "^/Users/[0-9]+$" matched no recorded /Users/3. Mutation:
// unmarshal with viper's copy of test.globalNoise.
func TestPreProcessFlagsReadsGlobalNoiseKeysAsWritten(t *testing.T) {
	configDir := writeKeployYML(t, `test:
  globalNoise:
    global:
      url: {"^/Users/[0-9]+$": [], "^/v1\\.2/files/[^/]+$": []}
      requestbody: {"meta.ts": [], "items[].ts": []}
    test-sets:
      Set.V2: {body: {"data.id": []}}
`)
	appDir := t.TempDir()
	override := filepath.Join(appDir, filepath.Base(appDir)+".keploy.yml")
	if err := os.WriteFile(override, []byte("test:\n  globalNoise:\n    global:\n      requestbody: {\"data.Id\": []}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := preProcessIn(t, configDir, appDir)
	if err != nil {
		t.Fatalf("PreProcessFlags: %v", err)
	}
	want := config.Globalnoise{
		Global: config.GlobalNoise{
			"url":         {`^/Users/[0-9]+$`: {}, `^/v1\.2/files/[^/]+$`: {}},
			"requestbody": {"meta.ts": {}, "items[].ts": {}, "data.Id": {}},
		},
		Testsets: config.TestsetNoise{"Set.V2": {"body": {"data.id": {}}}},
	}
	if !reflect.DeepEqual(cfg.Test.GlobalNoise, want) {
		t.Fatalf("test.globalNoise read as\n%#v\nwant\n%#v", cfg.Test.GlobalNoise, want)
	}
}
