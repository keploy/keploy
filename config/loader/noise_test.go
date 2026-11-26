package loader

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
)

// readWithFiles reads dir's config as PreProcessFlags does: Find, then
// Files' Read, Merge for the override, and Unmarshal into keploy's Config.
func readWithFiles(dir string) (config.Config, error) {
	var cfg config.Config
	f := New(newViper())
	file := Find(dir)
	if file == "" {
		return cfg, f.Unmarshal(&cfg)
	}
	if err := f.Read(file); err != nil {
		return cfg, err
	}
	if _, err := os.Stat(overridePath(dir)); err == nil {
		if err := f.Merge(overridePath(dir)); err != nil {
			return cfg, err
		}
	}
	return cfg, f.Unmarshal(&cfg)
}

// keploy.yml's test.globalNoise keys are the user's url regexes and field
// paths, and keploy reads them as written: case and dots are kept, in global
// and in a test set, whose name is kept too. Only the names keploy reads them
// by -- globalNoise, global, test-sets and the buckets -- are read in any
// case, as viper reads them. Before, viper lowercased every key and split it
// on ".": "^/api/Users/[0-9]+$" never matched /api/Users/3, and any dotted
// key -- the requestbody entry keploy's own hints name for a nested field --
// made keploy refuse keploy.yml. Mutation: read the keys through viper, or
// keep a bucket's name as written.
func TestGlobalNoiseKeysAreReadAsWritten(t *testing.T) {
	body := `test:
  GlobalNoise:
    Global:
      url: {"^/api/Users/[0-9]+$": [], "^/v1\\.2/files/[^/]+$": []}
      requestBody: {"meta.ts": [], "items[].ts": [], "ids[]": [], "Data.ID": ["^[0-9]+$"]}
      Body: {"data.id": []}
      header: {X-Request-Id: []}
    test-sets:
      Set.V2:
        URL: {"^/Accounts/ACME/orders/[0-9]+$": []}
`
	cfg, err := readWithFiles(writeConfig(t, body, ""))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := config.Globalnoise{
		Global: config.GlobalNoise{
			"url":         {`^/api/Users/[0-9]+$`: {}, `^/v1\.2/files/[^/]+$`: {}},
			"requestbody": {"meta.ts": {}, "items[].ts": {}, "ids[]": {}, "Data.ID": {"^[0-9]+$"}},
			"body":        {"data.id": {}},
			"header":      {"X-Request-Id": {}},
		},
		Testsets: config.TestsetNoise{
			"Set.V2": {"url": {`^/Accounts/ACME/orders/[0-9]+$`: {}}},
		},
	}
	if !reflect.DeepEqual(cfg.Test.GlobalNoise, want) {
		t.Fatalf("test.globalNoise read as\n%#v\nwant\n%#v", cfg.Test.GlobalNoise, want)
	}
	// The rest of the file is viper's read of it.
	if _, oracle, err := readWithViper(writeConfig(t, "test:\n  delay: 9\n", "")); err != nil || oracle.Test.Delay != 9 {
		t.Fatalf("viper read delay %d (%v)", oracle.Test.Delay, err)
	}
}

func TestEndpointNoiseIsReadAsWritten(t *testing.T) {
	body := `test:
  globalNoise:
    global: {}
    endpoints:
      - method: GET
        path: /apps/{id}/Config
        body: {"data.heartbeats[].At": [], "Version": ["^[0-9a-f]{8}$"]}
        header: {X-Build: []}
      - Path: /version
        Body: {version: []}
`
	cfg, err := readWithFiles(writeConfig(t, body, ""))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []config.EndpointNoise{
		{Method: "GET", Path: "/apps/{id}/Config", Body: config.Noise{"data.heartbeats[].At": {}, "Version": {"^[0-9a-f]{8}$"}}, Header: config.Noise{"X-Build": {}}},
		{Path: "/version", Body: config.Noise{"version": {}}},
	}
	if !reflect.DeepEqual(cfg.Test.GlobalNoise.Endpoints, want) {
		t.Fatalf("endpoints read as\n%#v\nwant\n%#v", cfg.Test.GlobalNoise.Endpoints, want)
	}
}

// The override's test.globalNoise is merged over keploy.yml's as viper
// merges the rest: an entry it writes is added, or replaces keploy.yml's of
// the same key, and keys are compared as written, so "^/Users/" and
// "^/users/" are two entries. Bucket names merge in any case. Mutation:
// replace keploy.yml's noise with the override's, or merge keys folded.
func TestGlobalNoiseOverrideMergesAsWritten(t *testing.T) {
	base := `test:
  globalNoise:
    global:
      url: {"^/Users/[0-9]+$": []}
      requestbody: {"meta.ts": [], "q": []}
    test-sets:
      Set-A: {body: {"a.b": []}}
`
	override := `test:
  globalNoise:
    global:
      url: {"^/users/[0-9]+$": []}
      requestBody: {"meta.ts": ["^[0-9]+$"]}
    test-sets:
      Set-B: {body: {"c.d": []}}
`
	cfg, err := readWithFiles(writeConfig(t, base, override))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := config.Globalnoise{
		Global: config.GlobalNoise{
			"url":         {"^/Users/[0-9]+$": {}, "^/users/[0-9]+$": {}},
			"requestbody": {"meta.ts": {"^[0-9]+$"}, "q": {}},
		},
		Testsets: config.TestsetNoise{
			"Set-A": {"body": {"a.b": {}}},
			"Set-B": {"body": {"c.d": {}}},
		},
	}
	if !reflect.DeepEqual(cfg.Test.GlobalNoise, want) {
		t.Fatalf("test.globalNoise read as\n%#v\nwant\n%#v", cfg.Test.GlobalNoise, want)
	}
}

// Where viper keeps a key as written -- lowercase, with no dot -- keploy
// reads test.globalNoise exactly as viper reads it: each value with viper's
// weak typing and hooks (a string is split on ",", a number is a one-item
// list, an empty value is no list), and a value of the wrong type refused,
// as viper refuses it. Mutation: decode the values another way, or let a
// wrong type through.
func TestGlobalNoiseValuesAreDecodedAsViperDecodesThem(t *testing.T) {
	for _, c := range []struct {
		name, body, override string
		wantErr              bool
	}{
		{name: "lists", body: "test:\n  globalNoise:\n    global:\n      body: {id: [a, b], ts: []}\n"},
		{name: "a string, split on commas", body: "test:\n  globalNoise:\n    global:\n      body: {id: \"a,b\"}\n"},
		{name: "a number", body: "test:\n  globalNoise:\n    global:\n      body: {id: 5}\n"},
		{name: "an empty value", body: "test:\n  globalNoise:\n    global:\n      body: {id: }\n"},
		{name: "an empty string", body: "test:\n  globalNoise:\n    global:\n      body: {id: \"\"}\n"},
		{name: "a numeric key", body: "test:\n  globalNoise:\n    global:\n      body: {5: []}\n"},
		{name: "a test set", body: "test:\n  globalNoise:\n    test-sets:\n      s1: {body: {id: [x]}}\n"},
		{name: "an empty globalNoise", body: "test:\n  globalNoise:\n"},
		{name: "an override", body: "test:\n  globalNoise:\n    global:\n      body: {id: [a]}\n", override: "test:\n  globalNoise:\n    global:\n      body: {id: [b], ts: []}\n"},
		{name: "a bucket that is a list", body: "test:\n  globalNoise:\n    global:\n      body: [a]\n", wantErr: true},
		{name: "an entry that is a map", body: "test:\n  globalNoise:\n    global:\n      body: {id: {x: 1}}\n", wantErr: true},
		{name: "globalNoise a string", body: "test:\n  globalNoise: x\n", wantErr: true},
		{name: "test-sets a list", body: "test:\n  globalNoise:\n    test-sets: [1]\n", wantErr: true},
		// A bucket that is a list over a bucket that is a map: viper keeps
		// the map, and keploy reads it.
		{name: "a list over a map", body: "test:\n  globalNoise:\n    global:\n      body: {id: []}\n", override: "test:\n  globalNoise:\n    global:\n      body: [a]\n"},
		// A key that is not a string makes YAML's map a map[any]any: Body is
		// still read as body.
		{name: "a numeric bucket name", body: "test:\n  globalNoise:\n    global:\n      Body: {id: []}\n      5: {x: []}\n"},
		{name: "globalNoise a string, an override without it", body: "test:\n  globalNoise: x\n", override: "test:\n  delay: 5\n", wantErr: true},
		{name: "globalNoise a string, emptied by the override", body: "test:\n  globalNoise: x\n", override: "test:\n  globalNoise:\n"},
		{name: "globalNoise a map, emptied by the override", body: "test:\n  globalNoise:\n    global:\n      body: {id: []}\n", override: "test:\n  globalNoise:\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := writeConfig(t, c.body, c.override)
			_, want, wantErr := readWithViper(dir)
			got, err := readWithFiles(dir)
			if (wantErr != nil) != c.wantErr {
				t.Fatalf("viper's read errs %v; the row wants an error: %v", wantErr, c.wantErr)
			}
			if (err != nil) != (wantErr != nil) {
				t.Fatalf("read errs %v; viper's %v", err, wantErr)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "globalnoise") && !strings.Contains(err.Error(), "globalNoise") {
					t.Fatalf("error %q names no globalNoise", err)
				}
				return
			}
			if !reflect.DeepEqual(got.Test.GlobalNoise, want.Test.GlobalNoise) {
				t.Fatalf("test.globalNoise read as\n%#v\nviper reads\n%#v", got.Test.GlobalNoise, want.Test.GlobalNoise)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("the rest of the config is not viper's read of it")
			}
		})
	}
}

// A file with no test.globalNoise is read as viper reads it, and the default
// keploy.yml's empty one too.
func TestNoGlobalNoiseIsViperUnmarshal(t *testing.T) {
	for _, body := range []string{"test:\n  delay: 9\n", config.GetDefaultConfig(), marshalledConfig(t)} {
		dir := writeConfig(t, body, "")
		_, want, wantErr := readWithViper(dir)
		got, err := readWithFiles(dir)
		if err != nil || wantErr != nil {
			t.Fatalf("read errs %v; viper's %v", err, wantErr)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read\n%#v\nviper reads\n%#v", got.Test.GlobalNoise, want.Test.GlobalNoise)
		}
	}
}

// Keys of one name in different cases -- two buckets, or globalNoise twice --
// are merged, the same way every run, where viper keeps one of them, which
// one varying with Go's map order. Mutation: keep one of them.
func TestNamesInDifferentCasesAreMerged(t *testing.T) {
	body := `test:
  globalNoise:
    global:
      body: {a: []}
      Body: {b: ["^x$"]}
  GlobalNoise:
    global:
      url: {"^/Users/[0-9]+$": []}
`
	for i := 0; i < 5; i++ {
		cfg, err := readWithFiles(writeConfig(t, body, ""))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		want := config.GlobalNoise{
			"body": {"a": {}, "b": {"^x$"}},
			"url":  {"^/Users/[0-9]+$": {}},
		}
		if !reflect.DeepEqual(cfg.Test.GlobalNoise.Global, want) {
			t.Fatalf("read %d: test.globalNoise.global read as\n%#v\nwant\n%#v", i, cfg.Test.GlobalNoise.Global, want)
		}
	}
}

// With no file read that has test.globalNoise, Unmarshal is viper's: a
// test.globalNoise set on viper itself is decoded as viper holds it.
// Mutation: put the files' (none) in its place.
func TestNoiseSetOnViperOnlyIsViperUnmarshal(t *testing.T) {
	f := New(newViper())
	if err := f.Read(filepath.Join(writeConfig(t, "test:\n  delay: 9\n", ""), "keploy.yml")); err != nil {
		t.Fatalf("read: %v", err)
	}
	f.v.Set("test.globalNoise.global.body", map[string]any{"id": []string{"x"}})
	var cfg config.Config
	if err := f.Unmarshal(&cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := config.GlobalNoise{"body": {"id": {"x"}}}
	if !reflect.DeepEqual(cfg.Test.GlobalNoise.Global, want) || cfg.Test.Delay != 9 {
		t.Fatalf("read globalNoise.global %#v, delay %d; want %#v, 9", cfg.Test.GlobalNoise.Global, cfg.Test.Delay, want)
	}
}
