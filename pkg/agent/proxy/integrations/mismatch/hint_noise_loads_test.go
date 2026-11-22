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
// one in an array, one with upper case, an array's elements ("ids[]"), keys
// YAML reads otherwise when written plain ("@type", "a: b", "#tag", ""), keys
// that start with '-', '.' or '/', and a header. Before, keploy refused
// keploy.yml for the first dotted key ("failed to unmarshal the config"),
// and the hint wrote every key plain, so the hint for an array's elements
// was no YAML: a user who followed the hint stopped every keploy command.
func TestHintNoiseEntriesLoadAsKeployYml(t *testing.T) {
	drift := func(path string) models.MockFieldDiff {
		return models.MockFieldDiff{Path: path, Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"}
	}
	hint, ok := matchingNoise(ProtocolHTTP, []models.MockFieldDiff{drift("body.meta.ts"), drift("body.items.0.ts"), drift("body.Data.Id"), drift("body.ids[]"), drift("body.@type"), drift("body.a: b"), drift("body.#tag"), drift("body.-x"), drift("body.-"), drift("body..x"), drift("body./x"), drift("body.x_y"), drift("body."), drift("header.X-Request-Id")}, noiseFacts{})
	if !ok {
		t.Fatal("no noise offered")
	}
	entries := regexp.MustCompile(`(requestbody|header): \{[^}]*\}`).FindAllString(hint, -1)
	if len(entries) != 2 {
		t.Fatalf("hint %q names %d keploy.yml entries; want 2", hint, len(entries))
	}
	got := loadGlobalNoise(t, entries)
	if want := `requestbody: {meta.ts: [], items.0.ts: [], Data.Id: [], "ids[]": [], "@type": [], "a: b": [], "#tag": [], -x: [], -: [], .x: [], /x: [], x_y: [], "": []}`; !strings.Contains(hint, want) {
		t.Errorf("hint %q does not write %s: plain only the keys YAML reads as written plain", hint, want)
	}
	for bucket, keys := range map[string][]string{"requestbody": {"meta.ts", "items.0.ts", "Data.Id", "ids[]", "@type", "a: b", "#tag", "-x", "-", ".x", "/x", "x_y", ""}, "header": {"x-request-id"}} {
		for _, k := range keys {
			if _, ok := got[bucket][k]; !ok {
				t.Errorf("%s key %q is not in test.globalNoise.global as loaded: %v (hint %q)", bucket, k, got, hint)
			}
		}
	}
}

// A Pulsar miss's hint names its payload fields under test.globalNoise's body
// bucket, the one Pulsar's matcher reads: the keys there are written as the
// request-body ones are, so "ids[]" and "@type" are quoted and the entry
// loads with the keys the miss named.
func TestHintNoiseEntriesLoadAsKeployYmlForAPulsarPayload(t *testing.T) {
	drift := func(path string) models.MockFieldDiff {
		return models.MockFieldDiff{Path: path, Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"}
	}
	hint, ok := matchingNoise(ProtocolPulsar, []models.MockFieldDiff{drift("body.meta.ts"), drift("body.ids[]"), drift("body.@type")}, noiseFacts{})
	if !ok {
		t.Fatal("no noise offered")
	}
	entries := regexp.MustCompile(`\bbody: \{[^}]*\}`).FindAllString(hint, -1)
	if len(entries) != 1 {
		t.Fatalf("hint %q names %d keploy.yml body entries; want 1", hint, len(entries))
	}
	if want := `body: {meta.ts: [], "ids[]": [], "@type": []}`; entries[0] != want {
		t.Errorf("hint %q writes %s, want %s: plain only the keys YAML reads as written plain", hint, entries[0], want)
	}
	got := loadGlobalNoise(t, entries)
	for _, k := range []string{"meta.ts", "ids[]", "@type"} {
		if _, ok := got["body"][k]; !ok {
			t.Errorf("body key %q is not in test.globalNoise.global as loaded: %v (hint %q)", k, got, hint)
		}
	}
}

// A header whose name YAML reads otherwise when written plain (an HTTP
// token may start with '#', which opens a YAML comment, or '&', which opens
// an anchor) is quoted in the hint's header entry, so the entry loads with
// the header the miss named.
func TestHintNoiseEntriesLoadAsKeployYmlForAHeaderYAMLReadsOtherwise(t *testing.T) {
	drift := func(path string) models.MockFieldDiff {
		return models.MockFieldDiff{Path: path, Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"}
	}
	hint, ok := matchingNoise(ProtocolHTTP, []models.MockFieldDiff{drift("header.#trace"), drift("header.&span"), drift("header.x-id")}, noiseFacts{})
	if !ok {
		t.Fatal("no noise offered")
	}
	entries := regexp.MustCompile(`header: \{[^}]*\}`).FindAllString(hint, -1)
	if len(entries) != 1 {
		t.Fatalf("hint %q names %d keploy.yml header entries; want 1", hint, len(entries))
	}
	if want := `header: {"#trace": [], "&span": [], x-id: []}`; entries[0] != want {
		t.Errorf("hint %q writes %s, want %s: plain only the keys YAML reads as written plain", hint, entries[0], want)
	}
	got := loadGlobalNoise(t, entries)
	for _, k := range []string{"#trace", "&span", "x-id"} {
		if _, ok := got["header"][k]; !ok {
			t.Errorf("header %q is not in test.globalNoise.global as loaded: %v (hint %q)", k, got, hint)
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
