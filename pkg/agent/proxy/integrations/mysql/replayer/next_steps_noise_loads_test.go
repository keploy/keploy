package replayer

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/config/loader"
)

// The keploy.yml entry each exact-drift next step names loads as keploy.yml,
// through keploy's own read of it (loader.Files, as PreProcessFlags reads
// it), with its dotted key whole, under the requestbody bucket the replayer
// reads (requestBodyNoise). Before, keploy read the key through viper,
// which splits a key on "." and lowercases it.
func TestExactDriftNextStepsNoiseLoadsAsKeployYml(t *testing.T) {
	for _, c := range []struct{ steps, key string }{
		{exactDriftNextSteps, "body.parameters.0.value"},
		{exactLiteralNextSteps, "body.query.literals.0"},
	} {
		entry := regexp.MustCompile(`requestBody: \{[^}]*\}`).FindString(c.steps)
		if entry == "" {
			t.Fatalf("next steps %q name no requestBody entry", c.steps)
		}
		dir := t.TempDir()
		yml := "test:\n  globalNoise:\n    global:\n      " + entry + "\n"
		if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
		v := viper.New()
		v.SetConfigType("yml")
		files := loader.New(v)
		if err := files.Read(loader.Find(dir)); err != nil {
			t.Fatalf("read keploy.yml %q: %v", yml, err)
		}
		cfg := config.New()
		if err := files.Unmarshal(cfg); err != nil {
			t.Fatalf("keploy refuses keploy.yml %q: %v", yml, err)
		}
		if _, ok := cfg.Test.GlobalNoise.Global["requestbody"][c.key]; !ok {
			t.Errorf("keploy.yml %q loads test.globalNoise.global as %v; want requestbody %q", yml, cfg.Test.GlobalNoise.Global, c.key)
		}
	}
}
