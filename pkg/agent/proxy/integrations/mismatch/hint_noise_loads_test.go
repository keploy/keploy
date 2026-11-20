package mismatch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/config/loader"
	"go.keploy.io/server/v3/pkg/models"
)

// The keploy.yml entries a miss's hint names load as keploy.yml, through
// keploy's own read of it (loader.Files, as PreProcessFlags reads it), with
// each key as the hint writes it: a nested request-body field's dotted key,
// one in an array, one with upper case, and a header. Before, keploy refused
// keploy.yml for the first dotted key ("failed to unmarshal the config"), so
// a user who followed the hint stopped every keploy command.
func TestHintNoiseEntriesLoadAsKeployYml(t *testing.T) {
	drift := func(path string) models.MockFieldDiff {
		return models.MockFieldDiff{Path: path, Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"}
	}
	hint, ok := matchingNoise(ProtocolHTTP, []models.MockFieldDiff{drift("body.meta.ts"), drift("body.items.0.ts"), drift("body.Data.Id"), drift("header.X-Request-Id")}, noiseFacts{})
	if !ok {
		t.Fatal("no noise offered")
	}
	entries := regexp.MustCompile(`(requestbody|header): \{[^}]*\}`).FindAllString(hint, -1)
	if len(entries) != 2 {
		t.Fatalf("hint %q names %d keploy.yml entries; want 2", hint, len(entries))
	}
	got := loadGlobalNoise(t, entries)
	for bucket, keys := range map[string][]string{"requestbody": {"meta.ts", "items.0.ts", "Data.Id"}, "header": {"x-request-id"}} {
		for _, k := range keys {
			if _, ok := got[bucket][k]; !ok {
				t.Errorf("%s key %q is not in test.globalNoise.global as loaded: %v (hint %q)", bucket, k, got, hint)
			}
		}
	}
}

// loadGlobalNoise is test.globalNoise.global of a keploy.yml that holds the
// entries, each a YAML "bucket: {...}" line, read as keploy reads it.
func loadGlobalNoise(t *testing.T, entries []string) config.GlobalNoise {
	t.Helper()
	dir := t.TempDir()
	yml := "test:\n  globalNoise:\n    global:\n      " + strings.Join(entries, "\n      ") + "\n"
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
	return cfg.Test.GlobalNoise.Global
}
