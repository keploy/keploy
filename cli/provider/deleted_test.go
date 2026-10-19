package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// deletedRun runs keploy test's flag handling with args and keploy.yml yml,
// and returns the config it left, its warnings, and its error.
func deletedRun(t *testing.T, yml string, args ...string) (*config.Config, *observer.ObservedLogs, error) {
	t.Helper()
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	viper.Reset()
	t.Cleanup(viper.Reset)
	if yml != "" {
		viper.SetConfigType("yml")
		if err := viper.ReadConfig(strings.NewReader(yml)); err != nil {
			t.Fatal(err)
		}
	}
	return deletedRunKeepingViper(t, yml != "", args...)
}

// deletedRunKeepingViper is deletedRun over viper as the caller set it up,
// reading its values into the config when read is set.
func deletedRunKeepingViper(t *testing.T, read bool, args ...string) (*config.Config, *observer.ObservedLogs, error) {
	t.Helper()
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	cfg := config.New()
	if read {
		// What PreProcessFlags' viper.Unmarshal reads from it.
		cfg.InCi = viper.GetBool("inCi")
		cfg.Test.FallBackOnMiss = viper.GetBool("test.fallBackOnMiss")
	}
	core, logs := observer.New(zap.WarnLevel)
	c := NewCmdConfigurator(zap.New(core), cfg)
	cmd := &cobra.Command{Use: "test"}
	if err := c.AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "keploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags(append([]string{"-c", "python app.py", "-p", dir}, args...)); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	cfg.Command, cfg.Path = "python app.py", dir
	return cfg, logs, c.ValidateFlags(context.Background(), cmd)
}

// Under the proven switch a setting the proven model deletes (U18) that
// nothing reads is gone: its flag is an unknown flag, which keploy refuses,
// and its keploy.yml key is ignored, with one notice naming each key and its
// row. Without the switch both are taken as before.
func TestUnderTheSwitchADeletedSettingsFlagIsUnknownAndItsKeyIgnored(t *testing.T) {
	for _, args := range [][]string{{"--in-ci"}, {"--inCi"}, {"--fallBack-on-miss"}, {"--fallBackOnMiss"}} {
		t.Run(args[0], func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			_, _, err := deletedRun(t, "", args...)
			if err == nil || !strings.Contains(err.Error(), "unknown flag") || !strings.Contains(err.Error(), "U18") {
				t.Fatalf("ValidateFlags: %v; want the deleted flag refused as unknown", err)
			}

			t.Setenv(provenswitch.Env, "0")
			if _, _, err := deletedRun(t, "", args...); err != nil {
				t.Fatalf("without the switch: %v", err)
			}
		})
	}

	yml := "inCi: true\ntest:\n  fallBackOnMiss: true\n  delay: 7\n"
	t.Run("keploy.yml", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		cfg, logs, err := deletedRun(t, yml)
		if err != nil {
			t.Fatalf("ValidateFlags: %v", err)
		}
		if cfg.InCi || cfg.Test.FallBackOnMiss {
			t.Fatalf("a deleted key was kept: inCi %v, fallBackOnMiss %v", cfg.InCi, cfg.Test.FallBackOnMiss)
		}
		said := logs.FilterMessageSnippet("settings keploy deletes are ignored").All()
		if len(said) != 1 {
			t.Fatalf("said %d times, want once", len(said))
		}
		ignored := said[0].ContextMap()["ignored"]
		for _, key := range []string{"inCi, from keploy.yml (U18: read by nothing", "test.fallBackOnMiss, from keploy.yml (U18: read by nothing"} {
			if !strings.Contains(strings.Join(toStrings(ignored), "\n"), key) {
				t.Fatalf("the notice does not name %q: %v", key, ignored)
			}
		}

		t.Setenv(provenswitch.Env, "0")
		cfg, logs, err = deletedRun(t, yml)
		if err != nil || !cfg.InCi || !cfg.Test.FallBackOnMiss {
			t.Fatalf("without the switch: %v, inCi %v, fallBackOnMiss %v", err, cfg.InCi, cfg.Test.FallBackOnMiss)
		}
		if n := len(logs.FilterMessageSnippet("settings keploy deletes are ignored").All()); n != 0 {
			t.Fatalf("without the switch the notice was said %d times", n)
		}
	})

	// A key at its zero value sets nothing: an older keploy wrote its whole
	// default config, these keys among it, so it is not said.
	t.Run("keploy.yml at the zero values", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		_, logs, err := deletedRun(t, "inCi: false\ntest:\n  fallBackOnMiss: false\n")
		if err != nil {
			t.Fatalf("ValidateFlags: %v", err)
		}
		if n := len(logs.FilterMessageSnippet("settings keploy deletes are ignored").All()); n != 0 {
			t.Fatalf("keys at their zero values were said %d times", n)
		}
	})

	// The environment sets a key as keploy.yml does (viper's AutomaticEnv):
	// it is ignored, cleared and said the same way.
	t.Run("the environment", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		t.Setenv("KEPLOY_INCI", "true")
		original := NativeCommandSupported
		t.Cleanup(func() { NativeCommandSupported = original })
		viper.Reset()
		t.Cleanup(viper.Reset)
		viper.AutomaticEnv()
		viper.SetEnvPrefix("KEPLOY")
		cfg, logs, err := deletedRunKeepingViper(t, true)
		if err != nil {
			t.Fatalf("ValidateFlags: %v", err)
		}
		if cfg.InCi {
			t.Fatal("inCi from the environment was kept")
		}
		said := logs.FilterMessageSnippet("settings keploy deletes are ignored").All()
		if len(said) != 1 || !strings.Contains(strings.Join(toStrings(said[0].ContextMap()["ignored"]), "\n"), "inCi, from the environment's KEPLOY_INCI") {
			t.Fatalf("the notice %v does not name the environment's KEPLOY_INCI", said)
		}
	})

	// A row keploy's enterprise build registers is checked the same way.
	t.Run("a registered row", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		saved := deletedSettings()
		t.Cleanup(func() {
			deletedMu.Lock()
			deleted = saved
			deletedMu.Unlock()
		})
		RegisterDeletedSettings(DeletedSetting{Row: "read by nothing", Keys: []string{"test.fuzzyMatch"}, Instead: "nothing reads it: remove it"})
		_, logs, err := deletedRun(t, "test:\n  fuzzyMatch: true\n")
		if err != nil {
			t.Fatalf("ValidateFlags: %v", err)
		}
		said := logs.FilterMessageSnippet("settings keploy deletes are ignored").All()
		if len(said) != 1 || !strings.Contains(strings.Join(toStrings(said[0].ContextMap()["ignored"]), "\n"), "test.fuzzyMatch") {
			t.Fatalf("the notice %v does not name test.fuzzyMatch", said)
		}
	})
}

func toStrings(v interface{}) []string {
	switch vs := v.(type) {
	case []string:
		return vs
	case []interface{}:
		out := make([]string, len(vs))
		for i, x := range vs {
			out[i], _ = x.(string)
		}
		return out
	}
	return nil
}
