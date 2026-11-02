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
)

// keploy.yml's mock.local counts as --local does, through the CLI's own
// PreProcessFlags and ValidateFlags, as does MOCK_LOCAL; --local, when given,
// wins over it, also set false. The flag's default, read unguarded, dropped a
// committed `mock.local: true`.
func TestMockLocalRespectsKeployYml(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	t.Cleanup(viper.Reset)
	for _, verb := range []string{"record", "replay"} {
		for _, tc := range []struct {
			name, yml string
			env       map[string]string
			flags     []string
			want      bool
		}{
			{name: "keploy.yml mock.local, no flag", yml: "mock:\n  local: true\n", want: true},
			{name: "keploy.yml mock.local, --local=false", yml: "mock:\n  local: true\n", flags: []string{"--local=false"}, want: false},
			{name: "keploy.yml mock.local false, --local", yml: "mock:\n  local: false\n", flags: []string{"--local"}, want: true},
			{name: "keploy.yml mock.local false, no flag", yml: "mock:\n  local: false\n", want: false},
			{name: "no mock.local, no flag", yml: "mock:\n  name: orders\n", want: false},
			{name: "MOCK_LOCAL, no mock.local, no flag", yml: "mock:\n  name: orders\n", env: map[string]string{"MOCK_LOCAL": "true"}, want: true},
		} {
			t.Run(verb+": "+tc.name, func(t *testing.T) {
				viper.Reset()
				for k, v := range tc.env {
					t.Setenv(k, v)
				}
				dir := t.TempDir()
				t.Chdir(dir)
				if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(tc.yml), 0o644); err != nil {
					t.Fatal(err)
				}
				cmd, cfg, c := mockVerbCmd(t, dir, verb)
				if err := cmd.ParseFlags(append([]string{"-c", "python app.py", "-p", dir}, tc.flags...)); err != nil {
					t.Fatal(err)
				}
				if err := c.PreProcessFlags(cmd); err != nil {
					t.Fatalf("PreProcessFlags: %v", err)
				}
				if err := c.ValidateFlags(context.Background(), cmd); err != nil {
					t.Fatalf("ValidateFlags: %v", err)
				}
				if cfg.Mock.Local != tc.want {
					t.Fatalf("mock.local = %v, want %v", cfg.Mock.Local, tc.want)
				}
			})
		}
	}
}

// keploy.yml's mock.strict and mock.onMiss count as --strict and --on-miss do
// on `keploy mock replay`, as does the environment; the flag, when given,
// wins over both. The flags' defaults, read unguarded, dropped a committed
// `mock.strict: true` (a replay that missed its mocks still passed) and
// `mock.onMiss: passthrough`.
func TestMockReplayStrictAndOnMissRespectKeployYml(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	t.Cleanup(viper.Reset)
	for _, tc := range []struct {
		name, yml   string
		env         map[string]string
		flags       []string
		strict      bool
		onMiss      string
		errContains string
	}{
		{name: "keploy.yml mock.strict, no flag", yml: "mock:\n  strict: true\n", strict: true, onMiss: "fail"},
		{name: "keploy.yml mock.strict, --strict=false", yml: "mock:\n  strict: true\n", flags: []string{"--strict=false"}, strict: false, onMiss: "fail"},
		{name: "keploy.yml mock.strict false, --strict", yml: "mock:\n  strict: false\n", flags: []string{"--strict"}, strict: true, onMiss: "fail"},
		{name: "MOCK_STRICT, no keploy.yml strict", yml: "mock:\n  name: orders\n", env: map[string]string{"MOCK_STRICT": "true"}, strict: true, onMiss: "fail"},
		{name: "keploy.yml mock.onMiss, no flag", yml: "mock:\n  onMiss: passthrough\n", onMiss: "passthrough"},
		{name: "keploy.yml mock.onMiss, --on-miss fail", yml: "mock:\n  onMiss: passthrough\n", flags: []string{"--on-miss", "fail"}, onMiss: "fail"},
		{name: "keploy.yml mock.onMiss spelled loosely", yml: "mock:\n  onMiss: \" Record \"\n", onMiss: "record"},
		{name: "MOCK_ONMISS, no keploy.yml onMiss", yml: "mock:\n  name: orders\n", env: map[string]string{"MOCK_ONMISS": "record"}, onMiss: "record"},
		{name: "keploy.yml mock.onMiss unknown", yml: "mock:\n  onMiss: retry\n", errContains: `invalid on-miss value "retry"`},
		{name: "--on-miss unknown over keploy.yml", yml: "mock:\n  onMiss: record\n", flags: []string{"--on-miss", "retry"}, errContains: `invalid on-miss value "retry"`},
		{name: "neither set", yml: "mock:\n  name: orders\n", strict: false, onMiss: "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			dir := t.TempDir()
			t.Chdir(dir)
			if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(tc.yml), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd, cfg, c := mockVerbCmd(t, dir, "replay")
			if err := cmd.ParseFlags(append([]string{"-c", "python app.py", "-p", dir}, tc.flags...)); err != nil {
				t.Fatal(err)
			}
			if err := c.PreProcessFlags(cmd); err != nil {
				t.Fatalf("PreProcessFlags: %v", err)
			}
			err := c.ValidateFlags(context.Background(), cmd)
			if tc.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("ValidateFlags error = %v, want one containing %q", err, tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateFlags: %v", err)
			}
			if cfg.Mock.Strict != tc.strict || cfg.Mock.OnMiss != tc.onMiss {
				t.Fatalf("mock.strict = %v, mock.onMiss = %q; want %v, %q", cfg.Mock.Strict, cfg.Mock.OnMiss, tc.strict, tc.onMiss)
			}
		})
	}
}

// mockVerbCmd wires `keploy mock <verb>` the way the CLI does, with the
// config in dir so PreProcessFlags reads that keploy.yml.
func mockVerbCmd(t *testing.T, dir, verb string) (*cobra.Command, *config.Config, *CmdConfigurator) {
	t.Helper()
	cfg := &config.Config{}
	c := NewCmdConfigurator(zap.NewNop(), cfg)
	root := &cobra.Command{Use: "keploy"}
	root.PersistentFlags().String("config-path", dir, "")
	mock := &cobra.Command{Use: "mock"}
	sub := &cobra.Command{Use: verb}
	mock.AddCommand(sub)
	root.AddCommand(mock)
	if err := c.addMockFlags(sub); err != nil {
		t.Fatal(err)
	}
	return sub, cfg, c
}
