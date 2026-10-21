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
	"go.keploy.io/server/v3/utils"
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
		cfg.DisableMapping = viper.GetBool("disableMapping")
		cfg.Test.UpdateTestMapping = viper.GetBool("test.updateTestMapping")
		cfg.Test.RemoveUnusedMocks = viper.GetBool("test.removeUnusedMocks")
		cfg.Test.PreserveFailedMocks = viper.GetBool("test.preserveFailedMocks")
		cfg.Test.DisableStatefulMocks = viper.GetBool("test.disableStatefulMocks")
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
			if !utils.IsUsageError(err) {
				t.Fatalf("the refusal %v is not a usage error (exit 8), as cobra's unknown flag is", err)
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
		said := logs.FilterMessageSnippet("settings the proven model deletes are ignored").All()
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
		if n := len(logs.FilterMessageSnippet("settings the proven model deletes are ignored").All()); n != 0 {
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
		if n := len(logs.FilterMessageSnippet("settings the proven model deletes are ignored").All()); n != 0 {
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
		said := logs.FilterMessageSnippet("settings the proven model deletes are ignored").All()
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
		said := logs.FilterMessageSnippet("settings the proven model deletes are ignored").All()
		if len(said) != 1 || !strings.Contains(strings.Join(toStrings(said[0].ContextMap()["ignored"]), "\n"), "test.fuzzyMatch") {
			t.Fatalf("the notice %v does not name test.fuzzyMatch", said)
		}
	})
}

// The settings exact serving replaced (U18 rows of S12) are deleted where the
// proven model is the default (P220): under the switch their flags are
// unknown flags, which keploy refuses, and their keploy.yml keys are ignored
// and cleared, with the one notice naming each. With the switch off (Windows
// and macOS, or KEPLOY_PROVEN=0) they are taken as before.
func TestUnderTheSwitchTheSettingsExactServingReplacedAreDeleted(t *testing.T) {
	for _, flag := range []string{"--disable-mapping", "--update-test-mapping", "--remove-unused-mocks"} {
		t.Run(flag, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			_, _, err := deletedRun(t, "", flag)
			if err == nil || !strings.Contains(err.Error(), "unknown flag: "+flag) || !strings.Contains(err.Error(), "U18") {
				t.Fatalf("ValidateFlags: %v; want %s refused as an unknown flag", err, flag)
			}
			if !utils.IsUsageError(err) {
				t.Fatalf("the refusal %v is not a usage error (exit 8), as cobra's unknown flag is", err)
			}
			t.Setenv(provenswitch.Env, "0")
			if _, _, err := deletedRun(t, "", flag); err != nil {
				t.Fatalf("without the switch %s was refused: %v", flag, err)
			}
		})
	}

	yml := "disableMapping: true\ntest:\n  updateTestMapping: true\n  removeUnusedMocks: true\n  preserveFailedMocks: true\n  disableStatefulMocks: true\n"
	set := func(cfg *config.Config) []string {
		var on []string
		for name, v := range map[string]bool{
			"disableMapping":       cfg.DisableMapping,
			"updateTestMapping":    cfg.Test.UpdateTestMapping,
			"removeUnusedMocks":    cfg.Test.RemoveUnusedMocks,
			"preserveFailedMocks":  cfg.Test.PreserveFailedMocks,
			"disableStatefulMocks": cfg.Test.DisableStatefulMocks,
		} {
			if v {
				on = append(on, name)
			}
		}
		return on
	}
	t.Setenv(provenswitch.Env, "1")
	cfg, logs, err := deletedRun(t, yml)
	if err != nil {
		t.Fatalf("ValidateFlags: %v", err)
	}
	if kept := set(cfg); len(kept) != 0 {
		t.Fatalf("deleted keys were kept under the switch: %v", kept)
	}
	said := logs.FilterMessageSnippet("settings the proven model deletes are ignored").All()
	if len(said) != 1 {
		t.Fatalf("said %d times, want once", len(said))
	}
	ignored := strings.Join(toStrings(said[0].ContextMap()["ignored"]), "\n")
	for _, key := range []string{"disableMapping, from keploy.yml", "test.updateTestMapping, from keploy.yml", "test.removeUnusedMocks, from keploy.yml", "test.preserveFailedMocks, from keploy.yml", "test.disableStatefulMocks, from keploy.yml"} {
		if !strings.Contains(ignored, key) {
			t.Fatalf("the notice does not name %q: %s", key, ignored)
		}
	}

	t.Setenv(provenswitch.Env, "0")
	cfg, logs, err = deletedRun(t, yml)
	if err != nil {
		t.Fatalf("without the switch: %v", err)
	}
	if kept := set(cfg); len(kept) != 5 {
		t.Fatalf("without the switch only %v were kept, want all five", kept)
	}
	if n := len(logs.FilterMessageSnippet("settings the proven model deletes are ignored").All()); n != 0 {
		t.Fatalf("without the switch the notice was said %d times", n)
	}
}

// Under the switch a deleted setting's flag is an unknown flag, so --help
// does not list it either: a user who reads it there and passes it is refused
// (exit 8). --strict-failure, which the switch already holds on, says it
// changes nothing there instead of giving those flags as its remedy. Without
// the switch --help is as it was.
func TestUnderTheSwitchHelpListsNoDeletedFlag(t *testing.T) {
	// Every flag of the command the switch refuses: the deleted settings',
	// mock.real's passthrough flags (record and test), and test's readiness
	// ceiling.
	deletedFlags := map[string][]string{
		"test":   {"disable-mapping", "update-test-mapping", "remove-unused-mocks", "fallBack-on-miss", "global-passthrough", "mocking", "pass-through-ports", "health-poll-timeout"},
		"record": {"disable-mapping", "global-passthrough", "opportunistic-tls-intercept", "pass-through-ports"},
	}
	for _, on := range []bool{true, false} {
		value := "0"
		if on {
			value = "1"
		}
		t.Run("KEPLOY_PROVEN="+value, func(t *testing.T) {
			t.Setenv(provenswitch.Env, value)
			for name, flags := range deletedFlags {
				c := NewCmdConfigurator(zap.NewNop(), config.New())
				cmd := &cobra.Command{Use: name}
				if err := c.AddFlags(cmd); err != nil {
					t.Fatalf("AddFlags(%s): %v", name, err)
				}
				for _, flag := range flags {
					f := cmd.Flags().Lookup(flag)
					if f == nil {
						t.Fatalf("keploy %s has no --%s", name, flag)
					}
					// --fallBack-on-miss is deprecated, so hidden either way.
					if hidden := f.Hidden || f.Deprecated != ""; hidden != on && flag != "fallBack-on-miss" {
						t.Errorf("keploy %s --help lists --%s: %v, want %v", name, flag, !hidden, !on)
					}
					if on && !(f.Hidden || f.Deprecated != "") {
						t.Errorf("under the switch keploy %s --help lists the deleted --%s", name, flag)
					}
				}
				if name != "test" {
					continue
				}
				usage := cmd.Flags().Lookup("strict-failure").Usage
				gives := strings.Contains(usage, "--disable-mapping") || strings.Contains(usage, "--update-test-mapping")
				if on && (gives || !strings.Contains(usage, "changes nothing")) {
					t.Errorf("under the switch --strict-failure's help gives the deleted flags, or does not say it changes nothing: %q", usage)
				}
				if !on && !gives {
					t.Errorf("without the switch --strict-failure's help lost its remedy: %q", usage)
				}
			}
		})
	}
}

// keploy mock record and replay keep their own --pass-through-ports (the
// app's ingress port) under the switch, so their --help still lists it.
// Mutation: hide the passthrough flags of every command named record.
func TestUnderTheSwitchMockKeepsItsPassThroughPortsInHelp(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, verb := range []string{"record", "replay"} {
		c := NewCmdConfigurator(zap.NewNop(), config.New())
		parent := &cobra.Command{Use: "mock"}
		cmd := &cobra.Command{Use: verb}
		parent.AddCommand(cmd)
		if err := c.AddFlags(cmd); err != nil {
			t.Fatalf("AddFlags(mock %s): %v", verb, err)
		}
		f := cmd.Flags().Lookup("pass-through-ports")
		if f == nil {
			t.Fatalf("keploy mock %s has no --pass-through-ports", verb)
		}
		if f.Hidden {
			t.Errorf("under the switch keploy mock %s --help does not list its own --pass-through-ports", verb)
		}
	}
}

// keploy mock replay refuses --strict and --run-only under the switch (the
// proven model's verdict fails a run on every call the recording did not
// answer, and keploy skips only the tests verdicts.yaml quarantines), so its
// --help does not list them either. Without the switch it lists both, and
// keploy mock record, which refuses neither, lists what it has as before.
// Mutation: hide neither, or hide them without the switch.
func TestUnderTheSwitchMockReplayHelpListsNoFlagItRefuses(t *testing.T) {
	for _, on := range []bool{true, false} {
		value := "0"
		if on {
			value = "1"
		}
		t.Run("KEPLOY_PROVEN="+value, func(t *testing.T) {
			t.Setenv(provenswitch.Env, value)
			c := NewCmdConfigurator(zap.NewNop(), config.New())
			parent := &cobra.Command{Use: "mock"}
			cmd := &cobra.Command{Use: "replay"}
			parent.AddCommand(cmd)
			if err := c.AddFlags(cmd); err != nil {
				t.Fatalf("AddFlags(mock replay): %v", err)
			}
			for _, flag := range []string{"strict", "run-only"} {
				f := cmd.Flags().Lookup(flag)
				if f == nil {
					t.Fatalf("keploy mock replay has no --%s", flag)
				}
				if f.Hidden != on {
					t.Errorf("keploy mock replay --help lists --%s: %v, want %v", flag, !f.Hidden, !on)
				}
			}
		})
	}
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
