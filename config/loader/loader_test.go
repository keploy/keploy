package loader

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/safeyaml"
	"gopkg.in/yaml.v3"
)

// writeConfig drops keploy.yml (and, when override != "", the
// <dir>.keploy.yml override named for the directory) into a fresh dir and
// returns the dir.
func writeConfig(t *testing.T, body, override string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if override != "" {
		if err := os.WriteFile(overridePath(dir), []byte(override), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func overridePath(dir string) string {
	return filepath.Join(dir, filepath.Base(dir)+".keploy.yml")
}

func newViper() *viper.Viper {
	v := viper.New()
	v.SetConfigType("yml")
	return v
}

// readWithViper reads dir's config as the CLI did before its reads were
// bounded: viper's own SetConfigFile + ReadInConfig, then MergeInConfig for
// the override, then Unmarshal into keploy's Config. It is the oracle the
// bounded loader must match.
func readWithViper(dir string) (map[string]any, config.Config, error) {
	var cfg config.Config
	v := newViper()
	v.SetConfigFile(filepath.Join(dir, "keploy.yml"))
	if err := v.ReadInConfig(); err != nil {
		return nil, cfg, err
	}
	if _, err := os.Stat(overridePath(dir)); err == nil {
		v.SetConfigFile(overridePath(dir))
		if err := v.MergeInConfig(); err != nil {
			return nil, cfg, err
		}
	}
	err := v.Unmarshal(&cfg)
	return v.AllSettings(), cfg, err
}

// readWithLoader is readWithViper through Find, Read and Merge, as
// PreProcessFlags reads them.
func readWithLoader(dir string) (map[string]any, config.Config, error) {
	var cfg config.Config
	v := newViper()
	if err := readDir(v, dir); err != nil {
		return nil, cfg, err
	}
	err := v.Unmarshal(&cfg)
	return v.AllSettings(), cfg, err
}

// readDir reads dir's config into v as PreProcessFlags does, with dir the
// directory it runs in.
func readDir(v *viper.Viper, dir string) error {
	file := Find(dir)
	if file == "" {
		return nil
	}
	if err := Read(v, file); err != nil {
		return err
	}
	if _, err := os.Stat(overridePath(dir)); err == nil {
		return Merge(v, overridePath(dir))
	}
	return nil
}

// utf16LE is s in UTF-16, little-endian, after a byte-order mark.
func utf16LE(s string) string {
	b := []byte{0xFF, 0xFE}
	for _, r := range utf16.Encode([]rune(s)) {
		b = append(b, byte(r), byte(r>>8))
	}
	return string(b)
}

// marshalledConfig is every field of keploy's default Config, marshalled as
// CreateConfigFile marshals it before trimming it to the flags used.
func marshalledConfig(t *testing.T) string {
	t.Helper()
	b, err := yaml.Marshal(config.New())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestReadMatchesViper is the differential test: for configs as keploy
// writes them and as people edit them -- nested blocks, keys in any case,
// lists, anchors and merge keys, the override -- and for the configs viper
// refuses -- a duplicate key, a value of the wrong type, YAML that does not
// parse -- the bounded loader reads exactly what viper reads, and refuses
// exactly what viper refuses, with viper's own error rather than a bound.
func TestReadMatchesViper(t *testing.T) {
	cases := []struct {
		name, body, override string
		wantErr              bool
	}{
		// keploy.yml as `keploy config --generate` writes it: every option,
		// with its comments.
		{name: "the default keploy.yml", body: config.GetDefaultConfig()},
		{name: "every field of the default Config", body: marshalledConfig(t)},
		{name: "the default keploy.yml with an override", body: config.GetDefaultConfig(), override: "test:\n  delay: 9\n  selectedTests:\n    test-set-1: [test-1]\nrecord:\n  filters:\n    - path: /health\n"},
		{name: "flat", body: "path: ./out\nport: 16789\ndebug: true\n"},
		{name: "nested", body: "record:\n  upstreamTls:\n    verify: true\n    caCert: /etc/ca.pem\n"},
		{name: "keys in any case", body: "Record:\n  RecordTimer: 5s\nAPPName: demo\n"},
		{name: "the override in another case", body: "record:\n  recordTimer: 5s\nappName: a\n", override: "RECORD:\n  RecordTimer: 7s\nAppName: b\n"},
		{name: "lists", body: "test:\n  selectedTests:\n    set-0:\n      - test-1\n      - test-2\n"},
		{name: "flow style", body: "test: {delay: 5, selectedTests: {set-0: [a, b]}}\nbypassRules: [{host: example.com, port: 80}]\n"},
		{name: "merge keys", body: "base: &b\n  verify: true\n  port: 1\none:\n  <<: *b\n  port: 2\n"},
		{name: "a list of merges", body: "a: &a {x: 1}\nb: &b {y: 2}\nc:\n  <<: [*a, *b]\n  z: 3\n"},
		{name: "dotted and nested keys together", body: "record:\n  upstreamTls.verify: true\n  upstreamTls:\n    caCert: /x\n"},
		{name: "a second document", body: "appName: first\n---\nappName: second\n"},
		{name: "an empty override", body: "path: base\n", override: "# nothing\n"},
		{name: "override", body: "path: base\nport: 1\n", override: "port: 2\nappName: over\n"},
		{name: "empty", body: "\n"},
		{name: "comment only", body: "# nothing here\n"},
		{name: "a repeated key", body: "port: 1\nport: 2\n", wantErr: true},
		{name: "a nested repeated key", body: "test:\n  delay: 1\n  delay: 2\n", wantErr: true},
		{name: "a value of the wrong type", body: "port: not-a-port\n", wantErr: true},
		{name: "YAML that does not parse", body: "a: [1, 2\n", wantErr: true},
		{name: "a list at the root", body: "- a\n- b\n", wantErr: true},
		{name: "an override that does not parse", body: "path: base\n", override: "a: [1\n", wantErr: true},
		{name: "an override with a repeated key", body: "path: base\n", override: "port: 1\nport: 2\n", wantErr: true},
		// viper reads UTF-16 with a byte-order mark; an earlier guard refused it.
		{name: "the default keploy.yml in UTF-16", body: utf16LE(config.GetDefaultConfig())},
		{name: "an override in UTF-16", body: "path: base\n", override: utf16LE("port: 2\nappName: over\n")},
		// An alias to a node it is inside: viper's own error, at once. An
		// earlier guard walked it for good.
		{name: "an alias cycle", body: "a: &a\n  b: *a\n", wantErr: true},
		{name: "an alias cycle through a merge", body: "a: &a\n  <<: *a\n", wantErr: true},
		{name: "an override with an alias cycle", body: "path: base\n", override: "a: &a [*a]\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeConfig(t, tc.body, tc.override)

			wantSettings, wantCfg, wantErr := readWithViper(dir)
			gotSettings, gotCfg, err := readWithLoader(dir)

			if (wantErr != nil) != tc.wantErr {
				t.Fatalf("oracle: viper error = %v, want an error: %v (the case is wrong)", wantErr, tc.wantErr)
			}
			if (err != nil) != (wantErr != nil) {
				t.Fatalf("loader error = %v, viper error = %v", err, wantErr)
			}
			if err != nil {
				if safeyaml.IsRefused(err) {
					t.Fatalf("refused as past a limit (%v); viper refuses it with: %v", err, wantErr)
				}
				if err.Error() != wantErr.Error() {
					t.Errorf("loader error %q is not viper's %q", err, wantErr)
				}
				return
			}
			if !reflect.DeepEqual(gotSettings, wantSettings) {
				t.Errorf("settings differ:\n loader=%#v\n viper =%#v", gotSettings, wantSettings)
			}
			if !reflect.DeepEqual(gotCfg, wantCfg) {
				t.Errorf("Config differs:\n loader=%+v\n viper =%+v", gotCfg, wantCfg)
			}
		})
	}
}

// A file keploy does not read is refused naming it, the config or the
// override; viper's own errors are viper's, as PreProcessFlags logs them.
func TestReadNamesTheFileItRefuses(t *testing.T) {
	dir := writeConfig(t, "port: 1\n", "port: 2\n")
	big := strings.Repeat("#", MaxConfigBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Read(newViper(), Find(dir)); !safeyaml.IsRefused(err) || !strings.HasPrefix(err.Error(), "keploy.yml: ") {
		t.Errorf("Read of a keploy.yml past the size limit = %v; want it refused, named", err)
	}
	if err := os.WriteFile(overridePath(dir), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Merge(newViper(), overridePath(dir)); !safeyaml.IsRefused(err) || !strings.HasPrefix(err.Error(), filepath.Base(overridePath(dir))+": ") {
		t.Errorf("Merge of an override past the size limit = %v; want it refused, named", err)
	}
}

// TestFindOrder pins keploy.yaml before keploy.yml, and that a directory is
// not a config file.
func TestFindOrder(t *testing.T) {
	dir := t.TempDir()
	if got := Find(dir); got != "" {
		t.Errorf("empty dir: Find=%q, want \"\"", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Find(dir); filepath.Base(got) != "keploy.yml" {
		t.Errorf("Find=%q, want keploy.yml", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "keploy.yaml"), []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Find(dir); filepath.Base(got) != "keploy.yaml" {
		t.Errorf("with both present, Find=%q, want keploy.yaml", got)
	}
	// A directory named keploy.yaml is skipped for keploy.yml.
	dir2 := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir2, "keploy.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "keploy.yml"), []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Find(dir2); filepath.Base(got) != "keploy.yml" {
		t.Errorf("keploy.yaml as a dir: Find=%q, want keploy.yml", got)
	}
}

// readWithin reads dir's config as PreProcessFlags does, failing the test if
// it has not returned within d: a FIFO the loader did not refuse blocks it for
// good.
func readWithin(t *testing.T, d time.Duration, v *viper.Viper, dir string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- readDir(v, dir) }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("the read did not return within %s: it blocked on the file", d)
		return nil
	}
}

// A keploy.yml that is a FIFO in a cloned repo blocked every keploy command
// there for good, in the open itself; it is refused at once, and keploy.yaml's
// failure is not passed over for keploy.yml.
func TestReadRefusesAFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on windows")
	}
	for _, name := range configNames {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			mkfifo(t, filepath.Join(dir, name))
			if name == "keploy.yaml" {
				if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte("port: 1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := readWithin(t, 5*time.Second, newViper(), dir)
			if !errors.Is(err, safeyaml.ErrNotRegular) {
				t.Fatalf("FIFO %s = %v, want ErrNotRegular", name, err)
			}
			if !strings.HasPrefix(err.Error(), name+": ") {
				t.Errorf("message %q does not name %s", err, name)
			}
		})
	}
}

// The override is bounded as the config is: a FIFO override beside a valid
// keploy.yml is refused at once.
func TestMergeRefusesAFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on windows")
	}
	dir := writeConfig(t, "port: 1\n", "")
	mkfifo(t, overridePath(dir))
	if err := readWithin(t, 5*time.Second, newViper(), dir); !errors.Is(err, safeyaml.ErrNotRegular) {
		t.Fatalf("FIFO override = %v, want ErrNotRegular", err)
	}
}

// A keploy.yml linked to /dev/zero ran every keploy command out of memory.
func TestReadRefusesADevice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/zero on windows")
	}
	dir := t.TempDir()
	if err := os.Symlink("/dev/zero", filepath.Join(dir, "keploy.yml")); err != nil {
		t.Fatal(err)
	}
	if err := readWithin(t, 5*time.Second, newViper(), dir); !errors.Is(err, safeyaml.ErrNotRegular) {
		t.Fatalf("keploy.yml -> /dev/zero = %v, want ErrNotRegular", err)
	}
}
