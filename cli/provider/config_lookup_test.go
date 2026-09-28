package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Keploy's configuration is keploy.yml (what CreateConfigFile writes), or
// keploy.yaml. Nothing else in --config-path is it.
//
// The lookup used to be viper's generic one -- SetConfigName("keploy") plus
// SetConfigType("yml") -- which also accepts keploy.json/.toml/.env/.ini/...
// and, because a config type is set, a file named just `keploy`, all parsed as
// YAML. `keploy` is the binary itself: download it into a project and run a
// command beside it and, until a keploy.yml existed, it stopped with "failed
// to read config file" -- the YAML parser rejecting the binary.
func TestConfigLookupReadsOnlyKeployYAML(t *testing.T) {
	cases := []struct {
		name       string
		files      map[string]string
		wantFound  bool
		wantVerify bool
	}{
		{name: "the binary beside no config", files: map[string]string{"keploy": "\x7fELF\x02\x01\x01\xff\xfe binary"}, wantFound: false},
		{name: "any text file named keploy", files: map[string]string{"keploy": "just text\n"}, wantFound: false},
		{name: "the binary beside keploy.yml", files: map[string]string{"keploy": "\x7fELF\xff", "keploy.yml": verifyYAML}, wantFound: true, wantVerify: true},
		{name: "another tool's keploy.env", files: map[string]string{"keploy.env": "A=1\n"}, wantFound: false},
		{name: "keploy.yaml", files: map[string]string{"keploy.yaml": verifyYAML}, wantFound: true, wantVerify: true},
		// keploy's own test-data directory, and a directory with a config's
		// name: neither is a config file. (A name ending in / is a directory.)
		{name: "the keploy/ test-data directory", files: map[string]string{"keploy/": ""}, wantFound: false},
		{name: "a directory named keploy.yaml", files: map[string]string{"keploy.yaml/": "", "keploy.yml": verifyYAML}, wantFound: true, wantVerify: true},
		// Both spellings: keploy.yaml, as viper's lookup ordered them.
		{name: "keploy.yaml beside keploy.yml", files: map[string]string{"keploy.yaml": verifyYAML, "keploy.yml": "record: {}\n"}, wantFound: true, wantVerify: true},
		// viper tried .json and .toml before .yaml/.yml, and a comment-only
		// file parses as empty YAML: another tool's keploy.toml silently
		// replaced keploy.yml.
		{name: "a comment-only keploy.toml beside keploy.yml", files: map[string]string{"keploy.toml": "# another tool's settings\n", "keploy.yml": verifyYAML}, wantFound: true, wantVerify: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			saved := IsConfigFileFound
			t.Cleanup(func() { IsConfigFileFound = saved; viper.Reset() })
			IsConfigFileFound = false

			dir := t.TempDir()
			for name, body := range tc.files {
				if strings.HasSuffix(name, "/") {
					if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.New()
			cmd := newRecordCmdForTest(t, cfg)
			if err := cmd.Flags().Set("configPath", dir); err != nil {
				t.Fatalf("set configPath: %v", err)
			}
			if err := NewCmdConfigurator(zap.NewNop(), cfg).PreProcessFlags(cmd); err != nil {
				t.Fatalf("PreProcessFlags: %v", err)
			}
			if IsConfigFileFound != tc.wantFound {
				t.Errorf("config found = %v, want %v", IsConfigFileFound, tc.wantFound)
			}
			if cfg.Record.UpstreamTLS.Verify != tc.wantVerify {
				t.Errorf("record.upstreamTls.verify = %v, want %v (was the right file read?)", cfg.Record.UpstreamTLS.Verify, tc.wantVerify)
			}
		})
	}
}

const verifyYAML = "record:\n  upstreamTls:\n    verify: true\n"

// preProcessIn runs PreProcessFlags for a record command whose --config-path
// is configDir, from appDir -- where the <appDir>.keploy.yml override is
// looked for -- logging to an observer.
func preProcessIn(t *testing.T, configDir, appDir string) (*config.Config, *observer.ObservedLogs, error) {
	t.Helper()
	viper.Reset()
	saved := IsConfigFileFound
	t.Cleanup(func() { IsConfigFileFound = saved; viper.Reset() })
	t.Chdir(appDir)
	core, logs := observer.New(zap.InfoLevel)
	cfg := config.New()
	cmd := newRecordCmdForTest(t, cfg)
	if err := cmd.Flags().Set("configPath", configDir); err != nil {
		t.Fatalf("set configPath: %v", err)
	}
	return cfg, logs, NewCmdConfigurator(zap.New(core), cfg).PreProcessFlags(cmd)
}

// The <dir>.keploy.yml override in the directory keploy runs in is merged over
// keploy.yml, and said so; one that cannot be read is named, not blamed on
// keploy.yml.
func TestPreProcessFlagsMergesTheOverride(t *testing.T) {
	configDir := writeKeployYML(t, "record:\n  upstreamTls:\n    verify: false\n")
	appDir := t.TempDir()
	override := filepath.Join(appDir, filepath.Base(appDir)+".keploy.yml")
	if err := os.WriteFile(override, []byte(verifyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, logs, err := preProcessIn(t, configDir, appDir)
	if err != nil {
		t.Fatalf("PreProcessFlags: %v", err)
	}
	if !cfg.Record.UpstreamTLS.Verify {
		t.Errorf("record.upstreamTls.verify = false, want the override's true")
	}
	if got := logs.FilterMessage("merged override config file").All(); len(got) != 1 || got[0].ContextMap()["file"] != override {
		t.Errorf("logged %v, want the merged override named", got)
	}

	if err := os.WriteFile(override, []byte("record: [1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, logs, err = preProcessIn(t, configDir, appDir)
	want := "failed to merge override config file: " + override
	if err == nil || err.Error() != want {
		t.Fatalf("PreProcessFlags = %v, want %q", err, want)
	}
	if got := logs.FilterMessage(want).All(); len(got) != 1 {
		t.Errorf("logged %v, want %q once", got, want)
	}
}

// An override keploy cannot even stat -- here a symlink to itself -- stops
// the command in the CLI's own words, naming it. Mutation: report it as a
// failed merge, or as no override at all.
func TestPreProcessFlagsNamesAnOverrideItCannotStat(t *testing.T) {
	configDir := writeKeployYML(t, verifyYAML)
	appDir := t.TempDir()
	override := filepath.Join(appDir, filepath.Base(appDir)+".keploy.yml")
	if err := os.Symlink(override, override); err != nil {
		t.Skipf("no symlink: %v", err)
	}
	_, logs, err := preProcessIn(t, configDir, appDir)
	want := "failed to stat override config file: " + override
	if err == nil || err.Error() != want {
		t.Fatalf("PreProcessFlags = %v, want %q", err, want)
	}
	if got := logs.FilterMessage(want).All(); len(got) != 1 {
		t.Errorf("logged %v, want %q once", got, want)
	}
}
