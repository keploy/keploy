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

// A 3.8.x keploy.yml: record.passThroughPorts holds telemetry rules, not ports.
const keployYmlWithRules = `record:
  passThroughPorts:
    - port: 4317
      mode: skip
`

// mockRecordCmd wires `keploy mock record` the way the CLI does, with the
// config in dir so PreProcessFlags reads that keploy.yml.
func mockRecordCmd(t *testing.T, dir string) (*cobra.Command, *config.Config, *CmdConfigurator) {
	t.Helper()
	cfg := &config.Config{}
	c := NewCmdConfigurator(zap.NewNop(), cfg)
	root := &cobra.Command{Use: "keploy"}
	root.PersistentFlags().String("config-path", dir, "")
	mock := &cobra.Command{Use: "mock"}
	record := &cobra.Command{Use: "record"}
	mock.AddCommand(record)
	root.AddCommand(mock)
	if err := c.addMockFlags(record); err != nil {
		t.Fatal(err)
	}
	return record, cfg, c
}

// --pass-through-ports on `keploy mock record` used to be bound to
// record.passThroughPorts, whose entries are rules, and the config failed to
// load with `expected a map or struct, got "uint"`.
func TestMockRecordPassThroughPortsLoadWithARulesConfig(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(keployYmlWithRules), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, cfg, c := mockRecordCmd(t, dir)
	if err := cmd.ParseFlags([]string{"--pass-through-ports", "8080"}); err != nil {
		t.Fatal(err)
	}

	if err := c.PreProcessFlags(cmd); err != nil {
		t.Fatalf("PreProcessFlags: %v", err)
	}

	if len(cfg.Record.PassThroughPorts) != 1 || cfg.Record.PassThroughPorts[0].Port != 4317 {
		t.Fatalf("the keploy.yml rule was lost: %+v", cfg.Record.PassThroughPorts)
	}
	ports, err := cmd.Flags().GetUintSlice("pass-through-ports")
	if err != nil {
		t.Fatal(err)
	}
	config.SetByPassPorts(cfg, ports)
	if got := config.GetByPassPorts(cfg); len(got) != 1 || got[0] != 8080 {
		t.Fatalf("the flag did not reach bypassRules: %v", got)
	}
}

// keploy mock refuses at start, as record and test do, a bypassRule with a
// host or a path on a mysqlPorts port, naming it and the port-only rule; a
// port-only rule on that port is taken.
func TestMockRecordRefusesAHostRuleOnAMysqlPort(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	t.Cleanup(viper.Reset)
	for _, tc := range []struct {
		name, rule, refused string
	}{
		{"a host rule", "  - host: db.internal\n    port: 3307\n", `{host: "db.internal", path: "", port: 3307}`},
		{"a port-only rule", "  - port: 3307\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			dir := t.TempDir()
			t.Chdir(dir)
			yml := "mysqlPorts: [3307]\nbypassRules:\n" + tc.rule
			if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(yml), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd, cfg, c := mockRecordCmd(t, dir)
			if err := cmd.ParseFlags([]string{"-c", "python app.py", "-p", dir}); err != nil {
				t.Fatal(err)
			}
			if err := c.PreProcessFlags(cmd); err != nil {
				t.Fatalf("PreProcessFlags: %v", err)
			}
			err := c.ValidateFlags(context.Background(), cmd)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) || !strings.Contains(err.Error(), "{port: N}") {
					t.Fatalf("ValidateFlags = %v, want a refusal naming %s and the port-only rule", err, tc.refused)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateFlags = %v, want a port-only rule on a mysql port taken", err)
			}
			if got := config.GetByPassPorts(cfg); len(got) != 1 || got[0] != 3307 {
				t.Fatalf("the kernel's ports = %v, want [3307]", got)
			}
		})
	}
}
