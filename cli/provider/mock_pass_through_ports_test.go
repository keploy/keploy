package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
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
