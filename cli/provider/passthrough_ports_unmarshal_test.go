package provider

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// --pass-through-ports aborted `keploy record` and `keploy mock record` with
//
//	'record.passThroughPorts[0]' expected a map or struct, got "uint"
//
// because BindFlagsToViper derives <cmd.Name()>.<camelCase(flag)>, and both
// commands are named "record", so the []uint flag landed on
// Record.PassThroughPorts ([]models.PassThroughRule). These tests assert on the
// failure itself -- viper.Unmarshal -- rather than on key presence.
func bindPassThroughPortsOnRecordCmd(t *testing.T, annotate bool) error {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)

	cmd := &cobra.Command{Use: "record"}
	cmd.Flags().UintSlice("pass-through-ports", nil, "")
	if annotate {
		if err := cmd.Flags().SetAnnotation("pass-through-ports", utils.NoViperBindAnnotation, []string{"true"}); err != nil {
			t.Fatalf("SetAnnotation: %v", err)
		}
	}
	if err := cmd.Flags().Set("pass-through-ports", "3000"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := utils.BindFlagsToViper(zap.NewNop(), cmd, ""); err != nil {
		t.Fatalf("BindFlagsToViper: %v", err)
	}
	return viper.Unmarshal(&config.Config{})
}

func TestPassThroughPorts_UnmarshalSucceedsWhenAnnotated(t *testing.T) {
	if err := bindPassThroughPortsOnRecordCmd(t, true); err != nil {
		t.Fatalf("viper.Unmarshal failed with the annotation in place: %v", err)
	}
}

// Control: without the annotation the defect must still be reproducible, so the
// test above cannot pass for the wrong reason.
func TestPassThroughPorts_UnmarshalFailsWithoutAnnotation(t *testing.T) {
	err := bindPassThroughPortsOnRecordCmd(t, false)
	if err == nil {
		t.Fatal("viper.Unmarshal unexpectedly succeeded without the annotation; " +
			"the collision this test guards may have been fixed another way")
	}
	t.Logf("defect reproduced as expected: %v", err)
}

// COEXISTENCE: a user may legitimately have telemetry-egress rules under
// record.passThroughPorts in keploy.yml AND pass --pass-through-ports on the
// CLI. They are unrelated features that merely share a name, so both must
// survive: the yaml rules must decode intact and the flag must stay readable.
// On unpatched code the flag's viper bind overwrites the yaml value and the
// unmarshal dies.
func TestPassThroughPorts_YamlRulesCoexistWithFlag(t *testing.T) {
	const keployYML = `
record:
  passThroughPorts:
    - port: 4317
      mode: skip
    - port: 8126
      mode: recordOne
`
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.SetConfigType("yaml")
	if err := viper.ReadConfig(strings.NewReader(keployYML)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	cmd := &cobra.Command{Use: "record"}
	cmd.Flags().UintSlice("pass-through-ports", nil, "")
	if err := cmd.Flags().SetAnnotation("pass-through-ports", utils.NoViperBindAnnotation, []string{"true"}); err != nil {
		t.Fatalf("SetAnnotation: %v", err)
	}
	if err := cmd.Flags().Set("pass-through-ports", "7001"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := utils.BindFlagsToViper(zap.NewNop(), cmd, ""); err != nil {
		t.Fatalf("BindFlagsToViper: %v", err)
	}

	var cfg config.Config
	if err := viper.Unmarshal(&cfg); err != nil {
		t.Fatalf("viper.Unmarshal failed with yaml rules + CLI flag together: %v", err)
	}

	// The yaml telemetry rules must survive untouched.
	if got := len(cfg.Record.PassThroughPorts); got != 2 {
		t.Fatalf("Record.PassThroughPorts has %d rules, want 2 (the flag clobbered the yaml): %+v",
			got, cfg.Record.PassThroughPorts)
	}
	if p := cfg.Record.PassThroughPorts[0]; p.Port != 4317 || p.Mode != models.PassThroughSkip {
		t.Errorf("rule[0] = %+v, want {Port:4317 Mode:skip}", p)
	}
	if p := cfg.Record.PassThroughPorts[1]; p.Port != 8126 || p.Mode != models.PassThroughRecordOne {
		t.Errorf("rule[1] = %+v, want {Port:8126 Mode:recordOne}", p)
	}

	// ...and the CLI flag must still be readable by its direct consumers.
	ports, err := cmd.Flags().GetUintSlice("pass-through-ports")
	if err != nil {
		t.Fatalf("GetUintSlice: %v", err)
	}
	if len(ports) != 1 || ports[0] != 7001 {
		t.Errorf("flag value = %v, want [7001]", ports)
	}
}

// Control for the test above: WITHOUT the annotation the flag's viper bind
// overwrites the yaml rules and the unmarshal dies, so the coexistence test
// cannot pass for the wrong reason. This is the unpatched behaviour, kept in CI
// permanently rather than demonstrated once by reverting the fix locally.
func TestPassThroughPorts_YamlRulesClobberedWithoutAnnotation(t *testing.T) {
	const keployYML = `
record:
  passThroughPorts:
    - port: 4317
      mode: skip
`
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.SetConfigType("yaml")
	if err := viper.ReadConfig(strings.NewReader(keployYML)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	cmd := &cobra.Command{Use: "record"}
	cmd.Flags().UintSlice("pass-through-ports", nil, "") // deliberately NOT annotated
	if err := cmd.Flags().Set("pass-through-ports", "7001"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := utils.BindFlagsToViper(zap.NewNop(), cmd, ""); err != nil {
		t.Fatalf("BindFlagsToViper: %v", err)
	}

	var cfg config.Config
	if err := viper.Unmarshal(&cfg); err == nil {
		t.Fatalf("unmarshal unexpectedly succeeded; yaml rules survived as %+v -- "+
			"the collision this fix guards may have been resolved another way",
			cfg.Record.PassThroughPorts)
	} else {
		t.Logf("unpatched behaviour reproduced: %v", err)
	}
}
